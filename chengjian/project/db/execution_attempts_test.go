package db

import (
	"context"
	"encoding/json"
	"testing"
)

func TestExecutionAttemptRecoveryAndReconciliation(t *testing.T) {
	d := lifecycleTestDB(t)
	ctx := t.Context()
	task, err := d.CreateTask("execution recovery", "test", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	store := d.Exploration(task.ExplorationID)
	first, err := store.AddNode(KindIntent, map[string]any{"summary": "unknown"}, 1, "running", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	safe, err := store.AddNode(KindIntent, map[string]any{"summary": "safe"}, 1, "running", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := d.BeginExecutionAttempt(ctx, task.ID, first, "session", "call", "write", "hash")
	if err != nil || a.State != "new" {
		t.Fatalf("begin %+v %v", a, err)
	}
	duplicate, err := d.BeginExecutionAttempt(ctx, task.ID, first, "session", "call", "write", "hash")
	if err != nil || duplicate.State != "started" {
		t.Fatalf("duplicate executed: %+v %v", duplicate, err)
	}
	recovered, err := store.RecoverInterruptedIntents(ctx)
	if err != nil || recovered.Blocked != 1 || recovered.Reopened != 1 {
		t.Fatalf("recovery %+v %v", recovered, err)
	}
	if _, err = d.Exec(`UPDATE exploration_nodes SET state='open' WHERE id=$1`, first); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimIntentContext(ctx, first, "retry"); err != nil || claimed {
		t.Fatalf("unknown side effect was claimable: %v %v", claimed, err)
	}
	var state string
	if err = d.QueryRow(`SELECT state FROM exploration_nodes WHERE id=$1`, safe).Scan(&state); err != nil || state != "open" {
		t.Fatalf("safe intent %s %v", state, err)
	}
	if _, err = d.BeginExecutionAttempt(ctx, task.ID, first, "session", "new-call", "write", "hash"); err == nil {
		t.Fatal("unknown effect did not block new side effect")
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	err = requireToolAttemptsSettled(tx, task.ID)
	tx.Rollback()
	if err == nil {
		t.Fatal("unknown effect allowed archive")
	}
	if err = d.ResolveExecutionAttempt(ctx, task.ID+1, a.ID, "not_executed", "checked", nil); err == nil {
		t.Fatal("cross-task resolution accepted")
	}
	if err = d.ResolveExecutionAttempt(ctx, task.ID, a.ID, "not_executed", "verified remote state", nil); err != nil {
		t.Fatal(err)
	}
	again, err := d.BeginExecutionAttempt(ctx, task.ID, first, "session", "call", "write", "hash")
	if err != nil || again.State != "new" {
		t.Fatalf("confirmed retry %+v %v", again, err)
	}
	if err = d.FinishExecutionAttempt(ctx, a.ID, "completed", json.RawMessage(`{"output":"done"}`)); err != nil {
		t.Fatal(err)
	}
	cached, err := d.BeginExecutionAttempt(ctx, task.ID, first, "session", "call", "write", "hash")
	if err != nil || cached.State != "completed" || string(cached.Result) == "" {
		t.Fatalf("cached %+v %v", cached, err)
	}
	if _, err = d.BeginExecutionAttempt(ctx, task.ID, first, "session", "call", "write", "different"); err == nil {
		t.Fatal("changed input replay accepted")
	}
	main, err := d.BeginExecutionAttempt(ctx, task.ID, 0, "main-session", "main-call", "write", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishExecutionAttempt(ctx, main.ID, "unknown", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = d.BeginExecutionAttempt(ctx, task.ID, 0, "main-session", "new-main-call", "write", "hash"); err == nil {
		t.Fatal("main conversation ignored unknown outcome")
	}
	if _, err = d.CreateTaskWithOptionsContext(cancelledContext(), "cancelled", "test", TaskCreateOptions{}); err == nil {
		t.Fatal("cancelled task creation wrote to database")
	}
	if _, err = store.FrontierContext(cancelledContext(), 1); err == nil {
		t.Fatal("cancelled frontier ignored context")
	}
}
func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestTriggerPersistenceAndUnknownRecovery(t *testing.T) {
	d := lifecycleTestDB(t)
	ctx := t.Context()
	key := t.Name()
	defer d.Exec(`DELETE FROM trigger_deliveries WHERE agent_key=$1`, key)
	body := json.RawMessage(`{"task_id":1,"mergeable":true}`)
	for i := 0; i < 2; i++ {
		if err := d.EnqueueTrigger(ctx, key, key+":event", body); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := d.ClaimTriggers(ctx, key, "none", 1)
	if err != nil || len(jobs) != 1 || jobs[0].Attempts != 1 {
		t.Fatalf("claim %v %v", jobs, err)
	}
	if err = d.EnqueueTrigger(ctx, key, key+":next", body); err != nil {
		t.Fatal(err)
	}
	if err = d.StartTriggerExecution(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if err = d.RecoverTriggerDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = d.QueryRow(`SELECT state FROM trigger_deliveries WHERE id=$1`, jobs[0].ID).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("unknown delivery replayed: %s %v", state, err)
	}
	next, err := d.ClaimTriggers(ctx, key, "none", 1)
	if err != nil || len(next) != 1 || next[0].ID == jobs[0].ID {
		t.Fatalf("pending event lost: %v %v", next, err)
	}
}
