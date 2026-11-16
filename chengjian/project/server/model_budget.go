package server

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log"
	"net/http"
	"strconv"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/budget"
	"chengjian.local/chengjian/llmrec"
	"github.com/Autumn-27/norma/llm"
)

func (s *Server) findingBudgetContext(ctx context.Context, findingID int64) (context.Context, error) {
	f, err := s.m.pg.GetFinding(findingID)
	if err != nil {
		return ctx, err
	}
	if f == nil {
		return ctx, fmt.Errorf("finding budget source unavailable")
	}
	if f.TaskID != nil && *f.TaskID > 0 {
		ctx = llmrec.WithTaskID(ctx, strconv.FormatInt(*f.TaskID, 10))
	}
	return ctx, nil
}

// budgetProvider is installed below task-router retries/failover. Each attempt
// obtains its own durable reservation, including attempts that return no output.
type budgetProvider struct {
	inner   llm.Provider
	service *budget.Service
	taskID  int64
	denied  func(error)
}

// dispatchBudgetProvider sits immediately around each recorded concrete provider,
// inside pools. Context attribution selects the account for each physical member
// attempt, including judge calls and calls made outside the task router.
type dispatchBudgetProvider struct {
	llm.Provider
	server *Server
}
type budgetManagedProvider struct{ llm.Provider }

func (*dispatchBudgetProvider) modelBudgetManaged() {}
func (*budgetManagedProvider) modelBudgetManaged()  {}

func (s *Server) dispatchTaskBudget(p llm.Provider) llm.Provider {
	if _, ok := p.(interface{ modelBudgetManaged() }); ok {
		return p
	}
	return &dispatchBudgetProvider{Provider: p, server: s}
}

func (p *dispatchBudgetProvider) attributed(ctx context.Context) (context.Context, llm.Provider) {
	id, _ := strconv.ParseInt(llmrec.TaskIDFrom(ctx), 10, 64)
	if id <= 0 {
		id = agent.RunInfoFrom(ctx).TaskID
	}
	if id <= 0 {
		return ctx, p.Provider
	}
	ctx = llmrec.WithTaskID(ctx, strconv.FormatInt(id, 10))
	return ctx, p.server.withTaskBudget(p.Provider, id)
}

func (p *dispatchBudgetProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	ctx, provider := p.attributed(ctx)
	return provider.Complete(ctx, req)
}

func (p *dispatchBudgetProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		ctx, provider := p.attributed(ctx)
		for ev, err := range provider.Stream(ctx, req) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

// budgetService 返回进程内唯一的预算服务。它懒建的理由与 withTaskBudget 相同：
// 没有模型调用时不碰 DB。语义层（enrich_semantic.go）需要同一份实例——两套
// 配额会让账对不上，而「账对不上」正是预算失控的起点。
func (s *Server) budgetService() *budget.Service {
	if s == nil || s.m == nil || s.m.pg == nil {
		return nil
	}
	s.budgetOnce.Do(func() { s.modelBudget = budget.New(s.m.pg) })
	return s.modelBudget
}

func (s *Server) withTaskBudget(provider llm.Provider, taskID int64) llm.Provider {
	if taskID <= 0 {
		return provider
	}
	if _, ok := provider.(interface{ modelBudgetManaged() }); ok {
		return provider
	}
	s.budgetOnce.Do(func() { s.modelBudget = budget.New(s.m.pg) })
	return &budgetProvider{inner: provider, service: s.modelBudget, taskID: taskID, denied: func(err error) {
		var exceeded *budget.ExceededError
		if errors.As(err, &exceeded) && s.engine != nil {
			id := strconv.FormatInt(taskID, 10)
			if err := s.m.SetTaskPaused(id, true); err != nil {
				log.Printf("[budget] persist pause task %s: %v", id, err)
			}
			s.engine.Pause(id, agent.AbortBudgetExhausted)
		}
	}}
}

func (s *Server) taskBudgetStatus(w http.ResponseWriter, r *http.Request) {
	id, err := parseTaskID(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "任务 id 无效")
		return
	}
	status, err := s.m.pg.ModelBudgetStatus(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "预算状态读取失败")
		return
	}
	if status == nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (p *budgetProvider) reserve(ctx context.Context, req llm.CompletionRequest) (budget.Reservation, llm.CompletionRequest, error) {
	r, req, err := p.service.Reserve(ctx, p.taskID, req)
	if err != nil {
		if p.denied != nil {
			p.denied(err)
		}
		return r, req, &taskLLMError{taskID: strconv.FormatInt(p.taskID, 10), cause: err}
	}
	return r, req, nil
}

func (p *budgetProvider) settle(r budget.Reservation, usage llm.Usage, complete bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.service.Settle(ctx, r, usage, complete); err != nil {
		return &taskLLMError{taskID: strconv.FormatInt(p.taskID, 10), cause: fmt.Errorf("persist model budget settlement: %w", err)}
	}
	return nil
}

func (p *budgetProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	r, req, err := p.reserve(ctx, req)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	msg, stop, usage, callErr := p.inner.Complete(ctx, req)
	if err := p.settle(r, usage, callErr == nil); err != nil {
		return msg, stop, usage, err
	}
	return msg, stop, usage, callErr
}

func (p *budgetProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		r, req, err := p.reserve(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		var usage llm.Usage
		complete := false
		settled := false
		// A downstream consumer may stop immediately after a text delta. Iterator
		// unwinding still charges the reservation, even without message_stop.
		defer func() {
			if !settled {
				_ = p.settle(r, usage, complete)
			}
		}()
		var streamErr error
		for ev, err := range p.inner.Stream(ctx, req) {
			if err != nil {
				streamErr = err
				break
			}
			switch ev.Type {
			case llm.SEMessageStart, llm.SEMessageDelta:
				usage.Add(ev.Usage)
			case llm.SEMessageStop:
				complete = true
			}
			if !yield(ev, nil) {
				return
			}
		}
		settled = true
		if err := p.settle(r, usage, complete && streamErr == nil); err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		if streamErr != nil {
			yield(llm.StreamEvent{}, streamErr)
		}
	}
}
