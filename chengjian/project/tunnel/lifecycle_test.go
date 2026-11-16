package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/session"
)

type lifecycleStore struct {
	mu        sync.Mutex
	rows      map[int64]*db.TunnelRecord
	onCreate  func()
	onGet     func() error
	onList    func()
	paramsErr error
}

func copyTunnel(r *db.TunnelRecord) *db.TunnelRecord {
	if r == nil {
		return nil
	}
	out := *r
	out.DeployParams = append(json.RawMessage(nil), r.DeployParams...)
	return &out
}

func (s *lifecycleStore) Create(ctx context.Context, r *db.TunnelRecord) (int64, error) {
	s.mu.Lock()
	out := copyTunnel(r)
	out.ID = 1
	s.rows[1] = out
	s.mu.Unlock()
	if s.onCreate != nil {
		s.onCreate()
	}
	return 1, nil
}
func (s *lifecycleStore) Get(ctx context.Context, id int64) (*db.TunnelRecord, error) {
	if s.onGet != nil {
		if err := s.onGet(); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyTunnel(s.rows[id]), ctx.Err()
}
func (s *lifecycleStore) UpdateState(ctx context.Context, id int64, state, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rows[id]; r != nil {
		r.State, r.Error = state, message
	}
	return nil
}
func (s *lifecycleStore) UpdateDeployParams(ctx context.Context, id int64, pid int, params json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paramsErr != nil {
		return s.paramsErr
	}
	if r := s.rows[id]; r != nil {
		r.RemotePID = pid
		r.DeployParams = append(json.RawMessage(nil), params...)
	}
	return nil
}
func (s *lifecycleStore) ListActive(ctx context.Context) ([]*db.TunnelRecord, error) {
	s.mu.Lock()
	var rows []*db.TunnelRecord
	for _, r := range s.rows {
		if r.State != db.TunnelStopped {
			rows = append(rows, copyTunnel(r))
		}
	}
	s.mu.Unlock()
	if s.onList != nil {
		s.onList()
	}
	return rows, ctx.Err()
}
func (s *lifecycleStore) ListByTask(ctx context.Context, id int64) ([]*db.TunnelRecord, error) {
	return s.ListActive(ctx)
}
func (s *lifecycleStore) Delete(ctx context.Context, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, id)
	return nil
}

func lifecycleManager(p Plan) (*Manager, *lifecycleStore) {
	raw, _ := json.Marshal(p)
	s := &lifecycleStore{rows: map[int64]*db.TunnelRecord{1: {ID: 1, TaskID: p.TaskID, State: db.TunnelAlive, DeployParams: raw}}}
	return &Manager{store: s, sessions: session.NewRegistry()}, s
}

type lifecycleSession struct {
	calls  atomic.Int32
	execFn func(context.Context, string) (string, error)
}

func (s *lifecycleSession) ID() int64                  { return 7 }
func (s *lifecycleSession) Kind() string               { return "http_php" }
func (s *lifecycleSession) Test(context.Context) error { return nil }
func (s *lifecycleSession) Exec(ctx context.Context, cmd string, _ time.Duration) (string, string, error) {
	s.calls.Add(1)
	if s.execFn != nil {
		out, err := s.execFn(ctx, cmd)
		return out, "", err
	}
	if strings.Contains(cmd, "/cmdline") {
		return "/tmp/.owned/ch", "", nil
	}
	matches := regexp.MustCompile(`echo ([a-f0-9]+_OK)`).FindStringSubmatch(cmd)
	if len(matches) > 1 {
		return matches[1], "", nil
	}
	return "", "", nil
}
func (*lifecycleSession) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (*lifecycleSession) WriteFile(context.Context, string, []byte) error  { return nil }
func (*lifecycleSession) Close(context.Context) error                      { return nil }

func TestTeardownCancelsInflightAndPreventsResurrection(t *testing.T) {
	m, s := lifecycleManager(Plan{})
	opCtx, release, err := m.beginOperation(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- m.Teardown(context.Background(), 1) }()
	select {
	case <-opCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel in-flight operation")
	}
	release()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	r, _ := s.Get(context.Background(), 1)
	if r.State != db.TunnelStopped {
		t.Fatalf("state = %s", r.State)
	}
}

