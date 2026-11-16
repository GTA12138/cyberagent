// httpreplay_test.go 结构化重放执行层的测试：录制报文解析 round-trip、
// 单点变异落地、真实发送 + 噪声标定差分（本地 httptest 站点，无外联）。
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chengjian.local/chengjian/webtest"
)

func TestParseRecordedRequestRoundTrip(t *testing.T) {
	raw := "POST /api/login?from=web HTTP/1.1\r\nHost: target.local:8080\r\nContent-Type: application/x-www-form-urlencoded\r\nCookie: sid=abc\r\n\r\nuser=admin&pass=123"
	req, err := parseRecordedRequest(raw, "http")
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || !strings.Contains(req.URL, "http://target.local:8080/api/login?from=web") {
		t.Fatalf("方法/URL 解析错误: %+v", req)
	}
	if v, ok := req.Header("Cookie"); !ok || v != "sid=abc" {
		t.Fatalf("Cookie 头丢失: %q %v", v, ok)
	}
	if string(req.Body) != "user=admin&pass=123" {
		t.Fatalf("正文解析错误: %q", string(req.Body))
	}
	// LF-only 历史数据也要能解。
	req2, err := parseRecordedRequest(strings.ReplaceAll(raw, "\r\n", "\n"), "https")
	if err != nil || !strings.HasPrefix(req2.URL, "https://") {
		t.Fatalf("LF 形态/scheme 覆盖失败: %+v %v", req2, err)
	}
}

func TestParseRecordedRequestRejectsGarbage(t *testing.T) {
	if _, err := parseRecordedRequest("not a request", "http"); err == nil {
		t.Fatal("无请求行应报错")
	}
	if _, err := parseRecordedRequest("GET / HTTP/1.1\r\nX-A: 1\r\n\r\n", "http"); err == nil {
		t.Fatal("缺 Host 应报错（无法确定目标）")
	}
}

// 本地差分管线：对照组两次稳定、变体状态码不同 → different。
func TestReplayPipelineDifferentialVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("id") {
		case "1":
			_, _ = w.Write([]byte("user profile for id=1"))
		case "2":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("forbidden for other user"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	raw := "GET /profile?id=1 HTTP/1.1\r\nHost: " + host + "\r\nUser-Agent: test\r\n\r\n"
	base, err := parseRecordedRequest(raw, "http")
	if err != nil {
		t.Fatal(err)
	}
	variant, verr := webtest.Replay(base, webtest.Mutation{Kind: "query", Name: "id", From: "1", To: "2"})
	if verr != nil {
		t.Fatal(verr)
	}
	ctx := context.Background()
	controlA, err := sendReplayRequest(ctx, base, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	controlB, err := sendReplayRequest(ctx, base, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	vr, err := sendReplayRequest(ctx, variant, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !webtest.Stable(controlA, controlB) {
		t.Fatalf("本地站点对照组应稳定: %+v vs %+v", controlA, controlB)
	}
	cmp := webtest.CompareWithNoise(controlA, controlB, vr)
	if cmp.Verdict != webtest.VerdictDifferent {
		t.Fatalf("越权变体应判 different: %+v", cmp)
	}
	if cmp.FindingState() != "inconclusive" {
		t.Fatalf("different 不能直接确认漏洞: %q", cmp.FindingState())
	}
}

// 阴性路径：变异一个服务器忽略的参数（行为不变）→ same/inconclusive。
// 注意不能用「页面回显的参数」做阴性用例——回显变了就是真实差异，判定器
// 判 different 恰是正确行为（上一版测试在这里写反了预期）。
func TestReplayPipelineSameVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("stable page for " + r.URL.Query().Get("name")))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	base, err := parseRecordedRequest("GET /hello?name=abc&utm_source=x HTTP/1.1\r\nHost: "+host+"\r\n\r\n", "http")
	if err != nil {
		t.Fatal(err)
	}
	variant, verr := webtest.Replay(base, webtest.Mutation{Kind: "query", Name: "utm_source", To: "y"})
	if verr != nil {
		t.Fatal(verr)
	}
	ctx := context.Background()
	ca, _ := sendReplayRequest(ctx, base, "", 5*time.Second)
	cb, _ := sendReplayRequest(ctx, base, "", 5*time.Second)
	vr, _ := sendReplayRequest(ctx, variant, "", 5*time.Second)
	cmp := webtest.CompareWithNoise(ca, cb, vr)
	if cmp.Verdict != webtest.VerdictSame {
		t.Fatalf("服务器忽略的参数变异应判 same: %+v", cmp)
	}
	if cmp.FindingState() != "inconclusive" {
		t.Fatalf("same 不能直接否定漏洞: %q", cmp.FindingState())
	}
}
