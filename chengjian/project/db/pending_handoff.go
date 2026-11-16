// pending_handoff.go 立足点「待移交内网」登记（on_foothold 半自动移交）。
//
// 背景：外网→内网的跨任务移交此前只有一个人工按钮（POST /api/sessions/{id}/
// handoff），人必须盯着会话台账才知道「拿到立足点了、可以移交了」。长任务里
// 立足点往往在半夜出现，等人工发现时窗口期已过。
//
// 机制与边界（刻意保持人工闸门）：
//   - register_session 成功（会话 alive）即自动登记一条 pending 记录 + 活动流
//     通知卡——这是「发现侧」，全自动；
//   - 「执行侧」仍必须人工批准（approve 走与人工按钮完全相同的建子任务路径，
//     初始 scope 仍只有立足点 /32，内网授权边界不因自动化而放松）；dismiss
//     后同会话不再提示；
//   - 与 pending_scope 同一套幂等语义：UNIQUE(session_id)，人工按钮直接移交时
//     自动落 approved（两条路走到同一个终点）。
package db

import (
	"database/sql"
	"time"
)

// PendingHandoff 是一条【立足点待移交】登记。
type PendingHandoff struct {
	ID            int64      `json:"id"`
	SessionID     int64      `json:"session_id"`
	TaskID        int64      `json:"task_id"` // 立足点的来源（外网）任务
	HostIP        string     `json:"host_ip"`
	Status        string     `json:"status"` // pending|approved|dismissed
	HandoffTaskID string     `json:"handoff_task_id,omitempty"` // 批准后创建的内网子任务 id
	CreatedAt     time.Time  `json:"created_at"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
}

const pendingHandoffCols = `id, session_id, task_id, host_ip, status, handoff_task_id, created_at, decided_at`

func scanPendingHandoff(row interface{ Scan(...any) error }) (PendingHandoff, error) {
	var p PendingHandoff
	err := row.Scan(&p.ID, &p.SessionID, &p.TaskID, &p.HostIP, &p.Status, &p.HandoffTaskID,
		&p.CreatedAt, &p.DecidedAt)
	return p, err
}

// UpsertPendingHandoff 幂等登记：新插入返回 created=true；该会话已有登记时不动
// （会话只会成功登记一次，重复登记同会话是重试路径，不该刷屏）。sessionID<=0
// 或来源任务未知 → no-op（没有来源任务就移交不了，登记了也是死条目）。
func (s *SessionStore) UpsertPendingHandoff(sessionID, taskID int64, hostIP string) (bool, error) {
	if sessionID <= 0 || taskID <= 0 {
		return false, nil
	}
	var id int64
	err := s.d.QueryRow(`
INSERT INTO pending_handoff(session_id, task_id, host_ip) VALUES ($1, $2, $3)
ON CONFLICT (session_id) DO NOTHING
RETURNING id`, sessionID, taskID, hostIP).Scan(&id)
	if err == nil {
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	return false, nil
}

// ListPendingHandoff 列出登记；includeDecided=false 只给 pending。
func (s *SessionStore) ListPendingHandoff(includeDecided bool) ([]PendingHandoff, error) {
	query := `SELECT ` + pendingHandoffCols + ` FROM pending_handoff`
	if !includeDecided {
		query += ` WHERE status='pending'`
	}
	query += ` ORDER BY id DESC`
	rows, err := s.d.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingHandoff{}
	for rows.Next() {
		p, err := scanPendingHandoff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPendingHandoff 单条读取；不存在返回 sql.ErrNoRows。
func (s *SessionStore) GetPendingHandoff(id int64) (PendingHandoff, error) {
	return scanPendingHandoff(s.d.QueryRow(
		`SELECT `+pendingHandoffCols+` FROM pending_handoff WHERE id=$1`, id))
}

// DecidePendingHandoff 处置一条登记。approve 时可带上生成的内网子任务 id；
// 只迁移 pending 行（幂等/冲突语义与 pending_scope 一致，由调用方比对 Status）。
func (s *SessionStore) DecidePendingHandoff(id int64, approve bool, handoffTaskID string) (PendingHandoff, error) {
	status := "dismissed"
	if approve {
		status = "approved"
	}
	if _, err := s.d.Exec(`
UPDATE pending_handoff SET status=$2, decided_at=now(), handoff_task_id=CASE WHEN $3<>'' THEN $3 ELSE handoff_task_id END
WHERE id=$1 AND status='pending'`, id, status, handoffTaskID); err != nil {
		return PendingHandoff{}, err
	}
	return s.GetPendingHandoff(id)
}

// MarkHandoffDoneForSession 人工按钮直接移交时把对应登记落 approved（两条路同终点）。
// best-effort：没有登记（老会话/历史数据）时不报错。
func (s *SessionStore) MarkHandoffDoneForSession(sessionID int64, handoffTaskID string) error {
	if sessionID <= 0 {
		return nil
	}
	_, err := s.d.Exec(`
UPDATE pending_handoff SET status='approved', decided_at=now(), handoff_task_id=$2
WHERE session_id=$1 AND status='pending'`, sessionID, handoffTaskID)
	return err
}
