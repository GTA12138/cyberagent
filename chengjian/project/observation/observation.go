// Package observation 把「内网看到的一件事」变成可核验实体（I3 / T-13）。
//
// 在此之前，侦察结果只有一个出口：一段 JSON 塞进 exploration_nodes 的 payload，
// 加上一句 "confidence":"observed" 的字符串。这带来三个具体问题：
//
//  1. **hash 不能替代原文**。只有摘要没有原文时，「这条结论到底是从哪句输出读出来的」
//     无法回答；复核只能重跑命令，而重跑得到的是另一次观测。
//  2. **证据等级是自由文本**。`observed` / `inferred` 写在 payload 里，没有任何
//     代码在写之前检查「你说 observed，凭什么」，也没有任何东西在它过期后把它降级。
//  3. **拓扑没有时效**。邻居表十分钟前说有这台机器、监听表半小时前说开了 445，
//     这两件事在决策时被当成同等新鲜——而内网里它们的新鲜度差一个数量级。
//
// 本包只做纯规则（零 IO）：证据等级、时效、提升条件、边界判据。落库在
// db/observations.go，原文留存与读回在 observation/artifact.go。
package observation

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// Evidence 是证据等级。三者不是「置信度高低」，而是**获取方式的种类**：
//
//	observed — 这次观测直接看到了（命令输出里有这一行）
//	inferred — 从别的观测推出来的（有静态路由 ⇒ 那个网段大概通）
//	verified — 有一次独立的、专门针对它的探测确认过（另一次观测支持它）
//
// 分开的理由：`inferred` 可以被后续观测推翻而不算矛盾，`observed` 被推翻说明
// 环境变了，`verified` 被推翻说明之前那次探测本身有问题。三者的处置完全不同。
type Evidence string

const (
	EvidenceObserved Evidence = "observed"
	EvidenceInferred Evidence = "inferred"
	EvidenceVerified Evidence = "verified"
)

// Valid 判断证据等级是否是已知取值。未知取值一律拒绝写入——自由文本正是
// 之前那套问题的根源，不能在新结构里留一个同样的口子。
func (e Evidence) Valid() bool {
	switch e {
	case EvidenceObserved, EvidenceInferred, EvidenceVerified:
		return true
	}
	return false
}

// Rank 给证据等级排序，用于「只升不降」的比较。
// verified > observed > inferred：一次专门探测比一条顺带看到的输出强，
// 一条顺带看到的输出比一条推断强。
func (e Evidence) Rank() int {
	switch e {
	case EvidenceVerified:
		return 3
	case EvidenceObserved:
		return 2
	case EvidenceInferred:
		return 1
	}
	return 0
}

// EdgeKind 是拓扑边的种类。分开建模的理由：网卡上的监听端口是**本机事实**，
// 邻居是**二层事实**，路由是**配置事实**，可达性是**探测事实**。把它们混成
// 一张「主机-主机」图，会让「这台机器 445 开着」和「从我的立足点能连到 445」
// 变成同一条边——而后者才是能不能打的判据。
type EdgeKind string

const (
	EdgeNIC      EdgeKind = "nic"      // 会话宿主 ↔ 它的一个 IP
	EdgeNeighbor EdgeKind = "neighbor" // 二层邻居（ARP/ip neigh）
	EdgeRoute    EdgeKind = "route"    // 路由表里的一个目的网段
	EdgeListener EdgeKind = "listener" // 某 IP 上的一个监听端口
	EdgePeer     EdgeKind = "peer"     // 已建立连接的对端
	EdgeReach    EdgeKind = "reach"    // 从某处实测可达（唯一能支撑「能打」的边）
)

// Valid 判断边种类是否已知。
func (k EdgeKind) Valid() bool {
	switch k {
	case EdgeNIC, EdgeNeighbor, EdgeRoute, EdgeListener, EdgePeer, EdgeReach:
		return true
	}
	return false
}

// TTL 是各类边的有效期。
//
// 数值不是拍的，是按「这类事实在内网里多久会变」定的：ARP 表几分钟就过期，
// 路由表通常几小时不变，网卡配置可能几天不变，可达性受链路抖动影响最大。
// 过期的边不会被删除，而是标记为 stale——删除会让「曾经有过」这段历史消失，
// 而历史恰恰是判断「环境变了还是当时看错了」的唯一依据。
func (k EdgeKind) TTL() time.Duration {
	switch k {
	case EdgePeer:
		return 5 * time.Minute
	case EdgeNeighbor:
		return 10 * time.Minute
	case EdgeListener:
		return 15 * time.Minute
	case EdgeReach:
		return 10 * time.Minute
	case EdgeRoute:
		return 30 * time.Minute
	case EdgeNIC:
		return 2 * time.Hour
	}
	return 10 * time.Minute
}

