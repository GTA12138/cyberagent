package db

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"chengjian.local/chengjian/budget"
)

func modelBudgetFixture(t *testing.T, maxIn, maxOut int64) (*DB, *Task) {
	t.Helper()
	d := notifyTestDB(t)
	task, err := d.CreateTaskWithOptions("local model budget fixture", "no external targets", TaskCreateOptions{MaxInputTokens: maxIn, MaxOutputTokens: maxOut})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTask(task.ID); d.Exec(`DELETE FROM llm_usage WHERE task_id=$1`, fmt.Sprint(task.ID)) })
	return d, task
}

func TestModelBudgetConcurrentReservations(t *testing.T) {
	d, task := modelBudgetFixture(t, 1000, 100)
	var wg sync.WaitGroup
	results := make(chan budget.Reservation, 24)
	errs := make(chan error, 24)
	for range 24 {
		wg.Go(func() {
			r, err := d.ReserveModelTokens(t.Context(), task.ID, budget.Tokens{Input: 100}, 10)
			if err != nil {
				errs <- err
			} else {
				results <- r
			}
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	count := 0
	for range results {
		count++
	}
	if count != 10 {
		t.Fatalf("admitted %d simultaneous calls, want 10", count)
	}
	for err := range errs {
		var held *budget.ReservedError
		if !errors.As(err, &held) {
			t.Fatal(err)
		}
	}
	status, err := d.ModelBudgetStatus(t.Context(), task.ID)
	if err != nil || status.Reserved.Input != 1000 || status.Reserved.Output != 100 || status.PendingCalls != 10 {
		t.Fatal(status, err)
	}
	// Another task has its own account and capacity.
	other, err := d.CreateTask("other budget fixture", "local", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTask(other.ID) })
	if _, err := d.ReserveModelTokens(t.Context(), other.ID, budget.Tokens{Input: 100}, 10); err != nil {
		t.Fatal(err)
	}
}

func TestModelBudgetSettlementAndRestart(t *testing.T) {
	d, task := modelBudgetFixture(t, 1000, 100)
	if err := d.InsertLLMUsage(&LLMUsage{TaskID: fmt.Sprint(task.ID), InputTokens: 100, OutputTokens: 10, CacheRead: 20, CacheWrite: 5}); err != nil {
		t.Fatal(err)
	}
	r, err := d.ReserveModelTokens(t.Context(), task.ID, budget.Tokens{Input: 300}, 200)
	if err != nil || r.MaxTokens != 90 {
		t.Fatal(r, err)
	}
	// A new connection after a simulated process restart still sees the reservation.
	restarted, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	_, err = restarted.ReserveModelTokens(t.Context(), task.ID, budget.Tokens{Input: 10}, 1)
	var held *budget.ReservedError
	if !errors.As(err, &held) {
		t.Fatalf("lost outstanding reservation after restart: %v", err)
	}
	if err := restarted.SettleModelTokens(t.Context(), r, budget.Tokens{Input: 40, Output: 15}, true); err != nil {
		t.Fatal(err)
	}
	if err := restarted.SettleModelTokens(t.Context(), r, budget.Tokens{Input: 40, Output: 15}, true); err != nil {
		t.Fatal(err)
	}
	status, err := d.ModelBudgetStatus(t.Context(), task.ID)
	if err != nil || status.Spent.Input != 165 || status.Spent.Output != 25 || status.PendingCalls != 0 {
		t.Fatal(status, err)
	}
	r, err = d.ReserveModelTokens(t.Context(), task.ID, budget.Tokens{Input: 200}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SettleModelTokens(t.Context(), r, budget.Tokens{Input: 300, Output: 3}, false); err != nil {
		t.Fatal(err)
	}
	status, err = d.ModelBudgetStatus(t.Context(), task.ID)
	if err != nil || status.Spent.Input != 465 || status.Spent.Output != 35 || status.UncertainCalls != 1 {
		t.Fatal(status, err)
	}
}

func TestModelBudgetEnableDuringCall(t *testing.T) {
	d, task := modelBudgetFixture(t, 0, 0)
	r, err := d.ReserveModelTokens(t.Context(), task.ID, budget.Tokens{Input: 100}, 0)
	if err != nil || r.MaxTokens != 0 || r.ID == "" {
		t.Fatal(r, err)
	}
	if _, err := d.Exec(`UPDATE tasks SET max_input_tokens=100,max_output_tokens=50 WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	_, err = d.ReserveModelTokens(t.Context(), task.ID, budget.Tokens{Input: 1}, 1)
	var held *budget.ReservedError
	if !errors.As(err, &held) {
		t.Fatalf("in-flight unlimited call disappeared when enabling limit: %v", err)
	}
	if err := d.SettleModelTokens(t.Context(), r, budget.Tokens{Input: 100, Output: 10}, true); err != nil {
		t.Fatal(err)
	}
	_, err = d.ReserveModelTokens(t.Context(), task.ID, budget.Tokens{Input: 1}, 1)
	var exhausted *budget.ExceededError
	if !errors.As(err, &exhausted) {
		t.Fatal(err)
	}
}
