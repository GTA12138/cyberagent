package db

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestTriggerOutboxTransactionalTimeoutOrder(t *testing.T) {
	d := lifecycleTestDB(t)
	tr, err := d.CreateTrigger(&AgentTrigger{AgentKey: t.Name(), Enabled: true, OnTaskTimeout: true, TaskTimeoutMessage: "original policy"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTrigger(tr.ID)
	defer d.Exec(`DELETE FROM trigger_deliveries WHERE trigger_id=$1`, tr.ID)
	first, err := d.CreateTask(t.Name()+" first", "test", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(first.ID)
	second, err := d.CreateTask(t.Name()+" second", "test", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(second.ID)
	// A later-created task times out first. Both transitions must get a recipient.
	for _, id := range []int64{second.ID, first.ID, first.ID} {
		if _, err = d.Exec(`UPDATE tasks SET status='timeout' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := d.Query(`SELECT e.task_id,d.payload FROM trigger_deliveries d JOIN trigger_events e ON e.id=d.event_id WHERE d.trigger_id=$1 ORDER BY e.id`, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var payload json.RawMessage
		if err = rows.Scan(&id, &payload); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		var snap struct{ Trigger AgentTrigger }
		if err = json.Unmarshal(payload, &snap); err != nil || snap.Trigger.TaskTimeoutMessage != "original policy" {
			t.Fatal(string(payload), err)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != second.ID || ids[1] != first.ID {
		t.Fatalf("timeout order lost: %v", ids)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE tasks SET status='running' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE tasks SET status='timeout' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	var count int
	if err = d.QueryRow(`SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1`, tr.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("rollback leaked event/recipient: %d %v", count, err)
	}
	// Event write failure must roll back the business transition as well.
	if _, err = d.Exec(`UPDATE tasks SET status='running' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	tx, err = d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`ALTER TABLE trigger_events ADD CONSTRAINT test_reject_timeout CHECK(kind<>'timeout') NOT VALID`); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	_, err = tx.Exec(`UPDATE tasks SET status='timeout' WHERE id=$1`, first.ID)
	tx.Rollback()
	if err == nil {
		t.Fatal("business write succeeded without its event")
	}
	var status string
	if err = d.QueryRow(`SELECT status FROM tasks WHERE id=$1`, first.ID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("business state escaped rollback: %s %v", status, err)
	}
	// An event's lower sequence ID can commit after a higher one: neither is skipped.
	tx, err = d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE tasks SET status='timeout' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET status='running' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET status='timeout' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = d.QueryRow(`SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1`, tr.ID).Scan(&count); err != nil || count != 4 {
		t.Fatalf("commit inversion lost recipient: %d %v", count, err)
	}
}

func TestTriggerLeaseRecoveryAndFencing(t *testing.T) {
	d := lifecycleTestDB(t)
	ctx := t.Context()
	key := t.Name()
	defer d.Exec(`DELETE FROM trigger_deliveries WHERE agent_key=$1`, key)
	if err := d.EnqueueTrigger(ctx, key, key, json.RawMessage(`{"task_id":1}`)); err != nil {
		t.Fatal(err)
	}
	jobs, err := d.ClaimTriggers(ctx, key, "none", 1)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	// Crash between claim and starting the runner is safe to retry.
	if err = d.RecoverTriggerDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := d.ClaimTriggers(ctx, key, "none", 1)
	if err != nil || len(next) != 1 || next[0].Attempts != 2 {
		t.Fatal(next, err)
	}
	if err = d.FinishTrigger(ctx, jobs[0].ID, jobs[0].Attempts, "completed", ""); !errors.Is(err, ErrTriggerLeaseLost) {
		t.Fatalf("stale settlement accepted: %v", err)
	}
	if err = d.RenewTriggerLease(ctx, jobs); !errors.Is(err, ErrTriggerLeaseLost) {
		t.Fatalf("stale heartbeat accepted: %v", err)
	}
	if err = d.StartTriggerExecution(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err = d.RenewTriggerLease(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE trigger_deliveries SET lease_until=now()-interval '1 second' WHERE id=$1`, next[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.PendingTriggerAgents(ctx); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err = d.QueryRow(`SELECT state,last_error FROM trigger_deliveries WHERE id=$1`, next[0].ID).Scan(&state, &reason); err != nil || state != "failed" || reason == "" {
		t.Fatal(state, reason, err)
	}
	if err = d.FinishTrigger(ctx, next[0].ID, next[0].Attempts, "completed", ""); !errors.Is(err, ErrTriggerLeaseLost) {
		t.Fatalf("expired runner settled: %v", err)
	}
}

func TestTriggerIntervalOutboxAtomicSlot(t *testing.T) {
	d := lifecycleTestDB(t)
	ctx := t.Context()
	tr, err := d.CreateTrigger(&AgentTrigger{AgentKey: t.Name(), Enabled: true, IntervalSec: 3600, IntervalMessage: "interval snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTrigger(tr.ID)
	defer d.Exec(`DELETE FROM trigger_deliveries WHERE trigger_id=$1`, tr.ID)
	for i := 0; i < 2; i++ {
		if err = d.EnqueueDueTriggerIntervals(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = d.QueryRow(`SELECT count(*) FROM trigger_deliveries d JOIN agent_triggers t ON t.id=d.trigger_id WHERE t.id=$1 AND t.last_fire IS NOT NULL`, tr.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("interval slot: %d %v", count, err)
	}
}

func TestTriggerOutboxCapturesFindingGoalAndToolRecipients(t *testing.T) {
	d := lifecycleTestDB(t)
	tr, err := d.CreateTrigger(&AgentTrigger{AgentKey: t.Name(), Enabled: true, OnFinding: true, OnGoalMet: true, OnToolCall: true, ToolNames: []string{"bAsH"}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTrigger(tr.ID)
	defer d.Exec(`DELETE FROM trigger_deliveries WHERE trigger_id=$1`, tr.ID)
	task, err := d.CreateTask(t.Name(), "test", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	store := d.Exploration(task.ExplorationID)
	if _, err = store.AddNode(KindFinding, map[string]any{"summary": "finding", "vulnclass": "test", "severity": "high"}, 1, "confirmed", "test", nil); err != nil {
		t.Fatal(err)
	}
	goal, err := store.AddNode(KindGoal, map[string]any{"text": "goal"}, 1, "open", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = d.Exec(`UPDATE exploration_nodes SET state='met' WHERE id=$1`, goal); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.AppendActivity(Activity{Kind: "tool_use", Tool: "Bash", ToolUseID: "call", Detail: "input snapshot"}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.AppendActivity(Activity{Kind: "tool_result", Tool: "Bash", ToolUseID: "call", Detail: "output snapshot"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = d.QueryRow(`SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1`, tr.ID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("recipient count: %d %v", count, err)
	}
	var input, output string
	if err = d.QueryRow(`SELECT payload->'source_event'->>'tool_input',payload->'source_event'->>'tool_output'
 FROM trigger_deliveries WHERE trigger_id=$1 AND payload->>'source_kind'='tool'`, tr.ID).Scan(&input, &output); err != nil || input != "input snapshot" || output != "output snapshot" {
		t.Fatal(input, output, err)
	}
	// Old text filters can contain malformed JSON. Treat these as no match, while
	// preserving the business write and event; do not broaden tool selection.
	if _, err = d.Exec(`UPDATE agent_triggers SET tool_names='invalid-json' WHERE id=$1`, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.AppendActivity(Activity{Kind: "tool_result", Tool: "Bash", ToolUseID: "call"}); err != nil {
		t.Fatal(err)
	}
	if err = d.QueryRow(`SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1`, tr.ID).Scan(&count); err != nil || count != 3 {
		t.Fatal(count, err)
	}
}
