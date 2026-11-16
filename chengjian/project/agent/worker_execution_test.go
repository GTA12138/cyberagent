package agent

import (
	"context"
	"errors"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"iter"
	"strings"
	"testing"
)

type fakeExecutionSession struct {
	resumeErr       error
	messages        []llm.Message
	input           string
	resumed, closed bool
	missingTerminal bool
}

func (s *fakeExecutionSession) Messages() []llm.Message { return s.messages }
func (s *fakeExecutionSession) Resume(string) error     { s.resumed = true; return s.resumeErr }
func (s *fakeExecutionSession) Close() error            { s.closed = true; return nil }
func (s *fakeExecutionSession) Prompt(_ context.Context, input string) iter.Seq2[harness.Event, error] {
	s.input = input
	return func(yield func(harness.Event, error) bool) {
		if !s.missingTerminal {
			yield(harness.Event{Kind: harness.KindResult, Terminal: &harness.Terminal{Reason: harness.ReasonCompleted}}, nil)
		}
	}
}
func TestWorkerExecutionWithoutDatabase(t *testing.T) {
	session := &fakeExecutionSession{messages: []llm.Message{{Role: llm.RoleUser}}}
	_, err := RunWorkerExecution(t.Context(), WorkerExecutionRequest{Input: "initial", Resume: true, IntentID: 42}, func(agentcore.Options) WorkerExecutionSession { return session })
	if err != nil || !session.closed || !session.resumed || !strings.Contains(session.input, "#42") {
		t.Fatalf("execution failed: %+v %v", session, err)
	}
}

func TestWorkerExecutionRefusesBrokenTranscript(t *testing.T) {
	session := &fakeExecutionSession{resumeErr: errors.New("storage unavailable")}
	_, err := RunWorkerExecution(t.Context(), WorkerExecutionRequest{Input: "initial", Resume: true}, func(agentcore.Options) WorkerExecutionSession { return session })
	if err == nil || session.input != "" || !session.closed {
		t.Fatalf("unsafe restart: %+v %v", session, err)
	}
}
