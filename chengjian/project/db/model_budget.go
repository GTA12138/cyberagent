package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"chengjian.local/chengjian/budget"
	"github.com/google/uuid"
)

const defaultBudgetOutputReservation = 8192

// A persisted reservation can represent a live background compaction or a call
// interrupted by a crash. Neither is safe to remove during archive compaction.
func requireModelBudgetSettled(tx *sql.Tx, taskID int64) error {
	var pending bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM task_model_budget_calls WHERE task_id=$1 AND state='reserved')`, taskID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("%w: model calls have unresolved reservations; inspect task budget pending_calls before retrying archive", ErrTaskArchiveState)
	}
	return nil
}

// ReserveModelTokens uses the task row as the common lock for admission,
// settlement, deletion and archival. Calls are tracked even while limits are off,
// so enabling a budget during an in-flight call cannot miss its reservation.
func (d *DB) ReserveModelTokens(ctx context.Context, taskID int64, estimate budget.Tokens, outputCap int) (budget.Reservation, error) {
	if taskID <= 0 || estimate.Input < 0 || estimate.Output < 0 || outputCap < 0 {
		return budget.Reservation{}, fmt.Errorf("invalid model budget reservation")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return budget.Reservation{}, err
	}
	defer tx.Rollback()
	var maxIn, maxOut int64
	if err := tx.QueryRowContext(ctx, `SELECT max_input_tokens,max_output_tokens FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR UPDATE`, taskID).Scan(&maxIn, &maxOut); err != nil {
		return budget.Reservation{}, err
	}
	var frozen bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_archives WHERE task_id=$1 AND state<>'archive_failed')`, taskID).Scan(&frozen); err != nil {
		return budget.Reservation{}, err
	}
	if frozen {
		return budget.Reservation{}, ErrTaskArchiveState
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_model_budget WHERE task_id=$1)`, taskID).Scan(&exists); err != nil {
		return budget.Reservation{}, err
	}
	if !exists {
		// Import metered history once. The live ledger remains distinct from UI
		// activity summaries, which can omit failed/background/compaction calls.
		// cache_read / cache_write 属于输入侧 token：它们在供应商账单上打折，但
		// 确实占用了上下文预算。漏掉它们会让 max_input_tokens 系统性低估——
		// 长上下文 + 高缓存命中的任务会「看起来没花钱」，正是预算最该拦住的那类。
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_model_budget(task_id,input_spent,output_spent)
SELECT $1,COALESCE(sum(input_tokens::bigint+cache_read::bigint+cache_write::bigint),0),COALESCE(sum(output_tokens::bigint),0)
FROM llm_usage WHERE task_id=$2`, taskID, strconv.FormatInt(taskID, 10)); err != nil {
			return budget.Reservation{}, err
		}
	}
	used, err := modelBudgetTotal(ctx, tx, taskID)
	if err != nil {
		return budget.Reservation{}, err
	}
	var spent budget.Tokens
	if err := tx.QueryRowContext(ctx, `SELECT input_spent,output_spent FROM task_model_budget WHERE task_id=$1`, taskID).Scan(&spent.Input, &spent.Output); err != nil {
		return budget.Reservation{}, err
	}
	if maxIn > 0 && (used.Input >= maxIn || estimate.Input > maxIn-used.Input) {
		if spent.Input < maxIn && estimate.Input <= maxIn-spent.Input && used.Input > spent.Input {
			return budget.Reservation{}, &budget.ReservedError{}
		}
		return budget.Reservation{}, &budget.ExceededError{Dimension: "input", Used: used.Input, Requested: estimate.Input, Limit: maxIn}
	}
	reservedOut := int64(outputCap)
	if reservedOut == 0 {
		reservedOut = defaultBudgetOutputReservation
	}
	if maxOut > 0 {
		if used.Output >= maxOut {
			if spent.Output < maxOut && used.Output > spent.Output {
				return budget.Reservation{}, &budget.ReservedError{}
			}
			return budget.Reservation{}, &budget.ExceededError{Dimension: "output", Used: used.Output, Requested: reservedOut, Limit: maxOut}
		}
		reservedOut = min(reservedOut, maxOut-used.Output)
		outputCap = int(reservedOut)
	}
	r := budget.Reservation{ID: uuid.NewString(), TaskID: taskID, Reserved: budget.Tokens{Input: estimate.Input, Output: reservedOut}, MaxTokens: outputCap}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_model_budget_calls(id,task_id,input_reserved,output_reserved) VALUES($1,$2,$3,$4)`, r.ID, taskID, r.Reserved.Input, r.Reserved.Output); err != nil {
		return budget.Reservation{}, err
	}
	return r, tx.Commit()
}

type budgetQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func modelBudgetTotal(ctx context.Context, q budgetQuerier, taskID int64) (budget.Tokens, error) {
	var used budget.Tokens
	err := q.QueryRowContext(ctx, `SELECT b.input_spent+COALESCE(sum(c.input_reserved) FILTER(WHERE c.state='reserved'),0),
