package budget

import (
	"context"
	"testing"
)

// fakeStore 是 Store 的内存替身：记录每次申请的 token，并可按脚本返回错误。
type fakeStore struct {
	calls     []Tokens
	limits    []int
	reservedN int
	errs      []error // 依次返回的错误；耗尽后返回 nil
}

func (f *fakeStore) ReserveModelTokens(_ context.Context, taskID int64, t Tokens, maxTokens int) (Reservation, error) {
	if taskID <= 0 {
		return Reservation{}, &ExceededError{Dimension: "task", Limit: 0}
	}
	f.calls = append(f.calls, t)
	f.limits = append(f.limits, maxTokens)
	f.reservedN++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return Reservation{}, err
		}
	}
	return Reservation{ID: "r1", TaskID: taskID, Reserved: t, MaxTokens: maxTokens}, nil
}

func (f *fakeStore) SettleModelTokens(context.Context, Reservation, Tokens, bool) error { return nil }

func TestAdmitReservesAgainstTheSameLedger(t *testing.T) {
	fs := &fakeStore{}
	s := New(fs)
	if _, err := s.Admit(context.Background(), 7, Tokens{Input: 1200, Output: 300}); err != nil {
		t.Fatal(err)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("应申请一次，实际 %d", len(fs.calls))
	}
	if fs.calls[0] != (Tokens{Input: 1200, Output: 300}) {
		t.Fatalf("申请量错: %+v", fs.calls[0])
	}
	if fs.limits[0] != 0 {
		t.Fatalf("非 completion 开销不该带输出上限，实际 %d", fs.limits[0])
	}
}

func TestAdmitWaitsOutReservedCapacity(t *testing.T) {
	fs := &fakeStore{errs: []error{&ReservedError{}, &ReservedError{}, nil}}
	s := New(fs)
	if _, err := s.Admit(context.Background(), 7, Tokens{Input: 10}); err != nil {
		t.Fatalf("容量被别人临时占住时应等待并成功: %v", err)
	}
	if len(fs.calls) != 3 {
		t.Fatalf("应重试到成功，实际申请 %d 次", len(fs.calls))
	}
}

func TestAdmitSurfacesExceededImmediately(t *testing.T) {
	fs := &fakeStore{errs: []error{&ExceededError{Dimension: "input", Used: 100, Requested: 10, Limit: 105}}}
	s := New(fs)
	_, err := s.Admit(context.Background(), 7, Tokens{Input: 10})
	if err == nil {
		t.Fatal("超限必须直接报错，不能等满 30 秒")
	}
	if len(fs.calls) != 1 {
		t.Fatalf("超限不该重试，实际 %d 次", len(fs.calls))
	}
}

func TestAdmitRejectsInvalidInput(t *testing.T) {
	s := New(&fakeStore{})
	if _, err := s.Admit(context.Background(), 0, Tokens{Input: 1}); err == nil {
		t.Fatal("taskID 必须为正：0 会落进「不属于任何任务」的黑洞")
	}
	if _, err := s.Admit(context.Background(), 1, Tokens{Input: -1}); err == nil {
		t.Fatal("负估算必须被拒")
	}
}

func TestAdmitWithoutStoreFailsLoudly(t *testing.T) {
	var s *Service
	if _, err := s.Admit(context.Background(), 1, Tokens{Input: 1}); err == nil {
		t.Fatal("没有 store 时必须报错，不能静默放行（那等于账本外开销）")
	}
}
