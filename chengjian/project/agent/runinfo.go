package agent

import (
	"context"

	"chengjian.local/chengjian/db"
)

// RunInfo identifies WHICH run a tool call belongs to. Tool assembly only receives
// (ctx, agentKey) — the task/exploration ids live in the caller's arguments, not the
// ctx — so anything wired at assembly time (currently the Skill ledger in
// server/assembly.go) has no way to attribute a call to a task. Each run attaches
// its own RunInfo before calling AugmentTools; the wiring closure reads it once and
// captures it, so per-run attribution stays correct without threading parameters
// through the tool layer. Same pattern as TaskClock (see taskclock.go).
//
// Zero value = attribution unknown; every consumer must treat it as optional.
type RunInfo struct {
	TaskID        int64  // task registry id; 0 for non-task runs (chat sessions)
	ExplorationID int64  // exploration id; 0 when unknown
	IntentID      int64  // worker's intent node; 0 for planner/mainagent/chat
	SessionID     string // chat conversation id; empty for task runs
	// TrafficSource identifies the proxy/store selected for this run, not merely
	// its owning task. Empty means global for older/non-worker callers.
	TrafficSource string // "global" or "task/<task registry id>"
}

// explorationID reads a store's exploration id, tolerating a nil store (planner and
// worker runs can be driven without one in tests).
func explorationID(ts *db.ExplorationStore) int64 {
	if ts == nil {
		return 0
	}
	return ts.ID()
}

type runInfoKey struct{}

// WithRunInfo attaches run attribution to ctx.
func WithRunInfo(ctx context.Context, ri RunInfo) context.Context {
	return context.WithValue(ctx, runInfoKey{}, ri)
}

// RunInfoFrom reads the RunInfo (zero value if none attached).
func RunInfoFrom(ctx context.Context) RunInfo {
	ri, _ := LookupRunInfo(ctx)
	return ri
}

// LookupRunInfo distinguishes an absent attribution from an explicitly empty
// one. A child run may attach RunInfo{} to clear the parent task; consumers must
// not treat that deliberate clear as permission to restore a captured parent.
func LookupRunInfo(ctx context.Context) (RunInfo, bool) {
	ri, ok := ctx.Value(runInfoKey{}).(RunInfo)
	return ri, ok
}
