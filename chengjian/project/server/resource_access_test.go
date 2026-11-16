package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/session"
	"chengjian.local/chengjian/tunnel"
	actool "github.com/Autumn-27/norma/tool"
)

type resourceAccessSession struct {
	id    int64
	calls atomic.Int32
}

func (s *resourceAccessSession) ID() int64                  { return s.id }
func (s *resourceAccessSession) Kind() string               { return "fixture" }
func (s *resourceAccessSession) Test(context.Context) error { s.calls.Add(1); return nil }
func (s *resourceAccessSession) Exec(context.Context, string, time.Duration) (string, string, error) {
	s.calls.Add(1)
	return "fixture", "", nil
}
func (s *resourceAccessSession) ReadFile(context.Context, string) ([]byte, error) {
	s.calls.Add(1)
	return []byte("fixture"), nil
}
func (s *resourceAccessSession) WriteFile(context.Context, string, []byte) error {
	s.calls.Add(1)
	return nil
}
func (s *resourceAccessSession) Close(context.Context) error { return nil }

func TestResourceAccessRejectsTasklessBeforeSideEffects(t *testing.T) {
	s := &Server{}
	for _, tool := range []actool.CoreTool{s.registerSessionTool(), s.reverseListenTool()} {
		r, err := tool.Call(context.Background(), json.RawMessage(`{"url":"http://127.0.0.1:1/fixture","port":40000}`), nil)
		if err != nil || !r.IsError || !strings.Contains(r.Flatten(), "任务上下文") {
			t.Fatalf("%s taskless result=%v err=%v", tool.Name(), r, err)
		}
	}
	if _, err := s.lookupSession(context.Background(), 1, db.SessionActionExec); err == nil {
		t.Fatal("taskless session access allowed")
	}
	if err := s.authorizeTunnel(context.Background(), 1); err == nil {
		t.Fatal("taskless tunnel access allowed")
	}
}

