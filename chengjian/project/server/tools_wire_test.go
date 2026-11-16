package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
	actool "github.com/Autumn-27/norma/tool"
)

func names(tools []actool.CoreTool) map[string]actool.CoreTool {
	m := map[string]actool.CoreTool{}
	for _, t := range tools {
		m[t.Name()] = t
	}
	return m
}

// TestWireTools verifies end-to-end against the configured test PG: wireTools seeds the
// catalog, ToolResolve keeps record_fact for worker but drops it for planner (not
// bound), and an edited description + injected default flow through.
func TestWireTools(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer pg.Close()

	resolver := wireTools(pg, nil)
	resolve := func(ctx context.Context, key string, base []actool.CoreTool) []actool.CoreTool {
		tools, err := resolver(ctx, key, base)
		if err != nil {
			t.Fatal(err)
		}
		return tools
	} // nil domainReg: test only covers filter/decoration, not injection
	// Prevent interactive-shell Bash decoration: if the worker agent has
	// interactive_shell=true in the DB, ToolResolve would append a note to
	// Bash's description. Disable that for this test to isolate the separate,
	// unconditional worker UA note from the optional interactive-shell note.
	t.Setenv("AGENT_CORE_DISABLE_INTERACTIVE_SHELL", "1")

	// Seeding populated the catalog.
	rf, err := pg.GetTool("record_fact")
	if err != nil || rf == nil {
		t.Fatalf("record_fact not seeded: %v", err)
	}

	// Build a base as the worker does: its domain tools + defaults, resolved for
	// "worker" then for "planner".
	ts := agent.NewToolSet(nil, "")
	base := append(ts.WorkerTools(), actool.DefaultTools()...)
	ctx := context.Background()

	worker := names(resolve(ctx, "worker", base))
	if _, ok := worker["record_fact"]; !ok {
		t.Error("worker lost record_fact")
	}
	// SDK generic tools aren't catalog-filtered. The worker's Bash still receives
	// the existing mandatory UA guidance, even when interactive shell is disabled.
	if bash, ok := worker["Bash"]; !ok {
		t.Error("worker lost Bash (should pass through)")
	} else if want := actool.NewBash().Description() + expectedBashUANote(); bash.Description() != want {
		t.Errorf("worker Bash description = %q, want SDK description plus mandatory worker UA guidance %q", bash.Description(), want)
	}
	// planner is not bound to record_fact → resolving a base that contains it drops it.
	planner := names(resolve(ctx, "planner", base))
	if _, ok := planner["record_fact"]; ok {
		t.Error("planner should not get record_fact (not bound)")
	}
	if bash, ok := planner["Bash"]; !ok {
		t.Error("planner lost Bash")
	} else if bash.Description() != actool.NewBash().Description() {
		t.Error("planner Bash must not receive worker-only UA guidance")
	}

	// Edit description + add a default to a scalar param, then confirm the resolved
	// worker tool reflects both. Restore from code default on cleanup.
	t.Cleanup(func() { _ = pg.UpsertToolForce(rf.Key, rf.Description, rf.Schema, mustJSON(rf.Agents)) })
	var schema map[string]any
	_ = json.Unmarshal(rf.Schema, &schema)
	if props, ok := schema["properties"].(map[string]any); ok {
		if conf, ok := props["confidence"].(map[string]any); ok {
			conf["default"] = "inferred"
		}
	}
	edited := mustJSON(schema)
	if err := pg.UpdateTool(rf.Key, "EDITED DESC", edited, mustJSON([]string{"worker"}), true); err != nil {
		t.Fatal(err)
	}

	worker2 := names(resolve(ctx, "worker", base))
	got := worker2["record_fact"]
	if got == nil {
		t.Fatal("record_fact missing after edit")
	}
	if got.Description() != "EDITED DESC" {
		t.Errorf("description = %q, want EDITED DESC", got.Description())
	}
	// Default injection: omit confidence → handler input should carry the default.
	// We can't run the real handler (nil store), but InputSchema must show the default.
	sc := got.InputSchema()
	props := sc["properties"].(map[string]any)
	conf := props["confidence"].(map[string]any)
	if conf["default"] != "inferred" {
		t.Errorf("confidence.default = %v, want inferred", conf["default"])
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func TestWireToolsPolicyReadFailure(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	resolve := wireTools(pg, nil)
	// A cancelled read is a policy failure, even with a broad incoming SDK set.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tools, err := resolve(ctx, "worker", actool.DefaultTools())
	if err == nil || len(tools) != 0 {
		t.Fatal("cancelled policy read failed open", len(tools), err)
	}
	if err = pg.Close(); err != nil {
		t.Fatal(err)
	}
	tools, err = resolve(t.Context(), "worker", actool.DefaultTools())
	if err == nil || len(tools) != 0 {
		t.Fatal("closed database failed open", len(tools), err)
	}
	_, _, cleanup, err := agent.AugmentTools(agent.WithRuntime(t.Context(), agent.Runtime{ToolResolve: resolve}), "worker", actool.DefaultTools())
	defer cleanup()
	if !errors.Is(err, agent.ErrToolPolicyUnavailable) {
		t.Fatalf("policy error not propagated: %v", err)
	}
}
