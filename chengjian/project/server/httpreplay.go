// httpreplay.go 结构化 HTTP 重放（W1/T-16 执行层）。
//
// webtest 包早已给出三个纯函数原语：Replay（单点变异）、Compare/CompareWithNoise
// （噪声标定的差分判定）、Digest。缺的是把它们接到真实世界的三根线：
//  1. **基线来源**：录制流量里的一条请求原文（traffic_get 同源的 raw 文本）；
//  2. **发送通道**：经任务 MITM 实例发出——变异请求照常被录制（留痕链不断）、
//     且内网目标自动经任务隧道路由（RouteTable 选路），与 worker 的 curl 同路；
//  3. **对照纪律**：默认先对同一基线跑两次对照组标定目标自身抖动
//     （CompareWithNoise），再判变体——没有噪声标定的差分，判据只是猜。
//
// 与 worker 手写 curl 的本质差别不在「能发请求」，而在**可归因**：单点变异 +
// 噪声标定 + 状态/正文双信号，把「响应变了」翻译成「这次变异改变了目标行为」
// 或「没有改变」——这正是误报率的主战场。
//
// 安全边界：目标 host 过 RoE 任务范围校验；发送走任务代理（受 guard/审计同一
// 通道）；对照+变体默认共 3 次请求，是有界动作。
package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/webtest"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// replayTimeout 是单次重放请求的墙钟上限。
const replayTimeout = 30 * time.Second

// replayMaxBody 是返回给模型的正文截断（变体正文是判断依据，太大只会淹没结论）。
const replayMaxBody = 1500

// parseRecordedRequest 把录制层的请求 raw 文本解析成 webtest.Request。
//
// raw 形态（traffic.assembleRaw）：请求行 + 头 + 空行 + 正文，头与正文的分隔
// 兼容 \r\n\r\n 与 \n\n（历史数据两种都有）。URL 由 Host 头 + 请求行 path 拼成，
// scheme 录制层没存，由调用方给（默认 http）。
func parseRecordedRequest(raw, scheme string) (webtest.Request, error) {
	head, body := raw, ""
	crlf, lf := strings.Index(raw, "\r\n\r\n"), strings.Index(raw, "\n\n")
	if crlf >= 0 && (lf < 0 || crlf < lf) {
		head, body = raw[:crlf], raw[crlf+4:]
	} else if lf >= 0 {
		head, body = raw[:lf], raw[lf+2:]
	}
	head = strings.ReplaceAll(head, "\r\n", "\n")
	lines := strings.Split(head, "\n")
	if len(lines) == 0 {
		return webtest.Request{}, fmt.Errorf("空请求原文")
	}
	// 跳过录制层可能的杂散前缀行，找请求行（METHOD path HTTP/x）。
	var reqParts []string
	var start int
	for i, l := range lines {
		parts := strings.Fields(l)
		if len(parts) == 3 && strings.HasPrefix(parts[2], "HTTP/") {
			reqParts, start = parts, i
			break
		}
	}
	if reqParts == nil {
		return webtest.Request{}, fmt.Errorf("请求原文里找不到请求行（METHOD path HTTP/x.y）")
	}
	method, path := reqParts[0], reqParts[1]
	host := ""
	headers := [][2]string{}
	for _, l := range lines[start+1:] {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		headers = append(headers, [2]string{k, v})
		if strings.EqualFold(k, "host") {
			host = v
		}
	}
	if host == "" {
		return webtest.Request{}, fmt.Errorf("请求原文缺 Host 头，无法确定目标")
	}
	if scheme == "" {
		scheme = "http"
	}
	return webtest.Request{
		Method:  strings.ToUpper(strings.TrimSpace(method)),
		URL:     scheme + "://" + host + path,
		Headers: headers,
		Body:    []byte(body),
	}, nil
}

