// route_verified_test.go 实测网段优先选路（route.go 规则 0）的判定测试。
//
// 核心断言：reach_verified_ranges（tunnel_reach / 部署收尾的真实探测结论）
// 命中的 IP host 直接选对应隧道——冷启动不再「在错误隧道上白打一次连接才
// 学到路」；未命中的 host 维持「轮换发现、成功粘住」的既有行为。
package tunnel

import (
	"encoding/json"
	"testing"
	"time"

	"chengjian.local/chengjian/db"
)

func verifiedRec(id int64, socksPort int, verified ...string) *db.TunnelRecord {
	r := routeRec(id, socksPort, 1)
	raw, _ := json.Marshal(verified)
	r.ReachVerifiedRanges = raw
	now := time.Now()
	r.ReachVerifiedAt = &now
	return r
}

func TestSelectPrefersVerifiedRange(t *testing.T) {
	// 隧道 1 无实测网段、隧道 2 实测过 10.0.0.0/24。若无规则 0，轮换会先选
	// id 小的隧道 1（两条 tries 都是 0）——正是「冷启动在错误隧道上白打一次」。
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{
		routeRec(1, 20801, 1),
		verifiedRec(2, 20802, "10.0.0.0/24"),
	})

	up, id, ok := rt.Select("10.0.0.7")
	if !ok || id != 2 {
		t.Fatalf("10.0.0.7 应直接选实测网段命中的隧道 2，got id=%d ok=%v", id, ok)
	}
	if want := routeUpstream(20802); up != want {
		t.Fatalf("上游应为隧道 2 的入口 %q，got %q", want, up)
	}

	// 未命中实测网段的 host 走轮换：两条都没试过 → 选 id 小的。
	if _, id, ok := rt.Select("192.168.30.9"); !ok || id != 1 {
		t.Fatalf("未命中网段的 host 应按轮换选隧道 1，got id=%d ok=%v", id, ok)
	}
}

func TestSelectVerifiedRangePrefersShallowHop(t *testing.T) {
	// 两条隧道都实测过同一网段 → 取 hop 浅的（少一跳少一份可用性损耗）。
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{
		verifiedRec(1, 20801, "10.0.0.0/24"),
		verifiedRec(2, 20802, "10.0.0.0/24"),
	})
	rt.routes[1].HopDepth = 2 // 手工把隧道 1 拉深（Sync 后的运行期字段）
	rt.routes[2].HopDepth = 1
	if _, id, _ := rt.Select("10.0.0.8"); id != 2 {
		t.Fatalf("同网段命中时应选 hop 浅的隧道 2，got %d", id)
	}
}

func TestSelectHostnameIgnoresVerifiedRanges(t *testing.T) {
	// 主机名无法与 CIDR 匹配：不因存在实测网段而改变轮换行为（平台侧解析
	// 不到内网域，替目标猜名字只会选错路）。
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{verifiedRec(2, 20802, "10.0.0.0/24")})
	if _, id, ok := rt.Select("dc01.corp.local"); !ok || id != 2 {
		t.Fatalf("单隧道场景主机名也应选中唯一隧道，got id=%d ok=%v", id, ok)
	}
}

func TestParseVerifiedRangesSkipsInvalid(t *testing.T) {
	got := parseVerifiedRanges(json.RawMessage(`["10.0.0.0/24","not-a-cidr","10.0.1.0/25"]`))
	if len(got) != 2 {
		t.Fatalf("非法条目应跳过而不是整体失败，got %d 条", len(got))
	}
	if got[0].String() != "10.0.0.0/24" || got[1].String() != "10.0.1.0/25" {
		t.Fatalf("解析结果错误: %v %v", got[0], got[1])
	}
	if parseVerifiedRanges(nil) != nil {
		t.Fatal("空输入应返回 nil")
	}
	if parseVerifiedRanges(json.RawMessage(`{"mode":"x"}`)) != nil {
		t.Fatal("非数组输入应返回 nil")
	}
}

func TestSyncDropsVerifiedRangesWhenTunnelDies(t *testing.T) {
	// 失效语义：Sync 只收 alive 隧道——断链隧道的实测网段随之从选路消失，
	// 不会给已死的路续命。
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{verifiedRec(2, 20802, "10.0.0.0/24")})
	dead := verifiedRec(2, 20802, "10.0.0.0/24")
	dead.State = db.TunnelError
	rt.Sync([]*db.TunnelRecord{dead})
	if rt.Len() != 0 {
		t.Fatalf("断链隧道不应留在路由表，len=%d", rt.Len())
	}
	if _, _, ok := rt.Select("10.0.0.7"); ok {
		t.Fatal("隧道全死时 Select 应无意见（回落语义交给调用方）")
	}
}
