// katana.go 受管爬虫封装（爬虫 2.0 的第一步：把军火库里的 katana 从「裸 Bash
// 手拼」变成结构化工具，P1-③）。
//
// 封装原则与 tunnel 四件套同源：参数白名单（不给 extra args 任意拼接）、
// 超时受控、目标过 RoE、进程经 ctx 收割、结果截断。代理经环境变量注入任务
// MITM（Go 系工具尊标准 HTTP_PROXY/HTTPS_PROXY）——爬到的每个请求照常被
// 录制、内网目标自动走任务隧道路由。
//
// headless 的已知边界（如实声明，不装能做到）：katana 的 headless 模式由内嵌
// Chrome 执行页面，Chrome 不一定尊 HTTP_PROXY env（需要 --proxy-server 启动参，
// 由 katana 自身管理）——所以 headless 下「录制留痕」不保证，工具输出里对
// headless 显式提示这一点；需要全程留痕的爬取用默认 HTTP 模式。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"chengjian.local/chengjian/agent"
	"github.com/Autumn-27/norma/procguard"
	actool "github.com/Autumn-27/norma/tool"
)

// katanaDefaultTimeout 是单次爬取的墙钟上限。
const katanaDefaultTimeout = 180 * time.Second

// katanaMaxOutput 是返回给模型的 URL 列表截断。
const katanaMaxOutput = 300

