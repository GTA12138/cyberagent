package agent

import (
	"context"
	"errors"
	actool "github.com/Autumn-27/norma/tool"
	"testing"
)

func TestToolPolicyFailureStopsAssemblyAndCleansUp(t *testing.T) {
	cleaned := false
	runtime := Runtime{
		ToolAugment: func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
			return actool.DefaultTools(), DeferredInfo{}, func() { cleaned = true }
		},
		ToolResolve: func(context.Context, string, []actool.CoreTool) ([]actool.CoreTool, error) {
			return actool.DefaultTools(), errors.New("database offline")
		},
	}
	tools, def, cleanup, err := AugmentTools(WithRuntime(t.Context(), runtime), "worker", actool.DefaultTools())
	cleanup()
	if !errors.Is(err, ErrToolPolicyUnavailable) || len(tools) != 0 || len(def.Deferred) != 0 || !cleaned {
		t.Fatalf("policy failed open: %d %v %v", len(tools), cleaned, err)
	}
}

func TestToolPolicyCancellationKeepsCallerCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runtime := Runtime{ToolResolve: func(ctx context.Context, _ string, _ []actool.CoreTool) ([]actool.CoreTool, error) {
		return nil, ctx.Err()
	}}
	tools, _, cleanup, err := AugmentTools(WithRuntime(ctx, runtime), "worker", actool.DefaultTools())
	defer cleanup()
	if len(tools) != 0 || !errors.Is(err, context.Canceled) || errors.Is(err, ErrToolPolicyUnavailable) {
		t.Fatalf("caller cancellation became policy failure: %v", err)
	}
}
