package harness

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"iter"
	"sync/atomic"
	"testing"
)

func TestInterruptedStreamPreservesExecutedToolAndDropsPending(t *testing.T) {
	var calls atomic.Int32
	probe := tool.Build(tool.Spec{Name: "probe", Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		calls.Add(1)
		return tool.Text("retained result"), nil
	}})
	cause := errors.New("fixture stream disconnected")
	model := func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range []llm.StreamEvent{
				{Type: llm.SEToolUseStart, ToolID: "done", ToolName: "probe"},
				{Type: llm.SEToolInputJSON, Text: `{"ok":true}`},
				{Type: llm.SEToolUseStart, ToolID: "partial", ToolName: "probe"},
				{Type: llm.SEToolInputJSON, Text: `{"broken":`},
			} {
				if !yield(ev, nil) {
					return
				}
			}
			yield(llm.StreamEvent{}, cause)
		}
	}
	term := drain(t, QueryInput{Messages: []llm.Message{llm.UserText("go")}, Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, QueryDeps{CallModel: model})
	if term.Reason != ReasonModelError || calls.Load() != 1 {
		t.Fatalf("reason=%v calls=%d", term.Reason, calls.Load())
	}
	if len(term.Messages) != 3 {
		t.Fatalf("execution lost from resumable history: %+v", term.Messages)
	}
	uses := term.Messages[1].ToolUses()
	if len(uses) != 1 || uses[0].ID != "done" || term.Messages[2].Content[0].ToolUseID != "done" || term.Messages[2].Content[0].Content[0].Text != "retained result" {
		t.Fatalf("unpaired history: %+v", term.Messages)
	}
}

func TestMalformedToolArgumentsNeverExecute(t *testing.T) {
	for _, input := range []string{`{"broken":`, `not-json`, `null`, `[]`, `{invalid}`} {
		var calls atomic.Int32
		probe := tool.Build(tool.Spec{Name: "probe", Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			calls.Add(1)
			return tool.Text("bad"), nil
		}})
		term := drain(t, QueryInput{Messages: []llm.Message{llm.UserText("go")}, Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, QueryDeps{CallModel: alwaysModel(toolTurn("bad", "probe", input))})
		if calls.Load() != 0 || term.Reason != ReasonModelError {
			t.Fatalf("bad arguments dispatched: calls=%d terminal=%+v", calls.Load(), term)
		}
	}
}

type failedRecorder struct {
	err           error
	failOnMessage bool
}

func (r *failedRecorder) RecordMessage(llm.Message, llm.Usage) {
	if r.failOnMessage {
		r.err = errors.New("fixture disk full")
	}
}
func (r *failedRecorder) RecordBoundary(llm.BoundaryMeta) {}
func (r *failedRecorder) Err() error                      { return r.err }

func TestPersistenceFailureStopsModelAndCannotFinishSuccessfully(t *testing.T) {
	for _, initial := range []bool{true, false} {
		r := &failedRecorder{failOnMessage: !initial}
		if initial {
			r.err = errors.New("fixture disk full")
		}
		m := &scriptedModel{turns: [][]llm.StreamEvent{textTurn("done")}}
		term := drain(t, QueryInput{Messages: []llm.Message{llm.UserText("go")}, Tools: tool.NewRegistry(), Recorder: r}, QueryDeps{CallModel: m.call})
		var persistenceErr *PersistenceError
		if term.Reason != ReasonModelError || !errors.As(term.Err, &persistenceErr) || persistenceErr.AllowModelFailover() {
			t.Fatalf("disk failure hidden: %+v", term)
		}
		if initial && m.calls != 0 {
			t.Fatal("model called without a writable transcript")
		}
	}
}
