package server

// observation_wire.go 把 observation 包接进服务器（I3 / T-13 的接线层）。
//
// 分工：
//   - observation 包 = 纯规则（证据等级、时效、边界判据），零 IO。
//   - db.ObservationStore = 台账（行锁保证证据只升不降）。
//   - observation.ArtifactStore = 原文留存（内容寻址，读回校验）。
//   - 本文件 = 从侦察/探测结果**推导**观测与拓扑边，并投影成 planner 可读的态势。
//
// 「推导」这一步必须在 server 侧而不是 observation 包内：它要读 recon 的结构化
// 报告（那是 server 的解析产物），而 observation 包如果 import recon，规则层
// 就会绑死在某个具体解析器的输出形状上。

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/observation"
	"chengjian.local/chengjian/recon"
)

// initObservation 建立观测台账与原文库。DB 未就绪时保持 nil（工具面自动降级）。
func (s *Server) initObservation() {
	if s.m == nil || s.m.pg == nil {
		return
	}
	s.obsStore = db.NewObservationStore(s.m.pg)
	s.artifacts = observation.NewArtifactStore(filepath.Join(s.m.dir, "observations"))
}

// observationReady 判断观测链路是否可用。三条同时具备才算可用：
// 台账、原文库、DB。缺任何一条都不写——写一条读不回来的观测比不写更糟。
func (s *Server) observationReady() bool {
	return s != nil && s.obsStore != nil && s.artifacts != nil
}

// recordReconObservations 把一次侦察的命令输出落成观测，并推导拓扑边。
//
// 每条命令一份观测（而不是整个命令包一份）：观测的粒度必须等于「证据的粒度」。
// 把五条命令塞进一份观测，会让「网卡表读到了、ARP 表读失败」变成一条含糊的
// 记录，而这两件事对后续判断的意义完全不同。
//
// 返回观测 id 列表与推导出的边数；任何失败都只降级不阻断侦察主流程——
// 侦察结果本身已经进了资产图和探索图，观测层是加分的可核验性。
func (s *Server) recordReconObservations(ctx context.Context, taskID, sessionID int64, platform, route string, outputs map[string]string, cmdErrs map[string]string, report *recon.ReconReport, boundAddr string) ([]int64, int, string) {
	if !s.observationReady() || taskID <= 0 {
		return nil, 0, ""
	}
	obsIDs := []int64{}
	edgeCount := 0
	notes := []string{}

	// 1) 每条成功的命令 → 一条观测（原文 = 该命令的完整 stdout）。
	for _, key := range []string{"nics", "routes", "neighbors", "hosts", "conns"} {
		out, ok := outputs[key]
		if !ok {
			continue
		}
		ref, hash, size, err := s.artifacts.Put([]byte(out))
		if err != nil {
			notes = append(notes, "原文留存失败("+key+"): "+err.Error())
			continue
		}
		obs := &observation.Observation{
			TaskID:          taskID,
			Kind:            "recon:" + key,
			ObserverSession: sessionID,
			Route:           route,
			Command:         reconCommandFor(key, platform),
			ArtifactRef:     ref,
			ArtifactHash:    hash,
			ArtifactBytes:   size,
			BoundAddr:       boundAddr,
			Confidence:      reconQuality(out),
			CollectedAt:     time.Now(),
			Notes:           "platform=" + platform,
		}
		id, err := s.obsStore.Put(ctx, obs)
		if err != nil {
			notes = append(notes, "观测落库失败("+key+"): "+err.Error())
			continue
		}
		obsIDs = append(obsIDs, id)
	}
	if len(obsIDs) == 0 {
		return nil, 0, strings.Join(notes, "; ")
	}
	// 边统一挂到第一条观测上（nics 是拓扑的根）。分挂到各命令会让「这条边
	// 是从哪份输出读出来的」更精确，但那份精确性只在复核时才需要，而复核
	// 入口是 list_observations（能看到全部五条）。取根观测是为了让边的来源
	// 永远指向一条**已存在**的观测，避免出现悬挂引用。
	rootObs := obsIDs[0]

	edges := deriveReconEdges(taskID, rootObs, report)
	for _, e := range edges {
		if _, _, err := s.obsStore.UpsertEdge(ctx, e); err != nil {
			notes = append(notes, "拓扑边写入失败: "+err.Error())
			continue
		}
		edgeCount++
	}
	return obsIDs, edgeCount, strings.Join(notes, "; ")
}

