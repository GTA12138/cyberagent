// intranet_activity.go 把内网链路的关键状态变化写进任务活动流。
//
// 动机：隧道建成/断链/重拉、立足点登记/失效——这些是内网作战最重要的态势事件，
// 但此前只写服务端日志（log.Printf）和 tunnels 表的 state 列。任务活动流里一片
// 空白，人类复盘时看不到「什么时候建的隧道、什么时候断的、重拉了几次」，planner
// 也拿不到任何触发信号。
//
// 与 fact 的分工：fact 表示「探索结论」（可被 prove_goal 引用、可被意图锚定），
// 隧道状态属于「基础设施状态」，塞进 fact 会污染血缘图并让意图锚点语义变味。
// 所以这里只写 activity（人类可见的时序记录）+ 依赖 intranet 快照键（planner
// 每轮读的实时台账）。两者合起来覆盖了「人看时序、机看现状」两种需求。
package server

import (
	"fmt"
	"log"
	"strconv"
	"time"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/tunnel"
)

// appendIntranetActivity 往某任务的 activity 写一条 system/text 记录。
// 任务已结束/删除、store 不可用时只留服务端日志（与 stage 记账同款降级）。
func (s *Server) appendIntranetActivity(taskID int64, tool, summary, detail string) {
	log.Printf("[intranet] task=%d tool=%s %s", taskID, tool, summary)
	if taskID <= 0 {
		return
	}
	key := strconv.FormatInt(taskID, 10)
	s.m.mu.Lock()
	t := s.m.tasks[key]
	s.m.mu.Unlock()
	if t == nil || t.Store == nil {
		return // 任务句柄已不在（结束/删除），日志已留
	}
	if _, err := t.Store.AppendActivity(db.Activity{
		Worker:  "system",
		Kind:    "text",
		Tool:    tool,
		Summary: summary,
		Detail:  detail,
	}); err != nil {
		log.Printf("[intranet] activity 写入失败(task=%d): %v", taskID, err)
	}
}

// noteSessionRegistered 记录立足点会话登记。
func (s *Server) noteSessionRegistered(taskID, sessionID int64, kind, host string) {
	s.appendIntranetActivity(taskID, "register_session",
		fmt.Sprintf("立足点已登记：会话 #%d（%s @ %s）", sessionID, kind, host),
		fmt.Sprintf("session_id=%d kind=%s host=%s", sessionID, kind, host))
}

// noteTunnelDeployed 记录隧道建成（含决定性验证通过）。
func (s *Server) noteTunnelDeployed(taskID, tunnelID int64, kind, adapter string, socksPort int, viaSession int64) {
	access := "socks5"
	if kind == "portfwd" {
		access = "portfwd"
	}
	summary := fmt.Sprintf("隧道已建成并验证：隧道 #%d（%s/%s，%s 入口）", tunnelID, kind, adapter, access)
	detail := fmt.Sprintf("tunnel_id=%d kind=%s adapter=%s socks_port=%d via_session=%d",
		tunnelID, kind, adapter, socksPort, viaSession)
	if socksPort > 0 {
		summary += fmt.Sprintf("，平台 127.0.0.1:%d", socksPort)
	}
	s.appendIntranetActivity(taskID, "tunnel_deploy", summary, detail)
}

// noteTunnelBroken 记录隧道断链（巡检发现或重拉失败）。
func (s *Server) noteTunnelBroken(taskID, tunnelID int64, reason string) {
	s.appendIntranetActivity(taskID, "tunnel_health",
		fmt.Sprintf("隧道 #%d 已断链：%s", tunnelID, truncateForSnapshot(reason, 120)),
		fmt.Sprintf("tunnel_id=%d state=error reason=%s at=%s",
			tunnelID, reason, time.Now().Format(time.RFC3339)))
}

// noteTunnelRedeployed 记录自动重拉成功。
func (s *Server) noteTunnelRedeployed(taskID, tunnelID int64, attempt int) {
	s.appendIntranetActivity(taskID, "tunnel_health",
		fmt.Sprintf("隧道 #%d 已自动重拉成功（第 %d 次尝试）", tunnelID, attempt),
		fmt.Sprintf("tunnel_id=%d redeploy_attempt=%d at=%s",
			tunnelID, attempt, time.Now().Format(time.RFC3339)))
}

// noteTunnelTornDown 记录隧道回收。
func (s *Server) noteTunnelTornDown(taskID, tunnelID int64) {
	s.appendIntranetActivity(taskID, "tunnel_teardown",
		fmt.Sprintf("隧道 #%d 已回收（teardown 完成）", tunnelID),
		fmt.Sprintf("tunnel_id=%d state=stopped", tunnelID))
}

// noteCredentialAdded 记录凭据收割。绝不写秘密值，只写类型与来源描述。
func (s *Server) noteCredentialAdded(taskID, credID int64, username, domain, credType, source string) {
	who := username
	if domain != "" {
		who = domain + `\` + username
	}
	s.appendIntranetActivity(taskID, "add_credential",
		fmt.Sprintf("收割凭据 #%d（%s，类型 %s）", credID, who, credType),
		fmt.Sprintf("credential_id=%d type=%s source=%s", credID, credType, truncateForSnapshot(source, 200)))
}

// recordTunnelLifecycle 是 tunnel.Manager.OnLifecycle 的 server 侧实现：
// 把隧道生命周期事件翻译成任务活动流里的一行人类可读记录。
func (s *Server) recordTunnelLifecycle(ev tunnel.LifecycleEvent) {
	if ev.TaskID <= 0 {
		return
	}
	switch ev.Kind {
	case tunnel.LifecycleDeployed:
		s.noteTunnelDeployed(ev.TaskID, ev.TunnelID, "socks", ev.Adapter, ev.SocksPort, 0)
	case tunnel.LifecycleBroken:
		s.noteTunnelBroken(ev.TaskID, ev.TunnelID, ev.Reason)
	case tunnel.LifecycleRedeployed:
		s.noteTunnelRedeployed(ev.TaskID, ev.TunnelID, 1)
	case tunnel.LifecycleRedeployFailed:
		s.appendIntranetActivity(ev.TaskID, "tunnel_health",
			fmt.Sprintf("隧道 #%d 自动重拉失败（第 %d 次）：%s", ev.TunnelID, ev.Attempt, truncateForSnapshot(ev.Reason, 120)),
			fmt.Sprintf("tunnel_id=%d attempt=%d err=%s", ev.TunnelID, ev.Attempt, ev.Reason))
	case tunnel.LifecycleTornDown:
		s.noteTunnelTornDown(ev.TaskID, ev.TunnelID)
	}
}
