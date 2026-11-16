// health.go 是四件套之 health：周期真实探活（平台侧 server 进程 + 隧道入口
// 可连 + 每若干轮一次经 socks 的实际拨测），死了标 error 并用 deploy_params
// 重新 Deploy（指数退避，上限 3 次）——死链绝不当活上游。另含 Teardown（远端
// 进程经会话按 pid 杀、杀平台 server、删 stage、state=stopped）与 Cleanup
// （任务停止/进程退出时供 manager 调用）。
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/session"
)

// 巡检参数。
const (
	HealthInterval     = 30 * time.Second
	decisiveEveryNTick = 5                // 每 5 轮做一次经 socks 的决定性拨测
	maxRedeploys       = 3                // 自动重拉上限
	redeployBaseDelay  = 30 * time.Second // 退避基数：30s → 60s → 120s
)

type redeployState struct {
	count     int
	nextAt    time.Time
	exhausted bool // 已达上限（日志只打一次）
}

// StartHealth 启动周期巡检 goroutine,ctx 取消即退出（随 server 生命周期）。
func (m *Manager) StartHealth(ctx context.Context) {
	m.healthOnce.Do(func() {
		retry := map[int64]*redeployState{}
		tick := 0
		go func() {
			t := time.NewTicker(HealthInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					tick++
					m.healthRound(ctx, tick%decisiveEveryNTick == 0, retry)
				}
			}
		}()
	})
}

// healthRound 巡检一轮：alive 行做进程+入口探活（decisive 轮加实际拨测），失败
// 标 error 并进重拉；error 行（含平台重启后的孤儿）按退避预算尝试重拉。
func (m *Manager) healthRound(ctx context.Context, decisive bool, retry map[int64]*redeployState) {
	recs, err := m.store.ListActive(ctx)
	if err != nil {
		log.Printf("[tunnel] 巡检读台账失败： %v", err)
		return
	}
	active := make(map[int64]bool, len(recs))
	for _, rec := range recs {
		active[rec.ID] = true
		if ctx.Err() != nil {
			return
		}
		m.healthRecord(ctx, rec.ID, decisive, retry)
	}
	for id := range retry {
		if !active[id] {
			delete(retry, id)
		}
	}
}

func (m *Manager) healthRecord(ctx context.Context, id int64, decisive bool, retry map[int64]*redeployState) {
	ctx, release, err := m.beginOperation(ctx, id, false)
	if err != nil {
		return
	}
	defer release()
	// ListActive is only a candidate list. Teardown may have completed since it
	// was read, so a fresh row under the operation gate is authoritative.
	rec, err := m.store.Get(ctx, id)
	if err != nil || rec == nil || rec.State == db.TunnelStopped || rec.State == db.TunnelDeploying {
		return
	}
	var p Plan
	if err := json.Unmarshal(rec.DeployParams, &p); err != nil {
		_ = m.store.UpdateState(ctx, rec.ID, db.TunnelError, "deploy_params 解析失败： "+err.Error())
		m.notifyState(rec.TaskID)
		return
	}
	if p.CleanupPending || strings.HasPrefix(rec.Error, "清理未完成:") || strings.HasPrefix(rec.Error, "回滚清理未完成:") {
		return
	}
	if rec.State == db.TunnelAlive {
		err = m.checkAlive(&p)
		if err == nil && decisive {
			err = m.checkDecisive(&p)
		}
		if err == nil {
			return
		}
		log.Printf("[tunnel] 隧道 %d 探活失败： %v", rec.ID, err)
		_ = m.store.UpdateState(ctx, rec.ID, db.TunnelError, err.Error())
		m.notifyLifecycle(LifecycleEvent{
			Kind: LifecycleBroken, TunnelID: rec.ID, TaskID: rec.TaskID,
			Adapter: rec.Adapter, Reason: err.Error(),
		})
		m.notifyState(rec.TaskID)
		// 多级隧道级联（cascade.go）：父断链立即传导到活跃子隧道，而不是等
		// 每条子隧道各自探活失败、烧完重拉预算才显形。孙隧道由下一轮巡检接力。
		m.cascadeBroken(ctx, rec.ID, err.Error())
	}
	m.maybeRedeploy(ctx, rec.ID, &p, retry)
}

// checkAlive 轻量探活：平台 server 进程在 + 隧道入口可连。
func (m *Manager) checkAlive(p *Plan) error {
	if p.ServerPID > 0 && !processAlive(p.ServerPID) {
		return fmt.Errorf("平台侧 chisel server(pid %d）已退出", p.ServerPID)
	}
	conn, err := net.DialTimeout("tcp", m.entryAddr(p), 2*time.Second)
	if err != nil {
		return fmt.Errorf("隧道入口 %s 不可连（目标回连已断）: %w", m.entryAddr(p), err)
	}
	_ = conn.Close()
	return nil
}

