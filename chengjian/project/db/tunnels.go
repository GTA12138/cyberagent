// tunnels.go 是多层代理隧道台账的落库层（schema.sql §L，内网渗透期 3a,
// INTRANET-PIVOT-DESIGN.md §4.3/4.3a)。deploy_params 是完整部署参数 JSON
// （含 auth、server_pid、stage_token、远端路径、验证目标）——断链自动重拉全靠它
// (PivotHub 的教训：参数打散存列、不存会话引用，重拉名存实亡；这里必须存全）。
// deploy_params 含隧道 auth token，属平台信任域内数据，明文 JSONB 存储
// （与 sessions/credentials 的目标侧凭据不同：那是用户资产，这是平台自造的一次性令牌）。
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// 隧道状态机：deploying → alive → (error → 重拉 → alive)×N → stopped。
const (
	TunnelDeploying = "deploying"
	TunnelAlive     = "alive"
	TunnelError     = "error"
	TunnelStopped   = "stopped"
)

// TunnelRecord 是 tunnels 表的一行。DeployParams 是完整部署参数 JSON
// (tunnel.Plan 的序列化）。
type TunnelRecord struct {
	ID           int64           `json:"id"`
	TaskID       int64           `json:"task_id"`
	Kind         string          `json:"kind"` // socks | portfwd
	Adapter      string          `json:"adapter"`
	ListenHost   string          `json:"listen_host"`
	ListenPort   int             `json:"listen_port"`
	TargetHost   string          `json:"target_host,omitempty"`
	TargetPort   int             `json:"target_port,omitempty"`
	ViaSessionID int64           `json:"via_session_id"`
	RemotePID    int             `json:"remote_pid,omitempty"`
	DeployParams json.RawMessage `json:"deploy_params"`
	State        string          `json:"state"`
	LastCheck    time.Time       `json:"last_check"`
	Error        string          `json:"error,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	// ReachableRanges 是**声明**的可达网段（CIDR 列表，T-06）。空 = 未声明，
	// 不等于「什么都不可达」。它只是假设：本地 socks 端口打开 ≠ 所有网段可达。
	ReachableRanges json.RawMessage `json:"reachable_ranges,omitempty"`
	// DNSPolicy 是**声明**的目标侧解析策略（如 {"mode":"target","servers":[...]}）。
	DNSPolicy json.RawMessage `json:"dns_policy,omitempty"`
	// ReachVerifiedAt 只在真正做过可达性探测后写入。非空表示「声明被观测过」，
	// 为空表示「这只是声明」——两者在输出里必须能区分。
	ReachVerifiedAt *time.Time `json:"reach_verified_at,omitempty"`
	// ReachVerifiedRanges 是**实测**可达的范围（探测确认过连通或被拒连的 host）。
	// 与 ReachableRanges 分开存：后者是「我以为」，前者是「我试过」。
	// 合在一列会让一次局部探测把没探到的声明一起冲掉。
	ReachVerifiedRanges json.RawMessage `json:"reach_verified_ranges,omitempty"`
}

// TunnelStore 是 tunnels 表的访问层。
type TunnelStore struct {
	d *DB
}

// NewTunnelStore 构造访问层。
func NewTunnelStore(d *DB) *TunnelStore {
	return &TunnelStore{d: d}
}

// Create 插入一条隧道（初始 state 通常为 deploying）并返回主键。
func (s *TunnelStore) Create(ctx context.Context, rec *TunnelRecord) (int64, error) {
	if rec.Kind != "socks" && rec.Kind != "portfwd" {
		return 0, fmt.Errorf("非法隧道类型 %q（socks|portfwd)", rec.Kind)
	}
	adapter := rec.Adapter
	if adapter == "" {
		adapter = "chisel"
	}
	state := rec.State
	if state == "" {
		state = TunnelDeploying
	}
	params := rec.DeployParams
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	var id int64
	err := s.d.QueryRowContext(ctx, `
INSERT INTO tunnels(task_id, kind, adapter, listen_host, listen_port,
	target_host, target_port, via_session_id, remote_pid, deploy_params, state, error,
	reachable_ranges, dns_policy)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
RETURNING id`,
		rec.TaskID, rec.Kind, adapter, rec.ListenHost, rec.ListenPort,
		rec.TargetHost, rec.TargetPort, rec.ViaSessionID, rec.RemotePID,
		string(params), state, rec.Error,
		jsonOr(rec.ReachableRanges, "[]"), jsonOr(rec.DNSPolicy, "{}")).Scan(&id)
	return id, err
}

const tunnelCols = `id, task_id, kind, adapter, listen_host, listen_port,
	target_host, target_port, via_session_id, remote_pid, deploy_params,
	state, last_check, error, created_at, reachable_ranges, dns_policy, reach_verified_at,
	reach_verified_ranges`

func scanTunnel(row interface{ Scan(...any) error }) (*TunnelRecord, error) {
	var r TunnelRecord
	if err := row.Scan(&r.ID, &r.TaskID, &r.Kind, &r.Adapter, &r.ListenHost, &r.ListenPort,
		&r.TargetHost, &r.TargetPort, &r.ViaSessionID, &r.RemotePID, &r.DeployParams,
		&r.State, &r.LastCheck, &r.Error, &r.CreatedAt,
		&r.ReachableRanges, &r.DNSPolicy, &r.ReachVerifiedAt,
		&r.ReachVerifiedRanges); err != nil {
		return nil, err
	}
	return &r, nil
}

// Get 取一条；不存在返回 (nil, nil)。
func (s *TunnelStore) Get(ctx context.Context, id int64) (*TunnelRecord, error) {
	r, err := scanTunnel(s.d.QueryRowContext(ctx,
		`SELECT `+tunnelCols+` FROM tunnels WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// UpdateState 更新状态与错误信息，并刷新 last_check。
func (s *TunnelStore) UpdateState(ctx context.Context, id int64, state, errMsg string) error {
	switch state {
	case TunnelDeploying, TunnelAlive, TunnelError, TunnelStopped:
	default:
		return fmt.Errorf("非法隧道状态 %q", state)
	}
	_, err := s.d.ExecContext(ctx,
		`UPDATE tunnels SET state=$2, error=$3, last_check=now() WHERE id=$1`, id, state, errMsg)
	return err
}

// UpdateDeployParams 全量回写部署参数（部署推进时逐层补齐 pid/stage_token 等），
// 可同时回填 remote_pid 列。
func (s *TunnelStore) UpdateDeployParams(ctx context.Context, id int64, remotePID int, params json.RawMessage) error {
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	_, err := s.d.ExecContext(ctx,
		`UPDATE tunnels SET deploy_params=$2, remote_pid=$3, last_check=now() WHERE id=$1`,
		id, string(params), remotePID)
	return err
}

// ListByTask 取某任务的全部隧道（按 id 升序）。
func (s *TunnelStore) ListByTask(ctx context.Context, taskID int64) ([]*TunnelRecord, error) {
	return s.list(ctx, ` WHERE task_id=$1`, taskID)
}

// ListAlive 取全部存活（alive）隧道——健康巡检与端口审计用。
func (s *TunnelStore) ListAlive(ctx context.Context) ([]*TunnelRecord, error) {
	return s.list(ctx, ` WHERE state='alive'`)
}

// ListActive 取全部未终结（deploying|alive|error）隧道。
func (s *TunnelStore) ListActive(ctx context.Context) ([]*TunnelRecord, error) {
	return s.list(ctx, ` WHERE state IN ('deploying','alive','error')`)
}

// ListActiveByViaSession 取经某立足点会话部署、且尚未终结的隧道。
//
// 用途是依赖收尾：会话是隧道的传输前提，删掉会话而留下隧道，会得到一个
// 「平台侧 server 还在监听、目标侧 client 还在重连、但没人能再操作它」的
// 悬空资源——它不会自己消失，也不会进任何健康检查的视野（会话没了，重拉
// 也就无从谈起）。调用方必须先按依赖逆序把这些隧道收掉。
func (s *TunnelStore) ListActiveByViaSession(ctx context.Context, sessionID int64) ([]*TunnelRecord, error) {
	if sessionID <= 0 {
		return nil, nil
	}
	return s.list(ctx, ` WHERE via_session_id=$1 AND state IN ('deploying','alive','error')`, sessionID)
}

// List 取台账全部行（含 stopped)，按 id 升序——内网作战页台账视图用。
func (s *TunnelStore) List(ctx context.Context) ([]*TunnelRecord, error) {
	return s.list(ctx, ``)
}

func (s *TunnelStore) list(ctx context.Context, where string, args ...any) ([]*TunnelRecord, error) {
	rows, err := s.d.QueryContext(ctx, `SELECT `+tunnelCols+` FROM tunnels`+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TunnelRecord{}
	for rows.Next() {
		r, err := scanTunnel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete 删除台账行（部署失败逐层回滚的最后一步）。
func (s *TunnelStore) Delete(ctx context.Context, id int64) error {
	_, err := s.d.ExecContext(ctx, `DELETE FROM tunnels WHERE id=$1`, id)
	return err
}

// jsonOr 在 JSONB 参数为空时给一个合法默认值。
//
// 必要性：pgx 把 nil json.RawMessage 当作 NULL 发送，而这两列是 NOT NULL；
// 显式给 '[]' / '{}' 才能让「未声明」写进库里而不是报错。
func jsonOr(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	return string(raw)
}

// MarkReachVerified 记录「这条隧道的可达性被实际探测过」。
//
// 只有真正发过探测的调用方才能调它——没有探测就把时间戳写上，等于把声明
// 伪装成观测，比不写更糟。
//
// 写入的是**实测**范围列（reach_verified_ranges），不动 reachable_ranges：
// 声明是「我以为能到哪」，实测是「我试过能到哪」，后者是前者的子集证据。
// 若把实测覆盖到声明上，一次只探了 3 个 host 的探测会把声明里另外 200 个
// 网段悄悄抹掉——声明消失得无声无息，比不做探测危害更大。
func (s *TunnelStore) MarkReachVerified(ctx context.Context, id int64, ranges json.RawMessage) error {
	if id <= 0 {
		return nil
	}
	_, err := s.d.ExecContext(ctx, `
UPDATE tunnels SET reach_verified_at = now(),
       reach_verified_ranges = CASE WHEN $2::jsonb = '[]'::jsonb THEN reach_verified_ranges ELSE $2::jsonb END
WHERE id = $1`, id, jsonOr(ranges, "[]"))
	return err
}

// SetReachDeclaration 写入可达范围与 DNS 策略声明（T-06）。
//
// 这是**声明**，不是观测：调用方只是把「我以为这条隧道能到哪些网段」记下来。
// 因此它不写 reach_verified_at——那个字段只由 MarkReachVerified 写，两者
// 混用会让「声明」在输出里看起来像「已验证」。
func (s *TunnelStore) SetReachDeclaration(ctx context.Context, id int64, ranges, dns json.RawMessage) error {
	if id <= 0 {
		return nil
	}
	_, err := s.d.ExecContext(ctx, `
UPDATE tunnels SET reachable_ranges = $2::jsonb, dns_policy = $3::jsonb WHERE id = $1`,
		id, jsonOr(ranges, "[]"), jsonOr(dns, "{}"))
	return err
}
