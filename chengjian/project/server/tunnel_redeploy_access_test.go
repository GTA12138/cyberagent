package server

import (
	"context"
	"testing"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
)

func TestTunnelRedeployAuthorizationFailsClosedWithoutContextOrStore(t *testing.T) {
	s := &Server{}
	for _, rec := range []*db.TunnelRecord{nil, {TaskID: 1}, {TaskID: 1, ViaSessionID: 2}} {
		if err := s.authorizeTunnelRedeploy(context.Background(), rec); err == nil {
			t.Fatalf("invalid redeploy authorized: %+v", rec)
		}
	}
}

func TestTunnelRedeployAuthorizationUsesLiveGrantAndRecordOwner(t *testing.T) {
	s, store, pg, fake := newSessionHealthFixture(t)
	other, err := pg.CreateTask("redeploy grantee fixture", "local fake", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(other.ID) })
	rec := &db.TunnelRecord{TaskID: other.ID, ViaSessionID: fake.id}
	// A maintenance context has no task identity. A forged RunInfo owner must
	// not substitute for the persistent tunnel's actual task.
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: fake.ownerID})
	if err := s.authorizeTunnelRedeploy(ctx, rec); err == nil {
		t.Fatal("record without grant inherited the maintenance caller's rights")
	}
	if err := store.GrantAccess(ctx, fake.id, other.ID, []string{db.SessionActionRead}, nil, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := s.authorizeTunnelRedeploy(ctx, rec); err == nil {
		t.Fatal("read-only grant authorized redeployment")
	}
	if err := store.GrantAccess(ctx, fake.id, other.ID, []string{db.SessionActionTunnelDeploy}, nil, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := s.authorizeTunnelRedeploy(context.Background(), rec); err != nil {
		t.Fatalf("current explicit grant rejected: %v", err)
	}
	if err := store.RevokeAccess(ctx, fake.id, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.authorizeTunnelRedeploy(ctx, rec); err == nil {
		t.Fatal("revoked grant authorized redeployment")
	}
	expires := time.Now().Add(time.Hour)
	if err := store.GrantAccess(ctx, fake.id, other.ID, []string{db.SessionActionTunnelDeploy}, &expires, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`UPDATE session_access_grants SET expires_at=now()-interval '1 minute' WHERE session_id=$1 AND task_id=$2`, fake.id, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.authorizeTunnelRedeploy(ctx, rec); err == nil {
		t.Fatal("expired grant authorized redeployment")
	}
	ownerRec := &db.TunnelRecord{TaskID: fake.ownerID, ViaSessionID: fake.id}
	if err := s.authorizeTunnelRedeploy(context.Background(), ownerRec); err != nil {
		t.Fatalf("resource owner rejected: %v", err)
	}
	if _, err := pg.Exec(`UPDATE tasks SET archived_at=now() WHERE id=$1`, fake.ownerID); err != nil {
		t.Fatal(err)
	}
	if err := s.authorizeTunnelRedeploy(context.Background(), ownerRec); err == nil {
		t.Fatal("archived task authorized redeployment")
	}
	if _, err := pg.Exec(`UPDATE tasks SET archived_at=NULL, deleted_at=now() WHERE id=$1`, fake.ownerID); err != nil {
		t.Fatal(err)
	}
	if err := s.authorizeTunnelRedeploy(context.Background(), ownerRec); err == nil {
		t.Fatal("deleted task authorized redeployment")
	}
	if _, err := pg.Exec(`UPDATE tasks SET deleted_at=NULL WHERE id=$1`, fake.ownerID); err != nil {
		t.Fatal(err)
	}
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	closedDB, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := closedDB.Close(); err != nil {
		t.Fatal(err)
	}
	unavailable := &Server{sessStore: db.NewSessionStore(closedDB, nil)}
	if err := unavailable.authorizeTunnelRedeploy(context.Background(), ownerRec); err == nil {
		t.Fatal("unavailable authorization database allowed redeployment")
	}
}