// reconCommandFor 还原某条侦察命令的原文，供观测的 command 列使用。
// 取命令包里的实际字符串（含回退写法），而不是「nics」这种内部键——
// 复核的人要能照着原样重跑。
func reconCommandFor(key, platform string) string {
	for _, c := range reconCommandPack(platform) {
		if c.key == key {
			return c.cmd
		}
	}
	return key
}

// reconQuality 判断一次命令输出是否完整。
//
// 判据只有一条：session_exec 侧对单条命令有输出上限，被截断的输出仍算
// observed，但支撑不了否定结论（「没有别的监听端口」）。这里用「结尾是否
// 突然中断在半个 token 上」这种启发式太脆，改用长度阈值 + 明显截断标记。
func reconQuality(out string) string {
	const truncatHint = "...[truncated]"
	if strings.Contains(out, truncatHint) || strings.Contains(out, "[output truncated") {
		return observation.QualityTruncated
	}
	if strings.TrimSpace(out) == "" {
		// 空输出是完整的观测结果（命令跑完了，目标没有这张表）。
		return observation.QualityComplete
	}
	return observation.QualityComplete
}

// deriveReconEdges 从结构化侦察报告推导拓扑边。
//
// 证据等级的判定是这里的核心，规则如下：
//   - NIC / neighbor / listener / peer / route：命令直接打印出来的 → observed。
//     注意「命令打印出来了」只证明**目标自己这么认为**，不证明从我们这边可达，
//     所以这些边没有一条是 verified。
//   - 没有任何一条边在推导阶段被标成 verified：验证必须来自一次独立的
//     探测（tunnel_reach），不能来自「我又读了一遍路由表」。
func deriveReconEdges(taskID, obsID int64, r *recon.ReconReport) []observation.Edge {
	if r == nil {
		return nil
	}
	now := time.Now()
	edges := []observation.Edge{}
	add := func(kind observation.EdgeKind, from, to, note string) {
		edges = append(edges, observation.Edge{
			TaskID:        taskID,
			ObservationID: obsID,
			Kind:          kind,
			From:          from,
			To:            to,
			Evidence:      observation.EvidenceObserved,
			Note:          note,
		}.WithTTL(now))
	}

	// 本机地址：NIC 的每个非回环地址都建一条 nic 边。
	hostIPs := []string{}
	for _, n := range r.NICs {
		if n.IP == "" {
			continue
		}
		if observation.IsLoopback(n.IP) {
			// 回环地址不进拓扑图：它对「能打到哪里」没有信息量，
			// 进了图反而会让「本机有 127.0.0.1」看起来像一条可用路径。
			continue
		}
		hostIPs = append(hostIPs, n.IP)
		add(observation.EdgeNIC, "host", n.IP, "iface="+n.Iface)
	}

	// 监听端口：挂到它实际绑定的地址上，并按可及范围标注。
	//
	// 这一步是 I3 验收「loopback-only 服务不会变成远程可达」的落点。
	// 此前所有监听端口都被挂到「主 IP」上，于是 127.0.0.1:8080 变成
	// 「10.0.0.5:8080 开着」——一个纯粹由建模方式制造出来的假入口。
	for _, l := range r.Listening {
		bind := l.Bind
		if bind == "" {
			bind = "0.0.0.0"
		}
		scope := observation.ClassifyListener(bind)
		target := bind
		if scope == observation.ScopeAnyInterface {
			// 0.0.0.0 本身不是可路由地址，落到每个真实网卡地址上各建一条边。
			for _, ip := range hostIPs {
				add(observation.EdgeListener, ip, joinHostPort(ip, l.Port),
					"proto="+l.Proto+" bind=any scope="+string(scope))
			}
			continue
		}
		add(observation.EdgeListener, target, joinHostPort(target, l.Port),
			"proto="+l.Proto+" bind="+bind+" scope="+string(scope))
	}

	// 邻居：二层证据。只能说明「同一个广播域里有过这台机器」。
	for _, nb := range r.Neighbors {
		if nb.IP == "" || observation.IsLoopback(nb.IP) {
			continue
		}
		add(observation.EdgeNeighbor, "host", nb.IP, "mac="+nb.MAC)
	}

	// 已建立连接的对端：被动证据里最强的一类（说明这台机器**真的**连过对方）。
	for _, p := range r.Peers {
		if p.IP == "" || observation.IsLoopback(p.IP) {
			continue
		}
		add(observation.EdgePeer, "host", p.IP, "established")
	}

	// 路由：静态路由指向的网段是「管理员显式配过」的配置事实，
	// 但它仍然只是 inferred 级别的**可达性**——路由存在不等于我们能到。
	// 所以这里建的是 route 边（observed，因为路由表确实是打印出来的），
	// 而**不**建 reach 边。reach 只能由探测产生。
	for _, rt := range r.Routes {
		if rt.Dest == "" || rt.Dest == "default" {
			continue
		}
		add(observation.EdgeRoute, "host", rt.Dest, "via="+rt.Via+" iface="+rt.Iface)
	}

	return observation.Dedupe(edges)
}

