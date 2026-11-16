package agent

import (
	"context"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// Runtime contains host dependencies for one server instance. Configure it before
// publishing runners; WithRuntime takes a value snapshot, so a second server can
// never replace the first server's database, tool policy or prompt callbacks.
// Callbacks may read live configuration, but must themselves be safe concurrently.
type Runtime struct {
	AttemptJournal                 AttemptJournal
	ModelProvider                  func(context.Context, llm.Provider) llm.Provider
	ToolAugment                    func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func())
	ToolResolve                    func(context.Context, string, []actool.CoreTool) ([]actool.CoreTool, error)
	PromptOverride                 func(string) (string, bool)
	WrapupOverride                 func(string) (string, bool)
	WrapupMaxTurnsOverride         func(string) (int, bool)
	WrapupTaskTimeoutOverride      func(string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(string) (int, bool)
	FindingTrafficBindingEnabled   func() bool
}

type runtimeKey struct{}

func WithRuntime(ctx context.Context, runtime Runtime) context.Context {
	return context.WithValue(ctx, runtimeKey{}, runtime)
}

// RuntimeFrom returns an empty standalone runtime when no host is attached.
func RuntimeFrom(ctx context.Context) Runtime {
	runtime, _ := ctx.Value(runtimeKey{}).(Runtime)
	return runtime
}
