package db

import (
	"context"
	"fmt"
	"time"
)

// AuthAttempt 是一条可核验的认证尝试记录。
//
// 设计要点：**credential 上的 verified 布尔位是缓存，本表才是真相**。
// 任何「这个口令对这个服务有效」的结论都必须能追到一条 outcome='success' 的
// 记录；反之，同一 failure_signature 反复出现应当进尝试预算，而不是让 agent
// 无休止重试。
type AuthAttempt struct {
	ID               int64
	TaskID           int64
	CredentialID     int64
	Principal        string
	Service          string
	Target           string
	Route            string
	AuthMethod       string
	Outcome          string // success / failure / error / blocked / unknown
	FailureSignature string
	ViaSessionID     int64
	Evidence         string
	CreatedAt        time.Time
}

// 认证结果取值。unknown 表示「发出去了但无法判定」——它既不能当成功也不能当
// 失败，故意保留第三态，避免把「不知道」压成「失败」。
const (
	AuthOutcomeSuccess = "success"
	AuthOutcomeFailure = "failure"
	AuthOutcomeError   = "error"
	AuthOutcomeBlocked = "blocked"
	AuthOutcomeUnknown = "unknown"
)

// AuthAttemptStore 认证尝试账本访问层。
type AuthAttemptStore struct {
	d *DB
}

// NewAuthAttemptStore 构造访问层。
func NewAuthAttemptStore(d *DB) *AuthAttemptStore {
	return &AuthAttemptStore{d: d}
}

// Record 写入一条尝试记录并返回 id。
func (s *AuthAttemptStore) Record(ctx context.Context, a AuthAttempt) (int64, error) {
	if a.Outcome == "" {
		a.Outcome = AuthOutcomeUnknown
	}
	var id int64
	err := s.d.QueryRowContext(ctx, `
INSERT INTO auth_attempts(task_id, credential_id, principal, service, target, route,
                          auth_method, outcome, failure_signature, via_session_id, evidence)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		a.TaskID, a.CredentialID, a.Principal, a.Service, a.Target, a.Route,
		a.AuthMethod, a.Outcome, a.FailureSignature, a.ViaSessionID, a.Evidence).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写入认证尝试失败: %w", err)
	}
	return id, nil
}

// HasSuccess 判断某个凭据对某个服务+目标是否已有成功记录。
//
// 这是「verified 必须由证据支撑」的读侧：调用方不要看 credential 上的布尔位，
// 而应该问这一句。
func (s *AuthAttemptStore) HasSuccess(ctx context.Context, credentialID int64, service, target string) (bool, error) {
	if credentialID <= 0 || target == "" {
		return false, nil
	}
	var exists bool
	err := s.d.QueryRowContext(ctx, `
SELECT EXISTS(
  SELECT 1 FROM auth_attempts
  WHERE credential_id=$1 AND service=$2 AND target=$3 AND outcome='success'
)`, credentialID, service, target).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("查询认证成功记录失败: %w", err)
	}
	return exists, nil
}

// Successes 返回某凭据**已实测成功过**的 (service, target) 组合。
//
// 这是复用打分的输入：HasSuccess 回答单个组合「成没成」，Successes 一次取回
// 全部已证明的组合，让打分能回答「这条凭据在这台机器上已经打通了哪些服务」。
// 逐条调 HasSuccess 会退化成 N×M 次查询——那正是「有账本但用不上」的典型。
//
// 只取最近 limit 条（默认 500），按 target 升序保证确定性。
func (s *AuthAttemptStore) Successes(ctx context.Context, credentialID int64, limit int) ([]AuthAttempt, error) {
	if credentialID <= 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.d.QueryContext(ctx, `
SELECT DISTINCT service, target FROM auth_attempts
WHERE credential_id=$1 AND outcome='success' AND target <> ''
ORDER BY target, service
LIMIT $2`, credentialID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取认证成功记录失败: %w", err)
	}
	defer rows.Close()
	out := []AuthAttempt{}
	for rows.Next() {
		var a AuthAttempt
		if err := rows.Scan(&a.Service, &a.Target); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RecentFailures 统计某凭据在某目标上、时间窗内的失败次数与最近失败签名。
//
// 用途是尝试预算：同一 failure_signature 连续出现说明「换个口令也一样」，
// 继续试只是烧预算并可能触发账户锁定。
func (s *AuthAttemptStore) RecentFailures(ctx context.Context, credentialID int64, service, target string, within time.Duration) (int, string, error) {
	if credentialID <= 0 || target == "" {
		return 0, "", nil
	}
	if within <= 0 {
		within = time.Hour
	}
	var (
		n       int
		lastSig string
	)
	err := s.d.QueryRowContext(ctx, `
SELECT count(*), COALESCE((
    SELECT failure_signature FROM auth_attempts
    WHERE credential_id=$1 AND service=$2 AND target=$3
      AND outcome IN ('failure','error')
      AND created_at >= now() - $4::interval
    ORDER BY id DESC LIMIT 1
), '')
FROM auth_attempts
WHERE credential_id=$1 AND service=$2 AND target=$3
  AND outcome IN ('failure','error')
  AND created_at >= now() - $4::interval`,
		credentialID, service, target, fmt.Sprintf("%d seconds", int(within.Seconds()))).Scan(&n, &lastSig)
	if err != nil {
		return 0, "", fmt.Errorf("统计认证失败次数失败: %w", err)
	}
	return n, lastSig, nil
}

// ListByTask 返回任务最近的认证尝试（新的在前），供态势快照与审计使用。
func (s *AuthAttemptStore) ListByTask(ctx context.Context, taskID int64, limit int) ([]AuthAttempt, error) {
	if taskID <= 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.d.QueryContext(ctx, `
SELECT id, task_id, credential_id, principal, service, target, route, auth_method,
       outcome, failure_signature, via_session_id, evidence, created_at
FROM auth_attempts WHERE task_id=$1 ORDER BY id DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取认证尝试失败: %w", err)
	}
	defer rows.Close()
	var out []AuthAttempt
	for rows.Next() {
		var a AuthAttempt
		if err := rows.Scan(&a.ID, &a.TaskID, &a.CredentialID, &a.Principal, &a.Service,
			&a.Target, &a.Route, &a.AuthMethod, &a.Outcome, &a.FailureSignature,
			&a.ViaSessionID, &a.Evidence, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
