package harness

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestSettlementCannotExecuteHiddenTool(t *testing.T) {
	var calls atomic.Int32
	probe := tool.Build(tool.Spec{Name: "probe", Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		calls.Add(1)
		return tool.Text("ok"), nil
	}})
	m := &scriptedModel{turns: [][]llm.StreamEvent{toolTurn("first", "probe", `{}`), toolTurn("forbidden", "probe", `{}`), textTurn("settled")}}
	term := drain(t, QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass, MaxTurns: 1,
		Settlement: &Settlement{Prompt: "settle", DisabledTools: []string{"probe"}}}, QueryDeps{CallModel: m.call})
	if calls.Load() != 1 || term.Reason != ReasonMaxTurns {
		t.Fatalf("hidden tool executed: calls=%d reason=%s", calls.Load(), term.Reason)
	}
	if !term.Messages[4].Content[0].IsError {
		t.Fatal("forbidden call did not retain an error result")
	}
}

func TestQueuedToolKeepsOriginalCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	probe := tool.Build(tool.Spec{Name: "probe", Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		calls.Add(1)
		return tool.Text("unexpected"), nil
	}})
	l := &loop{ctx: context.Background(), toolCtx: ctx, in: QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, sem: make(chan struct{}, 1)}
	e := newStreamExec(l)
	l.sem <- struct{}{} // queued behind another call
	tr := &tracked{block: llm.ContentBlock{ID: "old", Name: "probe", Input: []byte(`{}`)}, status: stExecuting}
	e.tools = []*tracked{tr}
	cancel()
	l.toolCtx = context.Background() // settlement entered while old call was queued
	done := make(chan struct{})
	go func() { e.run(tr); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled tool still waits for execution slot")
	}
	if calls.Load() != 0 || !e.results()[0].IsError {
		t.Fatal("old tool adopted the settlement context")
	}
}

func TestLateToolResultCannotReplaceEmittedInterruption(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	probe := tool.Build(tool.Spec{Name: "probe", Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		close(started)
		<-release
		return tool.Text("late success"), nil
	}})
	l := &loop{ctx: context.Background(), toolCtx: context.Background(), in: QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, sem: make(chan struct{}, 1), yield: func(Event, error) bool { return true }}
	e := newStreamExec(l)
	tr := &tracked{block: llm.ContentBlock{ID: "late", Name: "probe", Input: []byte(`{}`)}, status: stExecuting}
	e.tools = []*tracked{tr}
	done := make(chan struct{})
	go func() { e.run(tr); close(done) }()
	<-started
	e.drainSynthetic("interrupted; outcome unknown")
	close(release)
	<-done
	if !e.results()[0].IsError || e.results()[0].Content[0].Text != "interrupted; outcome unknown" || tr.status != stYielded {
		t.Fatal("late completion rewrote recorded interruption")
	}
}

func TestConsumerStopCancelsInFlightTool(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	probe := tool.Build(tool.Spec{Name: "probe", Run: func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return tool.Text("stopped"), ctx.Err()
	}})
	model := func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range toolTurn("call", "probe", `{}`)[:3] {
				if !yield(ev, nil) {
					return
				}
			}
			<-started
			yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "stop reading"}, nil)
		}
	}
	for ev := range Query(context.Background(), QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, QueryDeps{CallModel: model}) {
		if ev.Kind == KindText {
			break
		}
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("tool outlived stopped consumer")
	}
}

type replacingHooks struct{ fakeHooks }

func (*replacingHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", []byte(`{"required":42}`)
}

func TestHookUpdatedInputIsValidatedBeforeExecution(t *testing.T) {
	var calls atomic.Int32
	probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object", "properties": map[string]any{"required": map[string]any{"type": "string"}}}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		calls.Add(1)
		return tool.Text("unexpected"), nil
	}})
	l := &loop{ctx: context.Background(), in: QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass, Hooks: &replacingHooks{}}}
	res, _ := l.execOne(llm.ContentBlock{ID: "x", Name: "probe", Input: []byte(`{"required":"valid"}`)}, nil)
	if calls.Load() != 0 || !res.IsError || !strings.Contains(res.Content[0].Text, "updated") {
		t.Fatalf("invalid rewritten arguments executed: %+v", res)
	}
}

func TestDuplicateToolIdentityCannotExecuteTwice(t *testing.T) {
	for _, syncCall := range []bool{false, true} {
		var calls atomic.Int32
		probe := tool.Build(tool.Spec{Name: "probe", Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			calls.Add(1)
			return tool.Text("done"), nil
		}})
		deps := QueryDeps{}
		if syncCall {
			deps.CallModelSync = func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				block := llm.ContentBlock{Type: llm.BlockToolUse, ID: "same", Name: "probe", Input: []byte(`{}`)}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{block, block}}, "tool_use", llm.Usage{}, nil
			}
		} else {
			turn := append(toolTurn("same", "probe", `{}`)[:2], toolTurn("same", "probe", `{}`)...)
			deps.CallModel = alwaysModel(turn)
		}
		term := drain(t, QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass, NonStreaming: syncCall}, deps)
		want := int32(1)
		if syncCall {
			want = 0
		}
		if term.Reason != ReasonModelError || calls.Load() != want {
			t.Fatalf("duplicate identity executed: sync=%v count=%d reason=%s", syncCall, calls.Load(), term.Reason)
		}
		if !syncCall && len(term.Messages[0].ToolUses()) != 1 {
			t.Fatal("unexecuted duplicate polluted resumable history")
		}
	}
}

func TestPendingProgressMemoryIsBounded(t *testing.T) {
	large := strings.Repeat("x", maxProgressBytes+100)
	probe := tool.Build(tool.Spec{Name: "probe", Run: func(_ context.Context, _ json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
		for i := 0; i < maxPendingProgress*4; i++ {
			tc.Emit(tool.ProgressInfo{Message: large})
		}
		return tool.Text("complete final evidence"), nil
	}})
	l := &loop{ctx: context.Background(), toolCtx: context.Background(), in: QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, sem: make(chan struct{}, 1)}
	e := newStreamExec(l)
	tr := &tracked{block: llm.ContentBlock{ID: "progress", Name: "probe", Input: []byte(`{}`)}, status: stExecuting}
	e.tools = []*tracked{tr}
	e.run(tr)
	if len(tr.prog) != maxPendingProgress || !strings.Contains(tr.prog[len(tr.prog)-1], "omitted") || len(tr.prog[0]) > maxProgressBytes+100 {
		t.Fatal("progress queue retained unbounded messages")
	}
	if e.results()[0].Content[0].Text != "complete final evidence" {
		t.Fatal("progress limit discarded actual result")
	}
}
