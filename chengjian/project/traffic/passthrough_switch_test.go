package traffic

import (
	"errors"
	"net/url"
	"testing"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

// 内网阻断期必须能整体禁用透明透传：透传是「绕过上游直连目标」，与
// blocked 的语义直接冲突——否则一次协议类错误就能让该 host 在 passTTL
// 窗口内直连成功，blocked 就成了摆设。
func TestSetPassthroughEnabledBlocksFlagging(t *testing.T) {
	tr := &Traffic{}
	f := &mproxy.Flow{Request: &mproxy.Request{URL: &url.URL{Host: "target.test:443"}}}
	proxyErr := errors.New("protocol error: received DATA on a HEAD request")

	// 默认允许：协议类错误会标记透传。
	tr.maybePassthrough(f, proxyErr)
	if !tr.pass.tunneled("target.test") {
		t.Fatal("默认应允许透传标记")
	}

	// 关闭后：不再标记。
	tr.SetPassthroughEnabled(false)
	tr.maybePassthrough(f, proxyErr)
	if tr.pass.tunneled("target.test") {
		t.Fatal("关闭透传后不应再标记")
	}

	// 关闭时应顺带清空既有标记（已标记的主机要立刻重新走 MITM）。
	tr.SetPassthroughEnabled(true)
	tr.maybePassthrough(f, proxyErr)
	if !tr.pass.tunneled("target.test") {
		t.Fatal("重新开启后应恢复标记能力")
	}
	tr.SetPassthroughEnabled(false)
	if tr.pass.tunneled("target.test") {
		t.Fatal("关闭时应清空已有标记")
	}
}
