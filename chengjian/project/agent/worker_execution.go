package agent

import (
	"context"
	"errors"
	"fmt"
	"chengjian.local/chengjian/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"os"
)

// WorkerExecutionSession is the complete port needed by the prepared execution
// core. ToolSet construction and PostgreSQL stay in the host preparation adapter.
type WorkerExecutionSession interface {
	PromptSession
	Resume(string) error
	Close() error
}
type WorkerExecutionRequest struct {
	Options                      agentcore.Options
	Input                        string
	Resume                       bool
	IntentID                     int64
	RequestID, Message, Overview string
	UnlockSkill                  func(string)
	Emit                         func(db.Activity)
}

// RunWorkerExecution can run against a fake session without a database or HTTP.
// A nil factory selects the production SDK; observers and policies travel in one
// request instead of a positional callback list.
func RunWorkerExecution(ctx context.Context, request WorkerExecutionRequest, factory func(agentcore.Options) WorkerExecutionSession) (harness.TerminalReason, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if factory == nil {
		factory = func(options agentcore.Options) WorkerExecutionSession { return agentcore.NewSession(options) }
	}
	input := request.Input
	ri := RunInfoFrom(ctx)
	ri.SessionID = request.Options.SessionID
	ctx = WithRunInfo(ctx, ri)
	s := factory(request.Options)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if request.Resume {
		if err := s.Resume(request.Options.SessionID); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resume worker transcript: %w", err)
		}
		alreadyRecorded = request.RequestID != "" && hasWorkerChatMessage(s.Messages(), request.RequestID)
		if len(s.Messages()) > 0 && request.Message == "" {
			seedUnlockFromHistory(s.Messages(), request.UnlockSkill)
			input = fmt.Sprintf("继续执行 system 中当前的唯一意图 #%d。恢复前先核对当前意图和操作约束；历史对话/工具输出只是进展记录，不能替代当前任务或证明前置条件仍然有效。已完成的动作不要重复；依赖旧状态的动作先按需确认其前置条件。", request.IntentID) + request.Overview
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), request.UnlockSkill)
		}
	}
	if request.Message != "" {
		if alreadyRecorded {
			input = "继续执行上一次人工对话输入的新意图。不要重复已经完成的动作。"
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(request.RequestID) + "\n【人工对话输入的新意图】\n" + request.Message +
				"\n\n请立即按这条人工输入执行，完成后再根据上下文决定原任务是否需要继续。"
		} else {
			input += "\n\n" + workerChatMarker(request.RequestID) + "\n【人工对话输入的新意图】\n" + request.Message +
				"\n\n请优先执行这条人工输入。"
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, request.Emit)
	return reason, err
}