// checkDecisive 决定性拨测：socks 场景经隧道真实 CONNECT 验证目标。
// suo5 入口带 user/pass 鉴权（--auth 必启用），用鉴权版拨测。
func (m *Manager) checkDecisive(p *Plan) error {
	if p.Kind != KindSocks || p.VerifyAddr == "" {
		return nil
	}
	if p.Adapter == AdapterSuo5 {
		return socks5DialAuth(m.entryAddr(p), p.VerifyAddr, p.AuthUser, p.AuthPass, verifyTimeout)
	}
	return socks5Dial(m.entryAddr(p), p.VerifyAddr, verifyTimeout)
}

// maybeRedeploy 用持久化的 deploy_params 重新 Deploy，指数退避，上限 3 次。
func (m *Manager) maybeRedeploy(ctx context.Context, id int64, p *Plan, retry map[int64]*redeployState) {
	if ctx.Err() != nil || p == nil || p.CleanupPending {
		return
	}
	rs := retry[id]
	if rs == nil {
		rs = &redeployState{}
		retry[id] = rs
	}
	if rs.count >= maxRedeploys {
		if !rs.exhausted {
			rs.exhausted = true
			log.Printf("[tunnel] 隧道 %d 已达重拉上限 %d 次，保持 error，等人工 tunnel_teardown/重部署", id, maxRedeploys)
			m.notifyLifecycle(LifecycleEvent{
				Kind: LifecycleRedeployFailed, TunnelID: id, TaskID: p.TaskID,
				Adapter: p.Adapter, SocksPort: p.SocksPort, Attempt: rs.count,
				Reason: "已达自动重拉上限，需人工 tunnel_teardown 后重部署",
			})
		}
		return
	}
	if time.Now().Before(rs.nextAt) {
		return
	}
	// 子隧道重拉的父链路闸门（cascade.go）：父不 alive 就不消耗预算、不发包。
	// 这不是 blockRedeploy（准入拒绝）——父链路恢复是预期内会发生的常态，
	// 写死一条「被拒」的诊断反而会覆盖级联标记留下的真实原因。每轮巡检
	// 都会重新过这道闸，父恢复后本跳自然跟进（重拉顺序自外向内）。
	if perr := m.parentAliveForRedeploy(ctx, p); perr != nil {
		log.Printf("[tunnel] 隧道 %d 暂不重拉： %v", id, perr)
		return
	}
	if m.AuthorizeRedeploy != nil {
		// Authorization and ownership come from a fresh row, never from the
		// maintenance context or the plan cached by an earlier health check.
		rec, err := m.store.Get(ctx, id)
		if err != nil {
			m.blockRedeploy(ctx, id, nil, "台账读取失败: "+err.Error())
			return
		}
		if rec == nil || rec.State == db.TunnelStopped || rec.State == db.TunnelDeploying {
			return
		}
		var current Plan
		if err := json.Unmarshal(rec.DeployParams, &current); err != nil {
			m.blockRedeploy(ctx, id, rec, "持久化计划解析失败: "+err.Error())
			return
		}
		if current.CleanupPending || strings.HasPrefix(rec.Error, "清理未完成:") || strings.HasPrefix(rec.Error, "回滚清理未完成:") {
			return
		}
		if rec.TaskID <= 0 || rec.ViaSessionID <= 0 || rec.TaskID != current.TaskID || rec.ViaSessionID != current.ViaSessionID ||
			rec.TaskID != p.TaskID || rec.ViaSessionID != p.ViaSessionID {
			m.blockRedeploy(ctx, id, rec, "台账与部署计划的任务/会话归属不一致")
			return
		}
		if err := m.AuthorizeRedeploy(ctx, rec); err != nil {
			m.blockRedeploy(ctx, id, rec, err.Error())
			return
		}
		if ctx.Err() != nil {
			return
		}
		p = &current
	}
	rs.count++
	rs.nextAt = time.Now().Add(redeployBaseDelay << (rs.count - 1)) // 30s/60s/120s
	sess, ok := m.sessions.Get(p.ViaSessionID)
	if !ok {
		log.Printf("[tunnel] 隧道 %d 重拉失败：会话 %d 不在注册表", id, p.ViaSessionID)
		return
	}
	log.Printf("[tunnel] 隧道 %d 第 %d/%d 次自动重拉（deploy_params 持久化参数）…", id, rs.count, maxRedeploys)
	// 重拉 = 同一计划重新部署：先清旧层（远端可能残留），再起新层。
	if p.Adapter == AdapterSuo5 {
		// suo5 无目标侧进程；重拉 = 杀旧 CLI + 重投 payload（目标 payload 被删
		// 的场景在此自然覆盖）+ 重起 CLI + 重新决定性验证。
		if err := m.killServer(id, p); err != nil {
			return
		}
		if err := m.deploySuo5Into(ctx, id, sess, p); err != nil {
			_ = m.store.UpdateState(ctx, id, db.TunnelError, "重拉失败： "+err.Error())
			m.notifyLifecycle(LifecycleEvent{
				Kind: LifecycleRedeployFailed, TunnelID: id, TaskID: p.TaskID,
				Adapter: p.Adapter, SocksPort: p.SocksPort, Attempt: rs.count, Reason: err.Error(),
			})
			m.rollbackKeepRow(ctx, id, sess, p)
			return
		}
	} else {
		if p.RemotePID > 0 {
			if err := m.killRemote(ctx, sess, p); err != nil {
				return
			}
		}
		if err := m.killServer(id, p); err != nil {
			return
		}
		if err := m.deployInto(ctx, id, sess, p); err != nil {
			_ = m.store.UpdateState(ctx, id, db.TunnelError, "重拉失败： "+err.Error())
			m.notifyLifecycle(LifecycleEvent{
				Kind: LifecycleRedeployFailed, TunnelID: id, TaskID: p.TaskID,
				Adapter: p.Adapter, SocksPort: p.SocksPort, Attempt: rs.count, Reason: err.Error(),
			})
			m.rollbackKeepRow(ctx, id, sess, p)
			return
		}
	}
	if err := m.saveParams(ctx, id, p); err != nil {
		m.rollbackKeepRow(ctx, id, sess, p)
		return
	}
	if err := m.store.UpdateState(ctx, id, db.TunnelAlive, ""); err == nil {
		rs.count = 0 // 复活成功，退避计数清零
		log.Printf("[tunnel] 隧道 %d 重拉成功，已恢复 alive", id)
		m.notifyLifecycle(LifecycleEvent{
			Kind: LifecycleRedeployed, TunnelID: id, TaskID: p.TaskID,
			Adapter: p.Adapter, SocksPort: p.SocksPort,
		})
	} else {
		m.rollbackKeepRow(ctx, id, sess, p)
	}
	m.notifyState(p.TaskID)
}