func joinHostPort(host string, port int) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + itoa(int64(port))
	}
	return host + ":" + itoa(int64(port))
}

// recordReachEdges 把一次可达性探测落成 reach 边（唯一的 verified 来源）。
//
// reach 边是「能打」这个判断的唯一合法依据：listener 边说目标自己认为端口开着，
// reach 边说我们从这里真的连上了（或被明确拒连——拒连同样证明路通）。
// 两者缺一不可，所以它们必须分开建模。
func (s *Server) recordReachEdges(ctx context.Context, taskID, tunnelID int64, rep *reachReport) int {
	if !s.observationReady() || taskID <= 0 || rep == nil {
		return 0
	}
	// 探测报告本身作为观测原文：reach 结论必须能追到「哪次探测、探了哪些地址」。
	payload := reachReportArtifact(rep)
	ref, hash, size, err := s.artifacts.Put(payload)
	if err != nil {
		return 0
	}
	obsID, err := s.obsStore.Put(ctx, &observation.Observation{
		TaskID:       taskID,
		Kind:         "probe:reach",
		Route:        "tunnel:" + itoa(tunnelID),
		Command:      "tcp dial through tunnel socks",
		ArtifactRef:  ref,
		ArtifactHash: hash,
		ArtifactBytes: size,
		Confidence:   observation.QualityComplete,
		CollectedAt:  time.Now(),
		Notes:        rep.Note,
	})
	if err != nil {
		return 0
	}
	now := time.Now()
	n := 0
	for _, o := range rep.Observations {
		// ok / refused 都是「路通」的正面证据（拒连说明包到了、对方回了 RST）。
		// unreachable / unknown 不建边：没探到不等于不可达，建一条 inferred 的
		// 负边会让「还没试过」变成「试过不行」。
		if o.Outcome != "ok" && o.Outcome != "refused" {
			continue
		}
		e := observation.Edge{
			TaskID:        taskID,
			ObservationID: obsID,
			Kind:          observation.EdgeReach,
			From:          "tunnel:" + itoa(tunnelID),
			To:            o.Target,
			Evidence:      observation.EvidenceObserved,
			Note:          o.Outcome,
		}.WithTTL(now)
		if _, _, err := s.obsStore.UpsertEdge(ctx, e); err != nil {
			continue
		}
		n++
	}
	return n
}

