package session

import (
	"strings"
	"testing"
)

// TestSetControlProxyRoundTrip 钉住「设了能读回来、清空能回到直连」。
//
// 关键断言是 **不泄露凭据**：ControlProxyURL 只回 scheme://host:port，
// suo5 的代理口令内嵌在 URL 里，一旦整串回显，它会随注册输出进模型上下文
// 和审计日志。
func TestSetControlProxyRoundTrip(t *testing.T) {
	s, err := NewHTTPShell("http://example.invalid/x.php", "cmd", "php")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ControlProxyURL(); got != "" {
		t.Fatalf("fresh shell must be direct, got %q", got)
	}
	if err := s.SetControlProxy("socks5://chengjian:s3cret@127.0.0.1:20801"); err != nil {
		t.Fatal(err)
	}
	got := s.ControlProxyURL()
	if got != "socks5://127.0.0.1:20801" {
		t.Fatalf("control route = %q, want socks5://127.0.0.1:20801", got)
	}
	if strings.Contains(got, "s3cret") {
		t.Fatal("control route must never expose proxy credentials")
	}
	// 清空 → 回到直连。
	if err := s.SetControlProxy(""); err != nil {
		t.Fatal(err)
	}
	if got := s.ControlProxyURL(); got != "" {
		t.Fatalf("cleared route must be direct, got %q", got)
	}
}

// TestSetControlProxyRejectsGarbage 钉住「非法路由地址要报错而不是静默直连」。
// 静默直连等于「以为走隧道，其实从平台出口出去」——这比失败更危险。
func TestSetControlProxyRejectsGarbage(t *testing.T) {
	s, err := NewHTTPShell("http://example.invalid/x.php", "cmd", "php")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"://nohost", "socks5://", "not a url"} {
		if err := s.SetControlProxy(bad); err == nil {
			t.Fatalf("SetControlProxy(%q) must fail", bad)
		}
	}
	// 失败后必须仍是直连（不能留下半个配置）。
	if got := s.ControlProxyURL(); got != "" {
		t.Fatalf("after failed set, route = %q, want direct", got)
	}
}

// TestProbeViaRequiresExplicitLang 钉住「经隧道登记时 auto 被拒绝」。
// auto 判型会连发 php/sh/aspx 三个探针，无法统一走隧道；让它半路失败
// 会得到「探测失败」这种误导性错误，不如在入口说清。
func TestProbeViaRequiresExplicitLang(t *testing.T) {
	for _, lang := range []string{"", "auto"} {
		if _, _, err := ProbeVia(t.Context(), "http://example.invalid/x.php", "cmd", lang, "socks5://127.0.0.1:1"); err == nil {
			t.Fatalf("lang %q must be rejected when routing through a tunnel", lang)
		}
	}
}
