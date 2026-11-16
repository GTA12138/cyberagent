// intranet_snapshot.go 把内网作战台账投影成 planner 每轮可见的只读态势。
//
// 动机：sessions / tunnels / credentials / pending_scope 四张表此前完全不进
// 探索图。planner 因此对内网态势结构性失明——不知道有几条隧道、哪条 alive、
// 有什么凭据可用、哪个网段在等审批。后果是它反复派「再找入口 / 再建隧道」的
// 重复意图，或在无凭据可用时硬派横向意图，或朝未授权网段派意图然后被 RoE 拦。
//
// 设计约束：
//   - 只读投影，绝不触发探测/建连（快照每轮都跑，不能有副作用）。
//   - 各列表硬限长（默认 5 条）——prompt 预算由 agent 侧 contextSnapshot 再兜
//     一层，但源头就该有界。
//   - 不含任何秘密：凭据只给计数与类型分布，不给用户名/密文；会话只给
//     id/kind/宿主/状态，不给 URL 里的口令或 secret。
package server

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/session"
)

// intranetSnapshotLimits 是投影的硬限长。台账可能很长（几十条历史隧道），
// 但 planner 只需要「当前态势」，历史明细按需用 tunnel_list / list_credentials 查。
const intranetSnapshotLimits = 5

// intranetSnapshot 组装某任务的内网态势投影。返回 nil 表示「本任务没有内网
// 作战迹象」——此时 graph_overview 不输出 intranet 键，外网任务的 prompt 不受影响。
func (s *Server) intranetSnapshot(taskID int64) map[string]any {
	if taskID <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sessions := s.snapshotSessions(ctx, taskID)
	tunnels := s.snapshotTunnels(ctx, taskID)
	creds := s.snapshotCredentials(ctx, taskID)
	pending := s.snapshotPendingScope(taskID)
	// 拓扑观测（I3/T-13）：只给新鲜边，已验证与未验证分开列。
	// 不进这里的话，「目标自己说 445 开着」与「我们真的连上了」在 planner
	// 眼里长得一样，而这两件事决定的是「要不要打」。
	topology := s.snapshotTopology(ctx, taskID)

	if len(sessions) == 0 && len(tunnels) == 0 && creds == nil && len(pending) == 0 && topology == nil {
		return nil
	}
	out := map[string]any{}
	if len(sessions) > 0 {
		out["sessions"] = sessions
	}
	if len(tunnels) > 0 {
		out["tunnels"] = tunnels
	}
	if creds != nil {
		out["credentials"] = creds
	}
	if len(pending) > 0 {
		out["pending_scope"] = pending
	}
	if topology != nil {
		out["topology"] = topology
	}
	// 一句话结论，减少 planner 自己从列表里推断的成本。
	out["summary"] = intranetSummary(sessions, tunnels, creds, pending)
	return out
}

// snapshotSessions 列出本任务创建或获得明确授权的会话（含 dead，让 planner 知道「有过立足点
// 但现在死了」——这与「从未有过立足点」是完全不同的态势）。
func (s *Server) snapshotSessions(ctx context.Context, taskID int64) []map[string]any {
	if s.sessStore == nil {
		return nil
	}
	recs, _, err := s.sessStore.ListAccessiblePage(ctx, taskID, "", intranetSnapshotLimits, 0)
	if err != nil {
		return nil
	}
	// 授权来的会话要说清「能做什么」。只列出存在、不列出权限，planner 会对着
	// 一条只有 read 授权的立足点派 exec 意图，worker 在执行时才被拒——信息
	// 明明在库里，只是没到决策层。一次查询覆盖整页，不按行查。
	//
	// 查询失败时【什么都不说】，不要退化成「无授权」：把一个读库错误说成
	// 「未授权」，等于用一条假信息替换一条缺失信息，比不说更糟。
	grants, grantsErr := s.sessStore.GrantsForTask(ctx, taskID)
	out := []map[string]any{}
	for _, r := range recs {
		item := map[string]any{
			"id":     r.ID,
			"kind":   r.Kind,
			"status": r.Status,
		}
		if r.CreatedByTask != taskID && grantsErr == nil {
			// 非本任务创建 = 只能靠 grant 访问。空动作集只可能出现在「两次查询
			// 之间授权被撤销」的竞态里，此时保守说「未读到有效授权」。
			acts := grants[r.ID]
			if len(acts) == 0 {
				item["granted_actions"] = []string{}
				item["note"] = "非本任务创建的会话，未读到有效授权（可能刚被撤销/过期）；不要假设可 exec"
			} else {
				item["granted_actions"] = acts
			}
		}
		if r.Lang != "" {
			item["lang"] = r.Lang
		}
		// 宿主只给 host 部分，不给带凭据的完整 URL。
		if h := hostOfURL(r.URL); h != "" && len(h) <= 256 {
			item["host"] = h
		}
		if len(r.MetadataOmitted) > 0 {
			item["metadata_omitted"] = r.MetadataOmitted
		}
		// platform 是「这个立足点按什么方言说话」，由驱动自报，不是观测。
		//
		// 为什么必须进每轮快照：它此前只在 register_session 的返回值里出现一次，
		// 一轮之后就从上下文里消失。planner 于是对着 Windows 立足点派
		// 「跑 id / uname -a / ls -la」的意图，worker 执行全失败，再把失败记成
		// 「会话可能失效」——一个纯粹由信息不同步制造出来的假事实。这是唯一
		// 一条能同时改变 planner 派什么、worker 怎么写命令的字段。
		if p := s.sessionPlatform(r.ID); p != "" {
			item["platform"] = p
		}
		out = append(out, item)
	}
	return out
}