func TestHealthUsesFreshRecordAfterTeardown(t *testing.T) {
	m, s := lifecycleManager(Plan{})
	s.onList = func() {
		if err := m.Teardown(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	}
	retry := map[int64]*redeployState{}
	m.healthRound(context.Background(), true, retry)
	r, _ := s.Get(context.Background(), 1)
	if r.State != db.TunnelStopped || len(retry) != 0 {
		t.Fatalf("stopped tunnel was reconsidered: state=%s retry=%v", r.State, retry)
	}
}

func TestTeardownUnknownRemoteRetainsStopIntentAndCanRetry(t *testing.T) {
	m, s := lifecycleManager(Plan{ViaSessionID: 7, RemotePID: 42, RemoteBin: "/tmp/.owned/ch", RemoteDir: "/tmp/.owned"})
	if err := m.Teardown(context.Background(), 1); err == nil {
		t.Fatal("missing session must not confirm cleanup")
	}
	r, _ := s.Get(context.Background(), 1)
	var p Plan
	_ = json.Unmarshal(r.DeployParams, &p)
	if r.State != db.TunnelError || !p.CleanupPending {
		t.Fatalf("lost cleanup obligation: %+v", r)
	}
	m.MarkOrphansError(context.Background())
	retry := map[int64]*redeployState{}
	m.healthRound(context.Background(), true, retry)
	if len(retry) != 0 {
		t.Fatal("pending cleanup was automatically redeployed")
	}
	sess := &lifecycleSession{}
	m.sessions.Add(sess)
	if err := m.Teardown(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	calls := sess.calls.Load()
	if err := m.Teardown(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if sess.calls.Load() != calls {
		t.Fatal("idempotent teardown repeated remote commands")
	}
	r, _ = s.Get(context.Background(), 1)
	if r.State != db.TunnelStopped {
		t.Fatal(r.State)
	}
}

func TestRollbackSurvivesCallerCancellation(t *testing.T) {
	p := Plan{ViaSessionID: 7, RemoteDir: "/tmp/.owned"}
	m, s := lifecycleManager(p)
	sess := &lifecycleSession{execFn: func(ctx context.Context, cmd string) (string, error) {
		if err := ctx.Err(); err != nil {
			t.Errorf("rollback inherited cancellation: %v", err)
			return "", err
		}
		match := regexp.MustCompile(`echo ([a-f0-9]+_OK)`).FindStringSubmatch(cmd)
		return match[1], nil
	}}
	m.sessions.Add(sess)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.rollback(ctx, 1, sess, &p)
	r, _ := s.Get(context.Background(), 1)
	if r != nil {
		t.Fatal("confirmed rollback did not remove ledger row")
	}
	if sess.calls.Load() == 0 {
		t.Fatal("rollback skipped remote cleanup")
	}
}

func TestRollbackFailurePreservesLedger(t *testing.T) {
	p := Plan{ViaSessionID: 7, RemoteDir: "/tmp/.owned"}
	m, s := lifecycleManager(p)
	m.rollback(context.Background(), 1, nil, &p)
	r, _ := s.Get(context.Background(), 1)
	if r == nil || r.State != db.TunnelError || !strings.HasPrefix(r.Error, "回滚清理未完成:") {
		t.Fatal("unconfirmed rollback removed or mislabeled ledger")
	}
}

func TestRemoteCleanupUnknownIsNotConfirmed(t *testing.T) {
	p := Plan{RemotePID: 42, RemoteBin: "/tmp/.owned/ch"}
	m := &Manager{}
	for _, phase := range []string{"inspect", "after_signal"} {
		t.Run(phase, func(t *testing.T) {
			sess := &lifecycleSession{execFn: func(_ context.Context, cmd string) (string, error) {
				if strings.Contains(cmd, "/cmdline") && phase == "after_signal" {
					return p.RemoteBin, nil
				}
				match := regexp.MustCompile(`echo ([a-f0-9]+_UNKNOWN)`).FindStringSubmatch(cmd)
				if len(match) != 2 {
					t.Fatalf("missing explicit unknown outcome in %s", cmd)
				}
				return match[1], nil
			}}
			if err := m.killRemote(context.Background(), sess, &p); err == nil {
				t.Fatal("unknown remote status confirmed cleanup")
			}
		})
	}
}

func TestTeardownRequiresDurableStopIntent(t *testing.T) {
	m, s := lifecycleManager(Plan{ViaSessionID: 7, RemoteDir: "/tmp/.owned"})
	s.paramsErr = errors.New("storage unavailable")
	sess := &lifecycleSession{}
	m.sessions.Add(sess)
	if err := m.Teardown(context.Background(), 1); err == nil {
		t.Fatal("expected persistence error")
	}
	if sess.calls.Load() != 0 {
		t.Fatal("cleanup began before stop intent was durable")
	}
}

func TestConcurrentTeardownDoesNotCancelCleanupOrRepeatCommands(t *testing.T) {
	m, _ := lifecycleManager(Plan{ViaSessionID: 7, RemoteDir: "/tmp/.owned"})
	started, proceed := make(chan context.Context, 1), make(chan struct{})
	sess := &lifecycleSession{execFn: func(ctx context.Context, cmd string) (string, error) {
		started <- ctx
		select {
		case <-proceed:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		match := regexp.MustCompile(`echo ([a-f0-9]+_OK)`).FindStringSubmatch(cmd)
		return match[1], nil
	}}
	m.sessions.Add(sess)
	results := make(chan error, 2)
	go func() { results <- m.Teardown(context.Background(), 1) }()
	var cleanupCtx context.Context
	select {
	case cleanupCtx = <-started:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not begin")
	}
	go func() { results <- m.Teardown(context.Background(), 1) }()
	deadline := time.Now().Add(time.Second)
	for {
		m.opsMu.Lock()
		refs := m.ops[1].refs
		m.opsMu.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second teardown did not queue")
		}
		time.Sleep(time.Millisecond)
	}
	if err := cleanupCtx.Err(); err != nil {
		t.Fatalf("second stop canceled active cleanup: %v", err)
	}
	close(proceed)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if sess.calls.Load() != 1 {
		t.Fatalf("cleanup repeated %d commands", sess.calls.Load())
	}
}

func TestDeployCancellationAfterCreateDoesNotStrandDeploying(t *testing.T) {
	for _, adapter := range []string{AdapterChisel, AdapterSuo5} {
		t.Run(adapter, func(t *testing.T) {
			m, s := lifecycleManager(Plan{})
			m.chiselPath, m.suo5Path, m.suo5PayloadsDir = "unused", "unused", "unused"
			m.sessions.Add(&lifecycleSession{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.onCreate = cancel
			_, err := m.Deploy(ctx, &Plan{Kind: KindSocks, Adapter: adapter, ViaSessionID: 7, SocksPort: 12345})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v", err)
			}
			r, _ := s.Get(context.Background(), 1)
			if r != nil {
				t.Fatal("cancellation stranded a deploying row")
			}
		})
	}
}

func TestDeployInitialReadFailureDoesNotStrandDeploying(t *testing.T) {
	for _, adapter := range []string{AdapterChisel, AdapterSuo5} {
		for _, cancelRead := range []bool{false, true} {
			t.Run(adapter+"/cancel="+strconv.FormatBool(cancelRead), func(t *testing.T) {
				m, s := lifecycleManager(Plan{})
				m.chiselPath, m.suo5Path, m.suo5PayloadsDir = "unused", "unused", "unused"
				m.sessions.Add(&lifecycleSession{})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				readFailed := false
				s.onGet = func() error {
					if readFailed {
						return nil
					}
					readFailed = true
					if cancelRead {
						cancel()
						return ctx.Err()
					}
					return errors.New("temporary read failure")
				}
				if _, err := m.Deploy(ctx, &Plan{Kind: KindSocks, Adapter: adapter, ViaSessionID: 7, SocksPort: 12345}); err == nil {
					t.Fatal("expected initial read failure")
				}
				r, _ := s.Get(context.Background(), 1)
				if r != nil {
					t.Fatal("initial read failure stranded a deploying row")
				}
			})
		}
	}
}

func TestLifecycleProcessHelper(t *testing.T) {
	if os.Getenv("CHENGJIAN_TUNNEL_PROCESS_HELPER") != "1" {
		return
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

type discardCloser struct{ io.Writer }

func (discardCloser) Close() error { return nil }

func TestManagedProcessesConcurrentCleanup(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{}
	var wg sync.WaitGroup
	for id := int64(1); id <= 8; id++ {
		cmd := exec.Command(executable, "-test.run=^TestLifecycleProcessHelper$")
		cmd.Env = append(os.Environ(), "CHENGJIAN_TUNNEL_PROCESS_HELPER=1")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		m.trackProcess(id, cmd, discardCloser{io.Discard})
		wg.Add(1)
		go func(id int64, pid int) {
			defer wg.Done()
			if err := m.killServer(id, &Plan{ServerPID: pid}); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}(id, cmd.Process.Pid)
	}
	wg.Wait()
	m.procsMu.Lock()
	defer m.procsMu.Unlock()
	if len(m.procs) != 0 {
		t.Fatalf("retained %d process handles", len(m.procs))
	}
}