func reachReportArtifact(rep *reachReport) []byte {
	var b strings.Builder
	b.WriteString("probed=")
	b.WriteString(itoa(int64(rep.Probed)))
	b.WriteString(" reachable=")
	b.WriteString(itoa(int64(len(rep.Reachable))))
	b.WriteString(" refused=")
	b.WriteString(itoa(int64(len(rep.Refused))))
	b.WriteString(" unreachable=")
	b.WriteString(itoa(int64(len(rep.Unreachable))))
	b.WriteString(" unknown=")
	b.WriteString(itoa(int64(len(rep.Unknown))))
	b.WriteString("\n")
	for _, o := range rep.Observations {
		b.WriteString(o.Target)
		b.WriteString("\t")
		b.WriteString(o.Outcome)
		if o.Detail != "" {
			b.WriteString("\t")
			b.WriteString(o.Detail)
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// snapshotTopology 把拓扑边投影成 planner 每轮可见的态势。
//
// 三条设计约束：
//   - 只给「新鲜」的边。过期边不删，但不该继续影响决策——一份十分钟前的
//     ARP 表被当成当前事实，比没有拓扑更危险。
//   - 已验证与未验证分开列。planner 必须能一眼看出「这条是有人真的连过」
//     还是「目标自己说它开着」。
//   - 只本机可达的监听单独列，并明说「不能当远程入口」——否则 planner 会
//     对着 127.0.0.1:8080 派横向意图。
func (s *Server) snapshotTopology(ctx context.Context, taskID int64) map[string]any {
	if !s.observationReady() || taskID <= 0 {
		return nil
	}
	edges, err := s.obsStore.Edges(ctx, taskID, 400)
	if err != nil || len(edges) == 0 {
		return nil
	}
	now := time.Now()
	fresh := make([]observation.Edge, 0, len(edges))
	for _, e := range edges {
		if e.Fresh(now) {
			fresh = append(fresh, e)
		}
	}
	if len(fresh) == 0 {
		return map[string]any{
			"summary": "拓扑观测全部过期（" + itoa(int64(len(edges))) + " 条）；需要重新侦察才能作为当前事实",
			"stale":   len(edges),
		}
	}

	const perKind = 6
	byKind := map[observation.EdgeKind][]map[string]any{}
	localOnly := []map[string]any{}
	verified := []string{}
	for _, e := range fresh {
		item := map[string]any{"from": e.From, "to": e.To, "evidence": string(e.Evidence)}
		if e.Note != "" {
			item["note"] = e.Note
		}
		if e.Evidence == observation.EvidenceVerified {
			verified = append(verified, string(e.Kind)+" "+e.From+"→"+e.To)
		}
		if e.Kind == observation.EdgeListener && strings.HasSuffix(e.Note, string(observation.ScopeLocalOnly)) {
			localOnly = append(localOnly, item)
			continue
		}
		list := byKind[e.Kind]
		if len(list) < perKind {
			byKind[e.Kind] = append(list, item)
		}
	}
	out := map[string]any{"summary": observation.Summary(fresh, now)}
	for k, v := range byKind {
		out[string(k)] = v
	}
	if len(localOnly) > 0 {
		if len(localOnly) > perKind {
			localOnly = localOnly[:perKind]
		}
		out["listener_local_only"] = localOnly
		out["listener_local_only_note"] = "绑在回环地址上，只有目标自己连得到；不要当成远程入口"
	}
	if len(verified) > 0 {
		if len(verified) > perKind {
			verified = verified[:perKind]
		}
		out["verified"] = verified
	}
	// 计数按 kind 排序输出，保证同一态势每次渲染顺序一致（prompt 缓存友好）。
	counts := map[string]int{}
	for _, e := range fresh {
		counts[string(e.Kind)]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make([]string, 0, len(keys))
	for _, k := range keys {
		ordered = append(ordered, k+"="+itoa(int64(counts[k])))
	}
	out["counts"] = strings.Join(ordered, " ")
	return out
}