// sendReplayRequest 发送一次重放请求。proxyAddr 非空时经任务 MITM（录制 + 隧道
// 选路都由它完成）；TLS 跳过校验——经 MITM 时证书是其实例 CA 重签的，直连时
// 渗透目标本就没有可信 CA（与 lateral SSH 的 InsecureIgnoreHostKey 同一判断）。
// 返回的 Body 有界读取（2MB 上限，防超大响应吃内存）。
func sendReplayRequest(ctx context.Context, req webtest.Request, proxyAddr string, timeout time.Duration) (webtest.Response, error) {
	if timeout <= 0 {
		timeout = replayTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, uerr := url.Parse(req.URL); uerr != nil {
		return webtest.Response{}, fmt.Errorf("URL %q 非法: %w", req.URL, uerr)
	}
	hreq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return webtest.Response{}, err
	}
	// 录制的头原样带上（含 Cookie/Authorization——重放的意义就在身份一致），
	// 但 Content-Length 由 net/http 按新正文重算，Host 由 URL 推导。
	for _, kv := range req.Headers {
		if strings.EqualFold(kv[0], "content-length") || strings.EqualFold(kv[0], "host") {
			continue
		}
		hreq.Header.Add(kv[0], kv[1])
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	defer transport.CloseIdleConnections()
	if proxyAddr != "" {
		pu, perr := url.Parse(proxyAddr)
		if perr != nil || pu.Host == "" {
			return webtest.Response{}, fmt.Errorf("无效任务代理地址")
		}
		transport.Proxy = http.ProxyURL(pu)
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // 不跟重定向：3xx 本身是差分信号
	}}
	resp, err := client.Do(hreq)
	if err != nil {
		return webtest.Response{}, err
	}
	defer resp.Body.Close()
	const limit = 2 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return webtest.Response{}, fmt.Errorf("读响应正文失败: %w", err)
	}
	truncated := len(body) > limit
	if truncated {
		body = body[:limit]
	}
	return webtest.Response{Status: resp.StatusCode, Headers: resp.Header, Body: body, Truncated: truncated}, nil
}

