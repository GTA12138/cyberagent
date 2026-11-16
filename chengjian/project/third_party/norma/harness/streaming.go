package harness

import (
	"strings"
	"sync"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
)

const maxPendingProgress = 128
const maxProgressBytes = 8 << 10

// streamExec is the streaming tool executor: tool_use blocks are registered as
// they finish streaming and begin executing immediately, subject to a FIFO +
// exclusivity rule — a concurrency-safe tool may run alongside other safe tools,
// but a non-safe tool requires exclusive access (no other tool executing) and
// blocks every tool queued behind it until it finishes. Results are yielded in
// arrival order; progress is yielded as soon as it arrives.
type streamExec struct {
	l         *loop
	execution *loop // immutable phase/context snapshot for queued tool goroutines
	mu        sync.Mutex
	tools     []*tracked
	notify    chan struct{} // pinged when a tool completes or emits progress
}

type toolStatus int

const (
	stQueued toolStatus = iota
	stExecuting
	stCompleted
	stYielded
)

type tracked struct {
	block  llm.ContentBlock
	safe   bool
	status toolStatus
	result llm.ContentBlock
	extra  []llm.Message // messages the tool injects after its result (Result.Extra)
	prog   []string
}

func newStreamExec(l *loop) *streamExec {
	execution := &loop{ctx: l.ctx, toolCtx: l.toolCtx, in: l.in, settling: l.settling}
	return &streamExec{l: l, execution: execution, notify: make(chan struct{}, 1)}
}

func (e *streamExec) ping() {
	select {
	case e.notify <- struct{}{}:
	default:
	}
}

// add registers a completed tool_use block and tries to start it.
func (e *streamExec) add(block llm.ContentBlock) {
	e.mu.Lock()
	safe := false
	if e.l.in.Tools != nil {
		if t, ok := e.l.in.Tools.Get(block.Name); ok {
			safe = t.IsConcurrencySafe(block.Input)
		}
	}
	e.tools = append(e.tools, &tracked{block: block, safe: safe})
	e.mu.Unlock()
	e.processQueue()
}

// canStartLocked reports whether a tool with the given safety may start now.
func (e *streamExec) canStartLocked(safe bool) bool {
	for _, t := range e.tools {
		if t.status == stExecuting && (!safe || !t.safe) {
			return false
		}
	}
	return true
}

// processQueue starts every queued tool that may run, honoring FIFO: a non-safe
// tool that cannot start blocks the tools behind it.
func (e *streamExec) processQueue() {
	e.mu.Lock()
	var start []*tracked
	for _, t := range e.tools {
		if t.status != stQueued {
			continue
		}
		if e.canStartLocked(t.safe) {
			t.status = stExecuting
			start = append(start, t)
		} else if !t.safe {
			break
		}
	}
	e.mu.Unlock()
	for _, t := range start {
		go e.run(t)
	}
}

func (e *streamExec) run(t *tracked) {
	select {
	case e.l.sem <- struct{}{}:
		defer func() { <-e.l.sem }()
	case <-e.execution.toolCtx.Done():
		// execOne observes the captured canceled context without dispatching.
	}
	res, extra := e.execution.execOne(t.block, func(p tool.ProgressInfo) {
		e.mu.Lock()
		if t.status == stExecuting {
			message := p.Message
			if len(message) > maxProgressBytes {
				message = strings.ToValidUTF8(message[:maxProgressBytes], "") + " [progress shortened; see tool result]"
			}
			if len(t.prog) < maxPendingProgress {
				t.prog = append(t.prog, message)
			} else {
				t.prog[maxPendingProgress-1] = "[additional progress omitted; final tool result is retained separately]"
			}
		}
		e.mu.Unlock()
		e.ping()
	})
	e.mu.Lock()
	if t.status != stYielded { // a late return must not overwrite an already emitted interruption
		t.result = res
		t.extra = extra
		t.status = stCompleted
	}
	e.mu.Unlock()
	e.ping()
	e.processQueue() // a slot freed
}

