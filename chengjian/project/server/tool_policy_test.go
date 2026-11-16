package server

import (
	"context"
	"errors"
	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"strconv"
	"testing"
)

func TestToolPolicyFailureSuspendsTaskBeforeModelExecution(t *testing.T) {
	s, f, _ := trafficEvidenceServer(t)
	finding, err := s.m.pg.GetFinding(f.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	task, ok := s.m.Task(strconv.FormatInt(*finding.TaskID, 10))
	if !ok {
		t.Fatal("missing task")
	}
	s.engine.Resume(task)
	id, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "policy gate"}, 1, "running", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := task.Store.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	worker := agent.NewWorker(retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return llm.Message{}, "", llm.Usage{}, nil
	}}, "test", t.TempDir(), nil, 10000, 1)
	worker.SetRuntime(agent.Runtime{ToolResolve: func(context.Context, string, []actool.CoreTool) ([]actool.CoreTool, error) {
		return nil, errors.New("policy storage unavailable")
	}})
	if !s.engine.runIntent(t.Context(), task, "test", worker, intent, "", "") {
		t.Fatal("intent did not settle")
	}
	persisted, err := s.m.pg.GetTask(*finding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || !s.engine.IsPaused(task.ID) || !persisted.Paused {
		t.Fatalf("policy failure did not suspend: calls=%d paused=%v persisted=%v", calls, s.engine.IsPaused(task.ID), persisted.Paused)
	}
	node, err := task.Store.GetNode(id)
	if err != nil || node.State != "open" {
		t.Fatalf("unexecuted intent not recoverable: %+v %v", node, err)
	}
}