// Observation 是一次采集的可核验记录。
//
// 关键字段为什么必须存在：
//   - ObserverSessionID：同一条结论从不同立足点看可能不同（NAT、ACL、分区）。
//     没有它就无法回答「这是谁看到的」。
//   - Route：同一次采集可能经过不同出口。路由变了，旧结论的适用范围就变了。
//   - Command：证据是「这条命令的这段输出」，不是「我大概记得」。
//   - ArtifactRef + ArtifactHash：原文可读回，hash 只用于校验没被篡改。
//   - BoundAddr：从哪个地址看到的。loopback 与内网卡看到的监听表不是一回事。
//   - Confidence：这条观测自身的可信度（输出完整/被截断/命令部分失败）。
type Observation struct {
	ID               int64
	TaskID           int64
	Kind             string // recon:nics / recon:routes / probe:reach / …
	ObserverSession  int64
	Route            string // 出口描述（隧道 id / 直连 / 任务代理），空 = 未知
	Command          string
	ArtifactRef      string // 原文引用，必须可读回
	ArtifactHash     string // sha256 hex
	ArtifactBytes    int64
	BoundAddr        string // 观测发起侧地址，空 = 未知
	Confidence       string // observed / inferred / verified 之外的**采集质量**：complete / truncated / partial
	CollectedAt      time.Time
	Notes            string
}

// 采集质量取值。与 Evidence 分开：Evidence 说「这条结论怎么来的」，
// Quality 说「这次采集本身完整不完整」。一次被 64KB 截断的输出仍然是
// observed，但它支撑不了「没有别的监听端口」这种否定结论。
const (
	QualityComplete  = "complete"
	QualityTruncated = "truncated"
	QualityPartial   = "partial"
)

// Validate 在落库前检查观测是否可核验。拒绝写入比写入一条不可核验的记录好：
// 后者会在半年后变成一条谁都解释不了的「历史结论」。
func (o Observation) Validate() error {
	if o.TaskID <= 0 {
		return fmt.Errorf("observation: 缺少任务归属")
	}
	if strings.TrimSpace(o.Kind) == "" {
		return fmt.Errorf("observation: 缺少 kind")
	}
	if strings.TrimSpace(o.ArtifactRef) == "" || strings.TrimSpace(o.ArtifactHash) == "" {
		// hash 不能替代原文：没有 artifact_ref 的观测等于只有一句「我看到过」。
		return fmt.Errorf("observation: 缺少 artifact_ref/artifact_hash（hash 不能替代可读回的原文）")
	}
	switch o.Confidence {
	case QualityComplete, QualityTruncated, QualityPartial:
	case "":
		return fmt.Errorf("observation: 缺少采集质量（complete/truncated/partial）")
	default:
		return fmt.Errorf("observation: 未知采集质量 %q", o.Confidence)
	}
	return nil
}

// Edge 是拓扑边。
type Edge struct {
	ID            int64
	TaskID        int64
	ObservationID int64
	Kind          EdgeKind
	From          string
	To            string
	Evidence      Evidence
	ObservedAt    time.Time
	ValidUntil    time.Time
	Note          string
}

// Stale 判断边是否已过期（相对给定时刻）。
func (e Edge) Stale(now time.Time) bool {
	if e.ValidUntil.IsZero() {
		return true // 没有有效期的边不该被当成新鲜事实
	}
	return !now.Before(e.ValidUntil)
}

// Fresh 是 Stale 的反面，读起来更顺。
func (e Edge) Fresh(now time.Time) bool { return !e.Stale(now) }

// Validate 检查一条边能不能写。
func (e Edge) Validate() error {
	if e.TaskID <= 0 {
		return fmt.Errorf("edge: 缺少任务归属")
	}
	if !e.Kind.Valid() {
		return fmt.Errorf("edge: 未知边类型 %q", e.Kind)
	}
	if !e.Evidence.Valid() {
		return fmt.Errorf("edge: 未知证据等级 %q", e.Evidence)
	}
	if strings.TrimSpace(e.From) == "" || strings.TrimSpace(e.To) == "" {
		return fmt.Errorf("edge: 缺少端点（from=%q to=%q）", e.From, e.To)
	}
	if e.ObservationID <= 0 {
		return fmt.Errorf("edge: 缺少来源观测（无来源的边无法复核）")
	}
	return nil
}

// WithTTL 按边类型补上有效期。调用方不该自己算 TTL——每处各算一份，
// 迟早出现一处用小时、一处用秒。
func (e Edge) WithTTL(from time.Time) Edge {
	if from.IsZero() {
		from = time.Now()
	}
	e.ObservedAt = from
	e.ValidUntil = from.Add(e.Kind.TTL())
	return e
}

// Promote 判断能否把已有边的证据等级提升到 want。
//
// 规则只有一条硬约束：**inferred 永远不能直接变成 verified**。
// 推断被验证之前必须先有一次 observed（看到了实物），再由独立探测确认。
// 允许 inferred→verified 的后果是「静态路由指向 10.0.0.0/24」这条推断，
// 可以被另一次同样基于路由表的推断「确认」，于是图里出现一条看起来
// 已验证、实际从没有人连过的事实。
func Promote(cur, want Evidence) (Evidence, bool) {
	if !cur.Valid() || !want.Valid() {
		return cur, false
	}
	if cur == want {
		return cur, false
	}
	if want.Rank() <= cur.Rank() {
		return cur, false // 只升不降：重测未复现不该抹掉已有的强证据
	}
	if cur == EvidenceInferred && want == EvidenceVerified {
		return cur, false
	}
	return want, true
}

