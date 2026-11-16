// pending_handoff.go 立足点待移交的发现侧挂钩与审批 API（on_foothold 半自动移交）。
//
// 发现侧全自动：register_session 成功（会话 alive）即登记 pending_handoff 并往
// 任务活动流写通知卡——人不再需要盯着会话台账等「拿到立足点了」。
// 执行侧保持人工闸门：approve 走与人工按钮完全相同的 executeSessionHandoff
// （初始 scope 仍只有立足点 /32、新网段仍走 pending_scope 审批）——内网的授权
// 边界与外网几乎必然不同，这一点不因「发现自动化」而放松。
package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"chengjian.local/chengjian/db"
)

// notePendingHandoff 在立足点登记成功后自动登记待移交并写通知卡（仅首次登记
// 调用；UpsertPendingHandoff 的 UNIQUE(session_id) 保证同一会话只提示一次）。
func (s *Server) notePendingHandoff(taskID, sessionID int64, hostIP string) {
	if s.sessStore == nil || taskID <= 0 || sessionID <= 0 {
		return
	}
	created, err := s.sessStore.UpsertPendingHandoff(sessionID, taskID, hostIP)
	if err != nil {
		// 登记失败不阻断会话本身：登记是「提示人」的增强，立足点与移交按钮都还在。
		s.appendIntranetActivity(taskID, "pending_handoff",
			fmt.Sprintf("立足点会话 #%d 的待移交登记写入失败：%v（不影响会话，人工仍可从会话台账直接移交）", sessionID, err), "")
		return
	}
	if !created {
		return
	}
	if t, ok := s.m.Task(strconv.FormatInt(taskID, 10)); ok {
		detail, _ := json.Marshal(map[string]any{
			"session_id": sessionID, "host": hostIP,
			"list_api":    "/api/pending-handoff",
			"approve_api": fmt.Sprintf("/api/pending-handoff/by-session/%d/approve", sessionID),
		})
		s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
			Summary: fmt.Sprintf("拿到立足点会话 #%d（%s），已登记【待移交内网】；人工批准后自动创建内网子任务（初始授权仅立足点主机）", sessionID, hostIP),
			Detail:  string(detail)})
	}
}

// pendingHandoffListGET /api/pending-handoff — 待移交列表（默认只 pending，?all=1 含已决）。
func (s *Server) pendingHandoffListGET(w http.ResponseWriter, r *http.Request) {
	if s.sessStore == nil {
		writeErr(w, 503, "session store 未启用")
		return
	}
	items, err := s.sessStore.ListPendingHandoff(r.URL.Query().Get("all") == "1")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// pendingHandoffDecide POST /api/pending-handoff/{id}/decide {action:"approve"|"dismiss"}。
func (s *Server) pendingHandoffDecide(w http.ResponseWriter, r *http.Request) {
	if s.sessStore == nil {
		writeErr(w, 503, "session store 未启用")
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	s.decidePendingHandoffRow(w, r, id, body.Action)
}

// pendingHandoffApproveBySession POST /api/pending-handoff/by-session/{id}/approve —
// 通知卡里的直达入口：按会话 id 找 pending 登记并批准（比先查列表再带条目 id 少一步）。
func (s *Server) pendingHandoffApproveBySession(w http.ResponseWriter, r *http.Request) {
	if s.sessStore == nil {
		writeErr(w, 503, "session store 未启用")
		return
	}
	sessID, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	items, lerr := s.sessStore.ListPendingHandoff(false)
	if lerr != nil {
		writeErr(w, 500, lerr.Error())
		return
	}
	for _, it := range items {
		if it.SessionID == sessID {
			s.decidePendingHandoffRow(w, r, it.ID, "approve")
			return
		}
	}
	writeErr(w, 404, fmt.Sprintf("会话 %d 没有待审批的移交登记（可能已处置）", sessID))
}

// decidePendingHandoffRow 是审批的共用核心。approve = 执行移交（与人工按钮同一
// 代码路径）；幂等/冲突语义与 pending_scope 一致：重复同一决定返回现状，冲突
// 决定 409。approve 执行失败时登记保持 pending（不是把失败抹成已决）——失败
// 原因随响应返回，人工可重试。
func (s *Server) decidePendingHandoffRow(w http.ResponseWriter, r *http.Request, id int64, action string) {
	if action != "approve" && action != "dismiss" {
		writeErr(w, 400, "action 必须是 approve 或 dismiss")
		return
	}
	row, err := s.sessStore.GetPendingHandoff(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "pending_handoff 条目不存在")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	desired := "dismissed"
	if action == "approve" {
		desired = "approved"
	}
	if row.Status != "pending" {
		if row.Status == desired {
			writeJSON(w, 200, row) // 重复同一决定：幂等返回现状
			return
		}
		writeErr(w, 409, "条目已处置(status="+row.Status+")，冲突决定不予受理")
		return
	}
	handoffTaskID := ""
	if action == "approve" {
		rec, gerr := s.sessStore.Get(r.Context(), row.SessionID)
		if gerr != nil || rec == nil {
			writeErr(w, 500, fmt.Sprintf("立足点会话 #%d 读取失败（可能已删除）：dismiss 后由原任务重新拿立足点", row.SessionID))
			return
		}
		handoffTaskID, err = s.executeSessionHandoff(r.Context(), rec)
		if err != nil {
			writeErr(w, 500, "移交执行失败（登记保持待审批，可重试）："+err.Error())
			return
		}
	}
	row, err = s.sessStore.DecidePendingHandoff(id, action == "approve", handoffTaskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if handoffTaskID != "" {
		s.appendIntranetActivity(row.TaskID, "pending_handoff",
			fmt.Sprintf("立足点会话 #%d 的移交已批准，内网子任务 %s 已创建并启动", row.SessionID, handoffTaskID),
			fmt.Sprintf("session_id=%d handoff_task=%s", row.SessionID, handoffTaskID))
	}
	writeJSON(w, 200, row)
}
