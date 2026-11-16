// session_recon.go 内网被动侦察工具(内网渗透期 2,INTRANET-PIVOT-DESIGN.md §5 期 2):
// 经立足点会话逐条执行【只读】侦察命令包(ip addr/ip route/arp/hosts/netstat,
// 目标无 ip 命令自动回退 ifconfig/route -n),recon 包解析结构化结果后:
//
//	① 新 IP/邻居 → ip 资产、监听端口 → service 资产(复用 insert_assets 的落库路径);
//	② 高价值推断与结构化报告 record_fact 进探索图;
//	③ 返回摘要。
//
// 设计判据照 chains pivot 骨架「被动六表」(agent/chainskel/pivot.md):先不发包,
// 被动拿到什么算什么;逐条执行是为了绕开 session_exec 64KB 输出截断。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/capability"
	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/observation"
	"chengjian.local/chengjian/recon"
	"chengjian.local/chengjian/session"
	actool "github.com/Autumn-27/norma/tool"
)

// reconCommands 被动侦察命令包(全部只读;每条单独执行单独取输出,避开 64KB 截断)。
// shell 级 || 回退:目标无 ip/ss 时自动落 ifconfig/route -n/netstat。
var reconCommands = []struct {
	key string
	cmd string
}{
	{"nics", "ip -o -4 addr show 2>/dev/null || ifconfig -a 2>/dev/null"},
	{"routes", "ip route 2>/dev/null || route -n 2>/dev/null"},
	{"neighbors", "ip neigh 2>/dev/null || arp -a 2>/dev/null"},
	{"hosts", "cat /etc/hosts 2>/dev/null"},
	{"conns", "(ss -ant 2>/dev/null; ss -anu 2>/dev/null) || netstat -an 2>/dev/null"},
}

// reconCommandsWindows 是 Windows 立足点的对应命令包。此前只发 Linux 命令，
// 导致 Windows 马（aspx/asp）执行侦察时五条全失败——而 recon 包其实早就实现了
// ipconfig /all、route print、netstat -ano 的解析（recon/recon.go）。命令包与
// 解析器对齐后，Windows 立足点才能产出同样的结构化报告。
// 全部只读；用 cmd 内建的 2>nul 抑制错误噪音。
var reconCommandsWindows = []struct {
	key string
	cmd string
}{
	{"nics", "ipconfig /all 2>nul"},
	{"routes", "route print 2>nul"},
	{"neighbors", "arp -a 2>nul"},
	{"hosts", "type %SystemRoot%\\System32\\drivers\\etc\\hosts 2>nul"},
	{"conns", "netstat -ano 2>nul"},
}

// reconCommandPack 按方言选命令包。判据是 session.PlatformForSession 给出的
// 平台（驱动自报 > 马型推断），不再是本文件自己的字符串匹配——否则同一个
// 立足点会被隧道子系统判成 Windows、被侦察判成 Linux。
func reconCommandPack(platform string) []struct {
	key string
	cmd string
} {
	if platform == session.PlatformWindows {
		return reconCommandsWindows
	}
	return reconCommands
}

// runReconPack 逐条执行一个命令包，返回成功输出与逐条错误。
// 单条失败不中断整体（被动侦察：拿到什么算什么）。
func runReconPack(ctx context.Context, sess session.Session, pack []struct {
	key string
	cmd string
}) (map[string]string, map[string]string) {
	outputs := map[string]string{}
	cmdErrs := map[string]string{}
	for _, c := range pack {
		stdout, _, err := sess.Exec(ctx, c.cmd, 15*time.Second)
		if err != nil {
			cmdErrs[c.key] = err.Error()
			continue
		}
		outputs[c.key] = stdout
	}
	return outputs, cmdErrs
}

