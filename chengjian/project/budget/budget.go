// Package budget owns task-wide model-call admission. The store serializes
// reservations across runners and processes; the service never owns a second
// in-memory copy of the balance.
package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Autumn-27/norma/llm"
)

type Tokens struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

type Reservation struct {
	ID        string
	TaskID    int64
	Reserved  Tokens
	MaxTokens int // effective provider output cap; zero means unlimited
}

type ExceededError struct {
	Dimension              string
	Used, Requested, Limit int64
}

// ReservedError means an outstanding call holds capacity that could be refunded
// at settlement. It must not pause/cancel the calls that currently own capacity.
type ReservedError struct{}

func (*ReservedError) Error() string { return "task token capacity is held by outstanding model calls" }

func (e *ExceededError) Error() string {
	return fmt.Sprintf("task token budget exhausted (%s: used/reserved %d + requested %d, limit %d)", e.Dimension, e.Used, e.Requested, e.Limit)
}

type Store interface {
	ReserveModelTokens(context.Context, int64, Tokens, int) (Reservation, error)
	SettleModelTokens(context.Context, Reservation, Tokens, bool) error
}

type Service struct{ store Store }

func New(store Store) *Service { return &Service{store: store} }

// Reserve estimates the input from the complete neutral request, including tool
// schemas and history. Bytes plus framing overhead are deliberately conservative
// for common tokenizers, but cannot bound vendor-added prompts or all tokenizers.
// Only a provider tokenizer can make an exact input-token guarantee.
func (s *Service) Reserve(ctx context.Context, taskID int64, req llm.CompletionRequest) (Reservation, llm.CompletionRequest, error) {
	if s == nil || s.store == nil {
		return Reservation{}, req, fmt.Errorf("task budget store unavailable")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return Reservation{}, req, fmt.Errorf("estimate model request: %w", err)
	}
	estimate := int64(len(raw)) + 1024 + 256*int64(len(req.Messages)+len(req.Tools))
	reservation, err := s.reserveWithWait(ctx, taskID, Tokens{Input: estimate}, req.MaxTokens)
	if err != nil {
		return Reservation{}, req, err
	}
	req.MaxTokens = reservation.MaxTokens
	return reservation, req, nil
}

// reserveWithWait 是「申请容量 → 遇到 ReservedError 就短等重试」的唯一实现。
//
// 抽出来的理由：非 completion 类开销（语义分析、内联判定）也要走同一套准入，
// 但它们没有 llm.CompletionRequest 可以估。两处各写一遍等待逻辑，迟早出现
// 一份有超时、一份没有——而没有超时的那份会在预算耗尽时静默挂住。
func (s *Service) reserveWithWait(ctx context.Context, taskID int64, tokens Tokens, maxTokens int) (Reservation, error) {
	if s == nil || s.store == nil {
		return Reservation{}, fmt.Errorf("task budget store unavailable")
	}
	if taskID <= 0 {
		return Reservation{}, fmt.Errorf("task budget requires a positive task id")
	}
	var reservation Reservation
	var err error
	waitUntil := time.NewTimer(30 * time.Second)
	defer waitUntil.Stop()
	for {
		reservation, err = s.store.ReserveModelTokens(ctx, taskID, tokens, maxTokens)
		var held *ReservedError
		if !errors.As(err, &held) {
			break
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-waitUntil.C:
			timer.Stop()
			return Reservation{}, fmt.Errorf("%w; capacity wait timed out; inspect pending_calls before retrying", held)
		case <-ctx.Done():
			timer.Stop()
			return Reservation{}, fmt.Errorf("%w: %w", held, ctx.Err())
		case <-timer.C:
		}
	}
	if err != nil {
		return Reservation{}, err
	}
	return reservation, nil
}

// Admit 为非 completion 类开销申请准入（T-15）。
//
// 为什么需要独立入口：语义分析、内联判定这类调用不经过 llm.Provider，因此不会
// 被 provider 层的预算包装器拦住。没有这个入口，它们就是账本外的开销——而
// 「账本外开销」正是预算失控的起点（每次都不多，加起来没人知道）。
//
// 它复用同一份 store：不引入第二套配额，也就不存在「两套账对不上」的问题。
func (s *Service) Admit(ctx context.Context, taskID int64, tokens Tokens) (Reservation, error) {
	if tokens.Input < 0 || tokens.Output < 0 {
		return Reservation{}, fmt.Errorf("admit: negative token estimate")
	}
	return s.reserveWithWait(ctx, taskID, tokens, 0)
}

// Settle uses a fresh bounded context supplied by the host so cancelling a model
// call cannot cancel its accounting. Missing/error usage is conservatively charged
// against the reservation; a crash leaves the durable reservation outstanding.
func (s *Service) Settle(ctx context.Context, r Reservation, usage llm.Usage, complete bool) error {
	if r.ID == "" {
		return nil
	}
	// norma normalizes InputTokens to the total; cache counts are subsets.
	used := Tokens{Input: int64(max(usage.InputTokens, 0)), Output: int64(max(usage.OutputTokens, 0))}
	// Usage has no presence bits. Zero in either dimension cannot distinguish
	// an omitted field from a genuine zero, so retain the conservative charge.
	known := complete && used.Input > 0 && used.Output > 0
	return s.store.SettleModelTokens(ctx, r, used, known)
}
