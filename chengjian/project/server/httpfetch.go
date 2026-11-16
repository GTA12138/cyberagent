// httpfetch.go 结构化只读抓取 + 端点参数观测（W2 数据地基 / P0-②）。
//
// http_fetch 与 http_replay 的分工：replay 是差分实验（基线+变异+对照判定），
// fetch 是「结构化地看一眼」——补 WebFetch 只能 GET 的缺口，让 POST/带头/带体
// 的读取也走任务代理（录制+隧道路由）而不是 Bash 拼 curl。两者发送层同源
// （sendReplayRequest），观测落库同源（observeRequestParams）。
//
// endpoint_params 是观测的读出口：按任务列「参数 × 值集合 × 频次」，planner
// 据此发现「同一个参数出现过不同身份的取值」——IDOR 差分意图的派发依据。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/webtest"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// fetchMaxBody 是 http_fetch 返回正文的截断（读取工具，正文长了只提示去 traffic_get）。
const fetchMaxBody = 4000

// observeRequestParams 从一次请求里抽取参数观测（纯函数）。
//
// 口径：query 全量；body 只解 form 与 JSON 顶层键（浅层——嵌套结构展平会
// 产生大量伪参数名，噪声大于信号）；header 不抽（authorization/cookie 敏感，
// 其余头对差分用例价值低）。URL 存不带 query 的绝对形态，参数单列。
func observeRequestParams(req webtest.Request) []db.EndpointObsInput {
	u, err := url.Parse(req.URL)
	if err != nil {
		return nil
	}
	base := *u
	base.RawQuery = ""
	baseURL := base.String()
	var out []db.EndpointObsInput
	for k, vs := range u.Query() {
		for _, v := range vs {
			out = append(out, db.EndpointObsInput{Method: req.Method, URL: baseURL, Name: k, Loc: "query", Value: v})
		}
	}
	if len(req.Body) == 0 {
		return out
	}
	ct := ""
	for _, kv := range req.Headers {
		if strings.EqualFold(kv[0], "content-type") {
			ct = strings.ToLower(kv[1])
			break
		}
	}
	switch {
	case strings.Contains(ct, "application/x-www-form-urlencoded"):
		if vals, err := url.ParseQuery(string(req.Body)); err == nil {
			for k, vs := range vals {
				for _, v := range vs {
					out = append(out, db.EndpointObsInput{Method: req.Method, URL: baseURL, Name: k, Loc: "body", Value: v})
				}
			}
		}
	case strings.Contains(ct, "application/json"), ct == "" && req.Body[0] == '{':
		var m map[string]any
		if err := json.Unmarshal(req.Body, &m); err == nil {
			for k, v := range m {
				s := ""
				switch tv := v.(type) {
				case string:
					s = tv
				case float64, bool:
					s = fmt.Sprintf("%v", tv)
				default:
					continue // 对象/数组不抽：伪参数名噪声
				}
				out = append(out, db.EndpointObsInput{Method: req.Method, URL: baseURL, Name: k, Loc: "body", Value: s})
			}
		}
	}
	return out
}

// noteRequestObs 落一条请求的参数观测（best-effort：失败只留痕不影响动作本身）。
func (s *Server) noteRequestObs(taskID int64, req webtest.Request) {
	if s.m == nil || s.m.Assets() == nil {
		return
	}
	ins := observeRequestParams(req)
	if len(ins) == 0 {
		return
	}
	if n, err := s.m.Assets().UpsertEndpointObs(taskID, ins); err != nil {
		s.appendIntranetActivity(taskID, "http_fetch",
			fmt.Sprintf("端点参数观测写入失败：%v（不影响本次请求）", err), "")
	} else if n > 0 {
		_ = n // 观测是旁路事实，不进结果输出（避免噪声）
	}
}