// IsLoopback 判断地址是否是回环。监听在回环上的服务**不能**变成远程可达事实——
// 这是 I3 验收里明确的一条：「loopback-only 服务不会变成远程可达」。
func IsLoopback(addr string) bool {
	host := hostOnly(addr)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return a.IsLoopback()
}

// ListenerScope 描述一个监听端口的可及范围。
type ListenerScope string

const (
	// ScopeLocalOnly：只在本机回环上。远程打不到，别把它当入口。
	ScopeLocalOnly ListenerScope = "local_only"
	// ScopeHostOnly：绑在具体网卡地址上，只有能路由到该地址的主机能连。
	ScopeHostOnly ListenerScope = "host"
	// ScopeAnyInterface：绑在 0.0.0.0 / :: 上，凡能到本机的都能连。
	ScopeAnyInterface ListenerScope = "any_interface"
	// ScopeUnknown：地址写法认不出。**不猜**。
	ScopeUnknown ListenerScope = "unknown"
)

// ClassifyListener 判断一个监听端口的可及范围。
//
// bindAddr 是监听绑定地址（0.0.0.0 / 127.0.0.1 / 10.1.2.3 / :: / [::1]:445 …）。
// 认不出的写法返回 ScopeUnknown 而不是 any_interface：猜错方向会把一个
// 只在本机可用的端口当成远程入口，那是最贵的一种误判。
func ClassifyListener(bindAddr string) ListenerScope {
	host := hostOnly(bindAddr)
	if host == "" {
		return ScopeUnknown
	}
	switch host {
	case "::", "*", "0.0.0.0":
		return ScopeAnyInterface
	}
	if IsLoopback(host) {
		return ScopeLocalOnly
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return ScopeHostOnly
	}
	return ScopeUnknown
}

// RemoteReachable 判断「某 IP 上的某个监听」能不能作为远程目标。
// 这是 I3 的核心判据，单独成函数是为了让调用点读起来就是一句结论。
func RemoteReachable(bindAddr string) bool {
	switch ClassifyListener(bindAddr) {
	case ScopeAnyInterface, ScopeHostOnly:
		return true
	}
	return false
}

// hostOnly 去掉 IPv6 方括号与端口，返回纯主机部分。
//
// 必须同时处理三种写法：`10.0.0.5:445`、`[::1]:445`、裸 IPv6 `::1`。
// 用「冒号个数」区分裸 IPv6 与 host:port 是这里唯一的歧义来源，
// 所以只在恰好一个冒号且冒号后全是数字时才当端口切。
func hostOnly(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, "[") {
		if i := strings.Index(addr, "]"); i > 0 {
			return addr[1:i]
		}
		return strings.Trim(addr, "[]")
	}
	if strings.Count(addr, ":") == 1 {
		host, port, ok := strings.Cut(addr, ":")
		if ok && isAllDigits(port) {
			return host
		}
	}
	return addr
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Dedupe 去掉重复边（同 kind + from + to），保留证据等级最高的那条。
//
// 为什么要保留最高而不是最新：一次 verified 之后又来一条 observed，
// 说明环境还在，不是证据变弱了。用「最新覆盖」会让每次重扫都把验证状态抹掉。
func Dedupe(edges []Edge) []Edge {
	type key struct {
		kind      EdgeKind
		from, to  string
	}
	best := map[key]int{}
	out := make([]Edge, 0, len(edges))
	for _, e := range edges {
		k := key{e.Kind, e.From, e.To}
		if idx, ok := best[k]; ok {
			if e.Evidence.Rank() > out[idx].Evidence.Rank() {
				out[idx] = e
			}
			continue
		}
		best[k] = len(out)
		out = append(out, e)
	}
	// 稳定排序：按 kind → from → to，让输出可对比（评测基线需要确定性）。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

// Summary 是一句话态势，供 prompt 投影使用。
func Summary(edges []Edge, now time.Time) string {
	if len(edges) == 0 {
		return "无拓扑观测"
	}
	var fresh, stale, verified, localOnly int
	for _, e := range edges {
		if e.Fresh(now) {
			fresh++
		} else {
			stale++
		}
		if e.Evidence == EvidenceVerified {
			verified++
		}
		if e.Kind == EdgeListener && strings.HasSuffix(e.Note, string(ScopeLocalOnly)) {
			localOnly++
		}
	}
	s := fmt.Sprintf("拓扑边 %d 条（新鲜 %d / 过期 %d，已验证 %d）", len(edges), fresh, stale, verified)
	if localOnly > 0 {
		s += fmt.Sprintf("；其中 %d 条监听仅本机可达（不能当远程入口）", localOnly)
	}
	return s
}
