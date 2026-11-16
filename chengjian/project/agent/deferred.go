package agent

import (
	"encoding/json"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// deferredSystem builds an agent's system-prompt segments and cache boundary from
// its DeferredInfo. When globally-available MCP tools are present, their names + a
// "prefer core tools" instruction render into a <available-deferred-tools> block
// placed as the LAST system-prompt segment, with DynamicBoundary set so the whole
// (session-fixed) system prompt — including the block — is cached (design doc
// §2.1 / C1). Skill-gated MCP names are NOT in this block; they surface when their
// skill loads. Returns a single plain segment + boundary 0 when there is no global
// block to add.
func deferredSystem(sysText string, def DeferredInfo) (system []string, boundary int) {
	sysText += def.FindingGuidance
	block := actool.RenderDeferredToolsBlock(def.GlobalNames)
	if block == "" {
		return []string{sysText}, 0
	}
	system = []string{sysText, block}
	boundary = len(system) // b >= len → whole system prompt cached (SDK guard)
	return system, boundary
}

// seedUnlockFromHistory replays successful Skill() invocations in the conversation so
// their skill-gated MCPs are re-unlocked on a resumed session (design doc C2). The
// main agent builds a fresh session each turn; its in-memory unlock set would
// otherwise reset, leaving the model able to see a skill-revealed tool name yet
// unable to call it. No-op when unlockSkill is nil (no deferred tools).
func seedUnlockFromHistory(msgs []llm.Message, unlockSkill func(string)) {
	if unlockSkill == nil {
		return
	}
	// The SDK retains the original tool call/result history across compaction.
	// A requested invocation alone is not proof of a load: it may have been
	// denied, failed or interrupted. Never unlock from user-authored skill tags.
	pending := make(map[string]string)
	unlocked := make(map[string]bool)
	for _, m := range msgs {
		for _, b := range m.Content {
			if m.Role == llm.RoleAssistant && b.Type == llm.BlockToolUse && b.ID != "" {
				if _, duplicate := pending[b.ID]; duplicate {
					pending[b.ID] = "" // ambiguous unresolved id: neither call is proof
					continue
				}
				pending[b.ID] = "" // track non-Skill calls too, to detect id collisions
				if b.Name == "Skill" {
					var in struct {
						Name string `json:"name"`
					}
					if json.Unmarshal(b.Input, &in) == nil && in.Name != "" {
						pending[b.ID] = in.Name
					}
				}
			}
			if m.Role == llm.RoleUser && b.Type == llm.BlockToolResult {
				name, ok := pending[b.ToolUseID]
				delete(pending, b.ToolUseID)
				if ok && name != "" && !b.IsError && !unlocked[name] {
					unlocked[name] = true
					unlockSkill(name)
				}
			}
		}
	}
}