// httpFetchTool 是结构化只读抓取工具（绑 worker + planner：读取类，planner
// 开局探测理解也用得上）。
func (s *Server) httpFetchTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "http_fetch",
		Description: "【结构化抓取·只读】按 method/headers/body 抓一个 URL 的响应（默认 GET）。" +
			"与 WebFetch 的差别：支持全部方法与自定义头/体、经任务代理发出（照常录制、内网目标自动走任务隧道）。" +
			"与 Bash curl 的差别：结构化参数不转义、不进 shell、目标过 RoE 校验。" +
			"发送的参数会被记进端点观测（endpoint_params 可查：参数×值×频次，差分用例的数据来源）。" +
			"要改参数看差异用 http_replay（差分判定），这里只是「看一眼」。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":     map[string]any{"type": "string", "description": "完整 URL（含 query，必填）"},
				"method":  map[string]any{"type": "string", "description": "HTTP 方法，默认 GET"},
				"headers": map[string]any{"type": "object", "description": "附加请求头（如 Authorization/Cookie/Content-Type），可选"},
				"body":    map[string]any{"type": "string", "description": "请求正文（POST/PUT 用），可选"},
				"timeout": map[string]any{"type": "integer", "description": "超时秒数，默认 30"},
			},
			"required": []any{"url"},
		},
		ReadOnly: func(json.RawMessage) bool { return true },
		Permissions: func(_ context.Context, in json.RawMessage, _ permission.Context) permission.Decision {
			// 只读判定：GET/HEAD/OPTIONS 放行；其余方法有副作用面，走 ask。
			var a struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(in, &a)
			switch strings.ToUpper(strings.TrimSpace(a.Method)) {
			case "", "GET", "HEAD", "OPTIONS":
				return permission.Allowed()
			}
			return permission.AskUser("http_fetch 以写类方法发送请求，确认在授权范围内")
		},
		Run: func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
			var a struct {
				URL     string            `json:"url"`
				Method  string            `json:"method"`
				Headers map[string]string `json:"headers"`
				Body    string            `json:"body"`
				Timeout int               `json:"timeout"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.URL) == "" {
				return actool.Errorf("url 必填"), nil
			}
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("http_fetch 必须在任务上下文内使用"), nil
			}
			if s.m == nil || s.m.taskProxy == nil {
				return actool.Errorf("任务代理未启用（CHENGJIAN_TASK_PROXY），http_fetch 不可用"), nil
			}
			method := strings.ToUpper(strings.TrimSpace(a.Method))
			if method == "" {
				method = "GET"
			}
			req := webtest.Request{Method: method, URL: strings.TrimSpace(a.URL), Body: []byte(a.Body)}
			for k, v := range a.Headers {
				req.Headers = append(req.Headers, [2]string{k, v})
			}
			// 路由三态：blocked 直接拒绝（内网期无隧道禁止直连）；RoE 范围校验。
			route := s.m.taskProxy.RouteForTaskHost(ri.TaskID, hostOfURL(req.URL))
			if route.Mode == UpstreamBlocked {
				return actool.Errorf("任务处于内网期但没有存活隧道（上游已阻断），已拒绝直连；先恢复链路再抓取"), nil
			}
			if host := hostOfURL(req.URL); host != "" {
				if _, roeErr := s.roeCheckHost(ctx, host); roeErr != nil {
					return actool.Errorf(roeErr.Error()), nil
				}
			}
			proxyAddr, _ := s.m.taskProxy.ForTask(ctx, ri.TaskID)
			if proxyAddr == "" {
				return actool.Errorf("proxy_unavailable：任务代理启动失败，未发送请求、未回落直连"), nil
			}
			timeout := time.Duration(a.Timeout) * time.Second
			if a.Timeout <= 0 {
				timeout = replayTimeout
			}
			resp, err := sendReplayRequest(ctx, req, proxyAddr, timeout)
			if err != nil {
				return actool.Errorf("请求失败：" + err.Error()), nil
			}
			s.noteRequestObs(ri.TaskID, req)
			bodyFile, err := preserveToolBody(tc, resp.Body)
			if err != nil {
				return actool.Errorf("响应已收到，但保存正文失败，不能宣称信息完整：" + err.Error()), nil
			}
			out := map[string]any{
				"status":         resp.Status,
				"body":           truncateForSnapshot(string(resp.Body), fetchMaxBody),
				"resp_len":       len(resp.Body),
				"url":            req.URL,
				"method":         method,
				"headers":        resp.Headers,
				"body_file":      bodyFile,
				"body_truncated": len(resp.Body) > fetchMaxBody || resp.Truncated,
				"read_complete":  !resp.Truncated,
				"continuation":   "完整已接收字节在 body_file，用 Read(byte_offset=0, byte_limit=8192) 续读；二进制用 encoding=base64。",
			}
			if resp.Truncated {
				out["next_range_offset"] = len(resp.Body)
				out["continuation"] = "本次接收达到 2MiB 上限，body_file 只保留前缀。继续请求须带 Range: bytes=<next_range_offset>- 并核对 206、Content-Range、ETag；目标不支持 Range 时不能声称已读取完整资源。"
			}
			if ct := resp.Headers.Get("Content-Type"); ct != "" {
				out["content_type"] = ct
			}
			if loc := resp.Headers.Get("Location"); loc != "" {
				out["location"] = loc // 3xx 不跟重定向（与 replay 同判据），跳转目标给模型自己决定
			}
			// 使用点提示（algo.go）：目标命中预置算法时提醒一行——长任务里模型
			// 也不会忘掉算法库的存在；没命中则零开销。
			if hint := s.algoHintForURL(ctx, req.URL); hint != "" {
				out["algo_hint"] = hint
			}
			return jsonResult(out)
		},
	})
}

// seedWebTools 把 web 侧三个工具 seed 进 tools 表：http_fetch / endpoint_params
// 绑 worker+planner（读取类，planner 开局理解可用），katana_crawl 绑 worker
// （主动爬取）。与隧道工具同一套首插入语义。
func (s *Server) seedWebTools() {
	if s.m == nil || s.m.PG() == nil {
		return
	}
	for _, t := range []actool.CoreTool{s.httpFetchTool(), s.endpointParamsTool()} {
		schema, _ := json.Marshal(t.InputSchema())
		agents, _ := json.Marshal([]string{"worker", "planner"})
		if err := s.m.PG().SeedTool(t.Name(), t.Description(), schema, agents); err != nil {
			log.Printf("[tools] seed %s 失败: %v", t.Name(), err)
		}
	}
}

// endpointParamsTool 是端点参数观测的读出口（绑 worker + planner）。
func (s *Server) endpointParamsTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "endpoint_params",
		Description: "【端点参数观测】列出本任务受管 HTTP 动作（http_fetch/http_replay）记录过的" +
			"「参数 × 值集合 × 频次」，按端点聚合。用途：①找差分目标——同一参数出现过多个取值" +
			"（尤其是别人的 id/用户名）就是 IDOR 候选，用 http_replay 换值验证；②避免重复试探——" +
			"值集合里已有的取值不必再发。可选 url 子串过滤。这不是可达范围，只是已观测事实。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url_contains": map[string]any{"type": "string", "description": "URL 子串过滤（可选，如 /api/users）"},
				"limit":        map[string]any{"type": "integer", "description": "原始行上限，默认 500"},
			},
		},
		ReadOnly:    func(json.RawMessage) bool { return true },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("endpoint_params 必须在任务上下文内使用"), nil
			}
			if s.m == nil || s.m.Assets() == nil {
				return actool.Errorf("asset store 未启用"), nil
			}
			var a struct {
				URLContains string `json:"url_contains"`
				Limit       int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			views, err := s.m.Assets().ListEndpointObs(ri.TaskID, a.URLContains, a.Limit)
			if err != nil {
				return actool.Errorf("观测读取失败：" + err.Error()), nil
			}
			return jsonResult(map[string]any{"params": views, "count": len(views)})
		},
	})
}
