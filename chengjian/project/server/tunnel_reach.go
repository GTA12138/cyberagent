// tunnel_reach.go 隧道可达性**观测**（I1 / T-06 后半段）。
//
// 背景：tunnels 表里的 reachable_ranges 是**声明**——agent 建隧道时说「我以为
// 这条能到 10.0.0.0/24」。声明本身没有问题，问题是它无法被证伪：本地 socks
// 端口打开 ≠ 所有网段可达，而 worker 拿着这份声明去规划动作时，失败原因会被
// 读成「目标不通」而不是「这条隧道根本到不了那段」。
//
// 本文件补的是观测侧：经隧道实际 dial 一次，把「连通 / 被拒 / 不可达」记下来，
// 写进 reach_verified_ranges（与声明分列）。三个设计判断：
//
//  1. **被拒连也是正面证据。** TCP RST 说明包已经到达对端，是端口没开，不是路
//     不通。把它算成失败会让「目标端口没开」被误读成「这条隧道到不了那段」，
//     两者后续该做的事完全不同（前者换端口，后者换隧道）。
//
//  2. **采样，不扫全段。** /24 有 254 个地址，逐个 dial 是拿探测换噪音——而且
//     会触发目标侧 IDS。这里取确定性样本（首/尾/等距），上限 6 个，并拒绝比
//     /16 更宽的声明（那不是「一条隧道能到哪」，那是「整个私网」）。
//
//  3. **探测失败不写「已验证为空」。** 全不可达时只写时间戳与观测明细，不把
//     reach_verified_ranges 清空——否则「这次探测没探到」会被读成「确认没有」，
//     与「未声明」混成一团。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/tunnel"
	actool "github.com/Autumn-27/norma/tool"
	"golang.org/x/net/proxy"
)

const (
	// reachProbeDialTimeout 是单个 host:port 的 dial 上限。经隧道多一跳，
	// 给 3s：比直连宽，但不至于让一次探测拖到分钟级。
	reachProbeDialTimeout = 3 * time.Second
	// reachProbeMaxHosts 是一次探测最多 dial 多少个地址（采样上限）。
	reachProbeMaxHosts = 6
	// reachProbeConcurrency 是并发度。取 4：再高对目标侧是明显的扫描特征。
	reachProbeConcurrency = 4
	// reachProbeTotalBudget 是整次探测的墙钟上限，超过就带着已有结果返回。
	reachProbeTotalBudget = 12 * time.Second
	// reachProbeMinPrefix 是最小允许的前缀长度（/16）。比它更宽的声明不采样：
	// 「一条隧道能到 /8」不是可验证的断言，探 6 个地址也证明不了什么。
	reachProbeMinPrefix = 16
)

// reachProbePorts 是按顺序尝试的端口。先用 445（Windows 内网最常见，且关闭时
// 通常回 RST 而不是丢包，正好能给出「路通」的证据），再用 80 兜底。
var reachProbePorts = []int{445, 80}

// reachObservation 是单个 host:port 的观测结果。
type reachObservation struct {
	Target  string `json:"target"`
	Outcome string `json:"outcome"` // ok | refused | unreach | unknown
	Detail  string `json:"detail,omitempty"`
}

// reachReport 是一次可达性探测的完整结果。
type reachReport struct {
	TunnelID       int64              `json:"tunnel_id"`
	Probed         int                `json:"probed"`
	Reachable      []string           `json:"reachable,omitempty"`       // 连通（ok）
	Refused        []string           `json:"refused,omitempty"`         // 被拒（路通、端口关）
	Unreachable    []string           `json:"unreachable,omitempty"`     // 不可达
	Unknown        []string           `json:"unknown,omitempty"`         // 无法归类
	Observations   []reachObservation `json:"observations,omitempty"`    // 明细（上限 12 条）
	VerifiedRanges []string           `json:"verified_ranges,omitempty"` // 写入 reach_verified_ranges
	ObservedAt     time.Time          `json:"observed_at"`
	Note           string             `json:"note,omitempty"`
}