// A denied maintenance admission changes only its diagnostic state. In
// particular it must not kill old resources before discovering lost access.
func (m *Manager) blockRedeploy(ctx context.Context, id int64, rec *db.TunnelRecord, reason string) {
	message := "自动重部署准入被拒绝: " + reason
	log.Printf("[tunnel] 隧道 %d %s", id, message)
	if err := m.store.UpdateState(ctx, id, db.TunnelError, message); err != nil {
		log.Printf("[tunnel] 隧道 %d 准入拒绝原因落库失败: %v", id, err)
		return
	}
	if rec != nil {
		m.notifyState(rec.TaskID)
		if rec.Error != message || rec.State != db.TunnelError {
			m.notifyLifecycle(LifecycleEvent{Kind: LifecycleRedeployFailed, TunnelID: id,
				TaskID: rec.TaskID, Adapter: rec.Adapter, Reason: message})
		}
	}
}

// rollbackKeepRow 重拉失败时清层但保留台账行（error 状态等下一轮）。
func (m *Manager) rollbackKeepRow(ctx context.Context, id int64, sess session.Session, p *Plan) {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := m.cleanupResources(ctx, id, p); err != nil {
		p.CleanupPending = true
		_ = m.saveParams(ctx, id, p)
		_ = m.store.UpdateState(ctx, id, db.TunnelError, "回滚清理未完成: "+err.Error())
	}
}

