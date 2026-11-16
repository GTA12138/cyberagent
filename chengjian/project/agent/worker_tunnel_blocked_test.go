package agent

import (
	"strings"
	"testing"
)

// tunnelBlockedForTask 决定是否向 worker 注入「内网链路不可用」纪律。
// 判据必须严格：只有 resolver 明确返回 "blocked" 才注入，其余（nil resolver、
// 空字符串、global/tunnel）都不注入——否则外网任务会被无谓地限制。
func TestTunnelBlockedForTask(t *testing.T) {
	w := &Worker{}

	// 未装 resolver → 永不 blocked（外网任务默认形态）。
	if w.tunnelBlockedForTask(7) {
		t.Fatal("无 resolver 时不应判定 blocked")
	}

	// resolver 返回各种非 blocked 值 → 都不注入。
	for _, mode := range []string{"", "global", "tunnel", "unknown"} {
		w.SetTaskProxyModeResolver(func(int64) string { return mode })
		if w.tunnelBlockedForTask(7) {
			t.Fatalf("mode=%q 不应判定 blocked", mode)
		}
	}

	// 明确 blocked → 注入。
	w.SetTaskProxyModeResolver(func(int64) string { return TunnelBlocked })
	if !w.tunnelBlockedForTask(7) {
		t.Fatal("mode=blocked 应判定 blocked")
	}

	// taskID<=0（非任务运行）→ 即使 resolver 说 blocked 也不注入。
	if w.tunnelBlockedForTask(0) {
		t.Fatal("非任务运行不应判定 blocked")
	}
	if w.tunnelBlockedForTask(-1) {
		t.Fatal("负 taskID 不应判定 blocked")
	}
}

// resolver 必须收到真实的 taskID（否则会拿别的任务状态判本任务）。
func TestTunnelBlockedPassesTaskID(t *testing.T) {
	w := &Worker{}
	var got int64
	w.SetTaskProxyModeResolver(func(id int64) string {
		got = id
		return TunnelBlocked
	})
	_ = w.tunnelBlockedForTask(42)
	if got != 42 {
		t.Fatalf("resolver 应收到 42, got %d", got)
	}
}

// 纪律文本必须点明关键动作：禁止直连、先 probe 再 deploy、带 listen_port。
// 这是 worker 从「静默回落」改为「显式修链路」的唯一指引。
func TestTunnelBlockedRulesContent(t *testing.T) {
	for _, want := range []string{"禁止", "直连", "tunnel_probe", "tunnel_deploy", "listen_port", "tunnel_list"} {
		if !strings.Contains(tunnelBlockedRules, want) {
			t.Fatalf("tunnelBlockedRules 缺少关键指引 %q", want)
		}
	}
}
