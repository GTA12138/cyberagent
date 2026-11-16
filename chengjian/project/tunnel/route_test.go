package tunnel

import (
	"encoding/json"
	"errors"
	"testing"

	"chengjian.local/chengjian/db"
)

// routeRec 造一条 alive socks 台账行。
func routeRec(id int64, socksPort int, hopDepth int) *db.TunnelRecord {
	raw, _ := json.Marshal(Plan{Kind: KindSocks, SocksPort: socksPort, HopDepth: hopDepth})
	return &db.TunnelRecord{ID: id, Kind: KindSocks, Adapter: AdapterChisel, State: db.TunnelAlive, DeployParams: raw}
}

func routeUpstream(port int) string {
	return "socks5://127.0.0.1:" + itoaTest(port)
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// 错误归类只认网络层措辞；认不出来的归 unknown（不作证据），不归 unreach。
func TestClassifyRouteError(t *testing.T) {
	cases := []struct {
		err  error
		want RouteOutcome
	}{
		{nil, RouteOK},
		{errors.New("dial tcp 10.0.0.8:443: connect: connection refused"), RouteRefused},
		{errors.New("socks connect tcp 127.0.0.1:20801->10.0.0.8:443: connection refused"), RouteRefused},
		{errors.New("No connection could be made because the target machine actively refused it"), RouteRefused},
		{errors.New("dial tcp 10.9.9.9:80: connect: network is unreachable"), RouteUnreach},
		{errors.New("dial tcp: lookup no.such.host: no such host"), RouteUnknown}, // DNS 失败不是路由证据
		{errors.New("no route to host"), RouteUnreach},
		{errors.New("i/o timeout"), RouteUnreach},
		{errors.New("remote error: tls: handshake failure"), RouteUnknown},
		{errors.New("read: connection reset by peer"), RouteUnknown},
	}
	for _, c := range cases {
		if got := ClassifyRouteError(c.err); got != c.want {
			msg := "<nil>"
			if c.err != nil {
				msg = c.err.Error()
			}
			t.Errorf("ClassifyRouteError(%q) = %q, want %q", msg, got, c.want)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"10.0.0.8":         "10.0.0.8",
		"10.0.0.8:8080":    "10.0.0.8",
		"10.0.0.8.":        "10.0.0.8",
		"APP.INTERNAL":     "app.internal",
		"App.Internal:443": "app.internal",
		"  10.0.0.8  ":     "10.0.0.8",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
	// 同一台主机的三种写法必须落到同一个 key，否则粘不住也轮换不起来。
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{routeRec(1, 20801, 1)})
	for _, h := range []string{"10.0.0.8", "10.0.0.8:443", "10.0.0.8."} {
		if _, _, ok := rt.Select(h); !ok {
			t.Fatalf("host %q 未选中", h)
		}
	}
	rt.ObserveRoute("10.0.0.8:443", 1, nil)
	views := rt.Snapshot()
	if len(views) != 1 || len(views[0].ProvenHosts) != 1 || views[0].ProvenHosts[0] != "10.0.0.8" {
		t.Errorf("三种写法应归一为一条证据，得到 %+v", views)
	}
}

// 单隧道时必须与改造前完全一致：永远选它，不受任何证据影响。
// 这条守的是「无感」——只有一个网段的任务不该看到任何行为变化。
func TestRouteSingleTunnelAlwaysSelected(t *testing.T) {
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{routeRec(7, 20807, 1)})
	for i := 0; i < 5; i++ {
		up, id, ok := rt.Select("10.0.0.8")
		if !ok || up != routeUpstream(20807) || id != 7 {
			t.Fatalf("第 %d 次选路应恒为隧道 7，得到 (%q,%d,%v)", i, up, id, ok)
		}
	}
	// 失败也不改变结论：只有一条路，没有别的可换。
	rt.ObserveRoute("10.0.0.8", 7, errors.New("network is unreachable"))
	if up, id, _ := rt.Select("10.0.0.8"); id != 7 || up != routeUpstream(20807) {
		t.Errorf("单隧道时失败不应导致无路可选，得到 (%q,%d)", up, id)
	}
}

// 核心场景：两个网段、两条隧道。A 只能经 T2 到，B 只能经 T1 到。
// 期望：各自浪费一次连接后钉住到正确的那条，且两条隧道同时在用。
func TestRouteTwoSegmentsGetPinnedIndependently(t *testing.T) {
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{
		routeRec(1, 20801, 1), // 只能到 B 段
		routeRec(2, 20802, 1), // 只能到 A 段
	})

	// A 段主机：第一次按 id 小的分给 T1，失败 → 轮换到 T2 → 成功 → 钉住。
	if _, id, _ := rt.Select("10.1.0.5"); id != 1 {
		t.Fatalf("首次探索应按 tunnel_id 升序分给 T1，得到 %d", id)
	}
	rt.ObserveRoute("10.1.0.5", 1, errors.New("dial tcp 10.1.0.5:80: connect: network is unreachable"))
	if _, id, _ := rt.Select("10.1.0.5"); id != 2 {
		t.Fatalf("T1 不可达后应轮换到 T2，得到 %d", id)
	}
	rt.ObserveRoute("10.1.0.5", 2, nil)
	for i := 0; i < 3; i++ {
		if _, id, _ := rt.Select("10.1.0.5"); id != 2 {
			t.Fatalf("A 段主机应粘住 T2，第 %d 次得到 %d", i, id)
		}
	}

	// B 段主机：T1 在这个 host 上的 tries 是 0，T2 也是 0 → 按 id 升序分给 T1 → 成功 → 钉住。
	if _, id, _ := rt.Select("10.2.0.9"); id != 1 {
		t.Fatalf("B 段主机应分给 T1，得到 %d", id)
	}
	rt.ObserveRoute("10.2.0.9", 1, nil)
	if _, id, _ := rt.Select("10.2.0.9"); id != 1 {
		t.Fatalf("B 段主机应粘住 T1，得到 %d", id)
	}

	// 两条隧道都在承载流量——这就是「两个网段能同时推进」。
	views := rt.Snapshot()
	if len(views) != 2 {
		t.Fatalf("应有两条路由，得到 %d", len(views))
	}
	proven := map[int64][]string{}
	for _, v := range views {
		proven[v.TunnelID] = v.ProvenHosts
	}
	if len(proven[2]) != 1 || proven[2][0] != "10.1.0.5" {
		t.Errorf("T2 应只被证明到 A 段，得到 %v", proven[2])
	}
	if len(proven[1]) != 1 || proven[1][0] != "10.2.0.9" {
		t.Errorf("T1 应只被证明到 B 段，得到 %v", proven[1])
	}
}