// sessionReconTool 经会话执行被动侦察命令包并结构化回库。
func (s *Server) sessionReconTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "session_recon",
		Description: "【被动优先】经立足点会话在目标侧执行【只读】被动侦察命令包(ip addr / ip route / arp -a / /etc/hosts / netstat," +
			"目标无 ip 命令自动回退 ifconfig/route -n),不发任何探测包。" +
			"输出结构化报告(网卡/路由/邻居/hosts/监听端口/已建立对端)+ 高价值推断(非默认静态路由指向的网段、" +
			"已证明可通的对端主机、多网卡跳板);" +
			"发现的存活 IP 自动登记为 ip 资产、监听端口登记为 service 资产,报告与推断 record_fact 进探索图。" +
			"被动信息不够再考虑主动慢扫(另过 guard)。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "integer", "description": "register_session 返回的会话 id"},
			},
			"required": []any{"session_id"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				SessionID int64 `json:"session_id"`
			}
			_ = json.Unmarshal(in, &a)
			sess, err := s.lookupSession(ctx, a.SessionID, db.SessionActionRecon)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			// 能力前置检查（I2）：驱动明确不支持执行时，五条侦察命令注定全失败。
			// 直接返回能力缺失，不空打五条——那五条失败会被读成「目标没有网络
			// 信息」，而真实原因是这个通道不能执行。
			ct := capability.For(sess)
			if ok, reason := ct.Check(capability.OpExec); !ok {
				return actool.Errorf("capability_missing: " + reason + "。侦察依赖命令执行，换一个能执行的立足点（如命令马/反转 shell）。"), nil
			}
			// 逐条执行:每条单独取输出(绕开 64KB 截断),失败不中断整体——
			// 被动侦察的原则是「拿到什么算什么」,一条命令挂了其余照常。
			//
			// 命令包按【方言】选，而方言来自 session.PlatformForSession（驱动
			// 自报 > 马型推断）——不是本文件自己字符串匹配 Kind。此前这里有一份
			// 独立的匹配逻辑，于是「隧道按 Windows 走、侦察按 Linux 走」这种
			// 自相矛盾的现场是可能的。
			known := session.DescribePlatform(sess) // "" = 驱动没自报，方言是猜的
			plat := session.PlatformForSession(sess)
			// 租约（I4）：侦察同样是「在用这个会话」，必须计入存活声明。
			s.noteSessionLease(ctx, a.SessionID, "recon")
			outputs, cmdErrs := runReconPack(ctx, sess, reconCommandPack(plat))

			// 全灭且方言本来就是猜的：可能不是会话死了，而是猜错了方言。
			// 用一条哨兵回显现场测一次（sh 会算 $((40+2))，cmd 不会），
			// 测出来不同就换包再跑。只在这种情况下多花 1 条命令，正常路径零开销。
			//
			// 为什么要做：反转 shell 的驱动结构上不知道平台（PTY 不区分 OS），
			// 马型推断按 linux 兜底；若目标其实是 Windows，五条命令全失败，
			// 报告为空，planner 读到「网卡 0 / 路由 0」会当成「这个立足点没有
			// 任何网络信息」，进而放弃整条横向分支——一个由方言猜错制造的假事实。
			switched := false
			if len(outputs) == 0 && known == "" {
				if d := session.DetectShellDialect(ctx, sess, 8*time.Second); d != "" && d != plat {
					plat = d
					outputs, cmdErrs = runReconPack(ctx, sess, reconCommandPack(plat))
					switched = true
				}
			}
			if len(outputs) > 0 {
				s.sessStore.Touch(ctx, a.SessionID)
			}

			report := &recon.ReconReport{
				NICs:         recon.ParseNICsAuto(outputs["nics"]),
				Routes:       recon.ParseRoutesAuto(outputs["routes"]),
				Neighbors:    recon.ParseNeighbors(outputs["neighbors"]),
				HostsEntries: recon.ParseHosts(outputs["hosts"]),
			}
			report.Listening, report.Peers = recon.ParseConnTable(outputs["conns"])
			inferences := recon.Infer(report)

			ri := agent.RunInfoFrom(ctx)
			assetIDs, assetSummary, assetErrs := s.reconInsertAssets(ctx, ri.TaskID, report)
			factID := s.reconRecordFact(ri, a.SessionID, report, inferences, assetIDs)

			// I3/T-13：把这次侦察落成可核验的观测 + 拓扑边。
			//
			// 为什么值得多写这一步：探索图里的 fact 只承载「结论」，不承载
			// 「这条结论是从哪条命令的哪段输出读出来的」。复核时要重跑命令，
			// 而重跑得到的是另一次观测。观测台账把原文留下来，拓扑边把
			// 「目标自己说它开着」与「我们真的连上了」分开——这两件事混在
			// 一条 fact 里时，planner 无法判断某条端口信息到底能不能打。
			//
			// 失败只降级不阻断：侦察结果本身已经进了资产图与探索图。
			obsIDs, edgeCount, obsNote := s.recordReconObservations(
				ctx, ri.TaskID, a.SessionID, plat, "session:"+itoa(a.SessionID), outputs, cmdErrs, report, "")

			out := map[string]any{
				"report":     report,
				"inferences": inferences,
				"summary":    reconSummary(report, inferences),
				// 方言与命令包必须随结果一起返回：没有它，一份空报告无法区分
				// 「目标没有网络信息」和「命令根本没说对语言」。
				"platform":     plat,
				"command_pack": map[string]string{session.PlatformWindows: "windows", session.PlatformLinux: "linux"}[plat],
			}
			if len(obsIDs) > 0 {
				out["observation_ids"] = obsIDs
				out["topology_edges"] = edgeCount
			}
			if obsNote != "" {
				out["observation_note"] = obsNote
			}
			if known == "" {
				out["platform_source"] = "inferred" // 马型推断，非驱动自报
			} else {
				out["platform_source"] = "driver"
			}
			if switched {
				out["platform_detected"] = "首次命令包全灭，哨兵回显判定实际方言为 " + plat + "，已换包重跑"
			}
			if assetSummary != "" {
				out["assets"] = assetSummary
			}
			if factID > 0 {
				out["fact_id"] = factID
			}
			if len(cmdErrs) > 0 {
				out["command_errors"] = cmdErrs
			}
			if len(assetErrs) > 0 {
				out["asset_errors"] = assetErrs
			}
			// 全灭时把「这可能是方言/权限问题，不是目标没有网络」放到顶层。
			// 否则一份空报告会被读成结论，而它其实只是一次失败。
			if len(outputs) == 0 {
				out["note"] = "五条侦察命令均无输出：不要把空报告当成「目标没有网络信息」。" +
					"按 platform=" + plat + "（来源 " + out["platform_source"].(string) + "）发包；" +
					"若会话本身可用，更可能是方言/权限/命令缺失问题——用 session_exec 手工确认（Linux: uname -s; Windows: cmd /c ver）。"
			}
			return jsonResult(out)
		},
	})
}

