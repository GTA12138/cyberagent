package agent

import (
	"bytes"
	"encoding/json"
	"sort"
	"unicode/utf8"
)

// Only expendable, read-only snapshots go through this renderer. The worker's
// assigned intent, operation constraints and tool messages must remain intact:
// cutting their serialized form can change the job or break tool-call pairing.
const (
	contextSnapshotTextRunes = 20_000
	contextSnapshotMaxBytes  = 64_000
	contextSnapshotFieldCap  = 1_200
	contextSnapshotNodeCap   = 4_096
	contextSnapshotItemCap   = 300
	contextSnapshotDepthCap  = 12
)

// marshalContextSnapshot projects a fresh JSON tree, so callers can safely reuse
// the source map. UseNumber matters: exploration/asset ids are int64, and a JSON
// round trip through float64 would silently change ids above 2^53.
//
// atomicStrings is for target assets: a clipped URL, credential or parameter
// must never look like a usable value. Oversized strings there are omitted in
// full; the worker can reload the original using list_assets(id/ids).
func marshalContextSnapshot(data any, atomicStrings bool) ([]byte, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var normalized any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&normalized); err != nil {
		return nil, err
	}
	textCap, nodeCap := contextSnapshotTextRunes, contextSnapshotNodeCap
	for {
		p := contextSnapshotProjection{remainingText: textCap, remainingNodes: nodeCap, atomicStrings: atomicStrings}
		view := p.project(normalized, "", 0)
		if p.truncated {
			meta := map[string]any{
				"truncated": true,
				"note":      "Read-only partial snapshot; omitted text/items are not negative evidence. Reload full records before acting on incomplete values or judging a goal.",
			}
			if m, ok := view.(map[string]any); ok {
				m["_context_snapshot"] = meta
			} else {
				view = map[string]any{"items": view, "_context_snapshot": meta}
			}
		}
		out, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		if len(out) <= contextSnapshotMaxBytes {
			return out, nil
		}
		// Quoting/control characters and large collections can exceed a text-only
		// estimate. Re-project from the original tree; never slice encoded JSON.
		textCap /= 2
		nodeCap /= 2
	}
}

type contextSnapshotProjection struct {
	remainingText  int
	remainingNodes int
	atomicStrings  bool
	truncated      bool
}

func (p *contextSnapshotProjection) project(value any, key string, depth int) any {
	if depth > contextSnapshotDepthCap || p.remainingNodes <= 0 {
		p.truncated = true
		return nil
	}
	p.remainingNodes--
	switch value := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for k := range value {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			pi, pj := contextSnapshotKeyPriority(keys[i]), contextSnapshotKeyPriority(keys[j])
			if pi != pj {
				return pi < pj
			}
			return keys[i] < keys[j]
		})
		out := make(map[string]any, len(keys))
		for i, k := range keys {
			if i >= contextSnapshotItemCap || p.remainingNodes <= 0 {
				p.truncated = true
				break
			}
			if len(k) > 128 { // arbitrary target-controlled object keys are text too
				p.truncated = true
				continue
			}
			out[k] = p.project(value[k], k, depth+1)
		}
		return out
	case []any:
		count := len(value)
		if count > contextSnapshotItemCap {
			count = contextSnapshotItemCap
			p.truncated = true
		}
		out := make([]any, 0, count)
		for _, item := range value[:count] {
			if p.remainingNodes <= 0 {
				p.truncated = true
				break
			}
			out = append(out, p.project(item, key, depth+1))
		}
		return out
	case string:
		// Small identity/state fields survive exhaustion of the prose budget.
		// This does not exempt arbitrary large strings claiming to be a state.
		if contextSnapshotKeyPriority(key) == 0 && utf8.RuneCountInString(value) <= 64 {
			return value
		}
		limit := min(contextSnapshotFieldCap, p.remainingText)
		if utf8.RuneCountInString(value) <= limit {
			p.remainingText -= utf8.RuneCountInString(value)
			return value
		}
		p.truncated = true
		if p.atomicStrings {
			return nil // do not create a plausible but invalid partial credential/URL
		}
		if limit <= 0 {
			return ""
		}
		// Stop at a rune boundary without allocating a rune copy of a huge body.
		end, count := 0, 0
		for i := range value {
			if count >= limit-1 {
				end = i
				break
			}
			count++
		}
		p.remainingText -= limit
		return value[:end] + "…"
	default:
		return value
	}
}

// Visit counters/identity and the local mandate before bulk historical prose.
// Sorting also makes projection independent of Go's map iteration order.
func contextSnapshotKeyPriority(key string) int {
	switch key {
	case "id", "state", "status", "confidence", "type", "intent_id", "task_id", "source_task_id", "inherited", "frontier_open", "done_intents_total", "findings_total", "facts", "coverage":
		return 0
	case "task", "goals", "running_intents", "open_intents":
		return 1
	case "hints", "finding_list", "recent_facts", "recent_done_intents":
		return 2
	default:
		return 3
	}
}