// locateKatana 定位 katana 二进制：CHENGJIAN_KATANA_PATH > data/tools/katana > PATH。
func locateKatana(dataDir string) (string, error) {
	if p := strings.TrimSpace(os.Getenv("CHENGJIAN_KATANA_PATH")); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
		return "", fmt.Errorf("CHENGJIAN_KATANA_PATH=%q 不是可执行文件", p)
	}
	for _, name := range []string{"katana", "katana.exe"} {
		p := filepath.Join(dataDir, "tools", name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if p, err := exec.LookPath("katana"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("未找到 katana（受管爬虫需要它）。按 packaging/tools-manifest.json 的 katana 条目部署到 data/tools/，或设 CHENGJIAN_KATANA_PATH")
}

// katanaArgs 组装命令行参数（纯函数，可测）。白名单：只收受控参数，不透传。
func katanaArgs(targetURL string, depth int, headless bool, knownFields []string) []string {
	args := []string{"-u", targetURL}
	if depth > 0 {
		args = append(args, "-d", fmt.Sprintf("%d", depth))
	}
	if headless {
		args = append(args, "-headless")
	}
	// -known-field 只加不猜：调用方给什么用什么（katana 的 -kf 会请求常见路径）。
	for _, f := range knownFields {
		if f = strings.TrimSpace(f); f != "" {
			args = append(args, "-kf", f)
		}
	}
	args = append(args, "-silent", "-no-color")
	return args
}

// katanaCrawlTool 是受管爬虫工具（绑 worker：主动动作）。
func (s *Server) katanaCrawlTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "katana_crawl",
		Description: "【受管爬虫】用 katana 对一个起点 URL 做爬取，返回发现的 URL/端点列表。" +
			"平台负责：定位二进制（CHENGJIAN_KATANA_PATH > data/tools/katana）、注入任务代理（HTTP 模式下爬取流量照常录制、内网自动走隧道）、" +
			"超时收割、目标 RoE 校验、结果截断。" +
			"参数白名单：url/depth/headless/known_fields（不透传任意参数）。" +
			"⚠️ headless 模式由内嵌 Chrome 执行 JS（SPA 必须开），但其流量不经任务代理、不保证录制——需要全程留痕用默认 HTTP 模式。" +
			"爬到的端点会进资产图（配合 enrich 的自动抽取）；对已录流量找端点优先 traffic_search（零新请求），katana 用于发现未访问过的路径。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":          map[string]any{"type": "string", "description": "起点 URL（必填，过 RoE 校验）"},
				"depth":        map[string]any{"type": "integer", "description": "爬取深度，默认 2（建议 ≤3，深爬噪声大）"},
				"headless":     map[string]any{"type": "boolean", "description": "浏览器执行模式（SPA/JS 渲染站点必须 true；注意其流量不经任务代理）"},
				"known_fields": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "katana -kf 已知路径字段（如 admin,api），可选"},
				"timeout":      map[string]any{"type": "integer", "description": "墙钟秒数，默认 180"},
			},
			"required": []any{"url"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
			var a struct {
				URL         string   `json:"url"`
				Depth       int      `json:"depth"`
				Headless    bool     `json:"headless"`
				KnownFields []string `json:"known_fields"`
				Timeout     int      `json:"timeout"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.URL) == "" {
				return actool.Errorf("url 必填"), nil
			}
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("katana_crawl 必须在任务上下文内使用"), nil
			}
			if s.m == nil {
				return actool.Errorf("平台管理器未就绪"), nil
			}
			bin, err := locateKatana(s.m.dir)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			// RoE：爬取起点必须在任务授权范围内。
			if _, roeErr := s.roeCheckHost(ctx, hostOfURL(a.URL)); roeErr != nil {
				return actool.Errorf(roeErr.Error()), nil
			}
			if a.Depth <= 0 {
				a.Depth = 2
			}
			timeout := time.Duration(a.Timeout) * time.Second
			if a.Timeout <= 0 {
				timeout = katanaDefaultTimeout
			}
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.CommandContext(runCtx, bin, katanaArgs(strings.TrimSpace(a.URL), a.Depth, a.Headless, a.KnownFields)...)
			// 代理经标准 env 注入任务 MITM：Go 系爬虫尊 HTTP(S)_PROXY，
			// 于是 HTTP 模式的爬取流量与 worker 的 curl 同路同录制。
			if !a.Headless {
				if s.m.taskProxy == nil {
					return actool.Errorf("proxy_unavailable：任务代理未启用，未启动爬取"), nil
				}
				addr, _ := s.m.taskProxy.ForTask(ctx, ri.TaskID)
				if addr == "" {
					return actool.Errorf("proxy_unavailable：任务代理启动失败，未启动爬取"), nil
				}
				cmd.Env = append(os.Environ(), "HTTP_PROXY="+addr, "HTTPS_PROXY="+addr, "http_proxy="+addr, "https_proxy="+addr)
			}
			var buffer procguard.Buffer
			cmd.Stdout, cmd.Stderr = &buffer, &buffer
			cmd.WaitDelay = 2 * time.Second
			runErr := procguard.Run(runCtx, cmd)
			out := buffer.Bytes()
			outputFile, err := preserveToolBody(tc, out)
			if err != nil {
				return actool.Errorf("output_preservation_failed: " + err.Error()), nil
			}
			text := string(out)
			urls := splitKatanaURLs(text)
			if runCtx.Err() == context.DeadlineExceeded {
				m := map[string]any{
					"ok": false, "timed_out": true, "urls": truncateURLs(urls, katanaMaxOutput),
					"output_file": outputFile, "read_complete": false, "urls_truncated": len(urls) > katanaMaxOutput,
					"note": "超时已收割（已发现的 URL 仍返回）；缩小 depth 或换更小起点重试",
				}
				if len(urls) > 0 {
					m["partial"] = true
				}
				return jsonResult(m)
			}
			if runErr != nil || buffer.Overflow {
				return actool.Errorf(fmt.Sprintf("katana 执行失败/输出或资源达到上限，结果不完整：%v；output_file=%s，可用 Read 分段查看已保留输出（预览：%s）", runErr, outputFile, truncateForSnapshot(text, 300))), nil
			}
			if len(urls) == 0 {
				return jsonResult(map[string]any{"ok": true, "urls": []string{}, "count": 0,
					"output_file": outputFile, "read_complete": true,
					"note": "没有发现新 URL（可能已爬过/站点无外链）；先用 traffic_search 查已录流量再决定是否值得再爬"})
			}
			log.Printf("[katana] 任务 %d 爬取 %s：发现 %d 个 URL（depth=%d headless=%v）",
				ri.TaskID, hostOfURL(a.URL), len(urls), a.Depth, a.Headless)
			return jsonResult(map[string]any{
				"ok": true, "urls": truncateURLs(urls, katanaMaxOutput), "count": len(urls),
				"output_file": outputFile, "read_complete": true, "urls_truncated": len(urls) > katanaMaxOutput,
				"note": "把值得测的端点经 insert_assets 登记进资产图（enrich 会自动补全探测）；已在图里的不必重复登记",
			})
		},
	})
}

// splitKatanaURLs 从 katana 输出里挑出 URL 行（每行一个；容忍 JSON 行与杂散日志）。
func splitKatanaURLs(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			out = append(out, line)
		}
	}
	return out
}

func truncateURLs(urls []string, n int) []string {
	if len(urls) <= n {
		return urls
	}
	return urls[:n]
}

// seedKatanaTool 把 katana_crawl seed 进 tools 表（默认绑 worker）。
func (s *Server) seedKatanaTool() {
	if s.m == nil || s.m.PG() == nil {
		return
	}
	t := s.katanaCrawlTool()
	schema, _ := json.Marshal(t.InputSchema())
	agents, _ := json.Marshal([]string{"worker"})
	if err := s.m.PG().SeedTool(t.Name(), t.Description(), schema, agents); err != nil {
		log.Printf("[tools] seed katana_crawl 失败: %v", err)
	}
}
