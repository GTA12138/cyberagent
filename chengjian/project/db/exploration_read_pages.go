package db

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// ActivityTracePageWithSources uses an ID cursor, so long-running traces do not
// hide steps after the first limit and new rows cannot shift an offset page.
func (s *ExplorationStore) ActivityTracePageWithSources(nodeID int64, q string, afterID int64, limit int) ([]Activity, bool, error) {
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	node, err := s.GetNode(nodeID)
	if err != nil {
		return nil, false, err
	}
	if node != nil {
		return s.traceReadPage(nodeID, 0, q, afterID, limit, false)
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, false, err
	}
	for _, source := range sources {
		node, err = source.Store.GetNode(nodeID)
		if err != nil {
			return nil, false, err
		}
		if node == nil || node.Kind != KindIntent {
			continue
		}
		rows, more, err := source.Store.traceReadPage(nodeID, 0, q, afterID, limit, true)
		return markInheritedActivities(rows, source.Task.TaskID), more, err
	}
	return []Activity{}, false, nil
}

func (s *ExplorationStore) SearchWorkerTracePageWithSources(excludeNodeID int64, q string, afterID int64, limit int) ([]Activity, bool, error) {
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	rows, _, err := s.traceReadPage(0, excludeNodeID, q, afterID, limit+1, false)
	if err != nil {
		return nil, false, err
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, false, err
	}
	for _, source := range sources {
		part, _, err := source.Store.traceReadPage(0, 0, q, afterID, limit+1, true)
		if err != nil {
			return nil, false, err
		}
		rows = append(rows, markInheritedActivities(part, source.Task.TaskID)...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	return rows, more, nil
}

func (s *ExplorationStore) traceReadPage(nodeID, excludeNodeID int64, q string, afterID int64, limit int, terminal bool) ([]Activity, bool, error) {
	where := `a.exploration_id=$1 AND a.id>$2 AND a.kind NOT IN ('thinking','usage') AND a.node_id IS NOT NULL`
	args := []any{s.expID, afterID}
	if nodeID > 0 {
		args = append(args, nodeID)
		where += fmt.Sprintf(" AND a.node_id=$%d", len(args))
	}
	if excludeNodeID > 0 {
		args = append(args, excludeNodeID)
		where += fmt.Sprintf(" AND a.node_id<>$%d", len(args))
	}
	if strings.TrimSpace(q) != "" {
		args = append(args, "%"+q+"%")
		where += fmt.Sprintf(" AND (a.summary ILIKE $%d OR a.detail ILIKE $%d)", len(args), len(args))
	}
	if terminal {
		where += ` AND EXISTS (SELECT 1 FROM exploration_nodes n WHERE n.id=a.node_id AND n.exploration_id=a.exploration_id AND n.kind='intent' AND n.state IN ('done','blocked','exhausted','stopped'))`
	}
	args = append(args, limit+1)
	query := `SELECT a.id,a.node_id,COALESCE(a.worker,''),COALESCE(a.kind,''),COALESCE(a.tool,''),a.is_error,COALESCE(a.summary,'') FROM activity a WHERE ` + where + fmt.Sprintf(" ORDER BY a.id LIMIT $%d", len(args))
	r, err := s.db.Query(query, args...)
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	rows, err := scanTrace(r)
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	return rows, more, err
}

// LatestWorkerOutputWithSources asks the owning store for its latest result (or
// latest text if no result exists), instead of scanning just the first 1000 rows.
func (s *ExplorationStore) LatestWorkerOutputWithSources(nodeID int64) (*Activity, error) {
	candidates := []DirectSourceStore{{Store: s}}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, sources...)
	for i, source := range candidates {
		where := `a.exploration_id=$1 AND a.node_id=$2 AND a.kind IN ('result','text')`
		if i > 0 {
			where += ` AND EXISTS (SELECT 1 FROM exploration_nodes n WHERE n.id=a.node_id AND n.exploration_id=a.exploration_id AND n.kind='intent' AND n.state IN ('done','blocked','exhausted','stopped'))`
		}
		var id int64
		err := source.Store.db.QueryRow(`SELECT a.id FROM activity a WHERE `+where+` ORDER BY (a.kind='result') DESC,a.id DESC LIMIT 1`, source.Store.expID, nodeID).Scan(&id)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		// Re-check inherited visibility through the existing detail boundary.
		rows, err := s.ActivityByIDsWithSources([]int64{id})
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			return &rows[0], nil
		}
	}
	return nil, nil
}
