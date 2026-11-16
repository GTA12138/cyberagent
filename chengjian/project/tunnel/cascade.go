// cascade.go 多级隧道的级联治理。
//
// 为什么需要它：ParentTunnelID 此前只在部署瞬间被读一次（server/tunnel.go 校验
// 父隧道 alive 并把回连地址钉到父隧道的 socks 入口），此后无人再看。于是三层
// 链上任何一环断开时：
//   - 子隧道只会各自「探活失败 → 盲目重拉 → 烧完 3 次预算 → 躺 error」，
//     而它持久化的 PlatformAddr 指向父隧道的 socks 端口——父隧道若被回收、
//     端口被别的新隧道复用，重拉会拨到**不相干链路**的入口上（跨链路串线）；
//   - 父隧道被显式 Teardown 时，子隧道的远端进程没人过问，变成「平台台账
//     说 alive、实际链路已死」的悬空资源。
//
// 三条级联规则（全部在 tunnel 层，server 只消费事件）：
//  1. **父断链 → 活跃子隧道立即标 error**（原因写明是级联），并逐条派发
//     LifecycleBroken——planner 的 intranet 快照和活动流马上看到「整链断了」，
//     而不是等每条子隧道各自烧完重拉预算才显形；
//  2. **子隧道重拉前必须确认父隧道 alive**：父不 alive 就不消耗重拉预算、
//     不发包，等下一轮巡检（父恢复后子自动跟着恢复，顺序天然是自外向内）；
//  3. **Teardown 级联递归**：先拆子（依赖逆序），再拆自己，不留悬空远端进程。
//
// 层级上限 MaxHopDepth=3 保证递归深度有界；环不可能出现（部署时父必须已存在），
// 但 Teardown 仍带 visited 防御台账被外键改坏时的死循环。
package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"

	"chengjian.local/chengjian/db"
)

// parentIDOf 从台账行的 deploy_params 里读 ParentTunnelID（0 = 单跳直连平台）。
//
// 只解析这一个字段而不是整个 Plan：台账行可能来自旧版本（字段不全），
// 也可能被并发写——部分解析失败时按单跳处理是最保守的语义（不会把一条
// 单跳隧道误当子隧道去级联标记）。
func parentIDOf(rec *db.TunnelRecord) int64 {
	if rec == nil || len(rec.DeployParams) == 0 {
		return 0
	}
	var p struct {
		ParentTunnelID int64 `json:"parent_tunnel_id"`
	}
	if err := json.Unmarshal(rec.DeployParams, &p); err != nil {
		return 0
	}
	return p.ParentTunnelID
}

// ChildrenOf 返回以 parentID 为父隧道（经它转发回连）且尚未终结的隧道。
//
// ParentTunnelID 在 deploy_params JSON 里而不是独立列，无法在 SQL 过滤——
// 活跃隧道总数是个位数量级（每立足点一两条），全取回后内存过滤足够。
func (m *Manager) ChildrenOf(ctx context.Context, parentID int64) ([]*db.TunnelRecord, error) {
	if parentID <= 0 {
		return nil, nil
	}
	recs, err := m.store.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	var out []*db.TunnelRecord
	for _, r := range recs {
		if r != nil && parentIDOf(r) == parentID {
			out = append(out, r)
		}
	}
	return out, nil
}

// cascadeBroken 把一条隧道的断链传导到全部活跃子隧道。
//
// 只标 alive 的子隧道：已经是 error 的子隧道保留自己的失败原因（那是它
// 自己的诊断信息，用「级联」覆盖只会丢线索）；deploying 中的子隧道马上
// 会在自己的决定性验证处失败，不需要抢跑。
//
// 孙隧道不需要在这里递归：子隧道标 error 后，healthRound 下一轮处理它们时
// 会再次命中「父不 alive → 不重拉」，链式传播由每轮巡检自然完成——一次
// 递归标记反而可能与并发巡检竞争同一行的状态写入。
func (m *Manager) cascadeBroken(ctx context.Context, parentID int64, cause string) {
	children, err := m.ChildrenOf(ctx, parentID)
	if err != nil {
		log.Printf("[tunnel] 级联标记失败：读父隧道 %d 的子隧道台账出错: %v", parentID, err)
		return
	}
	for _, c := range children {
		if c == nil || c.State != db.TunnelAlive {
			continue
		}
		reason := fmt.Sprintf("级联:父隧道 #%d 断链（%s）——本跳经它转发回连，父链路恢复前无法自动重拉", parentID, cause)
		if err := m.store.UpdateState(ctx, c.ID, db.TunnelError, reason); err != nil {
			log.Printf("[tunnel] 子隧道 %d 级联标记落库失败: %v", c.ID, err)
			continue
		}
		m.notifyLifecycle(LifecycleEvent{
			Kind: LifecycleBroken, TunnelID: c.ID, TaskID: c.TaskID,
			Adapter: c.Adapter, Reason: reason,
		})
		m.notifyState(c.TaskID)
		log.Printf("[tunnel] 隧道 %d 因父隧道 %d 断链被级联标记为 error", c.ID, parentID)
	}
}

