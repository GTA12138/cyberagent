// taskproxy.go 期 3b:worker 流量按任务绑定上游——每任务懒建独立 traffic.Traffic
// (MITM)实例,上游 = 该任务当前 alive socks 隧道入口;无隧道回落全局出口代理
// (globalProxy)或直连。链路:worker Bash/WebFetch → 任务 MITM(本地照常录制,
// 留痕链不断) → socks5 隧道 → 内网目标(INTRANET-PIVOT-DESIGN.md §4.3「隧道消费」)。
//
// spike 结论(ROADMAP.md 6.3)的落地:
//   - go-mitmproxy 多实例共存可行,但每实例必须独立 CaRootPath 防首建竞争
//     → 实例目录 data/traffic-tasks/<taskID>/(_ca/_index/_auth 全在其中);
//   - SetUpstreamProxy 是实例字段、原子热切换 → 隧道新增/死亡/重拉只换上游,
//     不重建实例(经 tunnel.Manager.OnStateChange 触发 Refresh);
//   - Proxy.Close() 有 attacker goroutine 泄漏 → 实例生命周期与任务绑定
//     (任务删除/归档才 CloseTask,进程退出 Close 全清),实例数按任务数有界,
//     泄漏可接受;重启后不自动恢复实例(懒建,隧道状态由健康巡检负责)。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/traffic"
	"chengjian.local/chengjian/tunnel"
)

// tunnelLister 是任务隧道台账的最小读接口(*db.TunnelStore 满足;测试可注入假实现)。
type tunnelLister interface {
	ListByTask(ctx context.Context, taskID int64) ([]*db.TunnelRecord, error)
}

// taskProxyInst 是一个任务级 MITM 实例及其受管端口/当前上游。
type taskProxyInst struct {
	tr       *traffic.Traffic
	port     int
	upstream string             // 回落上游原文(""/globalProxy/socks5://...),变化检测用
	mode     UpstreamMode       // 回落上游的选型结果(见 UpstreamMode)
	routes   *tunnel.RouteTable // 按目标 host 选路的运行期路由表（派生物，非真相源）
	readers  sync.WaitGroup     // leases added only while registered under m.mu
}

// taskProxyManager 按 taskID 懒建并管理任务级 MITM 实例。零值不可用,经
// newTaskProxyManager 构造;全部方法并发安全。
type taskProxyManager struct {
	dir         string        // 实例根目录(data/traffic-tasks)
	store       tunnelLister  // 隧道台账(上游选择的数据源)
	globalProxy func() string // 无隧道时的回落上游(Manager.GlobalProxy,每轮读)
	// intranet 判定任务是否处于内网期(有立足点会话或活隧道)。用于区分
	// 「外网阶段无隧道 → 回落公网出口」与「内网阶段隧道断了 → 禁止回落」。
	intranet func(taskID int64) bool
	portMin  int // 实例监听端口池(默认 21100-21199,见 config.TaskProxyPortRange)
	portMax  int
	// globalTraffic 是进程级录制实例（可空）。任务实例里查不到的交换回退到它——
	// 老任务/未开任务代理时流量的落点。装配处设置（manager.go）。
	globalTraffic *traffic.Traffic
	mu            sync.Mutex
	proxies       map[int64]*taskProxyInst
	closing       map[int64]chan struct{}
	closed        bool
	recordOff     bool // under mu, also applied before a new proxy starts
}

func newTaskProxyManager(dir string, store tunnelLister, globalProxy func() string, portMin, portMax int) *taskProxyManager {
	return &taskProxyManager{
		dir: filepath.Join(dir, "traffic-tasks"), store: store, globalProxy: globalProxy,
		portMin: portMin, portMax: portMax, proxies: map[int64]*taskProxyInst{},
		closing: map[int64]chan struct{}{},
	}
}

