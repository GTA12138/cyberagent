// obs_katana_test.go 端点参数观测抽取与 katana 封装的纯函数测试。
package server

import (
	"strings"
	"testing"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/webtest"
)

func obsOf(t *testing.T, req webtest.Request) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, o := range observeRequestParams(req) {
		out[o.Loc+":"+o.Name] = o.Value
	}
	return out
}

func TestObserveRequestParamsQueryAndForm(t *testing.T) {
	req := webtest.Request{
		Method:  "POST",
		URL:     "http://t.local/api/users?id=1&tab=info",
		Headers: [][2]string{{"Content-Type", "application/x-www-form-urlencoded"}},
		Body:    []byte("user=admin&pass=123"),
	}
	got := obsOf(t, req)
	if got["query:id"] != "1" || got["query:tab"] != "info" {
		t.Fatalf("query 抽取错误: %v", got)
	}
	if got["body:user"] != "admin" || got["body:pass"] != "123" {
		t.Fatalf("form body 抽取错误: %v", got)
	}
	// URL 存不带 query 的形态：从原始输入直接验证。
	for _, o := range observeRequestParams(req) {
		if o.Loc == "query" && strings.Contains(o.URL, "?") {
			t.Fatalf("观测 URL 不应带 query: %s", o.URL)
		}
	}
}

func TestObserveRequestParamsJSONTopLevelOnly(t *testing.T) {
	req := webtest.Request{
		Method:  "PUT",
		URL:     "https://t.local/api/order",
		Headers: [][2]string{{"Content-Type", "application/json"}},
		Body:    []byte(`{"orderId": "A-77", "amount": 3, "nested": {"x": 1}, "flag": true}`),
	}
	got := obsOf(t, req)
	if got["body:orderId"] != "A-77" || got["body:amount"] != "3" || got["body:flag"] != "true" {
		t.Fatalf("JSON 顶层标量抽取错误: %v", got)
	}
	if _, ok := got["body:nested"]; ok {
		t.Fatal("嵌套对象不应抽成参数（伪参数名噪声）")
	}
}

func TestNormalizeEndpointObsTruncatesAndRejects(t *testing.T) {
	long := strings.Repeat("a", 300)
	in, ok := db.NormalizeEndpointObs(db.EndpointObsInput{Method: "get", URL: " http://t ", Name: " q ", Loc: "Query", Value: " " + long + " "})
	if !ok {
		t.Fatal("合法输入被拒")
	}
	if in.Method != "GET" || in.Loc != "query" || len(in.Value) != db.MaxObsValueLen {
		t.Fatalf("清洗错误: %+v", in)
	}
	if _, ok := db.NormalizeEndpointObs(db.EndpointObsInput{Method: "GET", URL: "u", Name: "n", Loc: "header"}); ok {
		t.Fatal("非法 loc 应被拒（观测口径只有 query/body）")
	}
	if _, ok := db.NormalizeEndpointObs(db.EndpointObsInput{Method: "GET", URL: "", Name: "n", Loc: "query"}); ok {
		t.Fatal("空 URL 应被拒")
	}
}

func TestKatanaArgsWhitelist(t *testing.T) {
	args := katanaArgs("http://t.local", 3, true, []string{"admin", " api "})
	joined := strings.Join(args, " ")
	for _, want := range []string{"-u http://t.local", "-d 3", "-headless", "-kf admin", "-kf api", "-silent"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("参数缺 %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "api ") && !strings.Contains(joined, "-kf api") {
		t.Fatal("known_field 未去空白")
	}
	// depth<=0 不带 -d（走 katana 默认）。
	if args2 := katanaArgs("http://t.local", 0, false, nil); strings.Contains(strings.Join(args2, " "), " -d") {
		t.Fatal("depth=0 不应传 -d")
	}
}

func TestSplitKatanaURLs(t *testing.T) {
	text := "[INF] starting\nhttp://t.local/a\nhttps://t.local/b?x=1\n  \nnoise line\nhttp://t.local/c\n"
	urls := splitKatanaURLs(text)
	if len(urls) != 3 {
		t.Fatalf("应抽出 3 个 URL, got %v", urls)
	}
}
