// route.go 是任务级路由表：哪条隧道能到哪个 host。
//
// 为什么需要它：此前一个任务只有一个上游——`pickUpstream` 在全部 alive socks
// 隧道里取 id 最大的那条（`r.ID <= bestID` 的单调取大），其余隧道闲置。于是
// 「有两个网段」时，全部流量被塞进其中一条：进得去的那段能用，进不去的那段
// 每次失败，而失败原因看起来是「目标不通」——真正的原因是「选错了路」。
//
// 本文件只做一件事：把「选哪条隧道」从「谁最新」改成「谁到得了这个 host」。
// 证据来源是**已经发生的流量**，不是新增的探测动作——worker 照旧发请求，
// 路由表在旁边把「这个 host 经这条隧道成没成」记下来。所以这是能力提升，
// 不是新增触发：agent 不需要知道路由表存在，同样的 curl 产出更多。
//
// 两条设计判断（都是「不做什么」比「做什么」更重要）：
//
//  1. 路由表是派生物，不是真相源。真相源是 tunnels 台账；路由表只缓存「可达性
//     观测」。删掉它不影响任何隧道，只是选路精度退回「取 id 最大」。反过来，
//     如果把 route 做成一张独立表并允许它和台账不一致，就会出现两个互相矛盾
//     的真相——那是这类系统最常见的坏味道。
//
//  2. 失败不作为排除依据。连接失败可能是目标端口没开、可能是瞬时抖动、可能是
//     防火墙刚好在那一刻丢包。一旦据此把某条路永久排除，隧道恢复后也不会再被
//     尝试（锁死），而系统不会告诉你「我把它排除了」。所以这里只做两件事：
//     成功的粘住，没成功过的轮换。轮换的代价最多是「每条路各浪费一次连接」，
//     换来的是「不需要任何额外探测就能发现第二条路」。
package tunnel

import (
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"chengjian.local/chengjian/db"
)

// RouteOutcome 是一条路由观测的结论。
type RouteOutcome string

const (
	// RouteOK 拿到响应：路由到得了这个 host。
	RouteOK RouteOutcome = "ok"
	// RouteRefused 对端拒连。这同样是**正面**证据：RST 说明包已经到达对端，
	// 是目标端口没开，不是路不通。把它算成失败会把「端口没开」误判成
	// 「这条路到不了」——两者后续该做的事完全不同。
	RouteRefused RouteOutcome = "refused"
	// RouteUnreach 网络层不可达（network unreachable / no route / 超时）：
	// 负面证据，但不用于排除，只用于排优先级。
	RouteUnreach RouteOutcome = "unreach"
	// RouteUnknown 无法归类的错误（TLS 握手、协议错误、对端重置等）：
	// 既不能证明路通，也不能证明路不通，不作为证据。
	RouteUnknown RouteOutcome = "unknown"
)

// ClassifyRouteError 把传输层错误归类为路由证据。纯函数。
//
// 判据只认网络层的措辞——这里刻意不做「猜测式」匹配：把不认识的情况归到
// RouteUnknown 而不是 RouteUnreach，是因为误判成「不可达」会让选路在两条路
// 之间来回抖，而误判成「未知」只会让它继续轮换，代价更小。
func ClassifyRouteError(err error) RouteOutcome {
	if err == nil {
		return RouteOK
	}
	if errors.Is(err, net.ErrClosed) {
		return RouteUnknown
	}
	s := strings.ToLower(err.Error())
	// 先判拒连：socks5 的 "connection refused"、Windows 的
	// "No connection could be made because the target machine actively refused it"、
	// Go 的 "connectex: ..." 都归这里。
	for _, p := range []string{"connection refused", "actively refused", "econnrefused"} {
		if strings.Contains(s, p) {
			return RouteRefused
		}
	}
	// DNS 解析失败不是路由证据：socks5 的目标地址由平台侧解析（go-mitmproxy 把
	// 地址 canonical 化后交给代理），解析不到说明的是「这个名字不存在」，不是
	// 「这条隧道到不了这台机器」。必须排在 "dial tcp" 之前，否则会被误归到
	// unreach——那会让选路为了一个不存在的名字在两条路之间反复轮换。
	for _, p := range []string{"no such host", "lookup ", "server misbehaving", "temporary failure in name resolution"} {
		if strings.Contains(s, p) {
			return RouteUnknown
		}
	}
	for _, p := range []string{
		"network is unreachable", "no route to host", "host is unreachable",
		"cannot assign requested address", "network unreachable", "host unreachable",
		"i/o timeout", "connection timed out", "dial tcp", "socks connect",
		"proxyconnect", "context deadline exceeded",
	} {
		if strings.Contains(s, p) {
			return RouteUnreach
		}
	}
	return RouteUnknown
}

