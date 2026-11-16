package traffic

import (
	"errors"
	"testing"
)

// 装/卸选路钩子。nil,nil 必须彻底清空，否则一个已销毁任务的路由表会被
// 继续用来选路——那会让请求打到已经不存在的隧道入口上。
func TestSetRouteHooksInstallClear(t *testing.T) {
	tr := &Traffic{}
	if tr.RouteHooksInstalled() {
		t.Fatal("零值不应有选路钩子")
	}
	tr.SetRouteHooks(func(string) (string, error) { return "", nil }, nil)
	if !tr.RouteHooksInstalled() {
		t.Fatal("安装后应报告已启用")
	}
	tr.SetRouteHooks(nil, nil)
	if tr.RouteHooksInstalled() {
		t.Fatal("nil,nil 应清空钩子")
	}
	// 只装观测也算启用（选路可以没有意见，观测仍需归因）。
	tr.SetRouteHooks(nil, func(string, error) {})
	if !tr.RouteHooksInstalled() {
		t.Fatal("只装 observe 也应报告已启用")
	}
}

// 选路优先级：钩子有意见时用钩子的；没意见时回落全局上游；
// 钩子报错必须让请求失败（「拒绝承载」要能生效，不能只是日志）。
func TestResolveUpstreamForPrecedence(t *testing.T) {
	tr := &Traffic{}
	if err := tr.SetUpstreamProxy("http://127.0.0.1:9000"); err != nil {
		t.Fatal(err)
	}

	// 无钩子 → 全局上游。
	u, err := tr.resolveUpstreamFor("10.0.0.8")
	if err != nil || u == nil || u.Host != "127.0.0.1:9000" {
		t.Fatalf("无钩子时应回落全局上游，得到 (%v,%v)", u, err)
	}

	// 钩子有意见 → 用钩子的，且按目标 host 分。
	var gotHost string
	tr.SetRouteHooks(func(host string) (string, error) {
		gotHost = host
		if host == "10.1.0.5" {
			return "socks5://127.0.0.1:20802", nil
		}
		return "socks5://chengjian:pw@127.0.0.1:20801", nil
	}, nil)
	u, err = tr.resolveUpstreamFor("10.1.0.5")
	if err != nil || u == nil || u.Host != "127.0.0.1:20802" {
		t.Fatalf("应按 host 选到 T2，得到 (%v,%v)", u, err)
	}
	if gotHost != "10.1.0.5" {
		t.Errorf("钩子应收到目标 host，得到 %q", gotHost)
	}
	// suo5 入口的口令要保留（不带口令连不上）。
	u, err = tr.resolveUpstreamFor("10.2.0.9")
	if err != nil || u == nil || u.Host != "127.0.0.1:20801" {
		t.Fatalf("应选到 T1，得到 (%v,%v)", u, err)
	}
	if pw, _ := u.User.Password(); u.User.Username() != "chengjian" || pw != "pw" {
		t.Errorf("上游口令丢失: %v", u)
	}

	// 钩子无意见 → 回落全局上游（无隧道/隧道全死/内网阻断都走这条）。
	tr.SetRouteHooks(func(string) (string, error) { return "", nil }, nil)
	u, err = tr.resolveUpstreamFor("10.0.0.8")
	if err != nil || u == nil || u.Host != "127.0.0.1:9000" {
		t.Fatalf("钩子无意见时应回落全局上游，得到 (%v,%v)", u, err)
	}

	// 钩子报错 → 请求失败。这是「禁止承载」的强制点。
	blocked := errors.New("blocked")
	tr.SetRouteHooks(func(string) (string, error) { return "", blocked }, nil)
	if _, err := tr.resolveUpstreamFor("10.0.0.8"); !errors.Is(err, blocked) {
		t.Fatalf("钩子报错应使请求失败，得到 %v", err)
	}

	// 钩子返回非法 URL → 失败，而不是静默回落到全局出口（那会造成
	// 「以为在隧道里、其实从平台直连」——最危险的一种降级）。
	tr.SetRouteHooks(func(string) (string, error) { return "nope://x", nil }, nil)
	if _, err := tr.resolveUpstreamFor("10.0.0.8"); err == nil {
		t.Fatal("非法上游 URL 应报错，不能静默回落")
	}
}

// 观测钩子必须能收到 (host, err)，且 nil err 表示拿到响应。
func TestObserveRouteReportsHostAndError(t *testing.T) {
	tr := &Traffic{}
	var hosts []string
	var errs []error
	tr.SetRouteHooks(nil, func(host string, err error) {
		hosts = append(hosts, host)
		errs = append(errs, err)
	})
	// 没有 flow 时不应 panic，也不应回调。
	tr.observeRoute(nil, nil)
	if len(hosts) != 0 {
		t.Fatalf("nil flow 不应触发观测，得到 %v", hosts)
	}
}