// 拒连是正面证据：包已经到了对端，只是端口没开。把它算成失败会让选路在
// 「端口没开」的主机上永远轮换，而真相是这条路是通的。
func TestRouteRefusedCountsAsReachable(t *testing.T) {
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{routeRec(1, 20801, 1), routeRec(2, 20802, 1)})
	// 先让 T1 在这个 host 上被拒连。
	if _, id, _ := rt.Select("10.3.0.1"); id != 1 {
		t.Fatalf("首次应选 T1，得到 %d", id)
	}
	rt.ObserveRoute("10.3.0.1", 1, errors.New("dial tcp 10.3.0.1:22: connect: connection refused"))
	for i := 0; i < 3; i++ {
		if _, id, _ := rt.Select("10.3.0.1"); id != 1 {
			t.Fatalf("拒连应视为可达并粘住 T1，第 %d 次得到 %d", i, id)
		}
	}
}

// 无法归类的错误（TLS 握手、连接重置）既不能证明路通也不能证明路不通，
// 所以不得改变选路——否则一次协议错误就能把 host 从一个好隧道上挤走。
func TestRouteUnknownErrorIsNotEvidence(t *testing.T) {
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{routeRec(1, 20801, 1), routeRec(2, 20802, 1)})
	rt.Select("10.4.0.1") // → T1
	rt.ObserveRoute("10.4.0.1", 1, errors.New("remote error: tls: handshake failure"))
	// 没有正面证据，所以仍按「试得少的优先」轮换到 T2；关键是 T1 没有被标成
	// 不可达，因此当 T2 也失败后 T1 仍会被再次考虑。
	if _, id, _ := rt.Select("10.4.0.1"); id != 2 {
		t.Fatalf("无证据时仍应轮换到 T2，得到 %d", id)
	}
	rt.ObserveRoute("10.4.0.1", 2, errors.New("read: connection reset by peer"))
	// 两条都只有 tries 没有证据 → 回到 tries 少的（T1 tries=1, T2 tries=1）→ id 小的。
	if _, id, _ := rt.Select("10.4.0.1"); id != 1 {
		t.Errorf("两条路都无证据时应回到 id 小的 T1，得到 %d", id)
	}
}

// 失败不作为排除依据：一条路失败后必须仍可被再次尝试（隧道恢复即恢复），
// 否则「排除」这个动作会静默地把可用路径永久关掉。
func TestRouteFailureNeverExcludesARoute(t *testing.T) {
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{routeRec(1, 20801, 1), routeRec(2, 20802, 1)})
	// 让 T1 在 host 上连失败三次。
	for i := 0; i < 3; i++ {
		_, id, _ := rt.Select("10.5.0.1")
		if id == 1 {
			rt.ObserveRoute("10.5.0.1", id, errors.New("network is unreachable"))
		} else {
			rt.ObserveRoute("10.5.0.1", id, errors.New("network is unreachable"))
		}
	}
	// 之后 T1 恢复：仍必须有机会被选中（tries 会被轮换拉平）。
	seen := map[int64]bool{}
	for i := 0; i < 8; i++ {
		_, id, ok := rt.Select("10.5.0.1")
		if !ok {
			t.Fatal("不应出现无路可选")
		}
		seen[id] = true
	}
	if !seen[1] {
		t.Error("失败过的隧道必须仍可被再次尝试（不允许静默永久排除）")
	}
}