// reachable 返回「有正面证据」的 host 数（ok + refused）。
func (r *reachReport) reachable() int { return len(r.Reachable) + len(r.Refused) }

// tunnelSocksURL 从台账行还原 socks5 入口地址。
//
// 与 tunnel/route.go 的 upstreamOf、server/session_control_route.go 的构造规则
// 一致——三处都在描述同一个事实「这条隧道的入口在哪」，所以都从 deploy_params
// 读，不各自维护。suo5 入口带口令，必须内嵌。
func tunnelSocksURL(rec *db.TunnelRecord) (string, error) {
	if rec == nil {
		return "", fmt.Errorf("隧道记录为空")
	}
	if rec.Kind != tunnel.KindSocks {
		return "", fmt.Errorf("隧道 #%d 是 %s 类型：可达性探测只对 socks 有意义（portfwd 只通向固定目标，探它等于探那一个目标）",
			rec.ID, rec.Kind)
	}
	var plan tunnel.Plan
	if err := json.Unmarshal(rec.DeployParams, &plan); err != nil || plan.SocksPort <= 0 {
		return "", fmt.Errorf("隧道 #%d 没有可用的本地 socks 端口（部署参数缺失）", rec.ID)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(plan.SocksPort))
	if rec.Adapter == tunnel.AdapterSuo5 && plan.AuthUser != "" {
		return "socks5://" + plan.AuthUser + ":" + plan.AuthPass + "@" + addr, nil
	}
	return "socks5://" + addr, nil
}

// sampleCIDR 从一条 CIDR 里取确定性样本（最多 max 个地址）。
//
// 采样点：网络首地址 +1、广播地址 -1、以及中间等距若干。**不随机**——同一份
// 声明每次探到同一批地址，观测才可比较（随机采样会让「上次通这次不通」变成
// 噪音，无法判断是路变了还是样本变了）。
func sampleCIDR(cidr string, max int) ([]string, error) {
	cidr = strings.TrimSpace(cidr)
	if cidr == "" {
		return nil, fmt.Errorf("空 CIDR")
	}
	// 允许直接写单个 IP。
	if ip := net.ParseIP(cidr); ip != nil {
		return []string{ip.String()}, nil
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("无法解析网段 %q: %w", cidr, err)
	}
	ones, bits := ipnet.Mask.Size()
	if bits == 0 {
		return nil, fmt.Errorf("非法掩码 %q", cidr)
	}
	if ones < reachProbeMinPrefix {
		return nil, fmt.Errorf("网段 %s 太宽（/%d，最小允许 /%d）：一条隧道能到 /%d 不是可验证的断言，"+
			"请按实际网段声明（如 10.0.0.0/24）", cidr, ones, reachProbeMinPrefix, ones)
	}
	base := ipnet.IP.To4()
	if base == nil {
		return nil, fmt.Errorf("暂只支持 IPv4 网段，收到 %q", cidr)
	}
	hostBits := 32 - ones
	total := 1 << hostBits // 地址总数（含网络/广播地址）
	// /31、/32 没有「可用主机」概念，直接取地址本身。
	if total <= 2 {
		out := []string{base.String()}
		if total == 2 {
			out = append(out, net.IPv4(base[0], base[1], base[2], base[3]+1).String())
		}
		return out, nil
	}
	usable := total - 2 // 去掉网络地址与广播地址
	if max <= 0 || max > usable {
		max = usable
	}
	if max > reachProbeMaxHosts {
		max = reachProbeMaxHosts
	}
	first := ipToUint32(base) + 1
	last := ipToUint32(base) + uint32(total) - 2
	out := make([]string, 0, max)
	if max == 1 {
		return []string{uint32ToIP(first).String()}, nil
	}
	// 等距取样：i 从 0 到 max-1 映射到 [first, last]。
	span := last - first
	for i := 0; i < max; i++ {
		var v uint32
		if max == 1 {
			v = first
		} else {
			v = first + uint32(uint64(span)*uint64(i)/uint64(max-1))
		}
		out = append(out, uint32ToIP(v).String())
	}
	// 去重（小网段等距取样可能重合）。
	seen := map[string]bool{}
	uniq := out[:0]
	for _, s := range out {
		if seen[s] {
			continue
		}
		seen[s] = true
		uniq = append(uniq, s)
	}
	return uniq, nil
}