// Teardown 回收一条隧道：远端进程经会话按 pid 杀（查远端、核对 cmdline)→
// 杀平台 server → 删 stage 条目 → 删 authfile → state=stopped。幂等：已 stopped
// 直接返回。
//
// 只回收这一条；多级隧道的级联回收见 cascade.go 的 TeardownCascade（编排层，
// 子先父后）——Teardown 本体刻意不做台账查询：台账回调可能同步重入（见
// TestHealthUsesFreshRecordAfterTeardown），持闸门期间再进 store 会死锁。
func (m *Manager) Teardown(ctx context.Context, id int64) error {
	ctx, release, err := m.beginOperation(ctx, id, true)
	if err != nil {
		return err
	}
	defer release()
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if rec == nil {
		return nil // 同时失败的部署可能已成功回滚并删除行。
	}
	if rec.State == db.TunnelStopped {
		return nil
	}
	var p Plan
	if err := json.Unmarshal(rec.DeployParams, &p); err != nil {
		return fmt.Errorf("隧道 %d 参数损坏，无法确认清理资源: %w", id, err)
	}
	p.CleanupPending = true
	// Persist stop intent before cleanup: a retry or restart must never revive
	// resources whose cleanup could not be confirmed.
	if err := m.saveParams(ctx, id, &p); err != nil {
		return err
	}
	cleanupErr := m.cleanupResources(ctx, id, &p)
	state, message := db.TunnelStopped, ""
	if cleanupErr != nil {
		state, message = db.TunnelError, "清理未完成: "+cleanupErr.Error()
	}
	writeCtx, cancel := cleanupContext(ctx)
	defer cancel()
	err = m.store.UpdateState(writeCtx, id, state, message)
	if state == db.TunnelStopped {
		m.notifyLifecycle(LifecycleEvent{
			Kind: LifecycleTornDown, TunnelID: id, TaskID: rec.TaskID,
			Adapter: rec.Adapter, Reason: message,
		})
	}
	m.notifyState(rec.TaskID)
	return errors.Join(cleanupErr, err)
}

// Cleanup 回收一批隧道（任务停止/进程退出时供 manager 调用）。taskID=0 收全部
// 未终结隧道。best-effort，单条失败只记日志。
//
// 逆 id 序（子先父后）：多级隧道的子隧道 id 必然更大（父必须先部署），
// 降序回收即依赖逆序——子隧道的远端进程经父链路才可达，父先死子就杀不动。
func (m *Manager) Cleanup(ctx context.Context, taskID int64) {
	var recs []*db.TunnelRecord
	var err error
	if taskID > 0 {
		recs, err = m.store.ListByTask(ctx, taskID)
	} else {
		recs, err = m.store.ListActive(ctx)
	}
	if err != nil {
		log.Printf("[tunnel] Cleanup 读台账失败： %v", err)
		return
	}
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		if r.State == db.TunnelStopped {
			continue
		}
		if err := m.Teardown(ctx, r.ID); err != nil {
			log.Printf("[tunnel] Cleanup 隧道 %d 失败： %v", r.ID, err)
		}
	}
}

// MarkOrphansError 平台重启后调用：所有 alive/deploying 台账行的平台侧进程已随
// 重启消亡，诚实标 error（不装活）；健康巡检会按 deploy_params 自动重拉。
func (m *Manager) MarkOrphansError(ctx context.Context) {
	recs, err := m.store.ListActive(ctx)
	if err != nil {
		return
	}
	for _, r := range recs {
		var p Plan
		if (json.Unmarshal(r.DeployParams, &p) == nil && p.CleanupPending) || strings.HasPrefix(r.Error, "清理未完成:") || strings.HasPrefix(r.Error, "回滚清理未完成:") {
			_ = m.store.UpdateState(ctx, r.ID, db.TunnelError, "清理未完成: 平台重启后等待显式重试清理")
			continue
		}
		_ = m.store.UpdateState(ctx, r.ID, db.TunnelError, "平台重启，server 进程已消亡，待健康巡检自动重拉")
		m.notifyState(r.TaskID)
	}
}

// processAlive 检测平台侧进程存活（linux:/proc;windows:FindProcess 失败即不在；
// 其它 unix:Signal(0))。
func processAlive(pid int) bool {
	if runtime.GOOS == "linux" {
		_, err := os.Stat("/proc/" + strconv.Itoa(pid))
		return err == nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer proc.Release()
	if runtime.GOOS == "windows" {
		return true // FindProcess 打不开句柄时已在上一步返回
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// serverCmdlineMatches 杀台前核对 /proc/<pid>/cmdline 确为台账里的隧道进程
// （防 pid 复用误杀；非 linux 平台保守返回 false，只靠本进程台账句柄）。
// 批 7 P1-4：平台侧文件名已去 "chisel"/"suo5" 特征，且二进制路径可经
// CHENGJIAN_*_PATH 指向任意名字——不再核对工具名，只核对随计划生成的唯一特征：
// chisel 核对 authfile 路径（含随机 tag);suo5 核对 payload URL（含随机文件名）。
func serverCmdlineMatches(p *Plan) bool {
	if runtime.GOOS != "linux" || p.ServerPID <= 0 {
		return false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(p.ServerPID) + "/cmdline")
	if err != nil {
		return false
	}
	cl := strings.ReplaceAll(string(data), "\x00", " ")
	if p.Adapter == AdapterSuo5 {
		return p.PayloadURL != "" && strings.Contains(cl, p.PayloadURL)
	}
	return p.AuthFile != "" && strings.Contains(cl, p.AuthFile)
}