// reconSummary 给模型的一句话摘要。
func reconSummary(r *recon.ReconReport, inf []recon.Inference) string {
	static := 0
	for _, rt := range r.Routes {
		if rt.IsStatic {
			static++
		}
	}
	return fmt.Sprintf("网卡 %d / 路由 %d(静态 %d)/ 邻居 %d / hosts %d / 监听 %d / 已建立对端 %d;高价值推断 %d 条",
		len(r.NICs), len(r.Routes), static, len(r.Neighbors), len(r.HostsEntries),
		len(r.Listening), len(r.Peers), len(inf))
}

// reconInsertAssets 把侦察发现落资产库(best-effort):邻居/对端/hosts/本机 NIC 的
// 新 IP → ip 资产;监听端口 → 本机主 IP 上的 service 资产。返回锚定资产 id 列表、
// 摘要与错误(逐条错误不中断整体)。
func (s *Server) reconInsertAssets(ctx context.Context, taskID int64, r *recon.ReconReport) ([]int64, string, []string) {
	if s.m.Assets() == nil || taskID <= 0 {
		return nil, "", nil
	}
	assets := s.m.Assets()
	ids := []int64{}
	errs := []string{}
	seenIP := map[string]bool{}
	ipCount := 0
	addIP := func(ip string) {
		if ip == "" || seenIP[ip] {
			return
		}
		seenIP[ip] = true
		id, err := assets.UpsertIP(db.UpsertIPReq{IP: ip, TaskID: taskID})
		if err != nil {
			errs = append(errs, fmt.Sprintf("ip %s: %v", ip, err))
			return
		}
		ids = append(ids, id)
		ipCount++
	}

	// 本机非回环 NIC IP(双网卡全部登记);主 IP(第一个)挂监听端口的 service 资产。
	hostIP := ""
	for _, n := range r.NICs {
		addr := n.IP
		if strings.HasPrefix(addr, "127.") || strings.HasPrefix(addr, "169.254.") {
			continue
		}
		if hostIP == "" {
			hostIP = addr
		}
		addIP(addr)
	}
	for _, nb := range r.Neighbors {
		addIP(nb.IP)
	}
	for _, p := range r.Peers {
		addIP(p.IP)
	}
	for _, h := range r.HostsEntries {
		if strings.HasPrefix(h.IP, "127.") {
			continue
		}
		addIP(h.IP)
	}

	svcCount := 0
	localOnly := 0
	if hostIP != "" {
		for _, l := range r.Listening {
			// 绑在回环上的监听不能挂到主网卡上（I3）。
			//
			// 此前这里无条件用 hostIP 登记，于是「127.0.0.1:3306 在听」被写成
			// 「10.0.0.5:3306 开着」——一个由建模方式制造的假入口。planner
			// 会据此派一条横向意图，worker 连不上，然后把失败记成「服务关了」。
			// 一个纯信息失真，最终变成一条关于目标的假事实。
			if observation.IsLoopback(l.Bind) {
				localOnly++
				continue
			}
			name := recon.ServiceOf(l.Port)
			if name == "" {
				name = fmt.Sprintf("unknown-%d", l.Port)
			}
			// 绑定在具体网卡地址上的，登记到那个地址；绑 0.0.0.0 的才归到主 IP。
			bind := l.Bind
			if bind == "" || bind == "0.0.0.0" {
				bind = hostIP
			}
			id, err := assets.UpsertOtherService(db.UpsertOtherServiceReq{
				IP: bind, Port: l.Port, ServiceName: name, TaskID: taskID,
			})
			if err != nil {
				errs = append(errs, fmt.Sprintf("service %s:%d: %v", bind, l.Port, err))
				continue
			}
			ids = append(ids, id)
			svcCount++
		}
	}
	summary := fmt.Sprintf("登记 ip 资产 %d 个、service 资产 %d 个", ipCount, svcCount)
	if localOnly > 0 {
		// 说清「没登记」而不是默默跳过：否则 agent 看到监听表里有 3306、
		// 资产表里没有，会以为是登记漏了而反复重跑侦察。
		summary += fmt.Sprintf("；另有 %d 个仅本机回环可达的监听未登记为远程服务（不可作为横向目标）", localOnly)
	}
	if len(ids) > 20 {
		ids = ids[:20] // 锚定列表控制规模
	}
	// 期 4:出当前授权范围的新 IP 按 /24 登记 pending_scope 等人工审批(见 pendingscope.go)。
	s.reconRegisterPendingScope(taskID, seenIP)
	return ids, summary, errs
}

