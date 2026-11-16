package llm

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The normalized event API has sequential tool inputs. Wire protocols may
// interleave argument deltas by index/item ID, so assemble them before emitting
// tool blocks. Otherwise later chunks get attached to the last-started tool.
type toolStream struct {
	calls   map[int]*streamCall
	aliases map[string]int
}
type streamCall struct {
	id, name string
	args     strings.Builder
}

func (s *toolStream) add(index int, id, name, delta string) error {
	if s.calls == nil {
		s.calls = map[int]*streamCall{}
		s.aliases = map[string]int{}
	}
	if index < 0 || index >= 128 {
		return fmt.Errorf("llm: invalid tool stream index")
	}
	c := s.calls[index]
	if c == nil {
		c = &streamCall{}
		s.calls[index] = c
	}
	if id != "" {
		if c.id != "" && c.id != id {
			return fmt.Errorf("llm: tool stream identity changed")
		}
		c.id = id
	}
	if name != "" && name != c.name {
		c.name += name
	}
	if c.args.Len()+len(delta) > maxSSEEventBytes {
		return ErrResponseLimit
	}
	c.args.WriteString(delta)
	return nil
}
func (s *toolStream) events() ([]StreamEvent, error) {
	keys := make([]int, 0, len(s.calls))
	for i := range s.calls {
		keys = append(keys, i)
	}
	sort.Ints(keys)
	events := make([]StreamEvent, 0, len(keys)*2)
	seen := map[string]bool{}
	for _, i := range keys {
		c := s.calls[i]
		raw := strings.TrimSpace(c.args.String())
		if raw == "" {
			raw = "{}"
		}
		if c.id == "" || c.name == "" || seen[c.id] || raw[0] != '{' || !json.Valid([]byte(raw)) {
			return nil, fmt.Errorf("llm: incomplete or invalid streamed tool call; no buffered tools executed")
		}
		seen[c.id] = true
		events = append(events, StreamEvent{Type: SEToolUseStart, ToolID: c.id, ToolName: c.name}, StreamEvent{Type: SEToolInputJSON, Text: raw})
	}
	return events, nil
}

func (s *toolStream) responsesFrame(event, data string) (bool, error) {
	var f struct {
		Type        string `json:"type"`
		OutputIndex int    `json:"output_index"`
		ItemID      string `json:"item_id"`
		Delta       string `json:"delta"`
		Arguments   string `json:"arguments"`
		Item        struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return false, fmt.Errorf("responses: invalid stream frame: %w", err)
	}
	if event == "" {
		event = f.Type
	}
	index := f.OutputIndex
	if f.ItemID != "" {
		if n, ok := s.aliases[f.ItemID]; ok {
			index = n
		}
	}
	switch event {
	case "response.output_item.added":
		if f.Item.Type != "function_call" {
			return false, nil
		}
		if err := s.add(index, f.Item.CallID, f.Item.Name, f.Item.Arguments); err != nil {
			return true, err
		}
		if f.Item.ID != "" {
			s.aliases[f.Item.ID] = index
		}
		return true, nil
	case "response.function_call_arguments.delta":
		return true, s.add(index, "", "", f.Delta)
	case "response.function_call_arguments.done":
		if c := s.calls[index]; c == nil || c.args.Len() == 0 {
			return true, s.add(index, "", "", f.Arguments)
		}
		return true, nil
	}
	return false, nil
}
