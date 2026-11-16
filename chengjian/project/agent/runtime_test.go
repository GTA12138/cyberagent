package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func TestRuntimeIsolation(t *testing.T) {
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runtime := Runtime{
				PromptOverride:                 func(string) (string, bool) { return name, true },
				WrapupOverride:                 func(string) (string, bool) { return name + " wrapup", true },
				WrapupMaxTurnsOverride:         func(string) (int, bool) { return len(name), true },
				WrapupTaskTimeoutOverride:      func(string) (string, bool) { return name + " timeout", true },
				WrapupTaskTimeoutTurnsOverride: func(string) (int, bool) { return len(name) + 1, true },
				FindingTrafficBindingEnabled:   func() bool { return name == "first" },
			}
			runtime.ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
				return []actool.CoreTool{readTool(name, name, obj(nil), func(ctx context.Context, _ json.RawMessage) (actool.Result, error) {
					return actool.Text(fmt.Sprintf("%s:%d", renderSystem(ctx, "custom", "default", nil), RunInfoFrom(ctx).TaskID)), nil
				})}, DeferredInfo{}, nil
			}
			runtime.ToolResolve = func(_ context.Context, _ string, tools []actool.CoreTool) ([]actool.CoreTool, error) {
				if len(tools) != 1 || tools[0].Name() != name {
					t.Error("crossed host tool registry")
				}
				return tools, nil
			}
			ctx := WithRuntime(t.Context(), runtime)
			// Later writes to the construction value must not change this run.
			runtime.PromptOverride = func(string) (string, bool) { return "replacement", true }
			for range 40 {
				if got := renderSystem(ctx, "custom", "default", nil); got != name {
					t.Fatal(got)
				}
				if got := resolveWrapup(ctx, "custom"); got != name+" wrapup" {
					t.Fatal(got)
				}
				if got := resolveTaskTimeoutWrapup(ctx, "worker"); got != name+" timeout" {
					t.Fatal(got)
				}
				if resolveWrapupTurns(ctx, "custom") != len(name) || resolveTaskTimeoutTurns(ctx, "worker") != len(name)+1 {
					t.Fatal("crossed settlement budget")
				}
				if findingTrafficBindingEnabled(ctx) != (name == "first") {
					t.Fatal("crossed evidence policy")
				}
				tools, _, cleanup, policyErr1 := AugmentTools(ctx, "custom", nil)
				if policyErr1 != nil {
					t.Fatal(policyErr1)
				}
				callCtx := WithRunInfo(t.Context(), RunInfo{TaskID: 17})
				result, err := tools[0].Call(callCtx, json.RawMessage(`{}`), nil)
				cleanup()
				if err != nil || result.Flatten() != name+":17" {
					t.Fatal(result, err)
				}
			}
		})
	}
}

func TestEmptyRuntimeClearsInheritedHost(t *testing.T) {
	parent := WithRuntime(t.Context(), Runtime{PromptOverride: func(string) (string, bool) { return "parent", true }})
	ctx := WithRuntime(parent, Runtime{})
	if got := chatSystem(ctx, "custom", "/data", "/data/session"); strings.HasPrefix(got, "parent") {
		t.Fatal("empty runtime inherited host callbacks")
	}
	if got := renderSystem(t.Context(), "custom", "standalone", nil); got != "standalone" {
		t.Fatal(got)
	}
}
