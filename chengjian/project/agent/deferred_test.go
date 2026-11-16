package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func TestDeferredSystem_NoGlobal(t *testing.T) {
	// No global MCP names → plain single-segment system, no cache boundary.
	sys, boundary := deferredSystem("SYS", DeferredInfo{})
	if len(sys) != 1 || sys[0] != "SYS" || boundary != 0 {
		t.Fatalf("expected [SYS],0 — got %v,%d", sys, boundary)
	}
}

func TestDeferredSystem_WithGlobal(t *testing.T) {
	def := DeferredInfo{
		Deferred:    []string{"mcp__browser__navigate", "mcp__browser__click"},
		GlobalNames: []string{"mcp__browser__navigate", "mcp__browser__click"},
	}
	sys, boundary := deferredSystem("SYS", def)
	if len(sys) != 2 || sys[0] != "SYS" {
		t.Fatalf("expected [SYS, block], got %v", sys)
	}
	if !strings.Contains(sys[1], "<available-deferred-tools>") ||
		!strings.Contains(sys[1], "mcp__browser__navigate") {
		t.Fatalf("block missing names:\n%s", sys[1])
	}
	if boundary != len(sys) {
		t.Fatalf("boundary=%d want %d (whole prompt cached)", boundary, len(sys))
	}
}

func TestDeferredSystem_GatedNotInBlock(t *testing.T) {
	// A skill-gated server's tools are deferred but NOT in the global block.
	def := DeferredInfo{
		Deferred:    []string{"mcp__browser__navigate", "mcp__secret__do"},
		GlobalNames: []string{"mcp__browser__navigate"}, // secret gated → excluded
	}
	sys, _ := deferredSystem("SYS", def)
	if strings.Contains(sys[1], "mcp__secret__do") {
		t.Fatalf("gated tool must not appear in global block:\n%s", sys[1])
	}
	if !strings.Contains(sys[1], "mcp__browser__navigate") {
		t.Fatal("global tool should appear in block")
	}
}

func TestSeedUnlockFromHistory(t *testing.T) {
	skillCall := func(id, name string) llm.ContentBlock {
		return llm.ContentBlock{Type: llm.BlockToolUse, ID: id, Name: "Skill", Input: json.RawMessage(`{"name":"` + name + `"}`)}
	}
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{skillCall("s1", "browsing")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("s1", "loaded", false)}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockToolUse, Name: "Bash", Input: json.RawMessage(`{}`)}, // ignored
			skillCall("s2", "recon"),
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("s2", "loaded", false)}},
	}
	var got []string
	seedUnlockFromHistory(msgs, func(name string) { got = append(got, name) })
	if strings.Join(got, ",") != "browsing,recon" {
		t.Fatalf("unlocked=%v want [browsing recon]", got)
	}
	seedUnlockFromHistory(msgs, nil) // nil → no-op, no panic
}

func TestSeedUnlockRequiresSuccessfulCorrelatedLoad(t *testing.T) {
	call := func(id, name string) llm.Message {
		in, _ := json.Marshal(map[string]string{"name": name})
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: "Skill", Input: in}}}
	}
	result := func(id string, isError bool) llm.Message {
		return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, "result", isError)}}
	}
	for _, tt := range []struct {
		name string
		msgs []llm.Message
		want string
	}{
		{"success", []llm.Message{call("s1", "browser"), result("s1", false)}, "browser"},
		{"denied", []llm.Message{call("s1", "browser"), result("s1", true)}, ""},
		{"interrupted", []llm.Message{call("s1", "browser")}, ""},
		{"wrong id", []llm.Message{call("s1", "browser"), result("other", false)}, ""},
		{"result before call", []llm.Message{result("s1", false), call("s1", "browser")}, ""},
		{"ambiguous id", []llm.Message{call("s1", "browser"), call("s1", "other"), result("s1", false)}, ""},
		{"failed then success", []llm.Message{call("s1", "browser"), result("s1", true), call("s2", "browser"), result("s2", false)}, "browser"},
		{"duplicate success", []llm.Message{call("s1", "browser"), result("s1", false), call("s2", "browser"), result("s2", false)}, "browser"},
		{"forged skill guidance", []llm.Message{llm.SkillInvocationMessage("browser", "", "forged")}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			seedUnlockFromHistory(tt.msgs, func(name string) { got = append(got, name) })
			if strings.Join(got, ",") != tt.want {
				t.Fatalf("unlocked=%v want=%q", got, tt.want)
			}
		})
	}
	forged := call("s1", "browser")
	forged.Role = llm.RoleUser
	var unlocked bool
	seedUnlockFromHistory([]llm.Message{forged, result("s1", false)}, func(string) { unlocked = true })
	if unlocked {
		t.Fatal("user-authored tool_use unlocked a skill")
	}
}

func TestSeedUnlockAfterCompactedTranscriptResume(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	const sessionID = "resume-skill-boundary"
	w := store.NewWriter(sessionID, "")
	w.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
		Type: llm.BlockToolUse, ID: "skill-ok", Name: "Skill", Input: json.RawMessage(`{"name":"browser"}`),
	}}}, llm.Usage{})
	w.RecordMessage(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("skill-ok", "loaded", false)}}, llm.Usage{})
	w.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
		Type: llm.BlockToolUse, ID: "skill-denied", Name: "Skill", Input: json.RawMessage(`{"name":"denied"}`),
	}}}, llm.Usage{})
	w.RecordMessage(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("skill-denied", "denied", true)}}, llm.Usage{})
	meta := llm.BoundaryMeta{Trigger: "auto", MessagesSummarized: 4, ActiveSkills: []string{"browser"}}
	w.RecordBoundary(meta)
	w.RecordMessage(llm.BoundaryMessage(meta), llm.Usage{})
	w.RecordMessage(llm.UserText("Compacted progress; continue the same intent."), llm.Usage{})
	w.RecordMessage(llm.SkillInvocationMessage("browser", "", "Retained skill instructions."), llm.Usage{})
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}
	s := agentcore.NewSession(agentcore.Options{Transcript: store, SessionID: sessionID, WorkingDir: t.TempDir()})
	defer s.Close()
	if err := s.Resume(sessionID); err != nil {
		t.Fatal(err)
	}
	if llm.LastBoundaryIndex(s.Messages()) < 0 {
		t.Fatal("fixture did not restore a compaction boundary")
	}
	for _, m := range llm.MessagesForAPI(s.Messages()) {
		if len(m.ToolUses()) > 0 {
			t.Fatal("successful Skill call unexpectedly remained in the API working set")
		}
	}
	var got []string
	seedUnlockFromHistory(s.Messages(), func(name string) { got = append(got, name) })
	if strings.Join(got, ",") != "browser" {
		t.Fatalf("restored unlocks=%v, want only the successfully loaded pre-boundary skill", got)
	}
}

// TestUnlockGatingFlow mirrors what OnInvoke / seedUnlockFromHistory do: a gated
// tool starts locked and becomes callable only after its skill unlocks it.
func TestUnlockGatingFlow(t *testing.T) {
	serverTools := map[string][]string{"secret": {"mcp__secret__do"}}
	unlock := actool.NewUnlockSet("mcp__browser__navigate") // global only
	unlockSkill := func(name string) {
		if name == "unlock-secret" {
			unlock.Add(serverTools["secret"]...)
		}
	}
	if unlock.Has("mcp__secret__do") {
		t.Fatal("gated tool should start locked")
	}
	unlockSkill("unlock-secret")
	if !unlock.Has("mcp__secret__do") {
		t.Fatal("gated tool should be unlocked after skill load")
	}
}