// reconRecordFact 把侦察报告与推断写进探索图(record_fact 同路径:fact 节点 +
// intent→yields 边)。非任务运行/图不可用则跳过(返回 0)。
func (s *Server) reconRecordFact(ri agent.RunInfo, sessionID int64, r *recon.ReconReport, inf []recon.Inference, anchors []int64) int64 {
	if ri.ExplorationID <= 0 || s.m.pg == nil {
		return 0
	}
	lines := []string{}
	for _, i := range inf {
		line := "· " + i.Summary
		if i.Detail != "" {
			line += " — " + i.Detail
		}
		lines = append(lines, line)
	}
	detail := reconSummary(r, inf)
	if len(lines) > 0 {
		detail += "\n高价值推断:\n" + strings.Join(lines, "\n")
	}
	payload := map[string]any{
		"summary":    fmt.Sprintf("session %d 内网被动侦察:%s", sessionID, reconSummary(r, inf)),
		"detail":     detail,
		"evidence":   "session_recon 被动命令包(ip addr/ip route/arp -a/hosts/netstat,只读)",
		"confidence": "observed",
	}
	store := s.m.pg.Exploration(ri.ExplorationID)
	id, err := store.AddNode(db.KindFact, payload, 5, "confirmed", "worker", anchors)
	if err != nil {
		return 0
	}
	if ri.IntentID > 0 {
		if node, nerr := store.GetNode(ri.IntentID); nerr == nil && node != nil && node.Kind == db.KindIntent {
			_ = store.Link(ri.IntentID, db.RelYields, id)
		}
	}
	return id
}