// hostEvidence 是 (隧道, host) 这一对的观测计数。
type hostEvidence struct {
	tries    int       // 选路次数（含尚未回报结果的）
	ok       int       // 正面证据（拿到响应或被拒连）
	fail     int       // 负面证据（不可达）
	lastOK   time.Time // 最近一次正面证据的时间（粘住时取最新的那条）
	lastFail time.Time
}

const routeEvidenceTTL = 5 * time.Minute

func positiveEvidence(ev *hostEvidence, now time.Time) bool {
	return ev != nil && ev.ok > 0 && now.Sub(ev.lastOK) < routeEvidenceTTL && ev.lastFail.Before(ev.lastOK)
}

// routeEntry 是一条隧道的运行期视图。
type routeEntry struct {
	TunnelID int64
	Adapter  string
	Upstream string // socks5://[user:pass@]127.0.0.1:port
	HopDepth int
	// verified 是台账 reach_verified_ranges 解析出的网段（tunnel_reach / 部署收尾
	// 探测的**实测**结论，不是声明）。Select 对落在这些网段里的 IP host 直接
	// 优先选这条隧道——这是「声明驱动选路」与「事后流量学习」的合流点。
	verified   []*net.IPNet
	verifiedAt time.Time
	ev         map[string]*hostEvidence
}

// RouteView 是路由的运行期快照。
//
// 用途分两档，别混：ProvenHosts 是 host 级明细，只给人类界面（复盘「两条隧道
// 是不是真的都在用」）；agent 侧只暴露计数（见 server.routeRowFields）——
// 把 host 清单喂给 planner/worker 会被读成「可达目标白名单」，那才是真正的
// 限定范围。
type RouteView struct {
	TunnelID    int64    `json:"tunnel_id"`
	Adapter     string   `json:"adapter"`
	HopDepth    int      `json:"hop_depth,omitempty"`
	ProvenHosts []string `json:"proven_hosts,omitempty"` // 已实测到得了的 host
	TriedHosts  int      `json:"tried_hosts,omitempty"`  // 试过多少个 host（含未成功）
	// VerifiedRanges 是台账实测网段（reach_verified_ranges 原文），供人类界面
	// 对账「选路用的声明证据来自哪次探测」。
	VerifiedRanges []string `json:"verified_ranges,omitempty"`
}

// RouteTable 是任务级路由表。零值不可用，用 NewRouteTable 构造；方法并发安全。
type RouteTable struct {
	mu     sync.Mutex
	order  []int64 // tunnel id 升序，保证选路的确定性
	routes map[int64]*routeEntry
}

// NewRouteTable 构造空表。
func NewRouteTable() *RouteTable {
	return &RouteTable{routes: map[int64]*routeEntry{}}
}

// Sync 按台账重建路由集合。
//
// 只收 alive 的 socks 隧道：portfwd 是「一个端口」不是「一条路」，dead/error
// 的隧道不承载流量。存活隧道的观测证据保留（重建时清零会让粘住的 host 抖动）；
// 消失的隧道证据随之丢弃（重新部署会拿到新 id，旧证据对新隧道无意义）。
func (rt *RouteTable) Sync(recs []*db.TunnelRecord) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	next := map[int64]*routeEntry{}
	order := make([]int64, 0, len(recs))
	for _, r := range recs {
		if r == nil || r.State != db.TunnelAlive || r.Kind != KindSocks {
			continue
		}
		up := upstreamOf(r)
		if up == "" {
			continue // deploy_params 缺 socks_port：没有入口就不是一条路
		}
		e := &routeEntry{
			TunnelID: r.ID, Adapter: r.Adapter, Upstream: up,
			HopDepth: planHopDepth(r), verified: parseVerifiedRanges(r.ReachVerifiedRanges),
			ev: map[string]*hostEvidence{},
		}
		if r.ReachVerifiedAt != nil {
			e.verifiedAt = *r.ReachVerifiedAt
		}
		if old := rt.routes[r.ID]; old != nil && old.Upstream == up {
			old.Adapter, old.HopDepth = e.Adapter, e.HopDepth
			old.verified, old.verifiedAt = e.verified, e.verifiedAt
			e = old // Keep identity only for the same live route endpoint.
		}
		next[r.ID] = e
		order = append(order, r.ID)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	rt.routes = next
	rt.order = order
}

// Len 返回当前可路由的隧道数（0 = 无路，调用方应回落到全局出口或阻断）。
func (rt *RouteTable) Len() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.order)
}

