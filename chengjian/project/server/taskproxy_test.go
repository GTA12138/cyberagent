package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/tunnel"
)

// fakeTunnelLister 注入式隧道台账(taskProxyManager 的 ListByTask 数据源)。
type fakeTunnelLister struct {
	recs []*db.TunnelRecord
	err  error
}

func (f *fakeTunnelLister) ListByTask(context.Context, int64) ([]*db.TunnelRecord, error) {
	return f.recs, f.err
}

func socksRec(id int64, state string, socksPort int) *db.TunnelRecord {
	raw, _ := json.Marshal(tunnel.Plan{Kind: tunnel.KindSocks, SocksPort: socksPort})
	return &db.TunnelRecord{ID: id, Kind: tunnel.KindSocks, State: state, DeployParams: raw}
}

// suo5Rec 造一条带 --auth 的 suo5 隧道台账行（上游 URL 必须内嵌 user:pass)。
func suo5Rec(id int64, state string, socksPort int) *db.TunnelRecord {
	raw, _ := json.Marshal(tunnel.Plan{Kind: tunnel.KindSocks, SocksPort: socksPort, AuthUser: "chengjian", AuthPass: "deadbeef"})
	return &db.TunnelRecord{ID: id, Kind: tunnel.KindSocks, Adapter: tunnel.AdapterSuo5, State: state, DeployParams: raw}
}

// pickUpstream:最新 alive socks 隧道胜出;无隧道且非内网期回落全局代理;
// 内网期无隧道返回 blocked(禁止回落公网出口);portfwd/非 alive 不算。
func TestPickUpstream(t *testing.T) {
	const global = "http://127.0.0.1:9000"
	cases := []struct {
		name     string
		recs     []*db.TunnelRecord
		intranet bool
		want     string
		wantMode UpstreamMode
	}{
		{"无隧道回落全局", nil, false, global, UpstreamGlobal},
		{"alive socks 当选", []*db.TunnelRecord{socksRec(1, db.TunnelAlive, 20801)}, false, "socks5://127.0.0.1:20801", UpstreamTunnel},
		{"多条取 id 最大", []*db.TunnelRecord{
			socksRec(1, db.TunnelAlive, 20801), socksRec(3, db.TunnelAlive, 20803), socksRec(2, db.TunnelAlive, 20802),
		}, false, "socks5://127.0.0.1:20803", UpstreamTunnel},
		{"error/stopped 不算", []*db.TunnelRecord{
			socksRec(1, db.TunnelError, 20801), socksRec(2, db.TunnelStopped, 20802),
		}, false, global, UpstreamGlobal},
		{"portfwd 不是上游", []*db.TunnelRecord{{
			ID: 1, Kind: tunnel.KindPortfwd, State: db.TunnelAlive,
			DeployParams: json.RawMessage(`{"kind":"portfwd","local_port":20810}`),
		}}, false, global, UpstreamGlobal},
		{"deploy_params 缺 socks_port 跳过", []*db.TunnelRecord{
			{ID: 1, Kind: tunnel.KindSocks, State: db.TunnelAlive, DeployParams: json.RawMessage(`{}`)},
		}, false, global, UpstreamGlobal},
		{"suo5 上游内嵌 auth", []*db.TunnelRecord{suo5Rec(9, db.TunnelAlive, 20809)}, false, "socks5://chengjian:deadbeef@127.0.0.1:20809", UpstreamTunnel},
		// 内网期掉隧道：必须 blocked，且上游要指向一个必然失败的地址——
		// 空串在 traffic 层等于「直连」，那正是要阻止的行为。
		{"内网期无隧道 blocked", nil, true, blockedUpstream, UpstreamBlocked},
		{"内网期隧道全 error blocked", []*db.TunnelRecord{
			socksRec(1, db.TunnelError, 20801), socksRec(2, db.TunnelStopped, 20802),
		}, true, blockedUpstream, UpstreamBlocked},
		// 内网期仍有 alive 隧道 → 正常走隧道。
		{"内网期有隧道走隧道", []*db.TunnelRecord{socksRec(5, db.TunnelAlive, 20805)}, true, "socks5://127.0.0.1:20805", UpstreamTunnel},
	}
	for _, c := range cases {
		got, mode := pickUpstream(c.recs, global, c.intranet)
		if got != c.want || mode != c.wantMode {
			t.Errorf("%s: pickUpstream = (%q,%s), want (%q,%s)", c.name, got, mode, c.want, c.wantMode)
		}
	}
	// 全局为空(直连)时无隧道上游也是空；非内网期仍是 global 模式而非 blocked。
	if got, mode := pickUpstream(nil, "", false); got != "" || mode != UpstreamGlobal {
		t.Errorf("无隧道+无全局代理应为直连(空,global), got (%q,%s)", got, mode)
	}
}