func ipToUint32(ip net.IP) uint32 {
	v := ip.To4()
	if v == nil {
		return 0
	}
	return uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
}

func uint32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// dialThroughSocks 经 socks5 建立一次 TCP 连接。proxy.SOCKS5 的 Dialer 是
// 标准库风格的，可以直接用 net.Dialer 做底层。
func dialThroughSocks(ctx context.Context, socksURL, target string, timeout time.Duration) (net.Conn, error) {
	u, err := parseSocksURL(socksURL)
	if err != nil {
		return nil, err
	}
	var auth *proxy.Auth
	if u.user != "" {
		auth = &proxy.Auth{User: u.user, Password: u.pass}
	}
	d, err := proxy.SOCKS5("tcp", u.addr, auth, &net.Dialer{Timeout: timeout})
	if err != nil {
		return nil, err
	}
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, derr := d.Dial("tcp", target)
		ch <- result{c, derr}
	}()
	select {
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				_ = r.c.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-ch:
		return r.c, r.err
	}
}

type socksAddr struct {
	addr string
	user string
	pass string
}

// parseSocksURL 解析 socks5://[user:pass@]host:port。
func parseSocksURL(raw string) (socksAddr, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return socksAddr{}, fmt.Errorf("空的 socks 地址")
	}
	raw = strings.TrimPrefix(raw, "socks5://")
	raw = strings.TrimPrefix(raw, "socks5h://")
	var out socksAddr
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		cred := raw[:at]
		raw = raw[at+1:]
		if colon := strings.Index(cred, ":"); colon >= 0 {
			out.user, out.pass = cred[:colon], cred[colon+1:]
		} else {
			out.user = cred
		}
	}
	if _, _, err := net.SplitHostPort(raw); err != nil {
		return socksAddr{}, fmt.Errorf("socks 地址 %q 缺少端口: %w", raw, err)
	}
	out.addr = raw
	return out, nil
}

// probeTunnelReach 经一条隧道探测若干目标的可达性。
//
// 结果只反映「这一刻、这些端口」的观测：不通不代表永远不通（隧道可能在重拉、
// 目标可能在重启），所以调用方不能据此永久排除某条路——这与 tunnel/route.go
// 「失败不作为排除依据」是同一条原则。
func (s *Server) probeTunnelReach(ctx context.Context, rec *db.TunnelRecord, hosts []string, ports []int) reachReport {
	rep := reachReport{TunnelID: rec.ID, ObservedAt: time.Now()}
	socksURL, err := tunnelSocksURL(rec)
	if err != nil {
		rep.Note = err.Error()
		return rep
	}
	if len(hosts) == 0 {
		rep.Note = "没有可探测的目标（未声明可达范围）"
		return rep
	}
	if len(ports) == 0 {
		ports = reachProbePorts
	}

	probeCtx, cancel := context.WithTimeout(ctx, reachProbeTotalBudget)
	defer cancel()

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, reachProbeConcurrency)

	for _, h := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-probeCtx.Done():
				return
			}
			obs := s.probeOneHost(probeCtx, socksURL, host, ports)
			mu.Lock()
			rep.Probed++
			rep.Observations = append(rep.Observations, obs)
			switch obs.Outcome {
			case "ok":
				rep.Reachable = append(rep.Reachable, obs.Target)
			case "refused":
				rep.Refused = append(rep.Refused, obs.Target)
			case "unreach":
				rep.Unreachable = append(rep.Unreachable, obs.Target)
			default:
				rep.Unknown = append(rep.Unknown, obs.Target)
			}
			mu.Unlock()
		}(h)
	}
	wg.Wait()

	sort.Strings(rep.Reachable)
	sort.Strings(rep.Refused)
	sort.Strings(rep.Unreachable)
	sort.Strings(rep.Unknown)

	// 只有拿到正面证据的 host 才进 verified_ranges。全不可达时留空——
	// 调用方据此不覆盖已有观测（「这次没探到」≠「确认没有」）。
	for _, t := range append(append([]string{}, rep.Reachable...), rep.Refused...) {
		host, _, err := net.SplitHostPort(t)
		if err != nil {
			continue
		}
		rep.VerifiedRanges = append(rep.VerifiedRanges, host+"/32")
	}
	sort.Strings(rep.VerifiedRanges)
	rep.VerifiedRanges = dedupeStrings(rep.VerifiedRanges)

	// 明细截断：观测是给 agent 读的，超过 12 条就是噪音（它只需要知道
	// 「通/不通/被拒」的分布，不需要 200 行）。
	if len(rep.Observations) > 12 {
		rep.Observations = rep.Observations[:12]
	}
	return rep
}

