package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"chengjian.local/chengjian/attackchain"
)

// findingAttackChain is a read-only reconstruction from existing database rows.
// Node references are resolved only inside the finding's originating exploration.
func (s *Server) findingAttackChain(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	if contextID := strings.TrimSpace(r.URL.Query().Get("context_task")); contextID != "" {
		t := s.m.ResolveTask(contextID)
		if _, _, allowed := findingProvenanceInTask(t, f.TaskID); !allowed {
			writeErr(w, 404, "finding not available in task context")
			return
		}
	}
	related := []attackchain.Related{}
	extra := []attackchain.Source{}
	warnings := []string{}
	if f.TaskID != nil {
		task, err := s.m.pg.GetTask(*f.TaskID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if task != nil {
			store := s.m.pg.Exploration(task.ExplorationID)
			refs := attackchain.References(f.Summary + "\n" + f.Evidence + "\n" + f.Report)
			if len(refs) > 64 {
				refs = refs[:64]
				warnings = append(warnings, "引用记录超过 64 项，其余请查阅原始报告。")
			}
			for _, ref := range refs {
				n, err := store.GetNode(ref.ID)
				if err != nil {
					writeErr(w, 500, err.Error())
					return
				}
				if n == nil || n.Kind != ref.Kind {
					warnings = append(warnings, fmt.Sprintf("%s%d 未匹配到同一任务中的对应类型记录。", ref.Kind, ref.ID))
					continue
				}
				if f.NodeID != nil && n.ID == *f.NodeID {
					continue
				}
				var payload map[string]any
				if err := json.Unmarshal(n.Payload, &payload); err != nil {
					warnings = append(warnings, fmt.Sprintf("探索节点 %d 的内容无法解析。", n.ID))
					continue
				}
				value := func(key string) string { v, _ := payload[key].(string); return v }
				title := value("name")
				if title == "" {
					title = value("summary")
				}
				if title == "" {
					title = value("description")
				}
				if title == "" {
					title = fmt.Sprintf("%s%d", ref.Kind, ref.ID)
				}
				entry := attackchain.Related{Reference: ref, Title: title, Summary: value("summary"), Detail: string(n.Payload)}
				if ref.Kind == "finding" {
					mapped, err := s.m.pg.FindingIDByNodeID(n.ID)
					if err != nil && !errors.Is(err, sql.ErrNoRows) {
						writeErr(w, 500, err.Error())
						return
					}
					entry.FindingID = mapped
				}
				related = append(related, entry)
			}
			if f.NodeID != nil {
				// Only the intent that actually yielded this finding; never a task-wide time guess.
				rows, err := s.m.pg.QueryContext(r.Context(), `SELECT a.id,a.kind,a.tool,LEFT(a.summary,500),LEFT(COALESCE(a.detail,''),8000)
				FROM activity a WHERE a.exploration_id=$1 AND a.node_id IN
				(SELECT src_id FROM exploration_edges WHERE exploration_id=$1 AND dst_id=$2 AND rel='yields')
				AND a.kind IN ('result','tool_result') ORDER BY a.id DESC LIMIT 12`, task.ExplorationID, *f.NodeID)
				if err != nil {
					writeErr(w, 500, err.Error())
					return
				}
				for rows.Next() {
					var seq int64
					var kind, tool, summary, detail sql.NullString
					if err := rows.Scan(&seq, &kind, &tool, &summary, &detail); err != nil {
						rows.Close()
						writeErr(w, 500, err.Error())
						return
					}
					extra = append(extra, attackchain.Source{ID: fmt.Sprintf("activity-%d", seq), Kind: "activity", Label: fmt.Sprintf("关联意图执行记录 #%d · %s", seq, tool.String), Locator: fmt.Sprintf("activity:%d", seq), Text: summary.String + "\n" + detail.String})
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					writeErr(w, 500, err.Error())
					return
				}
			}
		}
	}
	attackchain.SortRelated(related)
	title := f.Name
	if title == "" {
		title = f.VulnClass
	}
	chain := attackchain.Build(attackchain.Finding{ID: f.ID, Name: title, Summary: f.Summary, Evidence: f.Evidence, Report: f.Report, TrafficCount: f.TrafficCount}, related, extra, warnings)
	writeJSON(w, 200, chain)
}
