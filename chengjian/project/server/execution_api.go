package server

import (
	"context"
	"encoding/json"
	actool "github.com/Autumn-27/norma/tool"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) listExecutionAttempts(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid task id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	items, err := s.m.pg.ListExecutionAttempts(ctx, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) resolveExecutionAttempt(w http.ResponseWriter, r *http.Request) {
	taskID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || taskID <= 0 {
		writeErr(w, 400, "invalid task id")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("attempt"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid attempt id")
		return
	}
	var req struct {
		State      string `json:"state"`
		Assessment string `json:"assessment"`
	}
	if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	req.Assessment = strings.TrimSpace(req.Assessment)
	if (req.State != "completed" && req.State != "not_executed") || req.Assessment == "" {
		writeErr(w, 400, "请选择核对结论并填写依据")
		return
	}
	result, _ := json.Marshal(actool.Text("人工核对结果：" + req.Assessment))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err = s.m.pg.ResolveExecutionAttempt(ctx, taskID, id, req.State, req.Assessment, result); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