// SelectUpstream 是给流量层用的选路回调：给定目标 host 返回上游 URL。
// 返回空串表示「没有意见」，由调用方回落到全局上游——这样「无隧道」「隧道
// 全死」两种情况的语义完全交给既有逻辑（含内网期阻断），不在两处各写一遍。
func (rt *RouteTable) SelectUpstream(host string) (string, error) {
	up, _, ok := rt.Select(host)
	if !ok {
		return "", nil
	}
	return up, nil
}

// Select 为目标 host 选一条隧道，返回其上游、隧道 id 与是否选中。
//
// 选路规则（确定性，无随机）：
//
//  0. **实测网段优先（声明驱动）**：host 是 IP 且落在某条隧道的
//     reach_verified_ranges（tunnel_reach/部署收尾的真实探测结论）里 → 在
//     命中的隧道里选一条（hop 浅的、id 小的）。冷启动期每个新 host 都要在
//     错误隧道上白打一次连接才能学到路——对目标侧 IDS 就是扫描噪音，这与
//     tunnel_reach「采样不扫全段防触发 IDS」的初衷自相矛盾；实测网段正是
//     已经付过这次代价的结论，选路直接复用它。
//  1. 有正面证据（该 host 经某条隧道成功过或被拒连过）→ 取最近成功的那条。
//     这是「粘住」：一个 host 一旦被证明走某条路通，就不再动它，避免抖动。
//  2. 否则 → 取该 host **尝试次数最少**的那条；并列取 hop 深度小的（少一跳
//     少一份可用性损耗），再并列取 tunnel_id 小的。
//
// 规则 2 的效果就是「轮换」：两条隧道都没试过时第一次落在 id 小的那条，失败后
// 它的 tries 变成 1，第二次自然换到另一条；成功即按规则 1 粘住。于是两个网段
// 各自被钉到能到它的那条隧道上，可以同时推进——而总代价最多是「每条错路各
// 浪费一次连接」，不需要任何额外的探测动作。
func (rt *RouteTable) Select(host string) (upstream string, tunnelID int64, ok bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.selectLocked(host)
}

// SelectFeedback binds the completion callback to this exact route incarnation.
// Concurrent exchanges to the same host never share a mutable "last route" slot.
func (rt *RouteTable) SelectFeedback(host string) (string, func(error), error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	up, id, ok := rt.selectLocked(host)
	if !ok {
		return "", nil, nil
	}
	entry := rt.routes[id]
	return up, func(err error) {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if rt.routes[id] == entry {
			rt.observeLocked(host, id, err)
		}
	}, nil
}

func (rt *RouteTable) selectLocked(host string) (upstream string, tunnelID int64, ok bool) {
	if len(rt.order) == 0 {
		return "", 0, false
	}
	host = normalizeHost(host)
	now := time.Now()
	// 规则 0：实测网段优先。只对 IP host 生效——CIDR 无法匹配主机名，而平台
	// 侧拿到主机名时也没法替目标侧做反向解析。命中多条时取 hop 浅、id 小的，
	// 与规则 2 的并列判据一致。
	if ip := net.ParseIP(host); ip != nil {
		var pick *routeEntry
		for _, id := range rt.order {
			e := rt.routes[id]
			if e == nil || e.verifiedAt.IsZero() || now.Sub(e.verifiedAt) >= routeEvidenceTTL || !entryContainsIP(e, ip) {
				continue
			}
			if ev := e.ev[host]; ev != nil && !ev.lastFail.IsZero() && !ev.lastFail.Before(e.verifiedAt) && !positiveEvidence(ev, now) {
				continue
			}
			if pick == nil || e.HopDepth < pick.HopDepth || (e.HopDepth == pick.HopDepth && e.TunnelID < pick.TunnelID) {
				pick = e
			}
		}
		if pick != nil {
			ev := pick.ev[host]
			if ev == nil {
				ev = &hostEvidence{}
				pick.ev[host] = ev
			}
			ev.tries++
			return pick.Upstream, pick.TunnelID, true
		}
	}
	var best *routeEntry
	var bestEv *hostEvidence
	for _, id := range rt.order {
		e := rt.routes[id]
		if e == nil {
			continue
		}
		ev := e.ev[host]
		if ev == nil {
			ev = &hostEvidence{}
			e.ev[host] = ev
		}
		if best == nil {
			best, bestEv = e, ev
			continue
		}
		if preferRoute(e, ev, best, bestEv) {
			best, bestEv = e, ev
		}
	}
	if best == nil {
		return "", 0, false
	}
	bestEv.tries++
	return best.Upstream, best.TunnelID, true
}

