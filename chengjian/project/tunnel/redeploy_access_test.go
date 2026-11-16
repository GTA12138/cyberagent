package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"chengjian.local/chengjian/db"
)

func redeployAdmissionFixture() (*Manager, *lifecycleStore, *lifecycleSession, Plan) {
	p := Plan{TaskID: 21, ViaSessionID: 7, Kind: KindSocks, Adapter: AdapterChisel,
		RemotePID: 42, RemoteBin: "/tmp/.owned/ch", RemoteDir: "/tmp/.owned"}
	m, store := lifecycleManager(p)
	store.rows[1].ViaSessionID = p.ViaSessionID
	store.rows[1].Kind, store.rows[1].Adapter = p.Kind, p.Adapter
	store.rows[1].State = db.TunnelError
	sess := &lifecycleSession{}
	m.sessions.Add(sess)
	return m, store, sess, p
}

func TestRedeployAdmissionDeniedHasNoDeploymentOrCleanupSideEffects(t *testing.T) {
	m, store, sess, p := redeployAdmissionFixture()
	before := string(store.rows[1].DeployParams)
	called := 0
	m.AuthorizeRedeploy = func(ctx context.Context, rec *db.TunnelRecord) error {
		called++
		if rec.TaskID != 21 || rec.ViaSessionID != 7 {
			t.Fatalf("wrong authoritative owner: %+v", rec)
		}
		return errors.New("grant revoked")
	}
	retry := map[int64]*redeployState{}
	m.maybeRedeploy(context.Background(), 1, &p, retry)
	rec, _ := store.Get(context.Background(), 1)
	if called != 1 || sess.calls.Load() != 0 || rec.State != db.TunnelError || !strings.Contains(rec.Error, "grant revoked") {
		t.Fatalf("denial: callback=%d remote_calls=%d state=%s error=%q", called, sess.calls.Load(), rec.State, rec.Error)
	}
	if string(rec.DeployParams) != before || retry[1].count != 0 {
		t.Fatal("denial mutated deployment resources or consumed attempt")
	}
}

func TestRedeployAdmissionRejectsUntrustedPlanAndUnavailableStore(t *testing.T) {
	for _, mode := range []string{"stale_task", "stale_session", "persisted_task", "persisted_session", "read_error", "stopped", "cleanup_pending"} {
		t.Run(mode, func(t *testing.T) {
			m, store, sess, p := redeployAdmissionFixture()
			called := 0
			m.AuthorizeRedeploy = func(context.Context, *db.TunnelRecord) error { called++; return nil }
			switch mode {
			case "stale_task":
				p.TaskID++
			case "stale_session":
				p.ViaSessionID++
			case "persisted_task":
				stored := p
				stored.TaskID++
				store.rows[1].DeployParams, _ = json.Marshal(stored)
			case "persisted_session":
				stored := p
				stored.ViaSessionID++
				store.rows[1].DeployParams, _ = json.Marshal(stored)
			case "read_error":
				store.onGet = func() error { return errors.New("fixture database unavailable") }
			case "stopped":
				store.rows[1].State = db.TunnelStopped
			case "cleanup_pending":
				stored := p
				stored.CleanupPending = true
				store.rows[1].DeployParams, _ = json.Marshal(stored)
			}
			m.maybeRedeploy(context.Background(), 1, &p, map[int64]*redeployState{})
			if called != 0 || sess.calls.Load() != 0 {
				t.Fatalf("invalid candidate ran: callback=%d remote_calls=%d", called, sess.calls.Load())
			}
			if mode == "stopped" && store.rows[1].State != db.TunnelStopped {
				t.Fatal("stopped tunnel revived")
			}
			if mode != "stopped" && mode != "cleanup_pending" && !strings.Contains(store.rows[1].Error, "准入被拒绝") {
				t.Fatalf("missing admission diagnostic: %q", store.rows[1].Error)
			}
		})
	}
}

func TestRedeployAdmissionDoesNotRestrictExplicitCleanup(t *testing.T) {
	p := Plan{TaskID: 21, ViaSessionID: 7}
	m, store := lifecycleManager(p)
	called := 0
	m.AuthorizeRedeploy = func(context.Context, *db.TunnelRecord) error { called++; return errors.New("grant revoked") }
	if err := m.Teardown(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.Get(context.Background(), 1)
	if called != 0 || rec.State != db.TunnelStopped {
		t.Fatalf("cleanup restricted by deployment authorization: callback=%d state=%s", called, rec.State)
	}
}

func TestRedeployAdmissionAllowsAuthorizedAttempt(t *testing.T) {
	m, _, _, p := redeployAdmissionFixture()
	// An absent local driver stops the attempt before any remote operation, but
	// the authorized candidate must still pass admission and consume an attempt.
	m.sessions.Remove(p.ViaSessionID)
	called := 0
	m.AuthorizeRedeploy = func(context.Context, *db.TunnelRecord) error { called++; return nil }
	retry := map[int64]*redeployState{}
	m.maybeRedeploy(context.Background(), 1, &p, retry)
	if called != 1 || retry[1].count != 1 {
		t.Fatalf("authorized attempt blocked: callback=%d retry=%+v", called, retry[1])
	}
}

func TestRedeployAdmissionAllowsAuthorizedDriverAndHonorsCancellation(t *testing.T) {
	for _, cancelDuringAdmission := range []bool{false, true} {
		t.Run(map[bool]string{false: "allowed", true: "canceled"}[cancelDuringAdmission], func(t *testing.T) {
			m, _, sess, p := redeployAdmissionFixture()
			sess.execFn = func(context.Context, string) (string, error) {
				return "", errors.New("fixture stops before any real operation")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			called := 0
			m.AuthorizeRedeploy = func(context.Context, *db.TunnelRecord) error {
				called++
				if cancelDuringAdmission {
					cancel()
				}
				return nil
			}
			retry := map[int64]*redeployState{}
			m.maybeRedeploy(ctx, 1, &p, retry)
			wantCalls, wantAttempts := int32(1), 1
			if cancelDuringAdmission {
				wantCalls, wantAttempts = 0, 0
			}
			if called != 1 || sess.calls.Load() != wantCalls || retry[1].count != wantAttempts {
				t.Fatalf("admission behavior: callback=%d driver=%d attempts=%d", called, sess.calls.Load(), retry[1].count)
			}
		})
	}
}
