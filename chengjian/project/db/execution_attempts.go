package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

type ExecutionAttempt struct {
	ID         int64           `json:"id"`
	TaskID     int64           `json:"task_id"`
	IntentID   int64           `json:"intent_id"`
	Tool       string          `json:"tool"`
	State      string          `json:"state"`
	Assessment string          `json:"assessment"`
	Result     json.RawMessage `json:"-"`
}

func (d *DB) BeginExecutionAttempt(ctx context.Context, taskID, intentID int64, session, callID, tool, inputHash string) (ExecutionAttempt, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionAttempt{}, err
	}
	defer tx.Rollback()
	// Coordinate with archive/delete, which take FOR UPDATE on the parent task.
	// A stub hidden by compaction must never start new side effects.
	var explorationID int64
	if err = tx.QueryRowContext(ctx, `SELECT exploration_id FROM tasks WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, taskID).Scan(&explorationID); err != nil {
		return ExecutionAttempt{}, err
	}
	if intentID > 0 {
		var found int64
		if err = tx.QueryRowContext(ctx, `SELECT id FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 FOR UPDATE`, intentID, explorationID).Scan(&found); err != nil {
			return ExecutionAttempt{}, err
		}
	}
	var unresolved bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tool_execution_attempts WHERE task_id=$1 AND state='unknown' AND ((intent_id=$2 AND $2>0) OR ($2=0 AND intent_id IS NULL AND session_key=$3)))`, taskID, intentID, session).Scan(&unresolved); err != nil {
		return ExecutionAttempt{}, err
	}
	if unresolved {
		return ExecutionAttempt{}, errors.New("存在结果未知的历史调用，需要先核对")
	}
	var out ExecutionAttempt
	err = tx.QueryRowContext(ctx, `INSERT INTO tool_execution_attempts(task_id,intent_id,session_key,tool_use_id,tool,input_hash,state)
VALUES ($1,NULLIF($2,0),$3,$4,$5,$6,'started') ON CONFLICT DO NOTHING RETURNING id`, taskID, intentID, session, callID, tool, inputHash).Scan(&out.ID)
	if err == nil {
		out.State = "new"
	} else if errors.Is(err, sql.ErrNoRows) {
		var oldHash, oldTool string
		err = tx.QueryRowContext(ctx, `SELECT id,state,COALESCE(result,'null'::jsonb),input_hash,tool FROM tool_execution_attempts WHERE task_id=$1 AND session_key=$2 AND tool_use_id=$3 FOR UPDATE`, taskID, session, callID).Scan(&out.ID, &out.State, &out.Result, &oldHash, &oldTool)
		if err != nil {
			return out, err
		}
		if oldHash != inputHash || oldTool != tool {
			return out, errors.New("tool call identity changed")
		}
		if out.State == "not_executed" {
			_, err = tx.ExecContext(ctx, `UPDATE tool_execution_attempts SET state='started',result=NULL,started_at=now(),completed_at=NULL WHERE id=$1`, out.ID)
			out.State = "new"
		}
	} else {
		return out, err
	}
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}

func (d *DB) FinishExecutionAttempt(ctx context.Context, id int64, state string, result json.RawMessage) error {
	if state != "completed" && state != "unknown" {
		return errors.New("invalid execution outcome")
	}
	if len(result) == 0 {
		result = json.RawMessage(`null`)
	}
	res, err := d.ExecContext(ctx, `UPDATE tool_execution_attempts SET state=$2,result=$3,completed_at=now() WHERE id=$1 AND state='started'`, id, state, result)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("execution attempt no longer owned")
	}
	return nil
}

