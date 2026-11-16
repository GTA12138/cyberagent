package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/evidence"
	"chengjian.local/chengjian/traffic"
)

func seedScopedCapture(t *testing.T, tr *traffic.Traffic, id, body string) {
	t.Helper()
	if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path) VALUES(?,1,'scope.test','GET','/','http://scope.test/',200,'text/plain',0,?,'')`, id, len(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.DB().Exec(`INSERT INTO exchange_bodies(id,req_head,resp_head,req_body,resp_body) VALUES(?,'GET / HTTP/1.1','HTTP/1.1 200 OK',?,?)`, id, []byte{}, []byte(body)); err != nil {
		t.Fatal(err)
	}
}

func TestScopedEvidenceRecordAndBindUseFindingTask(t *testing.T) {
	dsn := os.Getenv("CHENGJIAN_PG_DSN")
	if dsn == "" {
		t.Skip("explicit isolated CHENGJIAN_PG_DSN required")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	task, err := pg.CreateTask("scoped evidence integration", "local fixtures", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteTask(task.ID)
	root := t.TempDir()
	global, err := traffic.Open(filepath.Join(root, "traffic"), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer global.Close()
	pm := newTaskProxyManager(root, &fakeTunnelLister{}, nil, 21100, 21101)
	taskDir := filepath.Join(pm.dir, fmt.Sprint(task.ID))
	taskCapture, err := traffic.Open(taskDir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pm.proxies[task.ID] = &taskProxyInst{tr: taskCapture}
	defer pm.Close()
	seedScopedCapture(t, global, "42", "global-body")
	seedScopedCapture(t, taskCapture, "42", "task-body")
	seedScopedCapture(t, taskCapture, "43", "history-body")
	s := &Server{m: &Manager{pg: pg, dir: root, traffic: global, taskProxy: pm}}
	store := s.evidenceStore()
	in := db.RecordFindingInput{TaskID: task.ID, ExplorationID: task.ExplorationID, Worker: "test", VulnClass: "TEST", Name: "Scoped fixture", Severity: "low", Summary: "source isolation"}
	taskSource := defaultTrafficSource(task.ID)
	finding, err := store.Record(context.Background(), in, []db.TrafficRef{{TrafficID: "42"}, {TrafficID: "global/42"}, {TrafficID: taskSource + "/42"}})
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteFinding(finding.FindingID)
	if len(finding.Traffic.Bindings) != 2 {
		t.Fatalf("alias dedup/collision failed: %+v", finding.Traffic.Bindings)
	}
	// Existing immutable raw/global snapshots survive an upgrade without a
	// duplicate binding or an evidence-version bump.
	seedScopedCapture(t, global, "44", "legacy-body")
	legacy := evidence.New(pg, global, filepath.Join(root, "evidence"))
	before, err := legacy.Bind(context.Background(), finding.FindingID, []db.TrafficRef{{TrafficID: "44"}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Bind(context.Background(), finding.FindingID, []db.TrafficRef{{TrafficID: "global/44"}})
	if err != nil || after.Version != before.Version || len(after.Bindings) != len(before.Bindings) {
		t.Fatalf("legacy alias changed immutable evidence: %+v %v", after, err)
	}
	for _, binding := range finding.Traffic.Bindings {
		f, _, err := store.OpenBody(binding.Snapshot, "response")
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(f)
		f.Close()
		want := "global-body"
		if binding.Snapshot.SourceTrafficID == taskSource+"/42" {
			want = "task-body"
		}
		if err != nil || string(b) != want {
			t.Fatalf("wrong store body: %s %v", b, err)
		}
	}
	// Bind must use the finding row's task, regardless of the calling run context.
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: task.ID + 1})
	if _, err := store.Bind(ctx, finding.FindingID, []db.TrafficRef{{TrafficID: defaultTrafficSource(task.ID+1) + "/42"}}); err == nil {
		t.Fatal("bound another task's traffic")
	}
	pm.CloseTask(task.ID)
	bound, err := store.Bind(ctx, finding.FindingID, []db.TrafficRef{{TrafficID: taskSource + "/43"}})
	if err != nil || len(bound.Bindings) != 4 {
		t.Fatalf("closed task history bind failed: %+v %v", bound, err)
	}
}

func TestTrafficScopeOwnership(t *testing.T) {
	for _, tc := range []struct {
		taskID int64
		source string
		ok     bool
	}{
		{7, "global", true}, {7, "task/7", true}, {7, "task/8", false},
		{0, "global", true}, {0, "task/7", false}, {7, "task/07", false},
	} {
		if err := checkTrafficSource(tc.taskID, tc.source); (err == nil) != tc.ok {
			t.Errorf("task=%d source=%q: %v", tc.taskID, tc.source, err)
		}
	}
}

func TestTaskTrafficLeaseSurvivesCloseAndHistoryDoesNotCreateProxy(t *testing.T) {
	root := t.TempDir()
	m := newTaskProxyManager(root, &fakeTunnelLister{}, nil, 21100, 21101)
	dir := filepath.Join(m.dir, "7")
	tr, err := traffic.Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m.proxies[7] = &taskProxyInst{tr: tr}
	t.Cleanup(m.Close)
	entered, release := make(chan struct{}), make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		readDone <- m.withTraffic(context.Background(), 7, func(tr *traffic.Traffic) error {
			close(entered)
			<-release
			return tr.DB().Ping()
		})
	}()
	<-entered
	closed := make(chan struct{})
	go func() { m.CloseTask(7); close(closed) }()
	select {
	case <-closed:
		t.Fatal("close interrupted active evidence lease")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close failed to finish after lease released")
	}
	if err := os.RemoveAll(filepath.Join(dir, "_ca")); err != nil {
		t.Fatal(err)
	}
	if err := m.withTraffic(context.Background(), 7, func(tr *traffic.Traffic) error { return tr.DB().Ping() }); err != nil {
		t.Fatal(err)
	}
	if len(m.proxies) != 0 {
		t.Fatal("history read created live proxy")
	}
	if _, err := os.Stat(filepath.Join(dir, "_ca")); !os.IsNotExist(err) {
		t.Fatal("history read created CA")
	}
	m.Close()
	if addr, _ := m.ForTask(context.Background(), 7); addr != "" {
		t.Fatal("manager reopened after shutdown")
	}
	if err := m.withTraffic(context.Background(), 8, func(*traffic.Traffic) error { t.Fatal("missing history callback"); return nil }); err == nil {
		t.Fatal("missing task history silently fell back")
	}
	if _, err := os.Stat(filepath.Join(m.dir, "8")); !os.IsNotExist(err) {
		t.Fatal("missing history created task directory")
	}
}

func TestManagerCloseDoesNotHoldSettingsLockWhileWaitingForEvidence(t *testing.T) {
	root := t.TempDir()
	pm := newTaskProxyManager(root, &fakeTunnelLister{}, nil, 21100, 21101)
	tr, err := traffic.Open(filepath.Join(pm.dir, "7"), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pm.proxies[7] = &taskProxyInst{tr: tr}
	m := &Manager{taskProxy: pm}
	entered, release := make(chan struct{}), make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		readDone <- pm.withTraffic(context.Background(), 7, func(*traffic.Traffic) error {
			close(entered)
			<-release
			_ = m.GlobalProxy() // read needs the manager lock while close waits
			return nil
		})
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	deadline := time.After(5 * time.Second)
	for {
		pm.mu.Lock()
		waiting := pm.closing[7] != nil
		pm.mu.Unlock()
		if waiting {
			break
		}
		select {
		case <-deadline:
			close(release)
			t.Fatal("close did not reach evidence wait")
		case <-time.After(time.Millisecond):
		}
	}
	close(release)
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close held manager settings lock while waiting for evidence")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close deadlocked")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("duplicate close: %v", err)
	}
}
