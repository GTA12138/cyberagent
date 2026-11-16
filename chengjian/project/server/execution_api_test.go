package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"chengjian.local/chengjian/db"
	"net/http/httptest"
	"testing"
)

func TestExecutionReconciliationAPIAndOwnershipGate(t *testing.T) {
	if _, _, err := db.DSN(); err != nil {
		t.Skip("no test database configured")
	}
	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	task, err := manager.CreateTask("reconciliation", "test", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := parseTaskID(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.pg.DeleteTask(taskID)
	intent, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "inspect"}, 1, "blocked", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := manager.pg.BeginExecutionAttempt(t.Context(), taskID, intent, "test-session", "test-call", "write", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.pg.FinishExecutionAttempt(t.Context(), attempt.ID, "unknown", nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	server := New(apiOnlyContext(), manager, dir, dir, dir)
	handler := server.Handler()
	token, err := signJWT(server.jwtKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	path := fmt.Sprintf("/api/tasks/%s/execution-attempts", task.ID)
	response := request("GET", path, "")
	if response.Code != 200 {
		t.Fatalf("control plane failed with stopped runners: %d %s", response.Code, response.Body.String())
	}
	var list struct {
		Items []db.ExecutionAttempt `json:"items"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	response = request("POST", fmt.Sprintf("%s/%d/resolve", path, attempt.ID), `{"state":"completed","assessment":"remote operation verified"}`)
	if response.Code != 200 {
		t.Fatalf("reconcile: %d %s", response.Code, response.Body.String())
	}
	var state string
	if err = manager.pg.QueryRow(`SELECT state FROM exploration_nodes WHERE id=$1`, intent).Scan(&state); err != nil || state != "blocked" {
		t.Fatalf("reconciliation resumed work: %s %v", state, err)
	}
	server.executionUnavailable.Store(true)
	if response = request("GET", path, ""); response.Code != 503 {
		t.Fatalf("ownership loss still accepted requests: %d", response.Code)
	}
}
