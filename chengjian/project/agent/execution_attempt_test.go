package agent

import (
	"context"
	"encoding/json"
	"errors"
	actool "github.com/Autumn-27/norma/tool"
	"testing"
)

type memoryJournal struct {
	decision            AttemptDecision
	beginErr, finishErr error
	states              []string
	starts              int
}

func (j *memoryJournal) Begin(context.Context, ToolAttempt) (AttemptDecision, error) {
	j.starts++
	return j.decision, j.beginErr
}
func (j *memoryJournal) Finish(ctx context.Context, _ int64, state string, _ actool.Result) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	j.states = append(j.states, state)
	return j.finishErr
}

func TestExecutionJournalAdmissionAndOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name, state         string
		beginErr, finishErr error
		cancel, panicCall   bool
		wantCalls           int
		wantState           string
		wantError           bool
	}{
		{name: "cached", state: "completed"},
		{name: "unknown", state: "unknown", wantError: true},
		{name: "unavailable", beginErr: errors.New("offline"), wantError: true},
		{name: "completed", state: "new", wantCalls: 1, wantState: "completed"},
		{name: "cancelled", state: "new", cancel: true, wantCalls: 1, wantState: "unknown"},
		{name: "panic", state: "new", panicCall: true, wantCalls: 1, wantState: "unknown", wantError: true},
		{name: "settlement failed", state: "new", finishErr: errors.New("offline"), wantCalls: 1, wantState: "completed", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			journal := &memoryJournal{decision: AttemptDecision{ID: 1, State: tt.state, Result: actool.Text("cached")}, beginErr: tt.beginErr, finishErr: tt.finishErr}
			ctx, cancel := context.WithCancel(WithRunInfo(context.Background(), RunInfo{TaskID: 1, IntentID: 2}))
			defer cancel()
			calls := 0
			tool := actool.Build(actool.Spec{Name: "write", Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				calls++
				if tt.cancel {
					cancel()
				}
				if tt.panicCall {
					panic("interrupted")
				}
				return actool.Text("ok"), nil
			}})
			result, err := guardPanic(tool, Runtime{AttemptJournal: journal}).Call(ctx, json.RawMessage(`{}`), &actool.ToolContext{ToolUseID: "call1"})
			if err != nil || result.IsError != tt.wantError || calls != tt.wantCalls {
				t.Fatalf("calls=%d error=%v result=%+v", calls, err, result)
			}
			if tt.wantState != "" && (len(journal.states) != 1 || journal.states[0] != tt.wantState) {
				t.Fatalf("states=%v", journal.states)
			}
			if tt.state == "completed" && result.Flatten() != "cached" {
				t.Fatal("completed result was not replayed")
			}
		})
	}
}