// probeOneHost 依次尝试 ports，返回第一个**可判定**的结果。
//
// 判定顺序的意义：445 不通（unreach）才试 80。若 445 就返回 refused，说明包
// 已经到达对端，路是通的，不需要再试第二个端口——多试一次只是多一次扫描特征。
func (s *Server) probeOneHost(ctx context.Context, socksURL, host string, ports []int) reachObservation {
	var lastErr string
	for _, port := range ports {
		target := net.JoinHostPort(host, strconv.Itoa(port))
		conn, err := dialThroughSocks(ctx, socksURL, target, reachProbeDialTimeout)
		if err == nil {
			_ = conn.Close()
			return reachObservation{Target: target, Outcome: "ok"}
		}
		outcome := tunnel.ClassifyRouteError(err)
		switch outcome {
		case tunnel.RouteOK:
			return reachObservation{Target: target, Outcome: "ok"}
		case tunnel.RouteRefused:
			// 路通、端口关：这是正面证据，不再试下一个端口。
			return reachObservation{Target: target, Outcome: "refused", Detail: truncateForSnapshot(err.Error(), 120)}
		case tunnel.RouteUnreach:
			lastErr = err.Error()
			continue // 换端口再试一次
		default:
			lastErr = err.Error()
			continue
		}
	}
	if lastErr == "" {
		lastErr = "无可用端口"
	}
	return reachObservation{
		Target:  net.JoinHostPort(host, strconv.Itoa(ports[0])),
		Outcome: "unreach",
		Detail:  truncateForSnapshot(lastErr, 120),
	}
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// declaredRangesOf 读台账行里的声明范围（字符串数组）。
func declaredRangesOf(rec *db.TunnelRecord) []string {
	if rec == nil || len(rec.ReachableRanges) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(rec.ReachableRanges, &out); err != nil {
		return nil
	}
	return out
}

// autoVerifyReach 在隧道刚建好、且声明了可达范围时，自动做一次有界探测。
//
// 为什么放在部署路径而不是让 agent 额外调工具：声明是 agent 自己写的，验一次
// 声明是**同一动作的收尾**，不是新动作。放进部署路径后，agent 的工作流一个字
// 都不用改，但同样的 tunnel_deploy 会多带回「实测到得了哪」——这正是「无感」
// 的判据。
//
// 失败不影响部署结论：探测不通可能是目标端口没开，不代表隧道坏了。
func (s *Server) autoVerifyReach(ctx context.Context, rec *db.TunnelRecord, declared []string) *reachReport {
	if s == nil || s.m == nil || s.m.pg == nil || rec == nil {
		return nil
	}
	hosts := []string{}
	for _, cidr := range declared {
		sample, err := sampleCIDR(cidr, reachProbeMaxHosts)
		if err != nil {
			continue // 太宽/非法的声明不探，让它留在「声明」状态
		}
		hosts = append(hosts, sample...)
		if len(hosts) >= reachProbeMaxHosts {
			hosts = hosts[:reachProbeMaxHosts]
			break
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	rep := s.probeTunnelReach(ctx, rec, hosts, nil)
	if rep.reachable() == 0 {
		// 全不可达：不写 verified_ranges（否则等于把「没探到」记成「确认没有」），
		// 但仍写时间戳——「探过、没通」本身是重要信息。
		rep.Note = "本次探测没有任何正面证据：可能是目标端口全关、隧道刚建尚未稳定，或这段确实到不了。" +
			"不要据此永久放弃这条隧道（重拉后可能恢复），但规划动作时要允许失败。"
	} else {
		rep.Note = "被拒连（refused）同样算路通：RST 说明包已到达对端，是端口没开，不是这条隧道到不了。"
	}
	// 写观测：只写实测到的范围，声明列保持不变。
	var raw json.RawMessage
	if len(rep.VerifiedRanges) > 0 {
		raw, _ = json.Marshal(rep.VerifiedRanges)
	}
	if err := db.NewTunnelStore(s.m.pg).MarkReachVerified(ctx, rec.ID, raw); err != nil {
		rep.Note += "（观测写入失败：" + err.Error() + "）"
		return &rep
	}
	// 补一次读回，让调用方拿到落库后的真实状态。
	if fresh, err := db.NewTunnelStore(s.m.pg).Get(ctx, rec.ID); err == nil && fresh != nil {
		rec.ReachVerifiedAt = fresh.ReachVerifiedAt
		rec.ReachVerifiedRanges = fresh.ReachVerifiedRanges
	}
	return &rep
}

// tunnelReachTool 是显式的可达性探测工具。
//
// 它不是「新增触发」而是「补一个原来做不到的动作」：此前 agent 想知道「这条
// 隧道到不到 10.0.0.0/24」，只能用 curl 去打某个具体服务——那既要求那个服务
// 存在，又会把「服务没起」误读成「路不通」。这个工具只做 TCP 可达性判定，
// 把「路」和「服务」两件事分开。
func (s *Server) tunnelReachTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "tunnel_reach",
		Description: "【隧道可达性观测】经指定 socks 隧道探测一组目标/网段的 TCP 可达性，" +
			"回答「这条隧道到底到不到那段」。结果分四类：ok（连通）/ refused（路通但端口关，**同样是路通**）/" +
			"unreach（不可达）/ unknown（无法归类）。只做 TCP connect，不发送任何应用层数据，动静最小。" +
			"探测结果写回隧道台账（reach_verified_ranges），tunnel_list 能看到；声明（reachable_ranges）不会被覆盖。" +
			"不传 targets 时用台账里声明的可达范围采样探测。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tunnel_id": map[string]any{"type": "integer", "description": "tunnel_deploy/tunnel_list 返回的隧道 id（必填）"},
				"targets": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "要探测的目标：CIDR（如 10.0.0.0/24，会采样）或单个 IP。省略则用台账里的声明范围",
				},
				"ports": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "integer"},
					"description": "要试的端口，按顺序，默认 [445,80]。只填一个更精确",
				},
			},
			"required": []any{"tunnel_id"},
		},
		ReadOnly:    func(json.RawMessage) bool { return true },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				TunnelID int64    `json:"tunnel_id"`
				Targets  []string `json:"targets"`
				Ports    []int    `json:"ports"`
			}
			_ = json.Unmarshal(in, &a)
			if a.TunnelID <= 0 {
				return actool.Errorf("tunnel_id 必填且 > 0"), nil
			}
			if err := s.authorizeTunnel(ctx, a.TunnelID); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			rec, err := s.tunnels.Get(ctx, a.TunnelID)
			if err != nil || rec == nil {
				return actool.Errorf(fmt.Sprintf("隧道 #%d 读取失败", a.TunnelID)), nil
			}
			if rec.State != db.TunnelAlive {
				return actool.Errorf(fmt.Sprintf("隧道 #%d 当前状态 %s，不是 alive：先修隧道再探可达性", a.TunnelID, rec.State)), nil
			}

			declared := a.Targets
			if len(declared) == 0 {
				declared = declaredRangesOf(rec)
			}
			if len(declared) == 0 {
				return actool.Errorf("没有可探测目标：targets 未传，且隧道台账里也没有可达范围声明。" +
					"要么显式传 targets（如 10.0.0.0/24 或 10.0.0.8），要么先用 tunnel_deploy 的 reachable_ranges 声明"), nil
			}
			hosts := []string{}
			var bad []string
			for _, t := range declared {
				sample, serr := sampleCIDR(t, reachProbeMaxHosts)
				if serr != nil {
					bad = append(bad, serr.Error())
					continue
				}
				hosts = append(hosts, sample...)
				if len(hosts) >= reachProbeMaxHosts {
					hosts = hosts[:reachProbeMaxHosts]
					break
				}
			}
			if len(hosts) == 0 {
				return actool.Errorf("没有可用目标：" + strings.Join(bad, "; ")), nil
			}
			rep := s.probeTunnelReach(ctx, rec, hosts, a.Ports)
			if rep.reachable() > 0 {
				rep.Note = "被拒连（refused）同样算路通：RST 说明包已到达对端，是端口没开，不是这条隧道到不了。"
				if raw, merr := json.Marshal(rep.VerifiedRanges); merr == nil {
					if werr := db.NewTunnelStore(s.m.pg).MarkReachVerified(ctx, rec.ID, raw); werr != nil {
						rep.Note += "（观测写入失败：" + werr.Error() + "）"
					}
				}
			} else {
				rep.Note = "本次探测没有任何正面证据：可能目标端口全关、隧道刚建未稳定，或这段确实到不了。" +
					"不要据此永久放弃这条隧道（重拉后可能恢复），但规划动作时要允许失败。"
			}
			ri := agent.RunInfoFrom(ctx)
			// I3/T-13：reach 边是「能打」这个判断的唯一合法依据，必须落进
			// 拓扑图而不只是隧道台账的一个 JSON 数组。台账说「这条隧道到过
			// 这段」，拓扑边说「从这条隧道到 10.0.0.8:445 实测可达」——
			// 后者才能被 planner 直接用于选目标。
			reachEdges := s.recordReachEdges(ctx, ri.TaskID, rec.ID, &rep)
			s.appendIntranetActivity(ri.TaskID, "tunnel_reach",
				fmt.Sprintf("隧道 #%d 可达性探测：%d 个目标中 %d 个有正面证据（通 %d / 被拒 %d）",
					rec.ID, rep.Probed, rep.reachable(), len(rep.Reachable), len(rep.Refused)),
				fmt.Sprintf("tunnel_id=%d probed=%d ok=%d refused=%d unreach=%d unknown=%d",
					rec.ID, rep.Probed, len(rep.Reachable), len(rep.Refused), len(rep.Unreachable), len(rep.Unknown)))
			out := map[string]any{
				"tunnel_id":       rep.TunnelID,
				"probed":          rep.Probed,
				"reachable":       rep.Reachable,
				"refused":         rep.Refused,
				"unreachable":     rep.Unreachable,
				"unknown":         rep.Unknown,
				"verified_ranges": rep.VerifiedRanges,
				"observations":    rep.Observations,
				"note":            rep.Note,
			}
			if len(bad) > 0 {
				out["skipped_declarations"] = bad
			}
			if reachEdges > 0 {
				out["topology_edges"] = reachEdges
			}
			return jsonResult(out)
		},
	})
}
