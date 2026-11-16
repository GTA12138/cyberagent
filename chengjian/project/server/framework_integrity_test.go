package server

import (
	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/Autumn-27/norma/harness"
)

func TestTaskProxyDatabaseFailureRevokesExistingRoute(t *testing.T) {
	lister := &fakeTunnelLister{recs: []*db.TunnelRecord{socksRec(1, db.TunnelAlive, 20801)}}
	port := freePort(t)
	m := newTaskProxyManager(t.TempDir(), lister, func() string { return "http://global.invalid:8080" }, port, port)
	defer m.Close()
	addr, _ := m.ForTask(context.Background(), 7)
	if addr == "" {
		t.Fatal("fixture proxy unavailable")
	}
	lister.err = errors.New("fixture DB unavailable")
	if route := m.RouteForTaskHost(7, "10.0.0.1"); route.Mode != UpstreamBlocked {
		t.Fatal("cached lateral route survived ledger failure")
	}
	addr, _ = m.ForTask(context.Background(), 7)
	inst := m.proxies[7]
	if addr != "" || inst.mode != UpstreamBlocked || inst.routes.Len() != 0 || inst.tr.UpstreamProxy() != blockedUpstream {
		t.Fatal("DB failure retained an uncertain route")
	}
	lister.err = nil
	addr, _ = m.ForTask(context.Background(), 7)
	if addr == "" || inst.mode != UpstreamTunnel || inst.routes.Len() != 1 {
		t.Fatal("healthy ledger did not restore routing")
	}
}

func TestActivityPersistenceFailureSuspendsExecution(t *testing.T) {
	s, f, _ := trafficEvidenceServer(t)
	finding, err := s.m.pg.GetFinding(f.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	task, ok := s.m.Task(strconv.FormatInt(*finding.TaskID, 10))
	if !ok {
		t.Fatal("missing fixture task")
	}
	ctx := s.engine.execContextFor(context.Background(), task.ID)
	// This is an isolated test DB handle, never the application's database.
	if err := s.m.pg.Close(); err != nil {
		t.Fatal(err)
	}
	s.engine.emitActivity(task, db.Activity{Kind: "tool_use", Tool: "fixture", ToolUseID: "fixture", Detail: "{}"})
	code, _, _, _ := agent.AbortReason(ctx)
	if !s.engine.IsPaused(task.ID) || code != "persistence_failed" {
		t.Fatalf("continued without activity: paused=%v cause=%s", s.engine.IsPaused(task.ID), code)
	}
	if !taskExecutionPaused(context.Cause(ctx)) {
		t.Fatal("persistence pause would strand the intent instead of allowing transcript resume")
	}
}

func TestTaskProxyInitialFailureDoesNotUseGlobalEgress(t *testing.T) {
	port := freePort(t)
	proxy := newTaskProxyManager(t.TempDir(), &fakeTunnelLister{err: errors.New("fixture DB unavailable")}, func() string { return "http://global.invalid:8080" }, port, port)
	defer proxy.Close()
	m := &Manager{taskProxy: proxy, globalProxy: "http://global.invalid:8080"}
	if addr, _ := m.TaskProxyForTask(7); addr != "" {
		t.Fatal("initial route failure reported usable proxy")
	}
	if got := m.enrichProxyForTask(7); got != blockedUpstream {
		t.Fatalf("enrichment used another route: %s", got)
	}
	if mode := proxy.ModeForTask(7); mode != UpstreamBlocked {
		t.Fatalf("unknown route allowed egress: %s", mode)
	}
	if route := proxy.RouteForTaskHost(7, "10.0.0.1"); route.Mode != UpstreamBlocked {
		t.Fatal("missing route ledger allowed lateral connection")
	}
}

func TestPersistenceFailureCannotReplayWorkerIntent(t *testing.T) {
	err := &harness.PersistenceError{Cause: errors.New("fixture storage offline")}
	if retryableWorkerModelError(harness.ReasonModelError, err) {
		t.Fatal("storage failure could replay an already executed action")
	}
	if !retryableWorkerModelError(harness.ReasonModelError, errors.New("fixture transient model error")) {
		t.Fatal("ordinary transient model error lost recovery")
	}
}

func TestWorkerCompletionRequiresExplicitSuccess(t *testing.T) {
	for _, tc := range []struct {
		reason harness.TerminalReason
		state  string
	}{
		{harness.ReasonCompleted, "done"}, {harness.ReasonOutputLimit, "exhausted"},
		{harness.ReasonMaxTurns, "exhausted"}, {harness.ReasonTimeout, "exhausted"},
		{harness.ReasonStopHookPrevented, "blocked"}, {harness.ReasonAbortedTools, "blocked"},
		{"", "blocked"}, {"future_reason", "blocked"},
	} {
		if got := workerTerminalState(tc.reason, nil); got != tc.state {
			t.Fatalf("%s: got %s want %s", tc.reason, got, tc.state)
		}
	}
	if workerTerminalState(harness.ReasonCompleted, errors.New("fixture error")) != "blocked" {
		t.Fatal("error reported completion")
	}
}

func TestSteeringCannotSurviveConcurrentWorkCompletion(t *testing.T) {
	e := NewEngine(nil)
	for i := 0; i < 1000; i++ {
		_, cancel := context.WithCancelCause(context.Background())
		e.registerWork(42, cancel)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _ = e.SteerWork(42, "old run only") }()
		go func() { defer wg.Done(); <-start; _, complete := e.detachWork(42); complete(nil) }()
		close(start)
		wg.Wait()
		if msg, ok := e.drainSteer(42); ok {
			t.Fatalf("completed run retained steering: %s", msg)
		}
	}
}
