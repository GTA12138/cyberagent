package db

import (
	"context"
	"fmt"
)

// SessionLeaseStore 管理「谁此刻正在用某个会话」。
//
// 与 SessionAccessStore 的区别（两者刻意分开，不能合并）：
//   - grant 是**准入**：这个任务被允许对这个会话做哪些动作，可以长期有效；
//   - lease 是**存活**：这个任务此刻正在用它，必须随任务终态释放。
//
// 合并的后果是「一个早已结束的任务因为 grant 还在，永久阻止别人删会话」——
// 准入本来就不该有生命周期语义。
type SessionLeaseStore struct {
	d *DB
}

// NewSessionLeaseStore 构造访问层。
func NewSessionLeaseStore(d *DB) *SessionLeaseStore {
	return &SessionLeaseStore{d: d}
}

// Acquire 登记「任务 taskID 正在使用会话 sessionID」。
//
// 幂等：重复获取只把 released_at 清空并刷新时间，不产生新行。purpose 用
// 逗号分隔的短标签（exec/recon/tunnel），只进审计，不参与判定。
//
// sessionID 或 taskID 非正数时直接返回 nil：没有任务维度的调用（例如平台
// 自检）不该凭空造出一条永远释放不掉的租约。
func (s *SessionLeaseStore) Acquire(ctx context.Context, sessionID, taskID int64, purpose string) error {
	if sessionID <= 0 || taskID <= 0 {
		return nil
	}
	if purpose == "" {
		purpose = "use"
	}
	_, err := s.d.ExecContext(ctx, `
INSERT INTO session_leases(session_id, task_id, purpose, acquired_at, released_at)
VALUES ($1, $2, $3, now(), NULL)
ON CONFLICT (session_id, task_id) DO UPDATE
SET purpose = CASE
        WHEN session_leases.purpose = EXCLUDED.purpose THEN session_leases.purpose
        WHEN position(EXCLUDED.purpose in session_leases.purpose) > 0 THEN session_leases.purpose
        ELSE session_leases.purpose || ',' || EXCLUDED.purpose
    END,
    released_at = NULL,
    acquired_at = now()`,
		sessionID, taskID, purpose)
	if err != nil {
		return fmt.Errorf("登记会话租约失败: %w", err)
	}
	return nil
}

// ReleaseTask 释放一个任务持有的全部租约，返回释放条数。
// 任务终态（停止/删除/归档）必须调用——这是「任务结束不留下存活引用」的唯一出口。
func (s *SessionLeaseStore) ReleaseTask(ctx context.Context, taskID int64) (int64, error) {
	if taskID <= 0 {
		return 0, nil
	}
	res, err := s.d.ExecContext(ctx,
		`UPDATE session_leases SET released_at = now()
		 WHERE task_id = $1 AND released_at IS NULL`, taskID)
	if err != nil {
		return 0, fmt.Errorf("释放任务会话租约失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// OtherHolders 返回除 excludeTaskID 之外仍在持有该会话的任务 id。
//
// 这是「删会话前的最后一道检查」：只要还有别的任务活着并用着它，删除就是
// 单方面掐断别人的执行。返回空切片表示可以安全删除。
func (s *SessionLeaseStore) OtherHolders(ctx context.Context, sessionID, excludeTaskID int64) ([]int64, error) {
	if sessionID <= 0 {
		return nil, nil
	}
	rows, err := s.d.QueryContext(ctx, `
SELECT task_id FROM session_leases
WHERE session_id = $1 AND released_at IS NULL AND task_id <> $2
ORDER BY task_id`, sessionID, excludeTaskID)
	if err != nil {
		return nil, fmt.Errorf("读取会话租约持有者失败: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LiveForTask 返回任务当前仍持有的会话 id（用于终态收尾前的日志与审计）。
func (s *SessionLeaseStore) LiveForTask(ctx context.Context, taskID int64) ([]int64, error) {
	if taskID <= 0 {
		return nil, nil
	}
	rows, err := s.d.QueryContext(ctx,
		`SELECT session_id FROM session_leases
		 WHERE task_id = $1 AND released_at IS NULL ORDER BY session_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("读取任务租约失败: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
