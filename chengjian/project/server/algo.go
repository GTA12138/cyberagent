// algo.go 预置算法库：登记、执行与使用点提示。
//
// 解决的问题：目标有服务端验签/前端滑块/参数加密时，worker 每次现场逆向是
// 最烧 token 的路径，且成果随任务消失。预置算法库把「提前解出来的算法」
// （人工逆向或 worker 逆向一次后 save_algo 沉淀）变成跨任务资源：
// apply_algo 在平台本地跑 Python（stdin JSON → stdout JSON），零 LLM 参与。
//
// 「模型如何长期记得它存在」的三层设计（用户核心关切）：
//  1. 存在性走工具面：apply_algo/list_algos 是注册进工具表的常规工具，
//     **工具 schema 每轮重发、不会被上下文压缩丢掉**——facts 会丢，工具不会；
//  2. 内容按需拉取：list_algos 返回的目录每条只有名字/类别/一句话说明
//     （十几字节），不注入 prompt，模型用时才取；
//  3. 使用点提示：http_fetch/http_replay 的目标命中某算法的 match 条件时，
//     在结果里追加一行 algo_hint——在「正需要它的那一刻」提醒，一行不占
//     上下文，长任务也不会忘。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"chengjian.local/chengjian/db"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/procguard"
	actool "github.com/Autumn-27/norma/tool"
)

// algoExecTimeout 是单次算法执行的墙钟上限（纯计算，15s 足够宽）。
const algoExecTimeout = 15 * time.Second

// algoStore 返回算法库访问层（DB 未就绪时 nil）。
func (s *Server) algoStore() *db.AlgorithmStore {
	if s == nil || s.m == nil || s.m.pg == nil {
		return nil
	}
	return db.NewAlgorithmStore(s.m.pg)
}

// runAlgorithmScript 执行一条算法脚本：stdin 喂入 JSON，stdout 必须是 JSON。
// 脚本落在平台临时目录（0700），用完即删——算法成果的正本在库里，不散落文件。
func runAlgorithmScript(ctx context.Context, script string, input json.RawMessage) (json.RawMessage, error) {
	return runAlgorithmScriptWithTimeout(ctx, script, input, algoExecTimeout)
}

// runAlgorithmScriptWithTimeout 是带显式超时的实现。
//
// Resolve interpreter shims before launch; cancellation kills the process tree.
// WaitDelay bounds pipe cleanup, including scripts that spawn child processes.
func runAlgorithmScriptWithTimeout(ctx context.Context, script string, input json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	dir, err := os.MkdirTemp("", "chengjian-algo-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "algo.py")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	python, err := algorithmPython(ctx)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, python, path)
	configureAlgorithmProcess(cmd)
	cmd.Stdin = strings.NewReader(string(input))
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr procguard.Buffer
	stdout.Limit, stderr.Limit = 1<<20, 64<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := procguard.Run(ctx, cmd)
	out := stdout.Bytes()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("算法执行取消或超时（%s）: %w", timeout, ctx.Err())
	}
	if runErr != nil {
		return nil, fmt.Errorf("算法脚本执行失败或达到资源限制（%v）：%s", runErr, truncateForSnapshot(string(stderr.Bytes())+string(out), 600))
	}
	if stdout.Overflow || stderr.Overflow {
		return nil, fmt.Errorf("算法输出超过上限（stdout 1MiB / stderr 64KiB），结果不完整，不能作为有效计算结果")
	}
	// 契约：stdout 是 JSON 对象。容忍脚本打印杂散日志：取最后一个以 { 开头的行。
	var payload string
	if trimmed := strings.TrimSpace(string(out)); json.Valid([]byte(trimmed)) && strings.HasPrefix(trimmed, "{") {
		payload = trimmed
	}
	if payload == "" {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "{") && json.Valid([]byte(strings.TrimSpace(line))) {
				payload = strings.TrimSpace(line)
			}
		}
	}
	if payload == "" {
		return nil, fmt.Errorf("算法脚本没有输出 JSON 对象（契约：stdout 至少一行以 { 开头）：%.200s", string(out))
	}
	if !json.Valid([]byte(payload)) {
		return nil, fmt.Errorf("算法脚本输出不是合法 JSON：%.200s", payload)
	}
	return json.RawMessage(payload), nil
}

// Resolve Windows .cmd shims to their real interpreter before execution.
func algorithmPython(ctx context.Context) (string, error) {
	path, err := exec.LookPath("python3")
	if err != nil {
		path, err = exec.LookPath("python")
	}
	if err != nil {
		return "", fmt.Errorf("算法执行需要 Python: %w", err)
	}
	if runtime.GOOS != "windows" || strings.EqualFold(filepath.Ext(path), ".exe") {
		return path, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	probe := exec.CommandContext(probeCtx, path, "-c", "import sys;print(sys.executable)")
	configureAlgorithmProcess(probe)
	probe.WaitDelay = time.Second
	out, err := probe.Output()
	if err != nil {
		return "", fmt.Errorf("无法解析 Python 解释器: %w", err)
	}
	path = strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Ext(path), ".exe") {
		return "", fmt.Errorf("Python 解释器路径无效")
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("Python 解释器不可用: %w", err)
	}
	return path, nil
}

