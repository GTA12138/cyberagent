package db

import (
	"context"
	"testing"
)

func TestSessionHealthCASRejectsNewHeartbeatAndDeletion(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer d.Close()
	store := NewSessionStore(d, []byte("health-test-master-key"))
	ctx := context.Background()
	id, err := store.Create(ctx, &SessionRecord{Kind: "http_php", Status: SessionAlive})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), id) })
	initial, err := store.Health(ctx, id)
	if err != nil || initial == nil {
		t.Fatalf("initial health: %v", err)
	}
	if _, err := d.Exec(`UPDATE sessions SET last_beat=last_beat+interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.ApplyProbe(ctx, id, *initial, false); err != nil || changed {
		t.Fatalf("old failure overwrote a newer heartbeat: changed=%v err=%v", changed, err)
	}
	current, _ := store.Health(ctx, id)
	if current.Status != SessionAlive {
		t.Fatalf("status=%s", current.Status)
	}
	if changed, err := store.ApplyProbe(ctx, id, *current, false); err != nil || !changed {
		t.Fatalf("fresh failure not recorded: changed=%v err=%v", changed, err)
	}
	dead, _ := store.Health(ctx, id)
	if dead.Status != SessionDead || !dead.LastBeat.Equal(current.LastBeat) {
		t.Fatalf("failure must not refresh heartbeat: %+v", dead)
	}
	if changed, err := store.ApplyProbe(ctx, id, *dead, true); err != nil || !changed {
		t.Fatalf("fresh success not recorded: changed=%v err=%v", changed, err)
	}
	alive, _ := store.Health(ctx, id)
	if alive.Status != SessionAlive {
		t.Fatalf("status=%s", alive.Status)
	}
	if err := store.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.ApplyProbe(ctx, id, *alive, false); err != nil || changed {
		t.Fatalf("probe recreated deleted row: changed=%v err=%v", changed, err)
	}
}