// drain yields all ready progress and completed results in FIFO order. Stops at
// the first not-yet-finished non-safe tool. Returns false if the consumer broke.
func (e *streamExec) drain() bool {
	e.mu.Lock()
	var evs []Event
	for _, t := range e.tools {
		for _, p := range t.prog {
			evs = append(evs, Event{Kind: KindProgress, Text: p})
		}
		t.prog = nil
		if t.status == stYielded {
			continue
		}
		if t.status == stCompleted {
			t.status = stYielded
			r := t.result
			evs = append(evs, Event{Kind: KindToolResult, ToolResult: &r})
			continue
		}
		// status is queued/executing: a non-safe one preserves FIFO ordering.
		if !t.safe {
			break
		}
	}
	e.mu.Unlock()
	for _, ev := range evs {
		if !e.l.emit(ev) {
			return false
		}
	}
	return true
}

// pending reports whether any tool is still queued or executing.
func (e *streamExec) pending() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range e.tools {
		if t.status == stQueued || t.status == stExecuting {
			return true
		}
	}
	return false
}

// finish drains to completion. It returns (consumerStopped, aborted): on context
// cancellation it synthesizes interrupted results for unfinished tools.
func (e *streamExec) finish() (consumerStopped, aborted bool) {
	for {
		if !e.drain() {
			return true, false
		}
		// toolCtx cancellation is either a parent cancel (pause/kill/shutdown) or the
		// run's own MaxDuration deadline firing on an in-flight tool. Either way we
		// synthesize interrupted results and report aborted; the turn loop then decides
		// (parent-cancel → abort, budget-only → wrap-up) via ctx vs toolCtx.
		if e.l.toolCtx.Err() != nil {
			return !e.drainSynthetic("tool execution interrupted"), true
		}
		if !e.pending() {
			if !e.drain() {
				return true, false
			}
			return false, false
		}
		select {
		case <-e.notify:
		case <-e.l.toolCtx.Done():
		}
	}
}

// drainSynthetic marks every not-yet-yielded tool with a synthetic error result
// and emits it. Returns consumer-not-stopped.
func (e *streamExec) drainSynthetic(msg string) bool {
	e.mu.Lock()
	var evs []Event
	for _, t := range e.tools {
		if t.status == stYielded {
			continue
		}
		if t.status == stCompleted {
			t.status = stYielded
			r := t.result
			evs = append(evs, Event{Kind: KindToolResult, ToolResult: &r})
			continue
		}
		t.status = stYielded
		t.result = llm.ToolResultText(t.block.ID, msg, true)
		r := t.result
		evs = append(evs, Event{Kind: KindToolResult, ToolResult: &r})
	}
	e.mu.Unlock()
	for _, ev := range evs {
		if !e.l.emit(ev) {
			return false
		}
	}
	return true
}

// results returns one tool_result per tool in arrival order (synthesizing for
// any that never finished).
func (e *streamExec) results() []llm.ContentBlock {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]llm.ContentBlock, len(e.tools))
	for i, t := range e.tools {
		if t.status == stCompleted || t.status == stYielded {
			out[i] = t.result
		} else {
			out[i] = llm.ToolResultText(t.block.ID, "tool execution interrupted", true)
		}
	}
	return out
}

// extraMessages returns, in tool arrival order, the extra messages tools chose
// to inject after their tool_result (Result.Extra). Empty for the common case
// where no tool injects anything.
func (e *streamExec) extraMessages() []llm.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []llm.Message
	for _, t := range e.tools {
		if (t.status == stCompleted || t.status == stYielded) && len(t.extra) > 0 {
			out = append(out, t.extra...)
		}
	}
	return out
}

func (e *streamExec) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.tools)
}

// acceptedMessage excludes unfinished tool blocks which were never dispatched.
// Keeping them would manufacture a tool_use without a real execution result.
func (e *streamExec) acceptedMessage(msg llm.Message) llm.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	accepted := make(map[string]bool, len(e.tools))
	for _, t := range e.tools {
		accepted[t.block.ID] = true
	}
	blocks := make([]llm.ContentBlock, 0, len(msg.Content))
	for _, b := range msg.Content {
		if b.Type != llm.BlockToolUse || accepted[b.ID] {
			blocks = append(blocks, b)
		}
	}
	msg.Content = blocks
	return msg
}