// freePort 拿一个当前可绑的端口(放还给系统;与隧道的 CanListen 同款的微小竞争,
// 测试可接受)。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("找空闲端口: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func managedPortRegistered(port int) bool {
	_, ok := dynManagedPorts.Load(port)
	return ok
}

// 选路层接线：实例必须装上按目标 host 选路的钩子，路由表按台账同步。
//
// 这条守的是「两条隧道真的同时在用」——只装 SetUpstreamProxy（单上游）时，
// 双网段任务的全部流量会被塞进一条隧道，另一条空转。
func TestTaskProxyInstallsRouteHooks(t *testing.T) {
	port := freePort(t)
	lister := &fakeTunnelLister{recs: []*db.TunnelRecord{
		socksRec(1, db.TunnelAlive, 20801),
		socksRec(2, db.TunnelAlive, 20802),
	}}
	m := newTaskProxyManager(t.TempDir(), lister, func() string { return "http://127.0.0.1:9000" }, port, port)
	ctx := context.Background()
	if _, _ = m.ForTask(ctx, 7); m.proxies[7] == nil {
		t.Fatal("实例应已建")
	}
	inst := m.proxies[7]
	defer m.Close()

	if !inst.tr.RouteHooksInstalled() {
		t.Fatal("任务 MITM 实例必须装选路钩子（否则双网段无法并行）")
	}
	if got := inst.routes.Len(); got != 2 {
		t.Fatalf("路由表应同步到 2 条 alive socks 隧道，得到 %d", got)
	}

	// 两条隧道都活着时，回落上游仍是「id 最大」（单隧道/回落语义不变），
	// 但逐请求的实际选路交给路由表。
	if got := inst.tr.UpstreamProxy(); got != "socks5://127.0.0.1:20802" {
		t.Errorf("回落上游应保持取 id 最大的既有语义，得到 %q", got)
	}

	// 隧道全死 → 路由表清空，回落逻辑接管（外网期回落全局代理）。
	lister.recs = []*db.TunnelRecord{
		socksRec(1, db.TunnelError, 20801),
		socksRec(2, db.TunnelStopped, 20802),
	}
	m.Refresh(ctx, 7)
	if got := inst.routes.Len(); got != 0 {
		t.Errorf("隧道全死后路由表应清空，得到 %d", got)
	}
	if got := inst.tr.UpstreamProxy(); got != "http://127.0.0.1:9000" {
		t.Errorf("隧道全死且非内网期应回落全局代理，得到 %q", got)
	}

	// 内网期隧道全死 → 阻断，且必须关掉透明透传（透传 = 绕过上游直连目标）。
	m.SetIntranetResolver(func(int64) bool { return true })
	m.Refresh(ctx, 7)
	if got := inst.tr.UpstreamProxy(); got != blockedUpstream {
		t.Errorf("内网期无隧道应置为阻断上游，得到 %q", got)
	}
}

// agent 侧只能看到计数，不能看到 host 清单。
//
// 这条守的是「路由不限定范围」：tunnel_list 同时绑 worker 与 planner，一旦
// 把 host 清单输出出去，planner 会把它读成「可达目标白名单」，于是不再派
// 「打这个网段」的意图——那正是「系统把 agent 的发散范围限死」的形态。
// 而且这份明细无法据以行动（agent 不能手动指定走哪条隧道），给了只有误读风险。
func TestRouteRowFieldsExposeCountsOnly(t *testing.T) {
	rv := tunnel.RouteView{
		TunnelID: 1, Adapter: tunnel.AdapterChisel,
		ProvenHosts: []string{"10.1.0.5", "10.1.0.6"}, TriedHosts: 3,
	}
	row := routeRowFields(rv)
	if row["proven_host_count"] != 2 || row["tried_host_count"] != 3 {
		t.Fatalf("应输出计数 (2,3)，得到 %+v", row)
	}
	// 不允许任何字段携带 host 明细（值里出现 host 字符串即视为泄漏）。
	for k, v := range row {
		s := fmt.Sprint(v)
		if strings.Contains(s, "10.1.0.5") || strings.Contains(s, "10.1.0.6") {
			t.Errorf("字段 %q 泄漏了 host 明细: %v", k, v)
		}
	}
	for _, banned := range []string{"proven_hosts", "tried_hosts", "hosts"} {
		if _, ok := row[banned]; ok {
			t.Errorf("不应输出字段 %q（会被读成可达目标白名单）", banned)
		}
	}
}

