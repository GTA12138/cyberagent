package db

// observations.go 是观测与拓扑边的访问层（I3 / T-13）。
//
// 两条规则写在这一层而不是上层，因为它们必须在**每一次写入**时成立，
// 而不是「调用方记得的时候」成立：
//
//  1. 观测必须有可读回的原文（artifact_ref + hash）。没有原文的观测在半年后
//     无法复核，而无法复核的证据在审查里等于不存在。
//  2. 拓扑边的证据等级只升不降。升级规则由 observation.Promote 单点决定，
//     这里用行锁做「读-改-写」，避免把同一套等级比较在 SQL 里再写一遍——
//     两份规则迟早会分叉，而分叉的表现是「同一个边在两处显示不同等级」。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"chengjian.local/chengjian/observation"
)

// ObservationStore 观测台账访问层。
type ObservationStore struct{ d *DB }

// NewObservationStore 构造访问层。
func NewObservationStore(d *DB) *ObservationStore { return &ObservationStore{d: d} }

// Put 写入一条观测，返回 id。
func (s *ObservationStore) Put(ctx context.Context, o *observation.Observation) (int64, error) {
	if err := o.Validate(); err != nil {
		return 0, err
	}
	collected := o.CollectedAt
	if collected.IsZero() {
		collected = time.Now()
	}
	var id int64
	err := s.d.QueryRowContext(ctx, `
INSERT INTO observations(task_id, kind, observer_session, route, command,
                         artifact_ref, artifact_hash, artifact_bytes, bound_addr,
                         confidence, notes, collected_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		o.TaskID, o.Kind, o.ObserverSession, o.Route, o.Command,
		o.ArtifactRef, o.ArtifactHash, o.ArtifactBytes, o.BoundAddr,
		o.Confidence, o.Notes, collected).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写入观测失败: %w", err)
	}
	return id, nil
}

// Get 按 id 读回一条观测（含 artifact_ref，供上层读原文复核）。
func (s *ObservationStore) Get(ctx context.Context, id int64) (*observation.Observation, error) {
	if id <= 0 {
		return nil, nil
	}
	row := s.d.QueryRowContext(ctx, `
SELECT id, task_id, kind, observer_session, route, command, artifact_ref, artifact_hash,
       artifact_bytes, bound_addr, confidence, notes, collected_at
FROM observations WHERE id=$1`, id)
	var o observation.Observation
	err := row.Scan(&o.ID, &o.TaskID, &o.Kind, &o.ObserverSession, &o.Route, &o.Command,
		&o.ArtifactRef, &o.ArtifactHash, &o.ArtifactBytes, &o.BoundAddr, &o.Confidence,
		&o.Notes, &o.CollectedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取观测失败: %w", err)
	}
	return &o, nil
}

// List 按任务列出最近观测（最新在前）。limit<=0 时取 20。
func (s *ObservationStore) List(ctx context.Context, taskID int64, limit int) ([]observation.Observation, error) {
	if taskID <= 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.d.QueryContext(ctx, `
SELECT id, task_id, kind, observer_session, route, command, artifact_ref, artifact_hash,
       artifact_bytes, bound_addr, confidence, notes, collected_at
FROM observations WHERE task_id=$1 ORDER BY collected_at DESC, id DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("列出观测失败: %w", err)
	}
	defer rows.Close()
	out := []observation.Observation{}
	for rows.Next() {
		var o observation.Observation
		if err := rows.Scan(&o.ID, &o.TaskID, &o.Kind, &o.ObserverSession, &o.Route, &o.Command,
			&o.ArtifactRef, &o.ArtifactHash, &o.ArtifactBytes, &o.BoundAddr, &o.Confidence,
			&o.Notes, &o.CollectedAt); err != nil {
			return nil, fmt.Errorf("扫描观测失败: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// UpsertEdge 写入或升级一条拓扑边。
//
// 返回生效后的证据等级，以及这次是否真的发生了升级——调用方据此决定要不要
// 记一条 activity。`(task_id, kind, from, to)` 是幂等键：同一条边重扫多少次
// 都只有一行，只刷新观测时间与有效期。
func (s *ObservationStore) UpsertEdge(ctx context.Context, e observation.Edge) (observation.Evidence, bool, error) {
	if err := e.Validate(); err != nil {
		return "", false, err
	}
	if e.ObservedAt.IsZero() || e.ValidUntil.IsZero() {
		e = e.WithTTL(time.Now())
	}
	tx, err := s.d.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("开启拓扑边事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var cur observation.Evidence
	lockErr := tx.QueryRowContext(ctx, `
SELECT evidence FROM topology_edges
WHERE task_id=$1 AND kind=$2 AND from_node=$3 AND to_node=$4
FOR UPDATE`, e.TaskID, string(e.Kind), e.From, e.To).Scan(&cur)

	created := false
	effective := e.Evidence
	upgraded := false
	switch {
	case errors.Is(lockErr, sql.ErrNoRows):
		created = true
		if _, err := tx.ExecContext(ctx, `
INSERT INTO topology_edges(task_id, observation_id, kind, from_node, to_node,
                           evidence, observed_at, valid_until, note)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			e.TaskID, e.ObservationID, string(e.Kind), e.From, e.To,
			string(e.Evidence), e.ObservedAt, e.ValidUntil, e.Note); err != nil {
			return "", false, fmt.Errorf("写入拓扑边失败: %w", err)
		}
	case lockErr != nil:
		return "", false, fmt.Errorf("读取拓扑边失败: %w", lockErr)
	default:
		next, ok := observation.Promote(cur, e.Evidence)
		effective = next
		upgraded = ok
		// 时间与来源永远刷新（「什么时候看到的」以最近一次为准），
		// 证据等级只在 Promote 允许时变化。
		if _, err := tx.ExecContext(ctx, `
UPDATE topology_edges
SET evidence=$1, observed_at=$2, valid_until=$3, observation_id=$4,
    note = CASE WHEN $5 <> '' THEN $5 ELSE note END
WHERE task_id=$6 AND kind=$7 AND from_node=$8 AND to_node=$9`,
			string(effective), e.ObservedAt, e.ValidUntil, e.ObservationID, e.Note,
			e.TaskID, string(e.Kind), e.From, e.To); err != nil {
			return "", false, fmt.Errorf("更新拓扑边失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("提交拓扑边失败: %w", err)
	}
	return effective, created || upgraded, nil
}

// Edges 按任务列出拓扑边（最新观测在前）。
func (s *ObservationStore) Edges(ctx context.Context, taskID int64, limit int) ([]observation.Edge, error) {
	if taskID <= 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.d.QueryContext(ctx, `
SELECT id, task_id, observation_id, kind, from_node, to_node, evidence,
       observed_at, valid_until, note
FROM topology_edges WHERE task_id=$1
ORDER BY observed_at DESC, id DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("列出拓扑边失败: %w", err)
	}
	defer rows.Close()
	out := []observation.Edge{}
	for rows.Next() {
		var (
			e    observation.Edge
			kind string
			evid string
		)
		if err := rows.Scan(&e.ID, &e.TaskID, &e.ObservationID, &kind, &e.From, &e.To, &evid,
			&e.ObservedAt, &e.ValidUntil, &e.Note); err != nil {
			return nil, fmt.Errorf("扫描拓扑边失败: %w", err)
		}
		e.Kind = observation.EdgeKind(kind)
		e.Evidence = observation.Evidence(evid)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneExpired 删除过期超过 grace 的边。
//
// 保留 grace 而不是立即删：过期边在 grace 内仍然解释「之前看到过什么」，
// 立即删除会让「环境变了」和「从来没有过」在快照里长得一样。
func (s *ObservationStore) PruneExpired(ctx context.Context, now time.Time, grace time.Duration) (int64, error) {
	if grace <= 0 {
		grace = 24 * time.Hour
	}
	res, err := s.d.ExecContext(ctx, `
DELETE FROM topology_edges WHERE valid_until < $1`, now.Add(-grace))
	if err != nil {
		return 0, fmt.Errorf("清理过期拓扑边失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