// entryContainsIP 报告隧道的实测网段是否包含该 IP。
func entryContainsIP(e *routeEntry, ip net.IP) bool {
	for _, n := range e.verified {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// parseVerifiedRanges 把台账 reach_verified_ranges（CIDR 字符串数组）解析成网段。
//
// 非法条目跳过而不是整体失败：这份 JSON 是探测收尾写的，一条坏数据不该让
// 整条隧道的实测证据失效。空/非数组返回 nil。
func parseVerifiedRanges(raw json.RawMessage) []*net.IPNet {
	if len(raw) == 0 {
		return nil
	}
	var cidrs []string
	if err := json.Unmarshal(raw, &cidrs); err != nil {
		return nil
	}
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(strings.TrimSpace(c))
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// preferRoute 判定 a 是否应优先于 b（纯函数，便于单测与阅读）。
func preferRoute(a *routeEntry, ae *hostEvidence, b *routeEntry, be *hostEvidence) bool {
	now := time.Now()
	aOK, bOK := positiveEvidence(ae, now), positiveEvidence(be, now)
	if aOK != bOK {
		return aOK // 有正面证据的优先
	}
	if aOK && bOK {
		if !ae.lastOK.Equal(be.lastOK) {
			return ae.lastOK.After(be.lastOK) // 最近证明过的优先
		}
	} else if ae.tries != be.tries {
		return ae.tries < be.tries // 都没证明过：试得少的优先（轮换）
	}
	if a.HopDepth != b.HopDepth {
		return a.HopDepth < b.HopDepth
	}
	return a.TunnelID < b.TunnelID
}

// ObserveRoute requires the route ID returned by Select for this exchange.
// Production callers use SelectFeedback, which also rejects stale incarnations.
func (rt *RouteTable) ObserveRoute(host string, id int64, err error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.observeLocked(host, id, err)
}

func (rt *RouteTable) observeLocked(host string, id int64, err error) {
	host = normalizeHost(host)
	if host == "" {
		return
	}
	outcome := ClassifyRouteError(err)
	if outcome == RouteUnknown {
		return // 不能作为证据的错误：不记，也就不会影响选路
	}
	e := rt.routes[id]
	if e == nil {
		return
	}
	ev := e.ev[host]
	if ev == nil {
		return // Unrouted traffic must not create evidence.
	}
	switch outcome {
	case RouteOK, RouteRefused:
		ev.ok++
		ev.lastOK = time.Now()
	case RouteUnreach:
		ev.fail++
		ev.lastFail = time.Now()
	}
}

// Snapshot 返回全部路由的对外视图（tunnel_list 用）。按 tunnel id 升序。
func (rt *RouteTable) Snapshot() []RouteView {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]RouteView, 0, len(rt.order))
	for _, id := range rt.order {
		e := rt.routes[id]
		if e == nil {
			continue
		}
		v := RouteView{TunnelID: e.TunnelID, Adapter: e.Adapter, HopDepth: e.HopDepth}
		for _, n := range e.verified {
			v.VerifiedRanges = append(v.VerifiedRanges, n.String())
		}
		for h, ev := range e.ev {
			if positiveEvidence(ev, time.Now()) {
				v.ProvenHosts = append(v.ProvenHosts, h)
			}
			if ev.tries > 0 {
				v.TriedHosts++
			}
		}
		sort.Strings(v.ProvenHosts)
		sort.Strings(v.VerifiedRanges)
		out = append(out, v)
	}
	return out
}

// normalizeHost 统一 host 形态：小写、去掉端口与尾部点，IP 归一化。
// 「10.0.0.8」「10.0.0.8:8080」「10.0.0.8.」必须落到同一个 key，否则同一台
// 主机会被当成三个不同目标，粘不住也轮换不起来。
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return h
}

// upstreamOf 从台账行还原 socks5 上游（suo5 入口带口令，必须内嵌）。
// 与 server 侧 pickUpstream 的构造规则一致——两处都在描述同一个事实
// 「这条隧道的入口在哪」，所以都从 deploy_params 读，不各自维护。
func upstreamOf(r *db.TunnelRecord) string {
	var p Plan
	if err := json.Unmarshal(r.DeployParams, &p); err != nil || p.SocksPort <= 0 {
		return ""
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.SocksPort))
	if r.Adapter == AdapterSuo5 && p.AuthUser != "" {
		return "socks5://" + p.AuthUser + ":" + p.AuthPass + "@" + addr
	}
	return "socks5://" + addr
}

// planHopDepth 读多级隧道的层级（缺省 1 = 单跳）。单跳优于多跳：每多一跳
// 延迟叠加、可用性相乘。
func planHopDepth(r *db.TunnelRecord) int {
	var p Plan
	if err := json.Unmarshal(r.DeployParams, &p); err != nil || p.HopDepth <= 0 {
		return 1
	}
	return p.HopDepth
}