// 懒建 → 端口登记 → 上游热切换(隧道出现/消失)→ CloseTask 清理 → 再懒建。
func TestTaskProxyLifecycle(t *testing.T) {
	port := freePort(t)
	lister := &fakeTunnelLister{}
	m := newTaskProxyManager(t.TempDir(), lister, func() string { return "http://127.0.0.1:9000" }, port, port)
	ctx := context.Background()

	addr, ca := m.ForTask(ctx, 42)
	if addr == "" || ca == "" {
		t.Fatalf("ForTask 应返回实例地址与 CA, got (%q, %q)", addr, ca)
	}
	if !strings.Contains(addr, ":"+strconv.Itoa(port)) {
		t.Errorf("实例地址应含分配端口 %d: %s", port, addr)
	}
	if _, err := os.Stat(ca); err != nil {
		t.Errorf("实例 CA 证书应已落盘 %s: %v", ca, err)
	}
	if !managedPortRegistered(port) {
		t.Error("实例端口应登记进端口审计台账(registerManagedPort)")
	}
	inst := m.proxies[42]
	if inst == nil {
		t.Fatal("实例应已入表")
	}
	if got := inst.tr.UpstreamProxy(); got != "http://127.0.0.1:9000" {
		t.Errorf("无隧道时上游应回落全局代理, got %q", got)
	}

	// 幂等:再次 ForTask 复用同一实例(端口不变)。
	addr2, _ := m.ForTask(ctx, 42)
	if addr2 != addr {
		t.Errorf("重复 ForTask 不应重建实例: %q → %q", addr, addr2)
	}

	// 隧道出现 → Refresh 热切换到 socks 上游。
	lister.recs = []*db.TunnelRecord{socksRec(7, db.TunnelAlive, 20807)}
	m.Refresh(ctx, 42)
	if got := inst.tr.UpstreamProxy(); got != "socks5://127.0.0.1:20807" {
		t.Errorf("隧道 alive 后上游应热切换, got %q", got)
	}

	// 隧道死亡 → 回落全局代理。
	lister.recs = []*db.TunnelRecord{socksRec(7, db.TunnelError, 20807)}
	m.Refresh(ctx, 42)
	if got := inst.tr.UpstreamProxy(); got != "http://127.0.0.1:9000" {
		t.Errorf("隧道死亡后上游应回落全局代理, got %q", got)
	}

	// 无实例的任务 Refresh 是 no-op,不触发懒建。
	m.Refresh(ctx, 99)
	if m.proxies[99] != nil {
		t.Error("Refresh 不应为无实例任务建实例(懒建语义)")
	}

	// CloseTask:端口注销、实例出表、目录(CA/录制树)保留。
	m.CloseTask(42)
	if managedPortRegistered(port) {
		t.Error("CloseTask 后端口应注销")
	}
	if m.proxies[42] != nil {
		t.Error("CloseTask 后实例应出表")
	}
	if _, err := os.Stat(ca); err != nil {
		t.Errorf("CloseTask 后 CA 目录应保留(留痕), stat %s: %v", ca, err)
	}
	m.CloseTask(42) // 幂等,不应 panic

	// 再懒建:拿新实例(同一端口池只剩一个口,复用同端口)。
	addr3, _ := m.ForTask(ctx, 42)
	if addr3 == "" {
		t.Fatal("CloseTask 后应能重新懒建实例")
	}
	m.Close()
	if managedPortRegistered(port) {
		t.Error("Close 后端口应全部注销")
	}
}