// algoHintForURL 返回使用点提示（零个或一行）：目标命中预置算法时在
// http_fetch/http_replay 的结果里附带，长任务也不会忘掉它的存在。
func (s *Server) algoHintForURL(ctx context.Context, rawURL string) string {
	st := s.algoStore()
	if st == nil {
		return ""
	}
	algos, err := st.List(ctx, false)
	if err != nil || len(algos) == 0 {
		return ""
	}
	host := hostOfURL(rawURL)
	for _, a := range algos {
		if a.MatchHost == "" && a.MatchPath == "" {
			continue
		}
		if a.MatchHost != "" && !strings.Contains(host, a.MatchHost) {
			continue
		}
		if a.MatchPath != "" && !strings.Contains(rawURL, a.MatchPath) {
			continue
		}
		return fmt.Sprintf("该目标命中预置算法 %s（kind=%s）：%s ——需要签名/加解密时用 apply_algo(name=%s)，不要现场逆向",
			a.Name, a.Kind, a.Description, a.Name)
	}
	return ""
}

// algoTools 返回算法三工具。
func (s *Server) algoTools() []actool.CoreTool {
	return []actool.CoreTool{s.applyAlgoTool(), s.listAlgosTool(), s.saveAlgoTool()}
}

// applyAlgoTool 执行预置算法（绑 worker——planner 读态势不计算）。
func (s *Server) applyAlgoTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "apply_algo",
		Description: "【预置算法·本地执行】按名执行一条预置算法（签名/滑块轨迹/参数加解密），" +
			"平台本地跑 Python 计算，零 LLM 参与、零网络请求。**目标有验签/滑块/参数加密时先用 list_algos 查有没有现成算法**——" +
			"现场逆向一次要烧几万 token，预置算法一次调用就出结果；http_fetch/http_replay 的结果里若出现 algo_hint 行，说明当前目标命中了某条算法。" +
			"输入 input 是算法所需的全部参数（字段含义见 list_algos 的 description）；输出为算法计算的 JSON 结果。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":  map[string]any{"type": "string", "description": "算法名（list_algos 返回的 name，必填）"},
				"input": map[string]any{"type": "object", "description": "算法入参（如 {\"ts\":1698000000,\"body\":\"...\",\"gap\":87}），必填"},
			},
			"required": []any{"name", "input"},
		},
		ReadOnly:    func(json.RawMessage) bool { return true }, // 纯本地计算
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			st := s.algoStore()
			if st == nil {
				return actool.Errorf("算法库未启用（DB 未就绪）"), nil
			}
			var a struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Name) == "" || len(a.Input) == 0 {
				return actool.Errorf("name 与 input 均为必填"), nil
			}
			algo, err := st.Get(ctx, strings.TrimSpace(a.Name))
			if err != nil {
				return actool.Errorf("算法库读取失败：" + err.Error()), nil
			}
			if algo == nil {
				return actool.Errorf(fmt.Sprintf("算法 %q 不存在或未启用；用 list_algos 查可用算法", a.Name)), nil
			}
			out, rerr := runAlgorithmScript(ctx, algo.Script, a.Input)
			if rerr != nil {
				return actool.Errorf(fmt.Sprintf("算法 %s 执行失败：%v（说明与入参契约见 list_algos 的 description）", algo.Name, rerr)), nil
			}
			st.NoteUse(ctx, algo.ID)
			return jsonResult(map[string]any{
				"algo": algo.Name, "kind": algo.Kind,
				"output": json.RawMessage(out),
				"note":   "结果是本地确定性计算，可直接用于请求构造（配 http_fetch/http_replay 发送）",
			})
		},
	})
}

// listAlgosTool 算法目录（绑 worker+planner：轻量索引，按需拉取不进 prompt）。
func (s *Server) listAlgosTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "list_algos",
		Description: "【预置算法·目录】列出平台预置算法（签名/滑块/加解密）的轻量目录：" +
			"名字、类别、匹配条件（host/path 子串）、一句话入参说明。**目标有验签/滑块/加密时先查这里**；" +
			"http_fetch/http_replay 结果带 algo_hint 时也按 hint 指名的算法用 apply_algo。",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		ReadOnly:    func(json.RawMessage) bool { return true },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			st := s.algoStore()
			if st == nil {
				return actool.Errorf("算法库未启用（DB 未就绪）"), nil
			}
			algos, err := st.List(ctx, false)
			if err != nil {
				return actool.Errorf("算法目录读取失败：" + err.Error()), nil
			}
			items := []map[string]any{}
			for _, a := range algos {
				items = append(items, map[string]any{
					"name": a.Name, "kind": a.Kind,
					"match_host": a.MatchHost, "match_path": a.MatchPath,
					"description": a.Description, "usage_count": a.UsageCount,
				})
			}
			return jsonResult(map[string]any{"algos": items, "count": len(items)})
		},
	})
}