// parentAliveForRedeploy 是子隧道重拉前的父链路闸门。
//
// 返回 nil 表示可以继续重拉；非 nil 的 error 是给调用方的阻断原因
// （父不存在 / 父非 alive / 读台账失败）。父隧道 dead 时绝不重拉子隧道：
// 子隧道重拉的全部动作（远端拉起 client、回连父 socks 入口）都依赖父链路，
// 父不通时重拉除了烧预算和目标侧动静之外不会有任何产出；更糟的是父隧道
// 回收后其 socks 端口回到端口池，可能被新隧道拿走——子隧道按持久化地址
// 回连会拨到不相干的链路上。
func (m *Manager) parentAliveForRedeploy(ctx context.Context, p *Plan) error {
	if p == nil || p.ParentTunnelID <= 0 {
		return nil
	}
	parent, err := m.store.Get(ctx, p.ParentTunnelID)
	if err != nil {
		return fmt.Errorf("父隧道 #%d 台账读取失败: %w", p.ParentTunnelID, err)
	}
	if parent == nil || parent.State == db.TunnelStopped {
		return fmt.Errorf("父隧道 #%d 已不存在（台账已回收）：本跳需在父链路重建后重新 tunnel_deploy，或先 tunnel_teardown 清理本跳", p.ParentTunnelID)
	}
	if parent.State != db.TunnelAlive {
		return fmt.Errorf("父隧道 #%d 状态为 %s：等父链路恢复 alive 后本跳自动重拉（重拉顺序自外向内，无需人工干预）", p.ParentTunnelID, parent.State)
	}
	return nil
}

// TeardownCascade 级联回收一条隧道：先递归回收以它为根的子隧道链（孙→子→
// 父的依赖逆序），最后回收自己。单条子隧道失败不阻塞其余回收，首个错误
// 会随返回值上报。
//
// 为什么是编排层薄方法而不是塞进 Teardown：Teardown 持每隧道闸门，闸门内
// 进 store（查子隧道）会在台账回调重入时死锁（TestHealthUsesFreshRecord
// AfterTeardown 锁的就是这个契约）。这里所有台账查询都发生在任何闸门之外，
// 逐条顺序调用 Teardown，不嵌套持闸——结构上无死锁面。
func (m *Manager) TeardownCascade(ctx context.Context, id int64) error {
	return m.teardownCascade(ctx, id, map[int64]bool{})
}

func (m *Manager) teardownCascade(ctx context.Context, id int64, seen map[int64]bool) error {
	if id <= 0 || seen[id] {
		return nil // 环防御：部署时父必须已存在，正常无环；台账被改坏时不发散。
	}
	seen[id] = true
	children, err := m.ChildrenOf(ctx, id)
	if err != nil {
		log.Printf("[tunnel] 隧道 %d 级联回收读子隧道失败（继续回收自身）: %v", id, err)
	}
	// id 降序：同层多子时回收顺序稳定，且与 Cleanup 的整体降序一致。
	sort.Slice(children, func(i, j int) bool { return children[i].ID > children[j].ID })
	var firstErr error
	for _, c := range children {
		if c == nil || c.State == db.TunnelStopped {
			continue
		}
		if err := m.teardownCascade(ctx, c.ID, seen); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := m.Teardown(ctx, id); err != nil {
		return err
	}
	return firstErr
}