func (d *DB) ListExecutionAttempts(ctx context.Context, taskID int64) ([]ExecutionAttempt, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,task_id,COALESCE(intent_id,0),tool,state,assessment FROM tool_execution_attempts WHERE task_id=$1 AND state IN ('unknown','started') ORDER BY id DESC LIMIT 200`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExecutionAttempt{}
	for rows.Next() {
		var a ExecutionAttempt
		if err := rows.Scan(&a.ID, &a.TaskID, &a.IntentID, &a.Tool, &a.State, &a.Assessment); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ResolveExecutionAttempt records an explicit human reconciliation. completed
// needs a result supplied by the caller; not_executed permits the original call
// to be attempted again. Reconciliation itself never resumes a task.
func (d *DB) ResolveExecutionAttempt(ctx context.Context, taskID, id int64, state, assessment string, result json.RawMessage) error {
	if (state != "completed" && state != "not_executed") || assessment == "" {
		return errors.New("需要确认执行结果并填写核对依据")
	}
	if state == "completed" && len(result) == 0 {
		return errors.New("completed requires a reconciled result")
	}
	if len(result) == 0 {
		result = json.RawMessage(`null`)
	}
	res, err := d.ExecContext(ctx, `UPDATE tool_execution_attempts SET state=$3,assessment=$4,result=$5,completed_at=now() WHERE id=$1 AND task_id=$2 AND state='unknown'`, id, taskID, state, assessment, result)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("只能确认结果未知的调用")
	}
	return nil
}

type RecoveryResult struct{ Reopened, Blocked int64 }

func (s *ExplorationStore) RecoverInterruptedIntents(ctx context.Context) (RecoveryResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecoveryResult{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE tool_execution_attempts SET state='unknown',assessment='进程中断，执行结果尚未核对' WHERE state='started' AND task_id IN (SELECT id FROM tasks WHERE exploration_id=$1)`, s.expID); err != nil {
		return RecoveryResult{}, err
	}
	// Existing installations had no journal. An unmatched tool_use is retained
	// as an unknown attempt instead of silently replaying a legacy side effect.
	_, err = tx.ExecContext(ctx, `INSERT INTO tool_execution_attempts(task_id,intent_id,session_key,tool_use_id,tool,input_hash,state,assessment)
SELECT t.id,n.id,'legacy:'||n.id,a.tool_use_id,COALESCE(a.tool,'unknown'),'legacy','unknown','旧版本中断调用，需要人工核对'
FROM exploration_nodes n JOIN tasks t ON t.exploration_id=n.exploration_id JOIN activity a ON a.exploration_id=n.exploration_id AND a.node_id=n.id AND a.kind='tool_use'
WHERE n.exploration_id=$1 AND n.kind='intent' AND n.state='running' AND COALESCE(a.tool_use_id,'')<>''
AND NOT EXISTS(SELECT 1 FROM activity r WHERE r.exploration_id=a.exploration_id AND r.node_id=n.id AND r.kind='tool_result' AND r.tool_use_id=a.tool_use_id)
AND NOT EXISTS(SELECT 1 FROM tool_execution_attempts x WHERE x.task_id=t.id AND x.intent_id=n.id AND x.tool_use_id=a.tool_use_id)
ON CONFLICT DO NOTHING`, s.expID)
	if err != nil {
		return RecoveryResult{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE exploration_nodes n SET state='blocked',blocked_reason='recovery_required: 存在结果未知的工具调用，请先核对执行记录',completed_at=NULL
WHERE exploration_id=$1 AND kind='intent' AND state='running' AND EXISTS(SELECT 1 FROM tool_execution_attempts a WHERE a.intent_id=n.id AND a.state='unknown')`, s.expID)
	if err != nil {
		return RecoveryResult{}, err
	}
	blocked, _ := res.RowsAffected()
	res, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET state='open',completed_at=NULL,blocked_reason=NULL WHERE exploration_id=$1 AND kind='intent' AND state='running'`, s.expID)
	if err != nil {
		return RecoveryResult{}, err
	}
	reopened, _ := res.RowsAffected()
	if err = tx.Commit(); err != nil {
		return RecoveryResult{}, fmt.Errorf("recover intents: %w", err)
	}
	return RecoveryResult{Reopened: reopened, Blocked: blocked}, nil
}

// Archive compaction must not erase an unresolved external side effect.
func requireToolAttemptsSettled(tx *sql.Tx, taskID int64) error {
	var unresolved bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tool_execution_attempts WHERE task_id=$1 AND state IN ('started','unknown'))`, taskID).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved {
		return fmt.Errorf("%w: unresolved tool execution; reconcile before archive", ErrTaskArchiveState)
	}
	return nil
}