// snapshotTunnels 列出本任务未终结的隧道。stopped 的历史行不投影（planner 不关心）。
func (s *Server) snapshotTunnels(ctx context.Context, taskID int64) []map[string]any {
	if s.tunnels == nil {
		return nil
	}
	recs, err := s.tunnels.TaskRecords(ctx, taskID)
	if err != nil {
		return nil
	}
	out := []map[string]any{}
	for _, r := range recs {
		if r.State == db.TunnelStopped {
			continue
		}
		if len(out) >= intranetSnapshotLimits {
			break
		}
		item := map[string]any{
			"id":      r.ID,
			"kind":    r.Kind,
			"adapter": r.Adapter,
			"state":   r.State,
		}
		if r.Kind == "portfwd" && r.TargetHost != "" {
			item["target"] = r.TargetHost
		}
		if r.Error != "" {
			item["error"] = truncateForSnapshot(r.Error, 200)
		}
		out = append(out, item)
	}
	return out
}

// snapshotCredentials 只给计数与类型分布，不给任何用户名/密文。planner 需要的是
// 「有没有凭据可复用、大概什么类型」，具体选哪条由 credential_reuse_plan 回答。
func (s *Server) snapshotCredentials(ctx context.Context, taskID int64) map[string]any {
	if s.credStore == nil {
		return nil
	}
	recs, err := s.credStore.ListByTask(ctx, taskID)
	if err != nil || len(recs) == 0 {
		return nil
	}
	byType := map[string]int{}
	verified := 0
	for _, c := range recs {
		byType[c.CredType]++
		if c.Verified {
			verified++
		}
	}
	return map[string]any{
		"count":    len(recs),
		"verified": verified,
		"by_type":  byType,
	}
}

// snapshotPendingScope 列出待授权网段。这是 planner 最需要的一条：它必须知道
// 「有网段在等人工批准」，否则会一直朝未授权段派意图、然后被 RoE 反复拦下。
func (s *Server) snapshotPendingScope(taskID int64) []map[string]any {
	as := s.m.Assets()
	if as == nil {
		return nil
	}
	rows, err := as.ListPendingScope(taskID, false) // false = 只给 pending
	if err != nil {
		return nil
	}
	out := []map[string]any{}
	for _, r := range rows {
		if len(out) >= intranetSnapshotLimits {
			break
		}
		out = append(out, map[string]any{
			"cidr": r.Value,
			"hits": r.Hits,
		})
	}
	return out
}

// intranetSummary 生成一句话态势，把「列表里有什么」压成「现在能做什么」。
// 这句是 planner 决策的主要依据：尤其「隧道存在但无 alive 入口」与「有网段
// 待授权」两条，直接决定它该不该派内网意图。
func intranetSummary(sessions, tunnels []map[string]any, creds map[string]any, pending []map[string]any) string {
	aliveSess, aliveTun := 0, 0
	winSess := 0
	for _, x := range sessions {
		if x["status"] == db.SessionAlive {
			aliveSess++
		}
		if x["platform"] == session.PlatformWindows {
			winSess++
		}
	}
	for _, x := range tunnels {
		if x["state"] == db.TunnelAlive {
			aliveTun++
		}
	}
	parts := []string{}
	if aliveSess > 0 {
		parts = append(parts, "有 "+strconv.Itoa(aliveSess)+" 个存活立足点会话")
	}
	// 只在真有 Windows 立足点时加这一句：它直接改变 planner 该派什么意图、
	// worker 该写什么命令（cmd/PowerShell，无 chmod/setsid//tmp/sh 管道）。
	if winSess > 0 {
		parts = append(parts, "其中 "+strconv.Itoa(winSess)+" 个是 Windows 立足点（命令按 cmd/PowerShell 写，不要用 sh 语法与 /tmp）")
	}
	if len(sessions) > 0 && aliveSess == 0 {
		parts = append(parts, "当前展示的会话均未处于 alive；请用 session_list 翻页核实，不能据此断言全部失效")
	}
	if aliveTun > 0 {
		parts = append(parts, "当前展示有 "+strconv.Itoa(aliveTun)+" 条台账状态 alive 的隧道；目标可达性需逐目标验证")
	}
	if len(tunnels) > 0 && aliveTun == 0 {
		parts = append(parts, "当前展示的隧道无 alive 入口；先用 tunnel_list 核实完整台账和链路状态")
	}
	if creds != nil {
		n, _ := creds["count"].(int)
		v, _ := creds["verified"].(int)
		parts = append(parts, "凭据库有 "+strconv.Itoa(n)+" 条（"+strconv.Itoa(v)+" 条已验证），可考虑 credential_reuse_plan 驱动横向")
	}
	if len(pending) > 0 {
		parts = append(parts, "有 "+strconv.Itoa(len(pending))+" 个网段待人工授权——批准前不得对其主动探测")
	}
	if len(parts) == 0 {
		return "内网作战尚无有效进展"
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "；"
		}
		out += p
	}
	return out
}

// hostOfURL 取 URL 的 host 部分（含端口），用于态势展示；解析失败返回空。
// 只给 host 不给完整 URL：webshell URL 的查询串里可能带连接口令。
func hostOfURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// truncateForSnapshot 截断长文本（错误信息可能很长），保留头部。
func truncateForSnapshot(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
