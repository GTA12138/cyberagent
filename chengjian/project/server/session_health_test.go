package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/session"
)

type healthFixtureSession struct {
	id        int64
	ownerID   int64
	testFn    func(context.Context) error
	execErr   error
	readErr   error
	closeErr  error
	execCalls atomic.Int64
	readCalls atomic.Int64
}

func (s *healthFixtureSession) ID() int64                      { return s.id }
func (*healthFixtureSession) Kind() string                     { return "fixture" }
func (s *healthFixtureSession) Test(ctx context.Context) error { return s.testFn(ctx) }
func (s *healthFixtureSession) Exec(context.Context, string, time.Duration) (string, string, error) {
	s.execCalls.Add(1)
	return "", "", s.execErr
}
func (s *healthFixtureSession) ReadFile(context.Context, string) ([]byte, error) {
	s.readCalls.Add(1)
	return []byte("fixture"), s.readErr
}
func (*healthFixtureSession) WriteFile(context.Context, string, []byte) error { return nil }
func (s *healthFixtureSession) Close(context.Context) error                   { return s.closeErr }

func TestClassifySessionProbe(t *testing.T) {
	for _, tc := range []struct {
		name             string
		ctxErr, probeErr error
		want             sessionProbeState
	}{
		{"success", nil, nil, sessionProbeAlive},
		{"protocol failure", nil, errors.New("unexpected HTTP 404"), sessionProbeDead},
		{"business text is not cancellation", nil, errors.New("remote message says deadline exceeded"), sessionProbeDead},
		{"caller canceled opaque driver", context.Canceled, errors.New("request aborted"), sessionProbeUnknown},
		{"local budget exhausted", context.DeadlineExceeded, errors.New("request failed"), sessionProbeUnknown},
		{"wrapped cancellation", nil, fmt.Errorf("driver: %w", context.Canceled), sessionProbeUnknown},
		{"driver deadline", nil, fmt.Errorf("driver: %w", context.DeadlineExceeded), sessionProbeUnknown},
		{"cancellation despite nil driver result", context.Canceled, nil, sessionProbeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySessionProbe(tc.ctxErr, tc.probeErr); got != tc.want {
				t.Fatalf("state=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestStartupSessionProbeCanceledBudgetDoesNotDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Server{ctx: ctx, sessReg: session.NewRegistry()}
	fake := &healthFixtureSession{id: 1, testFn: func(context.Context) error { t.Error("dispatched after budget cancellation"); return nil }}
	s.sessReg.Add(fake)
	// No store exists: cancellation must finish without any persistence attempt.
	s.startupProbeStale([]*db.SessionRecord{{ID: 1, Status: db.SessionAlive, LastBeat: time.Now().Add(-48 * time.Hour)}})
}

func TestStartupSessionProbeTypedDeadlineDoesNotPersistDead(t *testing.T) {
	s := &Server{ctx: context.Background(), sessReg: session.NewRegistry()}
	s.sessReg.Add(&healthFixtureSession{id: 1, testFn: func(context.Context) error { return fmt.Errorf("driver: %w", context.DeadlineExceeded) }})
	s.startupProbeStale([]*db.SessionRecord{{ID: 1, Status: db.SessionAlive, LastBeat: time.Now().Add(-48 * time.Hour)}})
}

func newSessionHealthFixture(t *testing.T) (*Server, *db.SessionStore, *db.DB, *healthFixtureSession) {
	t.Helper()
	pg := openIntranetTestDB(t)
	owner, err := pg.CreateTask("session health fixture", "local fake", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(owner.ID) })
	store := db.NewSessionStore(pg, []byte("session-health-fixture-key"))
	id, err := store.Create(context.Background(), &db.SessionRecord{Kind: "fixture", Status: db.SessionAlive, CreatedByTask: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), id) })
	fake := &healthFixtureSession{id: id, ownerID: owner.ID, testFn: func(context.Context) error { return nil }}
	s := &Server{ctx: context.Background(), sessReg: session.NewRegistry(), sessStore: store, m: &Manager{pg: pg}}
	s.sessReg.Add(fake)
	return s, store, pg, fake
}

func TestSessionHealthProbeCancellationRetainsState(t *testing.T) {
	for _, mode := range []string{"caller_cancel", "wrapped_deadline"} {
		t.Run(mode, func(t *testing.T) {
			s, store, _, fake := newSessionHealthFixture(t)
			before, _ := store.Health(context.Background(), fake.id)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake.testFn = func(context.Context) error {
				if mode == "caller_cancel" {
					cancel()
					return errors.New("opaque driver abort")
				}
				return fmt.Errorf("driver: %w", context.DeadlineExceeded)
			}
			req := httptest.NewRequest(http.MethodPost, "/probe", nil).WithContext(ctx)
			req.SetPathValue("id", strconv.FormatInt(fake.id, 10))
			w := httptest.NewRecorder()
			s.intranetProbeSession(w, req)
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || body["probe_state"] != "unknown" || body["alive"] != nil || body["state_updated"] != false {
				t.Fatalf("response %d %s", w.Code, w.Body.String())
			}
			after, _ := store.Health(context.Background(), fake.id)
			if *before != *after {
				t.Fatalf("inconclusive probe changed health: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestSessionHealthProbeDiscardsResultAfterNewHeartbeat(t *testing.T) {
	s, store, pg, fake := newSessionHealthFixture(t)
	fake.testFn = func(context.Context) error {
		if _, err := pg.Exec(`UPDATE sessions SET last_beat=last_beat+interval '1 second' WHERE id=$1`, fake.id); err != nil {
			t.Fatal(err)
		}
		return errors.New("old protocol failure")
	}
	w, body := doProbe(t, s, fake.id)
	if w.Code != 200 || body["probe_state"] != "unknown" || body["state_updated"] != false {
		t.Fatalf("stale response: %d %s", w.Code, w.Body.String())
	}
	health, _ := store.Health(context.Background(), fake.id)
	if health.Status != db.SessionAlive {
		t.Fatalf("old probe overwrote successful heartbeat: %+v", health)
	}
}

func TestSessionHealthFailedOperationsDoNotRefreshHeartbeat(t *testing.T) {
	s, store, _, fake := newSessionHealthFixture(t)
	fake.execErr = errors.New("fixture execution failed")
	fake.readErr = errors.New("fixture read failed")
	for _, method := range []string{"manual_exec", "manual_read", "manual_list", "tool_exec", "tool_read"} {
		t.Run(method, func(t *testing.T) {
			before, _ := store.Health(context.Background(), fake.id)
			beforeExec, beforeRead := fake.execCalls.Load(), fake.readCalls.Load()
			if strings.HasPrefix(method, "tool_") {
				ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: fake.ownerID})
				input := json.RawMessage(fmt.Sprintf(`{"session_id":%d,"command":"fixture","path":"/fixture"}`, fake.id))
				tool := s.sessionExecTool()
				if method == "tool_read" {
					tool = s.sessionReadTool()
				}
				result, err := tool.Call(ctx, input, nil)
				if err != nil || !result.IsError {
					t.Fatalf("failed operation result=%+v err=%v", result, err)
				}
			} else {
				req := httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(`{"command":"fixture","path":"/fixture"}`))
				req.SetPathValue("id", strconv.FormatInt(fake.id, 10))
				w := httptest.NewRecorder()
				switch method {
				case "manual_exec":
					s.intranetExec(w, req)
				case "manual_read":
					s.intranetReadFile(w, req)
				case "manual_list":
					s.intranetListDir(w, req)
				}
				if w.Code != 502 {
					t.Fatalf("failed operation status=%d body=%s", w.Code, w.Body.String())
				}
			}
			wantExec, wantRead := int64(1), int64(0)
			if strings.HasSuffix(method, "read") {
				wantExec, wantRead = 0, 1
			}
			if gotExec, gotRead := fake.execCalls.Load()-beforeExec, fake.readCalls.Load()-beforeRead; gotExec != wantExec || gotRead != wantRead {
				t.Fatalf("operation did not reach expected driver: exec=%d read=%d want=%d/%d", gotExec, gotRead, wantExec, wantRead)
			}
			after, _ := store.Health(context.Background(), fake.id)
			if *before != *after {
				t.Fatalf("failed operation refreshed success heartbeat: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestSessionHealthDeleteFailureRetainsRetryState(t *testing.T) {
	s, store, _, fake := newSessionHealthFixture(t)
	fake.closeErr = errors.New("upstream cleanup unconfirmed")
	req := httptest.NewRequest(http.MethodDelete, "/fixture", nil)
	req.SetPathValue("id", strconv.FormatInt(fake.id, 10))
	w := httptest.NewRecorder()
	s.intranetDeleteSession(w, req)
	if w.Code != 502 {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, ok := s.sessReg.Get(fake.id); !ok {
		t.Fatal("unconfirmed cleanup lost driver")
	}
	if rec, err := store.Get(context.Background(), fake.id); err != nil || rec == nil {
		t.Fatalf("unconfirmed cleanup lost ledger: %v", err)
	}
}

func TestSessionHealthOrphanDeleteReportsOnlyLocalRemoval(t *testing.T) {
	s, store, _, fake := newSessionHealthFixture(t)
	// This models a stale ledger entry after a restart. Manual forgetting stays
	// available, but the response must not claim the remote resource was stopped.
	s.sessReg.Remove(fake.id)
	req := httptest.NewRequest(http.MethodDelete, "/fixture", nil)
	req.SetPathValue("id", strconv.FormatInt(fake.id, 10))
	w := httptest.NewRecorder()
	s.intranetDeleteSession(w, req)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || body["cleanup_state"] != "local_record_only" || body["remote_cleanup"] != "unconfirmed" {
		t.Fatalf("orphan delete overclaimed cleanup: %d %s", w.Code, w.Body.String())
	}
	if rec, err := store.Get(context.Background(), fake.id); err != nil || rec != nil {
		t.Fatalf("local record not removed: %v", err)
	}
}

func TestSessionHealthReverseCloseRejectsAmbiguousAcknowledgement(t *testing.T) {
	for _, response := range []string{`{}`, `{"ok":false}`, `{"error":"failed"}`, `not json`} {
		fm := &fakeMCP{responses: map[string]string{"kill_session": response}}
		sh := &reverseShell{id: 7, penID: 3, caller: fm}
		if err := sh.Close(context.Background()); err == nil {
			t.Fatalf("accepted ambiguous acknowledgement %q", response)
		}
	}
}