b.output_spent+COALESCE(sum(c.output_reserved) FILTER(WHERE c.state='reserved'),0)
FROM task_model_budget b LEFT JOIN task_model_budget_calls c ON c.task_id=b.task_id WHERE b.task_id=$1
GROUP BY b.input_spent,b.output_spent`, taskID).Scan(&used.Input, &used.Output)
	return used, err
}

// SettleModelTokens is idempotent. Unknown usage consumes at least the estimate,
// so partial failures, missing usage and restarts cannot silently refund spend.
func (d *DB) SettleModelTokens(ctx context.Context, r budget.Reservation, used budget.Tokens, known bool) error {
	if r.ID == "" {
		return nil
	}
	if used.Input < 0 || used.Output < 0 {
		return fmt.Errorf("negative model usage")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taskID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, r.TaskID).Scan(&taskID); err != nil {
		return err
	}
	var reserved budget.Tokens
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT input_reserved,output_reserved,state FROM task_model_budget_calls WHERE id=$1 AND task_id=$2 FOR UPDATE`, r.ID, r.TaskID).Scan(&reserved.Input, &reserved.Output, &state); err != nil {
		return err
	}
	if state != "reserved" {
		return tx.Commit()
	}
	state = "settled"
	if !known {
		used.Input = max(used.Input, reserved.Input)
		used.Output = max(used.Output, reserved.Output)
		state = "uncertain"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_model_budget SET input_spent=input_spent+$2,output_spent=output_spent+$3 WHERE task_id=$1`, r.TaskID, used.Input, used.Output); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_model_budget_calls SET state=$2,input_used=$3,output_used=$4,updated_at=now() WHERE id=$1`, r.ID, state, used.Input, used.Output); err != nil {
		return err
	}
	return tx.Commit()
}

// ModelBudgetTotal includes both settled charges and outstanding reservations.
// The boolean distinguishes a never-budgeted task from a real zero balance.
func (d *DB) ModelBudgetTotal(ctx context.Context, taskID int64) (budget.Tokens, bool, error) {
	used, err := modelBudgetTotal(ctx, d, taskID)
	if err == sql.ErrNoRows {
		return budget.Tokens{}, false, nil
	}
	return used, err == nil, err
}

type ModelBudgetStatus struct {
	MaxInput       int64         `json:"max_input_tokens"`
	MaxOutput      int64         `json:"max_output_tokens"`
	Spent          budget.Tokens `json:"spent"`
	Reserved       budget.Tokens `json:"reserved"`
	PendingCalls   int64         `json:"pending_calls"`
	UncertainCalls int64         `json:"uncertain_calls"`
	Tracking       bool          `json:"tracking"`
}

// ModelBudgetStatus is a single consistent snapshot, including conservative
// charges and unresolved reservations so callers can explain unavailable capacity.
func (d *DB) ModelBudgetStatus(ctx context.Context, taskID int64) (*ModelBudgetStatus, error) {
	var out ModelBudgetStatus
	err := d.QueryRowContext(ctx, `SELECT t.max_input_tokens,t.max_output_tokens,
COALESCE(b.input_spent,0),COALESCE(b.output_spent,0),b.task_id IS NOT NULL,
COALESCE(sum(c.input_reserved) FILTER(WHERE c.state='reserved'),0),
COALESCE(sum(c.output_reserved) FILTER(WHERE c.state='reserved'),0),
count(c.id) FILTER(WHERE c.state='reserved'),count(c.id) FILTER(WHERE c.state='uncertain')
FROM tasks t LEFT JOIN task_model_budget b ON b.task_id=t.id
LEFT JOIN task_model_budget_calls c ON c.task_id=t.id
WHERE t.id=$1 AND t.deleted_at IS NULL AND t.archived_at IS NULL
GROUP BY t.id,b.task_id,b.input_spent,b.output_spent`, taskID).Scan(&out.MaxInput, &out.MaxOutput, &out.Spent.Input, &out.Spent.Output, &out.Tracking, &out.Reserved.Input, &out.Reserved.Output, &out.PendingCalls, &out.UncertainCalls)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &out, err
}
