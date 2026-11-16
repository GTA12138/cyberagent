package traffic

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	actool "github.com/Autumn-27/norma/tool"
)

// ParseReference makes a capture ID unambiguous across independently recorded
// stores. Raw IDs remain accepted against an explicit caller-selected default;
// they never trigger a search through other tasks or a fallback to global.
func ParseReference(id, defaultSource string) (source, raw string, err error) {
	id = strings.TrimSpace(id)
	source, raw = defaultSource, id
	parts := strings.Split(id, "/")
	if len(parts) > 1 {
		switch {
		case len(parts) == 2 && parts[0] == "global":
			source, raw = "global", parts[1]
		case len(parts) == 3 && parts[0] == "task":
			source, raw = "task/"+parts[1], parts[2]
		default:
			return "", "", fmt.Errorf("无效流量 ID：请原样使用 traffic_search 返回的 id")
		}
	}
	if err := ValidateSource(source); err != nil {
		return "", "", err
	}
	if raw == "" || raw == "." || raw == ".." || strings.ContainsAny(raw, "/\\\x00") {
		return "", "", fmt.Errorf("无效流量 ID")
	}
	return source, raw, nil
}

func ValidateSource(source string) error {
	if source == "global" {
		return nil
	}
	if strings.HasPrefix(source, "task/") {
		id := strings.TrimPrefix(source, "task/")
		if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > 0 && strconv.FormatInt(n, 10) == id {
			return nil
		}
	}
	return fmt.Errorf("无效流量源：应为 global 或 task/<正整数>")
}

// LookupSource leases an existing capture only for the duration of a read. A
// resolver must check ownership before returning it; Name is canonical.
type LookupSource struct {
	Name string
	With func(context.Context, func(*Traffic) error) error
}

type LookupResolver func(ctx context.Context, requestedSource string) (LookupSource, error)

// ScopedTools retains the existing tools and output shape while qualifying IDs
// and resolving the store on every call. No live DB/proxy pointer is captured by
// a long-running or resumed model conversation.
func ScopedTools(resolve LookupResolver) []actool.CoreTool {
	// Recover only exact aliases of references actually issued to this caller.
	// Never search stores by a suffix: an unseen/ambiguous alias is an error.
	var issuedMu sync.Mutex
	issued := make(map[string]map[string]string)
	remember := func(owner, canonical, raw string) {
		if legacyReference(raw) {
			return // preserve the historical meaning of unqualified numeric IDs
		}
		issuedMu.Lock()
		defer issuedMu.Unlock()
		m := issued[owner]
		if len(m) > 8192 || m == nil {
			m = make(map[string]string)
			issued[owner] = m
		}
		keys := []string{raw}
		if pos := strings.LastIndexByte(raw, '-'); pos >= 0 {
			keys = append(keys, raw[pos+1:])
		}
		for _, key := range keys {
			if old, ok := m[key]; ok && old != canonical {
				m[key] = "" // ambiguous forever within this cache generation
			} else if !ok {
				m[key] = canonical
			}
		}
	}
	bases := (&Traffic{}).Tools()
	out := make([]actool.CoreTool, 0, len(bases))
	for i, base := range bases {
		schema := base.InputSchema()
		props := schema["properties"].(map[string]any)
		if base.Name() != "traffic_get" {
			props["source"] = map[string]any{"type": "string", "description": "可选流量源，默认当前任务；历史全局流量显式使用 global。分段读取时沿用 traffic_get 返回的 source。"}
		}
		out = append(out, actool.Build(actool.Spec{
			Name: base.Name(), Description: base.Description() + " 流量 ID 包含来源，请完整保留；不会跨任务猜测或回落。",
			Prompt: base.Prompt(), Schema: schema, ReadOnly: base.IsReadOnly,
			Concurrent: base.IsConcurrencySafe, Permissions: base.CheckPermissions,
			Run: func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
				if err := ctx.Err(); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				var args map[string]json.RawMessage
				if err := json.Unmarshal(in, &args); err != nil {
					return actool.Errorf("流量查询参数无效"), nil
				}
				owner, err := resolve(ctx, "")
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if ValidateSource(owner.Name) != nil || owner.With == nil {
					return actool.Errorf("当前任务流量源不可用"), nil
				}
				var requested string
				if v := args["source"]; v != nil {
					if err := json.Unmarshal(v, &requested); err != nil {
						return actool.Errorf("source 必须为字符串"), nil
					}
				}
				var id, repaired string
				if base.Name() == "traffic_get" {
					if err := json.Unmarshal(args["id"], &id); err != nil {
						return actool.Errorf("id 必须为字符串"), nil
					}
					id = strings.TrimSpace(id)
					if !strings.Contains(id, "/") {
						issuedMu.Lock()
						full, known := issued[owner.Name][id]
						issuedMu.Unlock()
						if known && full != "" {
							repaired, id = id, full
						} else if known || !legacyReference(id) {
							return actool.Errorf("invalid_reference：流量编号不完整、未由当前查询返回或存在歧义。请重新 traffic_search，并将完整 id 原样传给 traffic_get；不会跨任务猜测编号。"), nil
						}
					}
					requested, id, err = ParseReference(id, "global")
					if err != nil {
						return actool.Errorf(err.Error()), nil
					}
				}
				// Old transcripts contain unqualified blob hashes from the global
				// recorder. New task reads include source explicitly in the result.
				if base.Name() == "traffic_blob" && requested == "" {
					requested = "global"
				}
				source, err := resolve(ctx, requested)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if err = ValidateSource(source.Name); err != nil || source.With == nil {
					return actool.Errorf("流量源不可用"), nil
				}
				if base.Name() == "traffic_get" {
					_, id, err = ParseReference(id, source.Name)
					if err != nil {
						return actool.Errorf(err.Error()), nil
					}
					args["id"], _ = json.Marshal(id)
				}
				delete(args, "source")
				input, _ := json.Marshal(args)
				var result actool.Result
				err = source.With(ctx, func(tr *Traffic) error {
					if tr == nil {
						return fmt.Errorf("流量源不可用")
					}
					var err error
					result, err = tr.Tools()[i].Call(ctx, input, tc)
					return err
				})
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if result.IsError {
					if base.Name() == "traffic_get" && strings.Contains(result.Flatten(), "no rows") {
						return actool.Errorf("reference_not_found：该来源中不存在此流量。请重新 traffic_search 获取完整 id；这不代表目标没有响应。"), nil
					}
					return result, nil
				}
				if base.Name() == "traffic_search" {
					var rows []map[string]any
					if json.Unmarshal([]byte(result.Flatten()), &rows) == nil {
						for _, row := range rows {
							raw := fmt.Sprint(row["id"])
							canonical := source.Name + "/" + raw
							remember(owner.Name, canonical, raw)
							row["id"] = canonical
						}
						b, _ := json.Marshal(rows)
						return actool.Text(string(b)), nil
					}
					return result, nil
				}
				prefix := "[source=" + source.Name + "]\n"
				if base.Name() == "traffic_get" {
					prefix += "[id=" + source.Name + "/" + id + "]\n"
					if repaired != "" {
						prefix += "[已根据本次搜索返回值精确恢复截短编号；后续引用请使用上方完整 id]\n"
					}
				}
				return actool.Text(prefix + result.Flatten()), nil
			},
		}))
	}
	return out
}

// Historic captures used numeric IDs or Unix-seconds plus a four-digit counter.
// New opaque nonces without their source are never silently treated as global.
func legacyReference(id string) bool {
	parts := strings.Split(id, "-")
	if len(parts) > 2 || len(parts) == 2 && len(parts[1]) != 4 || id == "" {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}
