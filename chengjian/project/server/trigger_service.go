package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"chengjian.local/chengjian/db"
)

type triggerRepository interface {
	EnqueueTrigger(context.Context, string, string, json.RawMessage) error
	PendingTriggerAgents(context.Context) ([]string, error)
	ClaimTriggers(context.Context, string, string, int) ([]db.TriggerDelivery, error)
	FinishTrigger(context.Context, int64, int, string, string) error
	StartTriggerExecution(context.Context, []db.TriggerDelivery) error
	RenewTriggerLease(context.Context, []db.TriggerDelivery) error
}

var errTriggerOutcomeUnknown = errors.New("trigger outcome unknown")

// TriggerService owns persistent delivery, merge policy and per-agent execution
// slots. It only depends on a repository and a conversation runner, never HTTP.
type TriggerService struct {
	ctx      context.Context
	repo     triggerRepository
	behavior func(string) triggerBehavior
	run      func(context.Context, triggeredRun) error
	ready    func(string) bool
	mu       sync.Mutex
	active   map[string]int
	wake     chan struct{}
	wg       sync.WaitGroup
}

func newTriggerService(ctx context.Context, repo triggerRepository, behavior func(string) triggerBehavior, ready func(string) bool, run func(context.Context, triggeredRun) error) *TriggerService {
	return &TriggerService{ctx: ctx, repo: repo, behavior: behavior, ready: ready, run: run, active: map[string]int{}, wake: make(chan struct{}, 1)}
}

type storedTrigger struct {
	AgentKey    string           `json:"agent_key"`
	Title       string           `json:"title"`
	Message     string           `json:"message"`
	TaskID      int64            `json:"task_id"`
	TaskDesc    string           `json:"task_description"`
	TaskGoal    string           `json:"task_goal"`
	Mergeable   bool             `json:"mergeable"`
	SourceKind  string           `json:"source_kind,omitempty"`
	SourceEvent *db.TaskEvent    `json:"source_event,omitempty"`
	Trigger     *db.AgentTrigger `json:"trigger,omitempty"`
}

func (s *TriggerService) Enqueue(ctx context.Context, key string, item triggeredRun) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(storedTrigger{AgentKey: item.agentKey, Title: item.title, Message: item.message, TaskID: item.taskID, TaskDesc: item.taskDesc, TaskGoal: item.taskGoal, Mergeable: item.mergeable})
	if err != nil {
		return err
	}
	if err = s.repo.EnqueueTrigger(ctx, item.agentKey, key, body); err != nil {
		return err
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}
func (s *TriggerService) Run() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			s.wg.Wait()
			return
		case <-s.wake:
		case <-ticker.C:
		}
		s.dispatch()
	}
}
func (s *TriggerService) dispatch() {
	if s.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	keys, err := s.repo.PendingTriggerAgents(ctx)
	if err != nil {
		log.Printf("[trigger] pending: %v", err)
		return
	}
	for _, key := range keys {
		if !s.ready(key) {
			continue
		}
		cfg := s.behavior(key)
		limit := 1
		if cfg.runMode == "parallel" {
			limit = cfg.maxParallel
			if limit <= 0 {
				limit = int(^uint(0) >> 1)
			}
		}
		for {
			s.mu.Lock()
			active := s.active[key]
			s.mu.Unlock()
			if active >= limit {
				break
			}
			merge, batch := cfg.mergeMode, 500
			if cfg.runMode == "parallel" || merge == "none" || key == db.FindingVerifierAgentKey {
				merge = "none"
				batch = 1
			}
			jobs, err := s.repo.ClaimTriggers(ctx, key, merge, batch)
			if err != nil {
				log.Printf("[trigger] claim: %v", err)
				break
			}
			if len(jobs) == 0 {
				break
			}
			items := make([]triggeredRun, 0, len(jobs))
			for _, job := range jobs {
				var it storedTrigger
				if err = json.Unmarshal(job.Payload, &it); err != nil {
					break
				}
				var item triggeredRun
				item, err = renderStoredTrigger(it)
				if err != nil {
					break
				}
				items = append(items, item)
			}
			if err != nil {
				for _, job := range jobs {
					_ = s.repo.FinishTrigger(ctx, job.ID, job.Attempts, "failed", "invalid trigger payload")
				}
				continue
			}
			item := items[0]
			if len(items) > 1 {
				if merge == "all" {
					item = mergeAllRuns(items)
				} else {
					item = mergeTriggeredRuns(items)
				}
			}
			item.deliveries = jobs
			s.mu.Lock()
			s.active[key]++
			s.mu.Unlock()
			s.wg.Add(1)
			go s.deliver(key, item, jobs)
		}
	}
}
func (s *TriggerService) deliver(key string, item triggeredRun, jobs []db.TriggerDelivery) {
	defer s.wg.Done()
	runCtx, cancelRun := context.WithCancel(s.ctx)
	defer cancelRun()
	// Keep long conversations leased without holding a database transaction.
	stopHeartbeat, heartbeatDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(db.TriggerLease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-runCtx.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(runCtx, 5*time.Second)
				err := s.repo.RenewTriggerLease(ctx, jobs)
				cancel()
				if err != nil {
					log.Printf("[trigger] renew lease: %v", err)
					cancelRun()
					return
				}
			}
		}
	}()
	started := false
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%w: %v", errTriggerOutcomeUnknown, r)
			}
		}()
		ctx, cancel := context.WithTimeout(runCtx, 5*time.Second)
		err = s.repo.StartTriggerExecution(ctx, jobs)
		cancel()
		if err != nil {
			return err
		}
		started = true
		return s.run(runCtx, item)
	}()
	close(stopHeartbeat)
	<-heartbeatDone
	if started && runCtx.Err() != nil {
		err = fmt.Errorf("%w: %v", errTriggerOutcomeUnknown, runCtx.Err())
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer cancel()
	for _, job := range jobs {
		state, reason := "completed", ""
		if err != nil {
			state = "failed"
			reason = err.Error()
			if !errors.Is(err, errTriggerOutcomeUnknown) && job.Attempts < 3 {
				state = "pending"
			}
		}
		if finishErr := s.repo.FinishTrigger(ctx, job.ID, job.Attempts, state, reason); finishErr != nil {
			log.Printf("[trigger] settle #%d: %v", job.ID, finishErr)
		}
	}
	s.mu.Lock()
	s.active[key]--
	if s.active[key] == 0 {
		delete(s.active, key)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