// SetRecordEnabled changes persistence on existing and future task proxies.
// Routing, CA trust, ports and connections remain independent of this switch.
func (m *taskProxyManager) SetRecordEnabled(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordOff = !enabled
	for _, inst := range m.proxies {
		inst.tr.SetRecordEnabled(enabled)
	}
}

// SetIntranetResolver wires the intranet-phase predicate (server.taskIntranet).
// nil = 视为外网阶段（回落公网出口，保持既有行为）。
func (m *taskProxyManager) SetIntranetResolver(fn func(taskID int64) bool) {
	m.mu.Lock()
	m.intranet = fn
	m.mu.Unlock()
}

// SetGlobalTraffic wires the process-wide recording instance as the lookup
// fallback for exchanges the task instance doesn't hold. 装配处调用；nil = 无回退。
func (m *taskProxyManager) SetGlobalTraffic(tr *traffic.Traffic) {
	m.mu.Lock()
	m.globalTraffic = tr
	m.mu.Unlock()
}

// blockedUpstream 是内网期无隧道时给 MITM 设置的上游。
//
// 为什么不用空串：`traffic.SetUpstreamProxy("")` 的语义是「无上游 = 直连」，
// 空串会让 worker 真的从平台出口打出去——那正是要阻止的行为。这里指向一个
// 必然失败的本地端口（1 号端口，无服务监听），使所有出站请求在 MITM 层就失败，
// 从而把「禁止直连」从提示词约束变成技术约束。worker 侧同时注入
// tunnelBlockedRules 说明原因，让它知道该去修链路而不是换姿势重试。
const blockedUpstream = "socks5://127.0.0.1:1"

// UpstreamMode 说明任务上游是怎么选出来的。回落到 global/direct 本身不是错误，
// 但「任务明明有内网隧道却回落到公网出口」是错误——那意味着 worker 以为自己在
// 内网、实际从平台直连，既打不中又暴露平台位置。调用方必须能区分这两种情况。
type UpstreamMode string

const (
	// UpstreamTunnel 走任务自己的 alive socks 隧道。
	UpstreamTunnel UpstreamMode = "tunnel"
	// UpstreamGlobal 无隧道可用，回落全局出口代理（外网阶段正常形态）。
	UpstreamGlobal UpstreamMode = "global"
	// UpstreamBlocked 任务处于内网期（有立足点会话或已部署过隧道），但当前没有
	// alive socks 入口。此时不能回落公网出口——内网目标必须经隧道访问。
	// 上游置空，由 worker 侧注入「隧道不可用」纪律并拒绝直连。
	UpstreamBlocked UpstreamMode = "blocked"
)

// pickUpstream 选任务的【回落】上游并说明选型理由。纯函数。
//   - 有 alive socks → (socks5://..., UpstreamTunnel)
//   - 无隧道但任务不处于内网期 → (globalProxy, UpstreamGlobal)
//   - 无隧道但任务处于内网期（intranet=true）→ ("", UpstreamBlocked)
//
// 注意「回落」二字：有隧道时，实际逐请求选路由 tunnel.RouteTable 负责（按目标
// host 选，见 tunnel/route.go）。本函数给出的是「选路层没有意见时用哪条」——
// 即无隧道 / 隧道全死 / 内网期阻断这三种情形。它仍然取 id 最大的隧道，因为
// 在回落语义下「最新的那条」是唯一有意义的猜测，且这条路径只在单隧道时被
// 走到（多隧道时 RouteTable 总会有意见）。
//
// portfwd 不是上游形态，跳过。suo5 入口带 --auth 鉴权，上游 URL 必须内嵌 user:pass。
func pickUpstream(recs []*db.TunnelRecord, globalProxy string, intranet bool) (string, UpstreamMode) {
	bestID := int64(-1)
	socksPort := 0
	var user, pass string
	for _, r := range recs {
		if r.State != db.TunnelAlive || r.Kind != tunnel.KindSocks || r.ID <= bestID {
			continue
		}
		var p tunnel.Plan
		if json.Unmarshal(r.DeployParams, &p) != nil || p.SocksPort <= 0 {
			continue
		}
		bestID, socksPort = r.ID, p.SocksPort
		user, pass = "", ""
		if r.Adapter == tunnel.AdapterSuo5 {
			user, pass = p.AuthUser, p.AuthPass
		}
	}
	if socksPort > 0 {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(socksPort))
		if user != "" {
			return "socks5://" + user + ":" + pass + "@" + addr, UpstreamTunnel
		}
		return "socks5://" + addr, UpstreamTunnel
	}
	// 无 alive 隧道：内网期拒绝回落（见 UpstreamBlocked 注释）。返回一个必然失败的
	// 上游而不是空串——空串在 traffic 层等于「直连」，那正是要阻止的行为。
	if intranet {
		return blockedUpstream, UpstreamBlocked
	}
	return globalProxy, UpstreamGlobal
}

