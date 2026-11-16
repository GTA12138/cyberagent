// algorithms.go 预置算法库（签名/滑块/加解密）的落库层。
//
// 动机：目标有服务端验签、前端滑块、参数加密时，让 worker 每次现场逆向
// 是最烧 token 的路径（读 bundle → 逆算法 → 写实现 → 试错），且成果随任务
// 消失。把「提前解出来的算法」沉淀为跨任务的一等资源：逆一次（人工或
// worker），所有后续任务直接调用——计算在平台本地完成，零 LLM 参与。
//
// 信任边界：script 是平台本地执行的 Python 代码——与军火库脚本、worker 的
// Bash 同一信任域（本就允许执行任意本地命令）；登记入口受控（管理 API 或
// worker 经 ask 审批的 save_algo），执行入口 apply_algo 只做纯计算契约
// （stdin JSON → stdout JSON，15s 超时）。
package db

import (
	"context"
	"database/sql"
	"time"
)

// Algorithm 是一条预置算法。
type Algorithm struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`  // 调用名（任务内 apply_algo 按名取）
	Kind        string     `json:"kind"`  // sign | slider | encrypt | decrypt | other
	MatchHost   string     `json:"match_host,omitempty"`  // 命中提示用：host 子串
	MatchPath   string     `json:"match_path,omitempty"`  // 命中提示用：path 子串
	Description string     `json:"description,omitempty"` // 一句话用途与入参说明
	Script      string     `json:"script"`                // Python：stdin JSON → stdout JSON
	Enabled     bool       `json:"enabled"`
	UsageCount  int        `json:"usage_count"`
	LastUsed    *time.Time `json:"last_used,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// AlgorithmStore 是 algorithms 表的访问层。
type AlgorithmStore struct {
	d *DB
}

func NewAlgorithmStore(d *DB) *AlgorithmStore { return &AlgorithmStore{d: d} }

const algorithmCols = `id, name, kind, match_host, match_path, description, script, enabled, usage_count, last_used, created_at`

func scanAlgorithm(row interface{ Scan(...any) error }) (Algorithm, error) {
	var a Algorithm
	err := row.Scan(&a.ID, &a.Name, &a.Kind, &a.MatchHost, &a.MatchPath, &a.Description,
		&a.Script, &a.Enabled, &a.UsageCount, &a.LastUsed, &a.CreatedAt)
	return a, err
}

// UpsertAlgorithm 按 name 幂等登记：存在即整体更新（script/说明/匹配条件），
// 保留 usage_count。同名覆盖是刻意语义——算法迭代不应堆积版本行。
func (s *AlgorithmStore) UpsertAlgorithm(ctx context.Context, a *Algorithm) (int64, error) {
	var id int64
	err := s.d.QueryRowContext(ctx, `
INSERT INTO algorithms(name, kind, match_host, match_path, description, script, enabled)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (name) DO UPDATE SET
  kind=$2, match_host=$3, match_path=$4, description=$5, script=$6, enabled=$7
RETURNING id`,
		a.Name, a.Kind, a.MatchHost, a.MatchPath, a.Description, a.Script, a.Enabled).Scan(&id)
	return id, err
}

// List 列出算法（默认只启用的；includeDisabled=true 全量）。不含 script 正文——
// 目录必须轻：它是按需拉取的索引，不是注入上下文的清单。
func (s *AlgorithmStore) List(ctx context.Context, includeDisabled bool) ([]Algorithm, error) {
	query := `SELECT ` + algorithmCols + ` FROM algorithms`
	if !includeDisabled {
		query += ` WHERE enabled`
	}
	query += ` ORDER BY name`
	rows, err := s.d.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Algorithm{}
	for rows.Next() {
		a, err := scanAlgorithm(rows)
		if err != nil {
			return nil, err
		}
		a.Script = "" // 目录不带正文
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get 按名取完整记录（含 script）。不存在返回 (nil, nil)。
func (s *AlgorithmStore) Get(ctx context.Context, name string) (*Algorithm, error) {
	a := &Algorithm{}
	err := s.d.QueryRowContext(ctx,
		`SELECT `+algorithmCols+` FROM algorithms WHERE name=$1 AND enabled`, name).Scan(
		&a.ID, &a.Name, &a.Kind, &a.MatchHost, &a.MatchPath, &a.Description,
		&a.Script, &a.Enabled, &a.UsageCount, &a.LastUsed, &a.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// NoteUse 使用计数 +1（best-effort）。
func (s *AlgorithmStore) NoteUse(ctx context.Context, id int64) {
	_, _ = s.d.ExecContext(ctx,
		`UPDATE algorithms SET usage_count = usage_count + 1, last_used = now() WHERE id=$1`, id)
}

// SetEnabled 启停（软删除：保留成果但不再命中提示/调用）。
func (s *AlgorithmStore) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := s.d.ExecContext(ctx, `UPDATE algorithms SET enabled=$2 WHERE id=$1`, id, enabled)
	return err
}
