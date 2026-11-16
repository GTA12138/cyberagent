package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime/debug"

	actool "github.com/Autumn-27/norma/tool"
)

// DeferredInfo carries the deferred-tools wiring an agent needs to build its
// Options: the MCP tool names whose schemas are withheld, the subset listed in the
// global system-prompt block (non-skill-gated), and the shared session unlock set.
// UnlockSkill unlocks a named skill's MCPs — hosts call it to rebuild the unlock set
// from history on a resumed session (design doc C2).
type DeferredInfo struct {
	FindingGuidance string            // derived from the final permitted tools, including DB overrides
	Deferred        []string          // all MCP tool names (schema withheld)
	GlobalNames     []string          // MCP names to list in the system-prompt block
	Unlock          *actool.UnlockSet // shared call-gate; nil when no MCP tools
	UnlockSkill     func(skillName string)
}

// ToolAugment, if set, returns the EXTRA tools an agent should see beyond its
// built-in base set — the agent's visible skills (packed into one Skill meta-tool)
// and visible MCP servers (expanded to mcp__server__tool). It also returns the
// DeferredInfo describing how those MCP tools are deferred/gated. The server wires
// it to the PG agent_visibility table. cleanup releases any spawned MCP clients.
//
// When nil, agents run with only their built-in tools — behavior is unchanged
// until a user assigns a skill/MCP to the agent in the UI.

// ErrToolPolicyUnavailable prevents a run from starting with unverified tools.
var ErrToolPolicyUnavailable = errors.New("tool policy unavailable; execution suspended")

// AugmentTools returns permitted tools and a cleanup function to defer even on
// error. A host policy read failure exposes no tools and aborts the run.
func AugmentTools(ctx context.Context, agentKey string, base []actool.CoreTool) ([]actool.CoreTool, DeferredInfo, func(), error) {
	var (
		def     DeferredInfo
		cleanup = func() {}
		out     = base
	)
	if RuntimeFrom(ctx).ToolAugment != nil {
		var extra []actool.CoreTool
		var cl func()
		extra, def, cl = RuntimeFrom(ctx).ToolAugment(ctx, agentKey)
		if cl != nil {
			cleanup = cl
		}
		if len(extra) > 0 {
			out = append(append([]actool.CoreTool{}, base...), extra...)
		}
	}
	// DB tools table has the final say on the built-in tools: drop the ones this
	// agent isn't bound to (or that are disabled) and swap in overridden
	// descriptions/schemas + default injection. MCP/skill/host tools have no row
	// and pass through untouched, so deferred/unlock wiring stays consistent.
	if RuntimeFrom(ctx).ToolResolve != nil {
		var err error
		out, err = RuntimeFrom(ctx).ToolResolve(ctx, agentKey, out)
		if err != nil {
			if ctx.Err() != nil {
				return nil, DeferredInfo{}, cleanup, ctx.Err()
			}
			return nil, DeferredInfo{}, cleanup, fmt.Errorf("%w: %v", ErrToolPolicyUnavailable, err)
		}
	}
	out, def.FindingGuidance = findingWorkflowTools(ctx, agentKey, out)
	for i, t := range out {
		if t.Name() == "Bash" {
			t = DecorateTool(t, t.Description()+"\n"+actool.ExecutionEnvironment(), nil)
		}
		out[i] = guardPanic(t, RuntimeFrom(ctx))
	}
	return out, def, cleanup, nil
}

// guardPanic turns a panicking tool handler into an ordinary tool error. The
// harness runs each tool on its own goroutine, so a panic inside a handler can't
// be recovered by the caller that started the run — it takes the whole process
// down, and on restart the agent replays the same call and crashes again. Applied
// last, so it covers every tool the agent can reach: domain, SDK, MCP and skill.
func guardPanic(t actool.CoreTool, runtime ...Runtime) actool.CoreTool {
	g := &guardedTool{CoreTool: t}
	if len(runtime) > 0 {
		r := runtime[0]
		g.runtime = &r
	}
	return g
}

type guardedTool struct {
	actool.CoreTool
	runtime *Runtime
}

func (g *guardedTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (res actool.Result, err error) {
	// A host can invoke a retained tool with a fresh context. Keep its originating
	// runtime policy while preserving the caller's cancellation and attribution.
	if g.runtime != nil {
		ctx = WithRuntime(ctx, *g.runtime)
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[tools] %s panic: %v\n%s", g.Name(), r, debug.Stack())
			res, err = actool.Errorf(fmt.Sprintf("工具 %s 内部错误：%v（本次调用已失败，可换个参数或改用别的工具）", g.Name(), r)), nil
		}
	}()
	return callWithJournal(ctx, g.CoreTool, in, tc, RuntimeFrom(ctx).AttemptJournal)
}
