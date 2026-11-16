package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// These routes are management-only, behind the same JWT boundary as manual
// session operations. They are deliberately not exposed as agent tools.
func (s *Server) grantSessionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if !s.intranetSessionsReady(w) {
		return 0, false
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return 0, false
	}
	// Revocation must remain available even when credentials are corrupt or
	// cannot be decrypted. Authorization management needs existence only.
	rec, err := s.sessStore.Health(r.Context(), id)
	if err != nil {
		writeErr(w, 500, "会话读取失败")
		return 0, false
	}
	if rec == nil {
		writeErr(w, 404, "会话不存在")
		return 0, false
	}
	return id, true
}

func (s *Server) intranetSessionGrants(w http.ResponseWriter, r *http.Request) {
	id, ok := s.grantSessionID(w, r)
	if !ok {
		return
	}
	grants, err := s.sessStore.AccessGrants(r.Context(), id)
	if err != nil {
		writeErr(w, 500, "授权台账读取失败")
		return
	}
	writeJSON(w, 200, map[string]any{"items": grants, "note": "资源所有者权限独立于共享授权；撤销只阻止后续调用，不表示远端作业已停止。"})
}

func (s *Server) intranetGrantSession(w http.ResponseWriter, r *http.Request) {
	id, ok := s.grantSessionID(w, r)
	if !ok {
		return
	}
	taskID, err := pathID(r, "task_id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var input struct {
		Actions   []string   `json:"actions"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&input); err != nil {
		writeErr(w, 400, "授权请求格式错误")
		return
	}
	if err := s.sessStore.GrantAccess(r.Context(), id, taskID, input.Actions, input.ExpiresAt, "manual_management"); err != nil {
		s.auditIntranet(r, "session_grant session=%d task=%d denied", id, taskID)
		writeErr(w, 400, fmt.Sprintf("授权失败: %v", err))
		return
	}
	s.auditIntranet(r, "session_grant session=%d task=%d actions=%v", id, taskID, input.Actions)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) intranetRevokeSession(w http.ResponseWriter, r *http.Request) {
	id, ok := s.grantSessionID(w, r)
	if !ok {
		return
	}
	taskID, err := pathID(r, "task_id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.sessStore.RevokeAccess(r.Context(), id, taskID); err != nil {
		writeErr(w, 500, "撤销授权失败")
		return
	}
	s.auditIntranet(r, "session_grant_revoke session=%d task=%d", id, taskID)
	writeJSON(w, 200, map[string]any{"ok": true, "note": "已撤销共享授权；资源所有者权限不受影响，正在执行的操作不由此取消。"})
}
