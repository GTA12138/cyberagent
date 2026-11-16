package server

import (
	"context"
	"errors"
	"chengjian.local/chengjian/db"
	"sync"
	"testing"
)

type serviceTaskRepo struct {
	TaskRepository
	tasks []*Task
	fail  bool
}

func (r *serviceTaskRepo) Task(id string) (*Task, bool) {
	for _, task := range r.tasks {
		if task.ID == id {
			return task, true
		}
	}
	return nil, false
}
func (r *serviceTaskRepo) List() []*Task                 { return r.tasks }
func (r *serviceTaskRepo) ConcurrencyLimit() (bool, int) { return true, 1 }
func (r *serviceTaskRepo) ApplyTaskAdmission(id, expected, status string, queued bool, mode string, preserve bool) error {
	if r.fail {
		return errors.New("commit failed")
	}
	task, _ := r.Task(id)
	task.updateLifecycle(func(s *taskLifecycleState) {
		s.Status = status
		s.Queued = queued
		s.Paused = false
		s.QueueMode = mode
	})
	return nil
}
func (r *serviceTaskRepo) ApplyTaskPause(id string) error {
	if r.fail {
		return errors.New("commit failed")
	}
	task, _ := r.Task(id)
	task.updateLifecycle(func(s *taskLifecycleState) { s.Paused = true })
	return nil
}

type serviceTaskRunner struct {
	taskExecution
	paused  map[string]bool
	resumed []string
}

func (r *serviceTaskRunner) IsDeleting(string) bool         { return false }
func (r *serviceTaskRunner) IsPaused(id string) bool        { return r.paused[id] }
func (r *serviceTaskRunner) ReadyFor(*Task) bool            { return true }
func (r *serviceTaskRunner) Started(string) bool            { return true }
func (r *serviceTaskRunner) ActiveLLMCalls(string) int64    { return 0 }
func (r *serviceTaskRunner) beginTaskOperation(string) bool { return true }
func (r *serviceTaskRunner) decInflight(string)             {}
func (r *serviceTaskRunner) Pause(id string, _ error)       { r.paused[id] = true }
func (r *serviceTaskRunner) Resume(task *Task) {
	r.paused[task.ID] = false
	r.resumed = append(r.resumed, task.ID)
}
func (r *serviceTaskRunner) Run(context.Context, *Task)                      {}
func (r *serviceTaskRunner) startDeadlineCoordinator(context.Context, *Task) {}
func (r *serviceTaskRunner) emitActivity(_ *Task, a db.Activity) db.Activity { return a }

func TestTaskServiceFIFOWithoutHTTP(t *testing.T) {
	newer := &Task{ID: "20", Status: "running", Queued: true, QueuedAt: 20, QueueMode: "resume", FirstRunAt: 1}
	older := &Task{ID: "10", Status: "running", Queued: true, QueuedAt: 10, QueueMode: "resume", FirstRunAt: 1}
	repo := &serviceTaskRepo{tasks: []*Task{newer, older}}
	runner := &serviceTaskRunner{paused: map[string]bool{"10": true, "20": true}}
	service := &TaskService{ctx: t.Context(), m: repo, engine: runner, concMu: &sync.Mutex{}}
	service.reconcileConcurrency()
	if older.lifecycleSnapshot().Queued || !newer.lifecycleSnapshot().Queued || len(runner.resumed) != 1 || runner.resumed[0] != "10" {
		t.Fatalf("FIFO violated: %+v", runner.resumed)
	}
}
func TestTaskServicePauseCompensatesFailedCommit(t *testing.T) {
	task := &Task{ID: "1", Status: "running", FirstRunAt: 1}
	repo := &serviceTaskRepo{tasks: []*Task{task}, fail: true}
	runner := &serviceTaskRunner{paused: map[string]bool{}}
	cancelled := false
	service := &TaskService{ctx: t.Context(), m: repo, engine: runner, concMu: &sync.Mutex{}, cancelChat: func(string, error) bool { cancelled = true; return true }}
	if _, err := service.applyTaskControlWithCause(task, "pause", nil); err == nil {
		t.Fatal("failed commit accepted")
	}
	if runner.IsPaused(task.ID) || task.lifecycleSnapshot().Paused || cancelled {
		t.Fatal("failed pause left execution or chat cancelled")
	}
}
