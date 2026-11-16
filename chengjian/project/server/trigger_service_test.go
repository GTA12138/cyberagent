package server

import (
	"context"
	"encoding/json"
	"errors"
	"chengjian.local/chengjian/db"
	"sync"
	"testing"
)

type serviceTriggerRepo struct {
	mu     sync.Mutex
	jobs   []db.TriggerDelivery
	states []string
	keys   []string
}

func (r *serviceTriggerRepo) EnqueueTrigger(context.Context, string, string, json.RawMessage) error {
	return nil
}
func (r *serviceTriggerRepo) PendingTriggerAgents(context.Context) ([]string, error) {
	if r.keys != nil {
		return r.keys, nil
	}
	return []string{"agent"}, nil
}
func (r *serviceTriggerRepo) ClaimTriggers(_ context.Context, _ string, _ string, limit int) ([]db.TriggerDelivery, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.jobs) == 0 {
		return nil, nil
	}
	n := min(len(r.jobs), limit)
	out := r.jobs[:n:n]
	r.jobs = r.jobs[n:]
	return out, nil
}
func (r *serviceTriggerRepo) FinishTrigger(_ context.Context, _ int64, _ int, state, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, state)
	return nil
}

func TestTriggerServiceLimitsAndOutcomePolicy(t *testing.T) {
	for _, tt := range []struct {
		name     string
		runErr   error
		attempts int
		state    string
	}{{"success", nil, 1, "completed"}, {"before execution", errors.New("create conversation failed"), 1, "pending"}, {"exhausted", errors.New("offline"), 3, "failed"}, {"unknown", errTriggerOutcomeUnknown, 1, "failed"}} {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(storedTrigger{AgentKey: "agent", Message: "event"})
			repo := &serviceTriggerRepo{jobs: []db.TriggerDelivery{{ID: 1, Payload: body, Attempts: tt.attempts}, {ID: 2, Payload: body, Attempts: 1}}}
			entered, release := make(chan struct{}), make(chan struct{})
			service := newTriggerService(t.Context(), repo, func(string) triggerBehavior { return triggerBehavior{runMode: "serial", mergeMode: "none"} }, func(string) bool { return true }, func(context.Context, triggeredRun) error { close(entered); <-release; return tt.runErr })
			service.dispatch()
			<-entered
			service.dispatch()
			repo.mu.Lock()
			remaining := len(repo.jobs)
			repo.mu.Unlock()
			if remaining != 1 {
				t.Fatal("serial runner admitted concurrent conversation")
			}
			close(release)
			service.wg.Wait()
			repo.mu.Lock()
			defer repo.mu.Unlock()
			if len(repo.states) != 1 || repo.states[0] != tt.state {
				t.Fatalf("delivery states: %v", repo.states)
			}
		})
	}
}

func (r *serviceTriggerRepo) StartTriggerExecution(context.Context, []db.TriggerDelivery) error {
	return nil
}
func (r *serviceTriggerRepo) RenewTriggerLease(context.Context, []db.TriggerDelivery) error {
	return nil
}

func TestTriggerServiceReadinessIsPerAgent(t *testing.T) {
	body, _ := json.Marshal(storedTrigger{AgentKey: "agent", Message: "event"})
	repo := &serviceTriggerRepo{keys: []string{"offline", "agent"}, jobs: []db.TriggerDelivery{{ID: 1, Payload: body, Attempts: 1}}}
	var checked []string
	service := newTriggerService(t.Context(), repo, func(string) triggerBehavior { return triggerBehavior{runMode: "serial", mergeMode: "none"} }, func(key string) bool { checked = append(checked, key); return key == "agent" }, func(context.Context, triggeredRun) error { return nil })
	service.dispatch()
	service.wg.Wait()
	if len(checked) != 2 || checked[0] != "offline" || checked[1] != "agent" || len(repo.states) != 1 || repo.states[0] != "completed" {
		t.Fatal(checked, repo.states)
	}
}