// resolveUpstream 读台账重算任务上游(容错:台账读失败保持现状,由调用方决定)。
//
// intranet 由调用方传入（已解析的布尔值），本函数不碰 m.mu、也不调回调：
// 调用方必须先在锁外执行 intranetPhase()，再进锁调用本函数。
//
// 这样彻底避开「持 m.mu 做 DB IO」与「回调里再取 m.mu 自死锁」两个坑
// （sync.Mutex 不可重入）。
func (m *taskProxyManager) resolveUpstream(ctx context.Context, taskID int64, intranet bool) (string, UpstreamMode, error) {
	recs, err := m.store.ListByTask(ctx, taskID)
	if err != nil {
		return "", UpstreamGlobal, err
	}
	gp := ""
	if m.globalProxy != nil {
		gp = m.globalProxy()
	}
	up, mode := pickUpstream(recs, gp, intranet)
	return up, mode, nil
}

// intranetPhase 判定任务是否处于内网期（自行取锁，调用方必须不持锁）。
func (m *taskProxyManager) intranetPhase(taskID int64) bool {
	m.mu.Lock()
	fn := m.intranet
	m.mu.Unlock()
	return fn != nil && fn(taskID)
}

// ForTask 返回任务 worker 应使用的代理地址与 CA:首次调用懒建该任务的 MITM
// 实例(独立 CA 目录防首建竞争、端口池分配并登记端口审计台账),之后每次调用
// 顺手重算上游(事件间隙的兜底,与 OnStateChange 的 Refresh 互补)。建实例失败
// 返回 "","" 并记录原因；调用方必须停止本次执行，不能改用另一出口。
func (m *taskProxyManager) ForTask(ctx context.Context, taskID int64) (addr, caCert string) {
	if taskID <= 0 {
		return "", ""
	}
	// 先在锁外解析「是否内网期」：该判定会读 DB（taskIntranet 查隧道/会话台账），
	// 持 m.mu 做 DB IO 会阻塞其它任务的上游重算。
	intranet := m.intranetPhase(taskID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.closing[taskID] != nil || ctx.Err() != nil {
		return "", ""
	}
	inst := m.proxies[taskID]
	if inst == nil {
		created, err := m.buildLocked(ctx, taskID, intranet)
		if err != nil {
			log.Printf("[taskproxy] 任务 %d MITM 实例创建失败(拒绝改用其他出口): %v", taskID, err)
			return "", ""
		}
		inst = created
		m.proxies[taskID] = inst
		log.Printf("[taskproxy] 任务 %d MITM 实例就绪 %s(CA %s)", taskID, inst.tr.ProxyAddrRedacted(), inst.tr.CACertPath())
	}
	if err := m.refreshLocked(ctx, taskID, inst, intranet); err != nil {
		return "", ""
	}
	return inst.tr.ProxyAddr(), inst.tr.CACertPath()
}

// ModeForTask reports the task's current upstream mode without building an
// instance (lazy-build semantics: 没跑过的任务不消耗端口)。未建实例时按当前
// 台账即时判定，使「隧道刚断、worker 还没跑」这一窗口也能得到 blocked。
func (m *taskProxyManager) ModeForTask(taskID int64) UpstreamMode {
	if taskID <= 0 {
		return UpstreamGlobal
	}
	m.mu.Lock()
	inst := m.proxies[taskID]
	if inst != nil {
		mode := inst.mode
		m.mu.Unlock()
		return mode
	}
	m.mu.Unlock()
	// 未建实例：按台账即时判定。resolveUpstream 读 m.intranet（在锁内拷贝），
	// 这里不持 m.mu，避免与 DB 查询/回调嵌套。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, mode, err := m.resolveUpstream(ctx, taskID, m.intranetPhase(taskID))
	if err != nil {
		return UpstreamBlocked // 路由来源未知，不能据此允许改用全局出口。
	}
	return mode
}

// buildLocked 建实例:分配端口 → traffic.Open(独立目录/CA)→ 装选路钩子 → 初始
// 上游 → 起监听 → 端口登记进审计台账。调用方必须持 m.mu；intranet 由调用方在
// 锁外解析后传入。
func (m *taskProxyManager) buildLocked(ctx context.Context, taskID int64, intranet bool) (*taskProxyInst, error) {
	used := map[int]bool{}
	for _, inst := range m.proxies {
		used[inst.port] = true
	}
	port, err := tunnel.AllocatePort(m.portMin, m.portMax, used, tunnel.CanListen)
	if err != nil {
		return nil, err
	}
	tr, err := traffic.Open(filepath.Join(m.dir, strconv.FormatInt(taskID, 10)),
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	tr.SetRecordEnabled(!m.recordOff)
	inst := &taskProxyInst{tr: tr, port: port, routes: tunnel.NewRouteTable()}
	// 选路钩子必须在 Start 之前装好：它决定每个请求走哪条隧道，装晚了第一批
	// 请求会全部落到回落上游（单隧道时看不出差别，双网段时就是「第一条路被
	// 塞满、第二条空转」——正是要修的那个毛病）。
	//
	// 两个回调都不碰 m.mu、不读 DB：它们跑在 MITM 的请求热路径上，RouteTable
	// 自带锁，台账刷新由 refreshLocked 在旁路做。
	tr.SetRouteSelector(inst.routes.SelectFeedback)
	if err := m.syncRoutesLocked(ctx, taskID, inst, intranet, true); err != nil {
		// 首建时上游无效就是不可用实例，关闭后由调用方停止执行。
		log.Printf("[taskproxy] 任务 %d 初始上游不可用: %v", taskID, err)
		_ = tr.Close()
		return nil, err
	}
	go func() {
		if err := tr.Start(); err != nil {
			log.Printf("[taskproxy] 任务 %d MITM 监听结束: %v", taskID, err)
		}
	}()
	registerManagedPort(port)
	return inst, nil
}

// syncRoutesLocked 读台账 → 重建路由表 → 重算回落上游 → 必要时热切换。
// 调用方持 m.mu；force=true 时无条件应用（首建，没有「上次的值」可比）。
//
// 一次台账读取同时喂两件事（路由表与回落上游），避免两处各读一次而在中间
// 出现「路由表已更新、回落上游还是旧的」的窗口。
//
// 错误语义刻意分两类：
//   - 台账读失败 → 清空已有路由并阻断出口，不能根据未知状态允许直连。
//   - 上游值非法 → 返回错误。首建时这就是不可用实例；热切换时调用方保持旧值。
func (m *taskProxyManager) syncRoutesLocked(ctx context.Context, taskID int64, inst *taskProxyInst, intranet bool, force bool) error {
	gp := ""
	if m.globalProxy != nil {
		gp = m.globalProxy()
	}
	recs, err := m.store.ListByTask(ctx, taskID)
	if err != nil {
		// Revoke stale route selectors as well as the fallback. Existing worker
		// clients may still hold this instance's address while the DB is down.
		if inst.routes != nil {
			inst.routes.Sync(nil)
		}
		_ = inst.tr.SetUpstreamProxy(blockedUpstream)
		inst.tr.SetPassthroughEnabled(false)
		inst.upstream, inst.mode = blockedUpstream, UpstreamBlocked
		return fmt.Errorf("任务 %d 路由台账不可读，已阻断请求: %w", taskID, err)
	}
	if inst.routes != nil {
		inst.routes.Sync(recs)
	}
	up, mode := pickUpstream(recs, gp, intranet)
	if !force && up == inst.upstream && mode == inst.mode {
		return nil
	}
	if err := inst.tr.SetUpstreamProxy(up); err != nil {
		return fmt.Errorf("上游 %q 无效: %w", redactURL(up), err)
	}
	// blocked 期间必须禁用透明透传：透传是「绕过上游直连目标」，与「禁止直连」
	// 的语义直接冲突（否则一次协议类错误就能让该 host 在 30 分钟内直连成功）。
	inst.tr.SetPassthroughEnabled(mode != UpstreamBlocked)
	// 内网期掉隧道是重要态势事件：显式告警，而不是静默降级成公网直连。
	if mode == UpstreamBlocked && inst.mode != UpstreamBlocked {
		log.Printf("[taskproxy] 任务 %d 处于内网期但无 alive 隧道，上游已置为阻断地址（禁止回落公网出口）", taskID)
	}
	if !force {
		log.Printf("[taskproxy] 任务 %d 回落上游热切换: %q(%s) → %q(%s)", taskID, redactURL(inst.upstream), inst.mode, redactURL(up), mode)
	}
	inst.upstream, inst.mode = up, mode
	return nil
}

// refreshLocked 重算并热切换单实例上游/路由表(变化才 SetUpstreamProxy)。调用方持 m.mu。
// intranet 由调用方在解锁后解析好再传入（见 ForTask）。
func (m *taskProxyManager) refreshLocked(ctx context.Context, taskID int64, inst *taskProxyInst, intranet bool) error {
	if err := m.syncRoutesLocked(ctx, taskID, inst, intranet, false); err != nil {
		log.Printf("[taskproxy] 任务 %d 路由刷新失败: %v", taskID, err)
		return err
	}
	return nil
}

// Refresh 在隧道状态变化(新增/死亡/重拉/teardown)时热切换该任务实例上游。
// 实例不存在时不建(懒建语义:只有 worker 真跑才消耗端口与实例)。
func (m *taskProxyManager) Refresh(ctx context.Context, taskID int64) {
	// 锁外解析内网期（回调读 DB），再进锁做热切换。
	intranet := m.intranetPhase(taskID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst := m.proxies[taskID]; inst != nil {
		m.refreshLocked(ctx, taskID, inst, intranet)
	}
}

// RefreshAll 重算全部实例上游(全局出口代理变更时,无隧道任务的回落上游随之变)。
func (m *taskProxyManager) RefreshAll(ctx context.Context) {
	// 先在锁外把每个任务的内网期判定算完，避免持锁期间做 DB IO。
	m.mu.Lock()
	ids := make([]int64, 0, len(m.proxies))
	for id := range m.proxies {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	phases := make(map[int64]bool, len(ids))
	for _, id := range ids {
		phases[id] = m.intranetPhase(id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		if inst := m.proxies[id]; inst != nil {
			m.refreshLocked(ctx, id, inst, phases[id])
		}
	}
}

// UpstreamForTaskHost 为某个目标 host 选一条任务隧道，返回 socks 上游地址。
//
// 选路复用 RouteTable.Select：它会顺带把「这个 host 试过哪条路」计入证据，
// 与流量层共享同一份判断。没有可用隧道时返回空串，调用方按直连处理。
//
// ⚠️ 空串有歧义（「外网阶段直连」与「内网期被阻断」都是空串），横向执行这类
// 动作应改用 RouteForTaskHost——它会明确告诉调用方 blocked，避免在「隧道刚断、
// worker 还没跑过」的窗口里从平台直连内网目标。
func (m *taskProxyManager) UpstreamForTaskHost(taskID int64, host string) string {
	return m.RouteForTaskHost(taskID, host).Upstream
}

// TaskRoute 是非 HTTP 受管动作（SSH 横向、impacket 横向、可达性探测）的选路结论。
type TaskRoute struct {
	// Upstream 是 socks 上游；空串只在 Mode=global 时出现（直连是合法形态）。
	Upstream string
	// Mode 说明上游怎么选出来的：tunnel（经任务隧道）/ global（外网阶段直连
	// 或全局出口）/ blocked（内网期无存活隧道，禁止直连）。
	Mode UpstreamMode
}

// RouteForTaskHost 为某个目标 host 给出带模式的选路结论。
//
// 与 UpstreamForTaskHost 的差别在「实例还没建」的窗口：此前该窗口一律返回
// 空串（调用方当直连），于是「隧道刚断、worker 还没跑过任何流量」时，SSH
// 横向会从平台出口直连内网目标——既打不中又暴露平台位置。现在没有实例时
// 按台账即时判定（与 ModeForTask 同一套 fallback 语义）：
//   - 有 alive socks → 给回落上游（id 最大那条），Mode=tunnel；
//   - 内网期无存活隧道 → Mode=blocked，调用方必须拒绝执行而不是直连；
//   - 外网阶段 → Mode=global，直连合法。
//
// 调用方必须处理 blocked：这是「禁止从平台直连内网」的技术约束出口之一，
// 与 worker 侧 blockedUpstream（MITM 层必败地址）互补——这里没有 MITM 可挡，
// 拒绝只能由调用方执行。
func (m *taskProxyManager) RouteForTaskHost(taskID int64, host string) TaskRoute {
	if taskID <= 0 {
		return TaskRoute{Mode: UpstreamGlobal}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	intranet := m.intranetPhase(taskID)
	m.mu.Lock()
	if inst := m.proxies[taskID]; inst != nil {
		// Revalidate before selecting: a cached route is not proof that the
		// tunnel is still alive or that its ledger is available.
		if err := m.syncRoutesLocked(ctx, taskID, inst, intranet, false); err != nil {
			m.mu.Unlock()
			return TaskRoute{Upstream: blockedUpstream, Mode: UpstreamBlocked}
		}
		if inst.routes != nil {
			if up, _, ok := inst.routes.Select(host); ok {
				m.mu.Unlock()
				return TaskRoute{Upstream: up, Mode: UpstreamTunnel}
			}
		}
		route := TaskRoute{Upstream: inst.upstream, Mode: inst.mode}
		m.mu.Unlock()
		return route
	}
	m.mu.Unlock()
	up, mode, err := m.resolveUpstream(ctx, taskID, intranet)
	if err != nil {
		return TaskRoute{Upstream: blockedUpstream, Mode: UpstreamBlocked}
	}
	return TaskRoute{Upstream: up, Mode: mode}
}

// RoutesForTask 返回任务路由表的快照。未建实例时返回 nil——路由表的证据全部
// 来自实际流量，worker 还没跑过就没有证据可言；这里不返回「空但有结构」的
// 对象，是为了让调用方能区分「没有证据」和「证据为零」。
func (m *taskProxyManager) RoutesForTask(taskID int64) []tunnel.RouteView {
	m.mu.Lock()
	inst := m.proxies[taskID]
	m.mu.Unlock()
	if inst == nil || inst.routes == nil {
		return nil
	}
	return inst.routes.Snapshot()
}

// ResponseBodyForTask 按任务流量实例取一条已录交换的响应原文（证据切片锚定用）。
//
// 读序：任务实例（worker 流量的录制处）→ 进程级全局实例（旧版本/未开任务代理
// 时流量的落点）兜底。两处都取不到才报错——锚定调用方据此拒绝引用，正好把
// 「引用了根本不存在的流量」拦在上报之前。
func (m *taskProxyManager) ResponseBodyForTask(ctx context.Context, taskID int64, trafficID string) (string, error) {
	_, resp, err := m.ExchangeForTask(ctx, taskID, trafficID)
	return resp, err
}

// ExchangeForTask 取一条已录交换的请求/响应原文（http_replay 的基线来源）。
// 读序与 ResponseBodyForTask 相同；返回的请求原文是录制层拼装的 raw 文本
// （请求行 + 头 + 空行 + 正文），由调用方解析。
func (m *taskProxyManager) ExchangeForTask(ctx context.Context, taskID int64, trafficID string) (req, resp string, err error) {
	if taskID <= 0 || trafficID == "" {
		return "", "", fmt.Errorf("无效参数 task=%d id=%q", taskID, trafficID)
	}
	err = m.withTraffic(ctx, taskID, func(tr *traffic.Traffic) error {
		q, r, gerr := tr.Get(trafficID)
		if gerr != nil {
			return gerr
		}
		req, resp = q, r
		return nil
	})
	if err == nil && req == "" {
		// 任务实例里没有：回退进程级全局实例（老任务/未开任务代理时的录制落点）。
		if m.globalTraffic != nil {
			if q, r, gerr := m.globalTraffic.Get(trafficID); gerr == nil {
				return q, r, nil
			}
		}
	}
	return req, resp, err
}

// CloseTask 销毁任务实例:停监听、关索引、端口注销;CA 目录保留(录制的留痕
// 数据不随实例销毁)。幂等。
func (m *taskProxyManager) CloseTask(taskID int64) {
	m.mu.Lock()
	if done := m.closing[taskID]; done != nil {
		m.mu.Unlock()
		<-done
		return
	}
	inst := m.proxies[taskID]
	if inst == nil {
		m.mu.Unlock()
		return
	}
	if m.closing == nil {
		m.closing = map[int64]chan struct{}{}
	}
	done := make(chan struct{})
	m.closing[taskID] = done
	// A leased store can keep the listener alive while CloseTask drains readers.
	// Stop recording before removing it from the set covered by capture toggles.
	inst.tr.SetRecordEnabled(false)
	delete(m.proxies, taskID)
	m.mu.Unlock()
	inst.readers.Wait()
	unregisterManagedPort(inst.port)
	if err := inst.tr.Close(); err != nil {
		log.Printf("[taskproxy] 任务 %d MITM 实例关闭: %v", taskID, err)
	}
	m.mu.Lock()
	delete(m.closing, taskID)
	close(done)
	m.mu.Unlock()
}

// Close 进程退出时全清(幂等)。
func (m *taskProxyManager) Close() {
	m.mu.Lock()
	m.closed = true
	ids := make([]int64, 0, len(m.proxies)+len(m.closing))
	for id := range m.proxies {
		ids = append(ids, id)
	}
	for id := range m.closing {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.CloseTask(id)
	}
}

// withTraffic never creates a proxy. Active stores are leased so CloseTask waits
// for an evidence copy; finished-task history is opened read-only and released.
func (m *taskProxyManager) withTraffic(ctx context.Context, taskID int64, fn func(*traffic.Traffic) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.mu.Lock()
		if done := m.closing[taskID]; done != nil {
			m.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if inst := m.proxies[taskID]; inst != nil {
			inst.readers.Add(1)
			m.mu.Unlock()
			defer inst.readers.Done()
			return fn(inst.tr)
		}
		m.mu.Unlock()
		tr, err := traffic.OpenReader(ctx, filepath.Join(m.dir, strconv.FormatInt(taskID, 10)))
		if err != nil {
			return err
		}
		defer tr.Close()
		return fn(tr)
	}
}