func TestResourceAccessAndGrantManagement(t *testing.T) {
	pg := openIntranetTestDB(t)
	store := db.NewSessionStore(pg, []byte("access-fixture-key"))
	owner, err := pg.CreateTask("resource owner fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(owner.ID) })
	other, err := pg.CreateTaskWithOptions("source only fixture", "fixture", db.TaskCreateOptions{SourceTaskIDs: []int64{owner.ID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(other.ID) })
	ctx := context.Background()
	id, err := store.Create(ctx, &db.SessionRecord{Kind: "fixture", Secret: json.RawMessage(`{}`), CreatedByTask: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, id) })
	fake := &resourceAccessSession{id: id}
	s := &Server{sessStore: store, sessReg: session.NewRegistry(), ctx: ctx}
	s.sessReg.Add(fake)
	otherCtx := agent.WithRunInfo(ctx, agent.RunInfo{TaskID: other.ID})
	input := json.RawMessage(fmt.Sprintf(`{"session_id":%d,"command":"fixture","path":"fixture","kind":"socks"}`, id))
	for _, tool := range []actool.CoreTool{s.sessionExecTool(), s.sessionReadTool(), s.sessionWriteTool(), s.sessionReconTool(), s.tunnelProbeTool(), s.tunnelDeployTool()} {
		r, err := tool.Call(otherCtx, input, nil)
		if err != nil || !r.IsError {
			t.Fatalf("%s must deny unrelated task: %v %v", tool.Name(), r, err)
		}
	}
	if fake.calls.Load() != 0 {
		t.Fatal("denied tool invoked driver")
	}
	ownerCtx := agent.WithRunInfo(ctx, agent.RunInfo{TaskID: owner.ID})
	r, err := s.sessionExecTool().Call(ownerCtx, input, nil)
	if err != nil || r.IsError || fake.calls.Load() != 1 {
		t.Fatalf("owner execution failed: %v %v", r, err)
	}
	// Human grant administration uses its existing authenticated management
	// boundary, independent of the agent caller's context.
	req := httptest.NewRequest(http.MethodPut, "/fixture", strings.NewReader(`{"actions":["read"]}`))
	req.SetPathValue("id", fmt.Sprint(id))
	req.SetPathValue("task_id", fmt.Sprint(other.ID))
	w := httptest.NewRecorder()
	s.intranetGrantSession(w, req)
	if w.Code != 200 {
		t.Fatalf("manual grant: %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/fixture", nil)
	req.SetPathValue("id", fmt.Sprint(id))
	w = httptest.NewRecorder()
	s.intranetSessionGrants(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"actions":["read"]`) {
		t.Fatalf("grant list: %d %s", w.Code, w.Body.String())
	}
	r, err = s.sessionReadTool().Call(otherCtx, input, nil)
	if err != nil || r.IsError {
		t.Fatalf("explicit read failed: %v %v", r, err)
	}
	r, err = s.sessionExecTool().Call(otherCtx, input, nil)
	if err != nil || !r.IsError {
		t.Fatalf("read grant escalated: %v %v", r, err)
	}
	if !s.taskIntranet(other.ID) {
		t.Fatal("authorized session not visible to task state")
	}
	if visible := s.snapshotSessions(ctx, other.ID); len(visible) != 1 || visible[0]["id"] != id {
		t.Fatalf("shared session not in planner overview: %v", visible)
	}
	r, err = s.sessionListTool().Call(otherCtx, json.RawMessage(`{}`), nil)
	if err != nil || r.IsError || !strings.Contains(r.Flatten(), `"capabilities"`) {
		t.Fatalf("accessible list: %v %v", r, err)
	}
	before := fake.calls.Load()
	if _, err := pg.Exec(`UPDATE sessions SET secret='corrupt' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if !s.taskIntranet(other.ID) {
		t.Fatal("corrupt secret poisoned metadata state")
	}
	if r, err := s.sessionListTool().Call(otherCtx, json.RawMessage(`{}`), nil); err != nil || r.IsError {
		t.Fatalf("corrupt secret poisoned metadata page: %v %v", r, err)
	}
	req = httptest.NewRequest(http.MethodDelete, "/fixture", nil)
	req.SetPathValue("id", fmt.Sprint(id))
	req.SetPathValue("task_id", fmt.Sprint(other.ID))
	w = httptest.NewRecorder()
	s.intranetRevokeSession(w, req)
	if w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	r, err = s.sessionReadTool().Call(otherCtx, input, nil)
	if err != nil || !r.IsError || fake.calls.Load() != before {
		t.Fatalf("revoked access invoked driver: %v %v", r, err)
	}
	// Closed/unavailable DB must never fall back to registry-only execution.
	closedDB, err := sql.Open("pgx", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := closedDB.Close(); err != nil {
		t.Fatal(err)
	}
	s.sessStore = db.NewSessionStore(&db.DB{DB: closedDB}, nil)
	r, err = s.sessionExecTool().Call(ownerCtx, input, nil)
	if err != nil || !r.IsError || fake.calls.Load() != before {
		t.Fatalf("DB outage did not fail closed: %v %v", r, err)
	}
}

func TestResourceAccessTunnelStopCannotAffectAnotherTask(t *testing.T) {
	pg := openIntranetTestDB(t)
	store := db.NewTunnelStore(pg)
	registry := session.NewRegistry()
	m, err := tunnel.NewManager(tunnel.Options{Store: store, Sessions: registry, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := store.Create(ctx, &db.TunnelRecord{TaskID: 42, Kind: "socks", State: db.TunnelDeploying})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM tunnels WHERE id=$1`, id) })
	s := &Server{tunnels: m}
	other := agent.WithRunInfo(ctx, agent.RunInfo{TaskID: 43})
	r, err := s.tunnelTeardownTool().Call(other, json.RawMessage(fmt.Sprintf(`{"tunnel_id":%d}`, id)), nil)
	if err != nil || !r.IsError {
		t.Fatalf("other task stop accepted: %v %v", r, err)
	}
	rec, err := store.Get(ctx, id)
	if err != nil || rec.State != db.TunnelDeploying {
		t.Fatalf("denied stop mutated deployment: %+v %v", rec, err)
	}
	owner := agent.WithRunInfo(ctx, agent.RunInfo{TaskID: 42})
	if err := s.authorizeTunnel(owner, id); err != nil {
		t.Fatalf("owner authorization failed: %v", err)
	}
}

func TestResourceAccessListenerOwnerCannotBeReassigned(t *testing.T) {
	h := &PenelopeHandler{running: true, taskID: 31, intentID: 1, listenPort: 40000, listenIface: "127.0.0.1"}
	if err := h.ensure(context.Background(), 40000, "127.0.0.1", 32, 2); err == nil {
		t.Fatal("unrelated task reused handler")
	}
	if h.taskID != 31 || h.intentID != 1 {
		t.Fatal("listener owner changed after rejected call")
	}
	if err := h.ensure(context.Background(), 40000, "127.0.0.1", 31, 3); err != nil {
		t.Fatal(err)
	}
	if h.taskID != 31 || h.intentID != 3 {
		t.Fatal("same-task reuse failed")
	}
}
