package server

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync/atomic"
	"testing"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/budget"
	"chengjian.local/chengjian/db"
	"github.com/Autumn-27/norma/llm"
)

type budgetTestProvider struct {
	complete func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
	stream   func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error]
}

func (p budgetTestProvider) Complete(ctx context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.complete(ctx, r)
}
func (p budgetTestProvider) Stream(ctx context.Context, r llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return p.stream(ctx, r)
}

func budgetProviderFixture(t *testing.T, maxOut int64) (*db.DB, *db.Task, *budget.Service) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTaskWithOptions("budget adapter fixture", "local fake providers only", db.TaskCreateOptions{MaxInputTokens: 100000, MaxOutputTokens: maxOut})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTask(task.ID) })
	return d, task, budget.New(d)
}

func TestBudgetProviderCompleteAndCap(t *testing.T) {
	d, task, svc := budgetProviderFixture(t, 10)
	var calls int
	p := &budgetProvider{service: svc, taskID: task.ID, inner: budgetTestProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if req.MaxTokens != 10 {
			t.Errorf("provider cap=%d", req.MaxTokens)
		}
		return llm.Message{}, "stop", llm.Usage{InputTokens: 20, OutputTokens: 10}, nil
	}}}
	if _, _, _, err := p.Complete(t.Context(), llm.CompletionRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := p.Complete(t.Context(), llm.CompletionRequest{}); err == nil {
		t.Fatal("admitted after budget was spent")
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	status, err := d.ModelBudgetStatus(t.Context(), task.ID)
	if err != nil || status.Spent.Input != 20 || status.Spent.Output != 10 || status.PendingCalls != 0 {
		t.Fatal(status, err)
	}
}

func TestBudgetProviderWaitsForConcurrentSettlement(t *testing.T) {
	d, task, svc := budgetProviderFixture(t, 10)
	holding, err := d.ReserveModelTokens(t.Context(), task.ID, budget.Tokens{Input: 2000}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	p := &budgetProvider{service: svc, taskID: task.ID, inner: budgetTestProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls.Add(1)
		if req.MaxTokens != 8 {
			t.Errorf("refunded output cap=%d", req.MaxTokens)
		}
		return llm.Message{}, "stop", llm.Usage{InputTokens: 10, OutputTokens: 1}, nil
	}}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, _, err := p.Complete(ctx, llm.CompletionRequest{}); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("did not wait for in-flight reservation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if calls.Load() != 0 {
		t.Fatal("provider called before admission")
	}
	if err := d.SettleModelTokens(t.Context(), holding, budget.Tokens{Input: 20, Output: 2}, true); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBudgetProviderCancellationAndMissingUsage(t *testing.T) {
	for _, mode := range []string{"cancelled", "missing", "output_only"} {
		t.Run(mode, func(t *testing.T) {
			d, task, svc := budgetProviderFixture(t, 10)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			p := &budgetProvider{service: svc, taskID: task.ID, inner: budgetTestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				if mode == "cancelled" {
					cancel()
					return llm.Message{}, "", llm.Usage{}, ctx.Err()
				}
				if mode == "output_only" {
					return llm.Message{}, "stop", llm.Usage{OutputTokens: 2}, nil
				}
				return llm.Message{}, "stop", llm.Usage{}, nil
			}}}
			_, _, _, _ = p.Complete(ctx, llm.CompletionRequest{})
			status, err := d.ModelBudgetStatus(t.Context(), task.ID)
			if err != nil || status.PendingCalls != 0 || status.UncertainCalls != 1 || status.Spent.Output != 10 || status.Spent.Input <= 0 {
				t.Fatal(status, err)
			}
		})
	}
}

func TestBudgetProviderEarlyStreamStop(t *testing.T) {
	d, task, svc := budgetProviderFixture(t, 10)
	p := &budgetProvider{service: svc, taskID: task.ID, inner: budgetTestProvider{stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 8}}, nil) {
				return
			}
			if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "partial"}, nil) {
				return
			}
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}}}
	for ev, err := range p.Stream(t.Context(), llm.CompletionRequest{}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == llm.SETextDelta {
			break
		}
	}
	status, err := d.ModelBudgetStatus(t.Context(), task.ID)
	if err != nil || status.PendingCalls != 0 || status.UncertainCalls != 1 || status.Spent.Output != 10 {
		t.Fatal(status, err)
	}
}

func TestBudgetProviderRetryCannotBypassLimit(t *testing.T) {
	_, task, svc := budgetProviderFixture(t, 10)
	calls := 0
	p := &budgetProvider{service: svc, taskID: task.ID, inner: budgetTestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return llm.Message{}, "", llm.Usage{}, fmt.Errorf("HTTP 503 temporary unavailable")
	}}}
	_, _, _, err := completeTaskLLM(t.Context(), fmt.Sprint(task.ID), llm.CompletionRequest{}, taskLLMStreamHooks{current: func() (taskLLMSelection, error) {
		return taskLLMSelection{provider: p, retry: agent.RetryConfig{StreamAttempts: 2, StreamInterval: time.Millisecond}}, nil
	}})
	if err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
	_, _, _, err = p.Complete(t.Context(), llm.CompletionRequest{})
	var exceeded *budget.ExceededError
	if !errors.As(err, &exceeded) || calls != 1 {
		t.Fatal(err, calls)
	}
}
