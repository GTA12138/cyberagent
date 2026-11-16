package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/Autumn-27/norma/harness"
)

func TestSessionWithoutTerminalIsIncompleteAndNotRetryable(t *testing.T) {
	_, reason, err := captureRunSession(context.Background(), &fakeExecutionSession{missingTerminal: true}, "fixture", nil)
	var policy interface{ AllowModelFailover() bool }
	if reason != harness.ReasonModelError || !errors.As(err, &policy) || policy.AllowModelFailover() {
		t.Fatalf("missing terminal treated as success/retryable: reason=%s err=%v", reason, err)
	}
}
