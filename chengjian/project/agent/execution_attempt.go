package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

type ToolAttempt struct {
	TaskID, IntentID                       int64
	SessionKey, ToolUseID, Tool, InputHash string
}
type AttemptDecision struct {
	ID     int64
	State  string // new | completed | unknown
	Result actool.Result
}

// AttemptJournal is the execution port. The runtime does not need a SQL store.
// Begin must atomically reject concurrent/recovered invocations of the same call.
type AttemptJournal interface {
	Begin(context.Context, ToolAttempt) (AttemptDecision, error)
	Finish(context.Context, int64, string, actool.Result) error
}

func callWithJournal(ctx context.Context, tool actool.CoreTool, in json.RawMessage, tc *actool.ToolContext, journal AttemptJournal) (result actool.Result, err error) {
	ri := RunInfoFrom(ctx)
	if journal == nil || ri.TaskID <= 0 || tc == nil || tc.ToolUseID == "" || tool.IsReadOnly(in) {
		return tool.Call(ctx, in, tc)
	}
	session := ri.SessionID
	if session == "" {
		session = fmt.Sprintf("intent:%d:agent:%s", ri.IntentID, tc.AgentID)
	}
	sum := sha256.Sum256(in)
	decision, err := journal.Begin(ctx, ToolAttempt{TaskID: ri.TaskID, IntentID: ri.IntentID, SessionKey: session, ToolUseID: tc.ToolUseID, Tool: tool.Name(), InputHash: fmt.Sprintf("%x", sum)})
	if err != nil {
		return actool.Errorf("执行记录无法确认，工具尚未执行：" + err.Error()), nil
	}
	if decision.State == "completed" {
		return decision.Result, nil
	}
	if decision.State != "new" {
		return actool.Errorf("此调用的上次执行结果未知，已停止重试。请先核对执行结果并在任务中确认。"), nil
	}
	defer func() {
		// A panic, cancellation or transport error cannot prove the side effect did
		// not happen. Preserve unknown instead of treating it as a retryable failure.
		state := "unknown"
		if err == nil && ctx.Err() == nil {
			state = "completed"
		}
		if r := recover(); r != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = journal.Finish(cleanup, decision.ID, "unknown", actool.Result{})
			cancel()
			panic(r)
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if finishErr := journal.Finish(cleanup, decision.ID, state, result); finishErr != nil {
			result = actool.Errorf("工具已调用，但执行结果未能落库；结果未知，请先核对再重试。")
			err = nil
		}
	}()
	return tool.Call(ctx, in, tc)
}