// Sync 只收 alive socks；重建时保留存活隧道的观测证据，丢弃已消失隧道的。
func TestRouteSyncFiltersAndPreservesEvidence(t *testing.T) {
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{
		routeRec(1, 20801, 1),
		routeRec(2, 20802, 1),
		{ID: 3, Kind: KindPortfwd, State: db.TunnelAlive, DeployParams: json.RawMessage(`{"kind":"portfwd","local_port":20810}`)},
		{ID: 4, Kind: KindSocks, State: db.TunnelError, DeployParams: json.RawMessage(`{"kind":"socks","socks_port":20804}`)},
		{ID: 5, Kind: KindSocks, State: db.TunnelAlive, DeployParams: json.RawMessage(`{}`)},
	})
	if got := rt.Len(); got != 2 {
		t.Fatalf("只应收 alive 且带 socks_port 的隧道，得到 %d", got)
	}
	rt.Select("10.6.0.1") // → T1
	rt.ObserveRoute("10.6.0.1", 1, nil)
	// 重建：T1 存活，T2 消失。
	rt.Sync([]*db.TunnelRecord{routeRec(1, 20801, 1)})
	if got := rt.Len(); got != 1 {
		t.Fatalf("重建后应只剩 1 条，得到 %d", got)
	}
	if _, id, _ := rt.Select("10.6.0.1"); id != 1 {
		t.Fatalf("存活隧道的证据应保留，得到 %d", id)
	}
	views := rt.Snapshot()
	if len(views) != 1 || len(views[0].ProvenHosts) != 1 {
		t.Errorf("存活隧道的正面证据应保留，得到 %+v", views)
	}
}

// 空表时选路层必须「没有意见」——把无隧道/隧道全死交给既有回落逻辑
// （含内网期阻断），不在两处各写一遍策略。
func TestRouteNoOpinionWhenEmpty(t *testing.T) {
	rt := NewRouteTable()
	if up, _, ok := rt.Select("10.0.0.8"); ok || up != "" {
		t.Errorf("空表应返回无意见，得到 (%q,%v)", up, ok)
	}
	up, err := rt.SelectUpstream("10.0.0.8")
	if err != nil || up != "" {
		t.Errorf("SelectUpstream 空表应返回 (\"\", nil)，得到 (%q,%v)", up, err)
	}
	// Sync 到空集合同样无意见。
	rt.Sync(nil)
	if up, err := rt.SelectUpstream("10.0.0.8"); err != nil || up != "" {
		t.Errorf("空台账应返回无意见，得到 (%q,%v)", up, err)
	}
}

// 都没证明过时优先单跳：每多一跳延迟叠加、可用性相乘。
func TestRoutePrefersLowerHopDepth(t *testing.T) {
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{
		routeRec(1, 20801, 3), // id 小但三跳
		routeRec(2, 20802, 1),
	})
	if _, id, _ := rt.Select("10.7.0.1"); id != 2 {
		t.Errorf("应优先单跳隧道 2，得到 %d", id)
	}
}

// suo5 入口带 --auth：上游 URL 必须内嵌口令，否则消费方连不上。
func TestRouteUpstreamEmbedsSuo5Auth(t *testing.T) {
	raw, _ := json.Marshal(Plan{Kind: KindSocks, SocksPort: 20809, AuthUser: "chengjian", AuthPass: "deadbeef"})
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{{
		ID: 9, Kind: KindSocks, Adapter: AdapterSuo5, State: db.TunnelAlive, DeployParams: raw,
	}})
	up, _, ok := rt.Select("10.8.0.1")
	if !ok {
		t.Fatal("应选中 suo5 隧道")
	}
	if up != "socks5://chengjian:deadbeef@127.0.0.1:20809" {
		t.Errorf("suo5 上游应内嵌口令，得到 %q", up)
	}
}

// 未经过选路的 host 不归因：全局出口上的流量不能被算到某条隧道头上。
func TestRouteObserveIgnoresUnroutedHost(t *testing.T) {
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{routeRec(1, 20801, 1)})
	rt.ObserveRoute("1.2.3.4", 1, nil) // 从未 Select 过
	if views := rt.Snapshot(); len(views) != 1 || len(views[0].ProvenHosts) != 0 {
		t.Errorf("未选路的 host 不应产生证据，得到 %+v", views)
	}
}
