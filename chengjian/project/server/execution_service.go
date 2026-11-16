package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
	actool "github.com/Autumn-27/norma/tool"
)

// IntentRecovery is deliberately smaller than the PostgreSQL exploration store.
type IntentRecovery interface {
	RecoverInterruptedIntents(context.Context) (db.RecoveryResult, error)
}

func (e *ExecutionService) Recover(ctx context.Context, taskID string, store IntentRecovery) error {
	result, err := store.RecoverInterruptedIntents(ctx)
	if err != nil {
		e.Pause(taskID, fmt.Errorf("execution recovery failed: %w", err))
		return err
	}
	if result.Reopened > 0 || result.Blocked > 0 {
		log.Printf("[execution] task %s 恢复 %d 条意图，%d 条等待核对", taskID, result.Reopened, result.Blocked)
	}
	return nil
}

type executionJournal struct{ db *db.DB }

func (j executionJournal) Begin(ctx context.Context, a agent.ToolAttempt) (agent.AttemptDecision, error) {
	r, err := j.db.BeginExecutionAttempt(ctx, a.TaskID, a.IntentID, a.SessionKey, a.ToolUseID, a.Tool, a.InputHash)
	out := agent.AttemptDecision{ID: r.ID, State: r.State}
	if r.State == "completed" && err == nil {
		err = json.Unmarshal(r.Result, &out.Result)
	}
	return out, err
}
func (j executionJournal) Finish(ctx context.Context, id int64, state string, result actool.Result) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return j.db.FinishExecutionAttempt(ctx, id, state, body)
}