// saveAlgoTool 沉淀算法（绑 worker；ask 审批——写入跨任务共享库）。
func (s *Server) saveAlgoTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "save_algo",
		Description: "【预置算法·沉淀】把逆向出的签名/滑块/加解密算法存成预置算法，" +
			"后续任务直接 apply_algo 调用，不再重复逆向（本次逆向成果的复利）。" +
			"script 契约：Python，从 stdin 读一行 JSON 入参，向 stdout 打印一行 JSON 结果（可先 print 调试日志，取最后一个 { 开头的行）；" +
			"纯计算、不发网络请求、15 秒内完成。description 必须写清入参字段含义（后人靠它调用）。" +
			"同名覆盖（算法迭代不堆版本）。会过人工审批（跨任务共享资源）。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":         map[string]any{"type": "string", "description": "算法名（英文短名，如 acme_sign_v1）"},
				"kind":         map[string]any{"type": "string", "enum": []string{"sign", "slider", "encrypt", "decrypt", "other"}},
				"match_host":   map[string]any{"type": "string", "description": "命中提示：host 子串（如 api.acme.com），可空"},
				"match_path":   map[string]any{"type": "string", "description": "命中提示：URL path 子串（如 /api/order），可空"},
				"description":  map[string]any{"type": "string", "description": "一句话用途+入参字段说明（必填，后人靠它调用）"},
				"script":       map[string]any{"type": "string", "description": "Python 实现（stdin JSON → stdout JSON 契约）"},
				"verify_input": map[string]any{"type": "object", "description": "可选：保存时用这组入参实跑一次验证契约（推荐）"},
			},
			"required": []any{"name", "kind", "description", "script"},
		},
		ReadOnly: func(json.RawMessage) bool { return false },
		Permissions: func(_ context.Context, _ json.RawMessage, _ permission.Context) permission.Decision {
			return permission.AskUser("保存跨任务共享算法（写入平台算法库），确认脚本内容已核对")
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			st := s.algoStore()
			if st == nil {
				return actool.Errorf("算法库未启用（DB 未就绪）"), nil
			}
			var a struct {
				Name        string          `json:"name"`
				Kind        string          `json:"kind"`
				MatchHost   string          `json:"match_host"`
				MatchPath   string          `json:"match_path"`
				Description string          `json:"description"`
				Script      string          `json:"script"`
				VerifyInput json.RawMessage `json:"verify_input"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Script) == "" || strings.TrimSpace(a.Description) == "" {
				return actool.Errorf("name / description / script 均为必填"), nil
			}
			// 保存即验证（推荐路径）：契约跑不通的脚本入库只会坑后人。
			verifyNote := "未做保存验证（建议带 verify_input）"
			if len(a.VerifyInput) > 0 {
				if _, verr := runAlgorithmScript(ctx, a.Script, a.VerifyInput); verr != nil {
					return actool.Errorf("保存被拒：verify_input 实跑失败（" + verr.Error() + "）——先修契约再保存"), nil
				}
				verifyNote = "verify_input 实跑通过"
			}
			id, err := st.UpsertAlgorithm(ctx, &db.Algorithm{
				Name: strings.TrimSpace(a.Name), Kind: a.Kind, MatchHost: a.MatchHost, MatchPath: a.MatchPath,
				Description: a.Description, Script: a.Script, Enabled: true,
			})
			if err != nil {
				return actool.Errorf("算法落库失败：" + err.Error()), nil
			}
			return jsonResult(map[string]any{"id": id, "name": a.Name, "verified": len(a.VerifyInput) > 0, "note": verifyNote})
		},
	})
}

// seedAlgoTools seed 三个算法工具（apply/save 绑 worker，list 绑 worker+planner）。
func (s *Server) seedAlgoTools() {
	if s.m == nil || s.m.PG() == nil {
		return
	}
	for _, t := range s.algoTools() {
		schema, _ := json.Marshal(t.InputSchema())
		agents := []string{"worker"}
		if t.Name() == "list_algos" {
			agents = []string{"worker", "planner"}
		}
		raw, _ := json.Marshal(agents)
		if err := s.m.PG().SeedTool(t.Name(), t.Description(), schema, raw); err != nil {
			log.Printf("[tools] seed %s 失败: %v", t.Name(), err)
		}
	}
}
