package tunnel

import (
	"strconv"
	"strings"
	"testing"
)

// 层级上限是项目自己 chains pivot 骨架的判据（「控制在三层以内，可用性相乘」），
// 这里把它落成代码约束，而不是留给模型自觉。
func TestValidateHopDepth(t *testing.T) {
	for _, ok := range []int{0, 1, 2, 3} {
		if err := ValidateHopDepth(ok); err != nil {
			t.Fatalf("depth=%d 应合法: %v", ok, err)
		}
	}
	for _, bad := range []int{4, 5, 99} {
		if err := ValidateHopDepth(bad); err == nil {
			t.Fatalf("depth=%d 应超限报错", bad)
		}
	}
	if err := ValidateHopDepth(-1); err == nil {
		t.Fatal("负数应报错")
	}
}

// 超限错误必须解释「为什么不行」并给替代方案，否则模型只会重试。
func TestValidateHopDepthMessageIsActionable(t *testing.T) {
	err := ValidateHopDepth(5)
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	for _, want := range []string{"上限", "可用性", "单跳"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息缺少可操作指引 %q: %s", want, msg)
		}
	}
}

// PlanSocks 必须拒绝超限层级（在装配层就挡住，不留到部署时才失败）。
func TestPlanSocksRejectsExcessiveHopDepth(t *testing.T) {
	o := &PlanOpts{
		TaskID: 1, ViaSessionID: 1, CallbackHost: "10.0.0.2",
		PortMin: 20000, PortMax: 20010, DataDir: t.TempDir(),
		HopDepth: MaxHopDepth + 1,
	}
	if _, err := PlanSocks(o); err == nil {
		t.Fatal("超出层级上限应报错")
	}
}

// 层级信息必须持久化进 Plan（deploy_params 全量序列化，重拉要靠它）。
func TestPlanCarriesHopInfo(t *testing.T) {
	o := &PlanOpts{
		TaskID: 1, ViaSessionID: 1, CallbackHost: "127.0.0.1:20801",
		PortMin: 20000, PortMax: 20010, DataDir: t.TempDir(),
		ParentTunnelID: 7, HopDepth: 2,
	}
	p, err := PlanSocks(o)
	if err != nil {
		t.Fatal(err)
	}
	if p.ParentTunnelID != 7 || p.HopDepth != 2 {
		t.Fatalf("层级信息未落进 Plan: parent=%d depth=%d", p.ParentTunnelID, p.HopDepth)
	}
}

// keepalive 必须抖动：固定周期是流量特征。同一进程内连续生成多次应出现不同值，
// 且全部落在 [8s,15s]。
func TestJitteredKeepAliveVariesAndStaysInRange(t *testing.T) {
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		v := jitteredKeepAlive()
		if !strings.HasSuffix(v, "s") {
			t.Fatalf("格式应为 Ns: %q", v)
		}
		var n int
		{
			var perr error
			n, perr = strconv.Atoi(strings.TrimSuffix(v, "s"))
			if perr != nil {
				t.Fatalf("解析失败 %q: %v", v, perr)
			}
		}
		if n < keepAliveMinSec || n > keepAliveMaxSec {
			t.Fatalf("越界: %d", n)
		}
		seen[v]++
	}
	// 200 次抽样若只出现 1 种值，说明抖动失效。
	if len(seen) < 3 {
		t.Fatalf("抖动不足，只出现 %d 种值: %v", len(seen), seen)
	}
}

// Plan 的 KeepAlive 必须真的进到 client 命令行（否则抖动了也没用）。
func TestPlanClientArgsUseJitteredKeepAlive(t *testing.T) {
	o := &PlanOpts{
		TaskID: 1, ViaSessionID: 1, CallbackHost: "10.0.0.2",
		PortMin: 20000, PortMax: 20010, DataDir: t.TempDir(),
	}
	p, err := PlanSocks(o)
	if err != nil {
		t.Fatal(err)
	}
	if p.KeepAlive == "" {
		t.Fatalf("KeepAlive 不应为空")
	}
	// 注意：不能断言「!= 10s」——10s 本身在 [8,15] 区间内，是合法的抖动结果。
	// 要验证「抖动生效」应该看多次采样的分布（见 TestJitteredKeepAliveVariesAndStaysInRange），
	// 单次采样只能验证「值合法且与命令行一致」。
	found := false
	for i, a := range p.ClientArgs {
		if a == "--keepalive" {
			if i+1 >= len(p.ClientArgs) || p.ClientArgs[i+1] != p.KeepAlive {
				t.Fatalf("client 参数里的 keepalive 与 Plan.KeepAlive 不一致: %v vs %q", p.ClientArgs, p.KeepAlive)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("client 参数缺 --keepalive: %v", p.ClientArgs)
	}
	// chisel 要求 --keepalive 在 server/remote 之前，否则被当成 remote 解析。
	if p.ClientArgs[0] != "client" || p.ClientArgs[1] != "--keepalive" {
		t.Fatalf("--keepalive 必须在 server 地址之前: %v", p.ClientArgs)
	}
}