// httpReplayTool 是结构化 HTTP 重放工具（绑定 worker）。
func (s *Server) httpReplayTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "http_replay",
		Description: "【结构化重放·差分验证】从录制流量取一条请求做基线，施加**单点变异**重放，" +
			"并按对照实验纪律给结论：默认先对同一基线发两次对照组标定目标自身抖动，再判变体差异" +
			"（verdict: different/same/inconclusive，仅为实验差异，不能直接确认或否定漏洞）。" +
			"变异类别 kind：path/query/body/header/cookie/method（单次一个点，归因才成立；一次改多处用多次调用逐步做）。" +
			"经任务代理发出：变异请求照常被录制、内网目标自动走任务隧道，与你的 curl 同路同留痕。" +
			"典型用法：traffic_search/traffic_get 找到可疑请求 → http_replay 换 payload 看差异 → " +
			"different 且差异可解释才是真信号；same 是有价值的阴性（记录下来，别硬报）。比手写 curl 的价值在**可归因**，不是能发请求。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"traffic_id": map[string]any{"type": "string", "description": "基线请求的流量 ID（traffic_search/traffic_get 返回的 id，必填）"},
				"scheme":     map[string]any{"type": "string", "description": "基线 URL 的 scheme（录制层未存，默认 http；HTTPS 站点填 https）"},
				"mutation": map[string]any{"type": "object", "description": "单点变异（必填）：{kind: path|query|body|header|cookie|method, name: 参数名, from: 原值（校验基线用）, to: 新值, replace: body 整体替换时 true}",
					"properties": map[string]any{
						"kind":    map[string]any{"type": "string", "enum": []string{"path", "query", "body", "header", "cookie", "method"}},
						"name":    map[string]any{"type": "string"},
						"from":    map[string]any{"type": "string"},
						"to":      map[string]any{"type": "string"},
						"replace": map[string]any{"type": "boolean"},
					}, "required": []any{"kind"}},
				"encode":        map[string]any{"type": "string", "enum": []string{"", "url", "url2", "base64", "hex", "unicode"}, "description": "（可选）对变异 to 值先做编码再注入——WAF/解析器绕过的双层解码场景：url（百分号一次）/ url2（两次）/ base64 / hex / unicode（%u00XX 宽字符）。留空不编码"},
				"control_twice": map[string]any{"type": "boolean", "description": "对照组跑两次标定噪声（默认 true；false 时单次对照，抖动大的站点会假阳性）"},
				"timeout":       map[string]any{"type": "integer", "description": "单请求超时秒数（默认 30）"},
			},
			"required": []any{"traffic_id", "mutation"},
		},
		ReadOnly: func(json.RawMessage) bool { return false },
		// 重放会向目标发出真实请求（含原身份的 Cookie/Authorization）：与
		// lateral_exec 同档，默认 ask 人工确认。
		Permissions: func(_ context.Context, _ json.RawMessage, _ permission.Context) permission.Decision {
			return permission.AskUser("HTTP 重放会带原请求身份向目标发 2-3 次请求（对照+变体），确认在授权范围内")
		},
		Run: func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
			var a struct {
				TrafficID    string           `json:"traffic_id"`
				Scheme       string           `json:"scheme"`
				Mutation     webtest.Mutation `json:"mutation"`
				Encode       string           `json:"encode"`
				ControlTwice *bool            `json:"control_twice"`
				Timeout      int              `json:"timeout"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.TrafficID) == "" {
				return actool.Errorf("traffic_id 必填（用 traffic_search/traffic_get 找基线）"), nil
			}
			// 编码预处理（webtest.Encode，纯函数）：只作用于变异的 to 值——
			// 「编码错一层」是手写绕过最常见的失败原因，这里由代码保证字节。
			if strings.TrimSpace(a.Encode) != "" {
				enc, eerr := webtest.Encode(a.Encode, a.Mutation.To)
				if eerr != nil {
					return actool.Errorf(eerr.Error()), nil
				}
				a.Mutation.To = enc
			}
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("http_replay 必须在任务上下文内使用"), nil
			}
			if s.m == nil || s.m.taskProxy == nil {
				return actool.Errorf("任务代理未启用（CHENGJIAN_TASK_PROXY），http_replay 不可用；改用 Bash curl 时注意差分对照要自己做"), nil
			}
			// 基线：录制请求原文 → 解析。查询序与证据锚定同一套（任务实例优先、
			// 全局实例回退）。
			raw, _, err := s.m.taskProxy.ExchangeForTask(ctx, ri.TaskID, a.TrafficID)
			if err != nil || strings.TrimSpace(raw) == "" {
				return actool.Errorf(fmt.Sprintf("基线流量 %s 取不到（%v）：核实 traffic_id 后重试", a.TrafficID, err)), nil
			}
			base, perr := parseRecordedRequest(raw, a.Scheme)
			if perr != nil {
				return actool.Errorf("基线请求解析失败：" + perr.Error()), nil
			}
			// RoE：重放目标必须在任务范围内（与 tunnel_deploy 的 portfwd 同一判据）。
			if host := hostOfURL(base.URL); host != "" {
				if warn, roeErr := s.roeCheckHost(ctx, host); roeErr != nil {
					return actool.Errorf(roeErr.Error()), nil
				} else if warn != "" {
					_ = warn // warn 模式放行；告警进不了结构化输出，日志侧已有
				}
			}
			// 变异（单点，webtest.Replay 负责校验 From 基线防张冠李戴）。
			variant, verr := webtest.Replay(base, a.Mutation)
			if verr != nil {
				return actool.Errorf("变异被拒：" + verr.Error()), nil
			}
			timeout := time.Duration(a.Timeout) * time.Second
			if a.Timeout <= 0 {
				timeout = replayTimeout
			}
			// 发送通道：任务 MITM（录制 + 隧道选路）。ForTask 懒建并热切换上游。
			proxyAddr, _ := s.m.taskProxy.ForTask(ctx, ri.TaskID)
			if proxyAddr == "" {
				return actool.Errorf("任务代理不可用：取消重放，避免绕过任务出口和录制"), nil
			}
			// 参数观测（W2）：基线与变体的参数-值都落 endpoint_observation——
			// 差分实验顺手就是观测来源，无需额外动作。
			s.noteRequestObs(ri.TaskID, base)
			s.noteRequestObs(ri.TaskID, variant)
			controlA, cerr := sendReplayRequest(ctx, base, proxyAddr, timeout)
			if cerr != nil {
				return actool.Errorf("对照组请求失败：" + cerr.Error()), nil
			}
			controlB := controlA
			controlTwice := a.ControlTwice == nil || *a.ControlTwice
			if controlTwice {
				controlB2, cerr2 := sendReplayRequest(ctx, base, proxyAddr, timeout)
				if cerr2 != nil {
					return actool.Errorf("对照组第二次请求失败（噪声标定需要两次）：" + cerr2.Error()), nil
				}
				controlB = controlB2
			}
			variantResp, vrerr := sendReplayRequest(ctx, variant, proxyAddr, timeout)
			if vrerr != nil {
				return actool.Errorf("变体请求失败（对照已发出，结论 inconclusive）：" + vrerr.Error()), nil
			}
			var cmp webtest.Comparison
			if controlTwice {
				cmp = webtest.CompareWithNoise(controlA, controlB, variantResp)
			} else {
				cmp = webtest.Compare(controlA, variantResp)
			}
			controlFile, err := preserveToolBody(tc, controlA.Body)
			if err != nil {
				return actool.Errorf("output_preservation_failed: " + err.Error()), nil
			}
			variantFile, err := preserveToolBody(tc, variantResp.Body)
			if err != nil {
				return actool.Errorf("output_preservation_failed: " + err.Error()), nil
			}
			out := map[string]any{
				"control_body_file": controlFile, "variant_body_file": variantFile,
				"control_body_truncated": len(controlA.Body) > replayMaxBody || controlA.Truncated,
				"variant_body_truncated": len(variantResp.Body) > replayMaxBody || variantResp.Truncated,
				"continuation":           "body_file 可用 Read(byte_offset=0) 分段读取；上游响应标记 truncated 时，须重新按 Range 获取缺失部分并验证 206/Content-Range。",
				"verdict":                string(cmp.Verdict),
				"finding_state":          cmp.FindingState(),
				"reason":                 cmp.Reason,
				"signals":                cmp.Signals,
				"baseline":               map[string]any{"url": base.URL, "method": base.Method, "status": controlA.Status, "digest": webtest.Digest(controlA), "truncated": controlA.Truncated},
				"control_repeat":         map[string]any{"status": controlB.Status, "digest": webtest.Digest(controlB), "truncated": controlB.Truncated},
				"variant":                map[string]any{"url": variant.URL, "method": variant.Method, "status": variantResp.Status, "digest": webtest.Digest(variantResp), "truncated": variantResp.Truncated},
				"control_body":           truncateForSnapshot(string(controlA.Body), replayMaxBody),
				"variant_body":           truncateForSnapshot(string(variantResp.Body), replayMaxBody),
				"note": "verdict=different 只说明「这次变异改变了目标行为」，还要差异可解释（对应漏洞机理）才算证据；" +
					"same 是有价值的阴性结论，record_fact 记下来避免重复试探。inconclusive 多为对照不稳或 5xx，先稳定再试。",
				"evidence_hint": "对照与变体两次请求已经任务代理照常录制：traffic_search 按 host 检索最新两条即可把" +
					"变体那条作为 report_finding 的 traffic_refs（role=proof，配 slice_text 引用变体响应里的差异片段）。",
			}
			if !controlTwice {
				out["note"] = "（本次未做噪声标定：目标页面若每次渲染都变，different 可能是假阳性）" + out["note"].(string)
			}
			// 使用点提示（algo.go）：目标命中预置算法时提醒一行。
			if hint := s.algoHintForURL(ctx, variant.URL); hint != "" {
				out["algo_hint"] = hint
			}
			s.appendIntranetActivity(ri.TaskID, "http_replay",
				fmt.Sprintf("差分重放 %s %s → %s（%s 变异）", variant.Method, hostOfURL(variant.URL), cmp.Verdict, a.Mutation.Kind),
				fmt.Sprintf("traffic_id=%s kind=%s name=%s control=%d variant=%d",
					a.TrafficID, a.Mutation.Kind, a.Mutation.Name, controlA.Status, variantResp.Status))
			return jsonResult(out)
		},
	})
}

// seedHTTPReplayTool 把 http_replay seed 进 tools 表（默认绑 worker；重放是主动
// 动作，planner 只读态势不发包）。与隧道/凭据工具同一套首插入语义。
func (s *Server) seedHTTPReplayTool() {
	if s.m == nil || s.m.PG() == nil {
		return
	}
	t := s.httpReplayTool()
	schema, _ := json.Marshal(t.InputSchema())
	agents, _ := json.Marshal([]string{"worker"})
	if err := s.m.PG().SeedTool(t.Name(), t.Description(), schema, agents); err != nil {
		log.Printf("[tools] seed http_replay 失败: %v", err)
	}
}
