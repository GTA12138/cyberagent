package server

import (
	"context"
	"encoding/json"
	"chengjian.local/chengjian/db"
	"strings"
	"testing"
)

type oneAgentTriggerRepo struct {
	*db.DB
	key string
}

func (r oneAgentTriggerRepo) PendingTriggerAgents(context.Context) ([]string, error) {
	return []string{r.key}, nil
}

func TestTriggerOutboxSurvivesMissingAgentAndServiceRestart(t *testing.T) {
	s, _, _ := trafficEvidenceServer(t)
	pg := s.m.pg
	ctx := t.Context()
	key := t.Name()
	tr, err := pg.CreateTrigger(&db.AgentTrigger{AgentKey: key, Enabled: true, OnTaskCreate: true, TaskCreateMessage: "snapshot before edit"})
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteTrigger(tr.ID)
	defer pg.Exec(`DELETE FROM trigger_deliveries WHERE agent_key=$1`, key)
	task, err := pg.CreateTask("source description", "source goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteTask(task.ID)
	repo := oneAgentTriggerRepo{pg, key}
	behavior := func(string) triggerBehavior { return triggerBehavior{runMode: "serial", mergeMode: "none"} }
	unavailable := newTriggerService(ctx, repo, behavior, func(string) bool { return false }, func(context.Context, triggeredRun) error { t.Error("unavailable runner started"); return nil })
	for i := 0; i < 2; i++ {
		if err = unavailable.Enqueue(ctx, key+":manual", triggeredRun{agentKey: key, message: "manual event"}); err != nil {
			t.Fatal(err)
		}
	}
	unavailable.dispatch()
	var pending int
	if err = pg.QueryRow(`SELECT count(*) FROM trigger_deliveries WHERE agent_key=$1 AND state='pending'`, key).Scan(&pending); err != nil || pending != 2 {
		t.Fatal(pending, err)
	}
	tr.TaskCreateMessage = "edited after event"
	if err = pg.UpdateTrigger(tr); err != nil {
		t.Fatal(err)
	}
	// A new service has no memory of the original instance or its queue.
	received := make(chan triggeredRun, 2)
	restarted := newTriggerService(ctx, repo, behavior, func(string) bool { return true }, func(_ context.Context, it triggeredRun) error { received <- it; return nil })
	for i := 0; i < 2; i++ {
		restarted.dispatch()
		restarted.wg.Wait()
	}
	if len(received) != 2 {
		t.Fatalf("restarted delivery count=%d", len(received))
	}
	source, manual := <-received, <-received
	if source.taskID != task.ID || source.taskDesc != task.Description || !strings.Contains(source.message, "snapshot before edit") || strings.Contains(source.message, "edited after event") {
		t.Fatalf("snapshot contract: %+v", source)
	}
	if manual.message != "manual event" {
		t.Fatal(manual.message)
	}
	var completed int
	if err = pg.QueryRow(`SELECT count(*) FROM trigger_deliveries WHERE agent_key=$1 AND state='completed'`, key).Scan(&completed); err != nil || completed != 2 {
		t.Fatal(completed, err)
	}
}

func TestTriggerOutboxIntervalSnapshotRendering(t *testing.T) {
	s, _, _ := trafficEvidenceServer(t)
	pg := s.m.pg
	key := t.Name()
	tr, err := pg.CreateTrigger(&db.AgentTrigger{AgentKey: key, Enabled: true, IntervalSec: 3600, IntervalMessage: "interval body"})
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteTrigger(tr.ID)
	defer pg.Exec(`DELETE FROM trigger_deliveries WHERE agent_key=$1`, key)
	if err = pg.EnqueueDueTriggerIntervals(t.Context()); err != nil {
		t.Fatal(err)
	}
	var body storedTrigger
	jobs, err := pg.ClaimTriggers(t.Context(), key, "none", 1)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	if err = json.Unmarshal(jobs[0].Payload, &body); err != nil {
		t.Fatal(err)
	}
	item, err := renderStoredTrigger(body)
	if err != nil || !strings.Contains(item.message, "interval body") || strings.Contains(item.message, "0001-") {
		t.Fatal(item.message, err)
	}
}
