package enrich

import (
	"net/url"
	"strings"
	"testing"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}

// endpointsOf 把候选压成 "METHOD path" 便于断言，path 只保留 Path 部分。
func endpointsOf(cs []Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		u, _ := url.Parse(c.URL)
		out = append(out, c.Method+" "+u.Path)
	}
	return out
}

func hasEndpoint(cs []Candidate, method, path string) bool {
	for _, c := range cs {
		u, _ := url.Parse(c.URL)
		if c.Method == method && u.Path == path {
			return true
		}
	}
	return false
}

func TestClassify(t *testing.T) {
	cases := []struct {
		url, ct string
		body    string
		want    Kind
	}{
		{"https://t/app.js", "text/javascript", "", KindJS},
		{"https://t/assets/a.6e31.js", "", "", KindJS},
		{"https://t/app.js.map", "application/json", "{}", KindSourceMap},
		{"https://t/runtime-config.json", "application/json", `{"api_base":"/api"}`, KindJSON},
		{"https://t/openapi.json", "application/json", `{"openapi":"3.0.0","paths":{}}`, KindOpenAPI},
		{"https://t/", "text/html; charset=utf-8", "<html></html>", KindHTML},
		{"https://t/x", "", "<!doctype html><html>", KindHTML},
		{"https://t/x", "", `{"a":1}`, KindJSON},
		{"https://t/logo.svg", "image/png", "\x89PNG", KindOther},
	}
	for _, c := range cases {
		if got := Classify(c.url, c.ct, []byte(c.body)); got != c.want {
			t.Errorf("Classify(%s,%s) = %s, want %s", c.url, c.ct, got, c.want)
		}
	}
}

func TestJSDeterministicEndpoints(t *testing.T) {
	base := mustURL(t, "https://t/static/app.js")
	src := `
		fetch("/api/v1/orders");
		fetch('/api/v1/bids', { method: 'post', body: x });
		xhr.open("PUT", "/api/v1/providers/7");
		axios.get("/api/v1/me");
		axios.post("/api/v1/login", {user: u, pass: p});
		http.delete("/api/v1/session");
	`
	m := Extract(base, KindJS, []byte(src))

	for _, want := range [][2]string{
		{"GET", "/api/v1/orders"},
		{"POST", "/api/v1/bids"},
		{"PUT", "/api/v1/providers/7"},
		{"GET", "/api/v1/me"},
		{"POST", "/api/v1/login"},
		{"DELETE", "/api/v1/session"},
	} {
		if !hasEndpoint(m.Endpoints, want[0], want[1]) {
			t.Errorf("missing %s %s; got %v", want[0], want[1], endpointsOf(m.Endpoints))
		}
	}
	// fetch(url, {method:'post'}) 不能同时产出 GET——那会造出一个不存在的端点。
	if hasEndpoint(m.Endpoints, "GET", "/api/v1/bids") {
		t.Errorf("spurious GET for explicitly-POST endpoint: %v", endpointsOf(m.Endpoints))
	}
}

// 文章 5.3 的混淆样例：确定性规则抽不出 URL，但必须被识别为「值得进语义层」。
func TestJSObfuscatedTriggersSemanticLayer(t *testing.T) {
	base := mustURL(t, "https://t/assets/capacity-planning.6e31d9a7.bundle.js")
	src := `
		const __ccRead__ = (order) => atob(order.map((index) => __ccPieces__[index]).join(""));
		const __ccRoute__ = () => __ccRead__([5,8,11,14,17,20]);
		const __ccEndpoint__ = () => __ccRead__([3,6,9,12,15,18,21,23,25,27,29,31]);
		fetch(__ccEndpoint__(), { method: "POST" });
	`
	m := Extract(base, KindJS, []byte(src))

	if len(m.Endpoints) != 0 {
		t.Fatalf("obfuscated calls must not produce guessed endpoints, got %v", endpointsOf(m.Endpoints))
	}
	if m.Trigger < triggerThreshold {
		t.Fatalf("trigger = %d, want >= %d (signals=%v)", m.Trigger, triggerThreshold, m.Signals)
	}
	if !contains(m.Signals, "decode") || !contains(m.Signals, "dynamic-call") {
		t.Fatalf("signals = %v, want decode + dynamic-call", m.Signals)
	}
}

// 全字面量的普通文件不应该触发语义层——这是成本闸门的第一道。
func TestJSPlainFileDoesNotTrigger(t *testing.T) {
	base := mustURL(t, "https://t/static/vendor.js")
	src := `fetch("/api/v1/orders"); axios.get("/api/v1/me");`
	m := Extract(base, KindJS, []byte(src))
	if m.Trigger != 0 {
		t.Fatalf("trigger = %d, want 0 (signals=%v)", m.Trigger, m.Signals)
	}
}

func TestJSCrossOriginDropped(t *testing.T) {
	base := mustURL(t, "https://t/app.js")
	src := `fetch("https://evil.example/api/x"); fetch("//other.example/api/y"); fetch("/api/ok");`
	m := Extract(base, KindJS, []byte(src))
	if len(m.Endpoints) != 1 || !hasEndpoint(m.Endpoints, "GET", "/api/ok") {
		t.Fatalf("cross-origin not dropped: %v", endpointsOf(m.Endpoints))
	}
}

func TestJSConfigAndSourceMap(t *testing.T) {
	base := mustURL(t, "https://t/assets/app.js")
	src := `
		const cfg = { baseURL: "/api/v2", wsUrl: "wss://t/ws" };
		//# sourceMappingURL=app.js.map
	`
	m := Extract(base, KindJS, []byte(src))
	if len(m.Services) == 0 || !containsSub(m.Services, "https://t/api/v2") {
		t.Fatalf("services = %v, want https://t/api/v2", m.Services)
	}
	if len(m.SourceMaps) != 1 || m.SourceMaps[0] != "https://t/assets/app.js.map" {
		t.Fatalf("sourcemaps = %v", m.SourceMaps)
	}
	// wss 与 https 不同源，必须丢弃——不能把 websocket 基址当成可抓取的页面。
	if containsSub(m.Services, "wss://t/ws") {
		t.Fatalf("wss service leaked into http services: %v", m.Services)
	}
}

func TestHTMLFormAndLinks(t *testing.T) {
	base := mustURL(t, "https://t/login")
	page := `<html><body>
		<a href="/admin/users">users</a>
		<a href="https://other.example/x">out</a>
		<a href="/logo.svg">logo</a>
		<form action="/session" method="POST">
			<input name="username"><input name="password" type="password">
		</form>
		<script src="/static/app.js"></script>
		<script>fetch("/api/inline");</script>
	</body></html>`
	m := Extract(base, KindHTML, []byte(page))

	if !hasEndpoint(m.Endpoints, "POST", "/session") {
		t.Fatalf("form endpoint missing: %v", endpointsOf(m.Endpoints))
	}
	if !hasEndpoint(m.Endpoints, "GET", "/api/inline") {
		t.Fatalf("inline script endpoint missing: %v", endpointsOf(m.Endpoints))
	}
	for _, c := range m.Endpoints {
		if u, _ := url.Parse(c.URL); u.Path == "/session" && len(c.Params) != 2 {
			t.Fatalf("form params = %v, want 2", c.Params)
		}
	}
	if len(m.Pages) != 1 || !strings.HasSuffix(m.Pages[0], "/admin/users") {
		t.Fatalf("pages = %v, want only same-origin /admin/users", m.Pages)
	}
	if len(m.Scripts) != 1 || !strings.HasSuffix(m.Scripts[0], "/static/app.js") {
		t.Fatalf("scripts = %v", m.Scripts)
	}
}

func TestJSONConfigRecovery(t *testing.T) {
	base := mustURL(t, "https://t/assets/coldchain/runtime-config.json")
	body := `{
		"api_base": "/api/v3",
		"asset_manifest": "/static/manifest.json",
		"release": {"version": "1.2.3", "build": 42},
		"integration_spec": {"baseUrl": "/gw"}
	}`
	m := Extract(base, KindJSON, []byte(body))
	if !containsSub(m.Services, "https://t/api/v3") {
		t.Fatalf("services = %v, want https://t/api/v3", m.Services)
	}
	if !containsSub(m.Services, "https://t/gw") {
		t.Fatalf("nested config key not recovered: %v", m.Services)
	}
	// 非基址类字段（version/build）不得被当成 URL。
	if containsSub(m.Services, "1.2.3") {
		t.Fatalf("version leaked into services: %v", m.Services)
	}
}

func TestOpenAPIPaths(t *testing.T) {
	base := mustURL(t, "https://t/openapi.json")
	body := `{
		"openapi": "3.0.0",
		"servers": [{"url": "https://t/api"}],
		"paths": {
			"/users": {"get": {"parameters": [
				{"name": "page", "in": "query", "schema": {"type": "integer"}}
			]}},
			"/users/{id}": {"delete": {}}
		}
	}`
	m := Extract(base, KindOpenAPI, []byte(body))

	if !hasEndpoint(m.Endpoints, "GET", "/api/users") {
		t.Fatalf("openapi endpoint missing: %v", endpointsOf(m.Endpoints))
	}
	if !hasEndpoint(m.Endpoints, "DELETE", "/api/users/{id}") {
		t.Fatalf("openapi path param endpoint missing: %v", endpointsOf(m.Endpoints))
	}
	for _, c := range m.Endpoints {
		if u, _ := url.Parse(c.URL); u.Path == "/api/users" {
			if len(c.Params) != 1 || c.Params[0]["name"] != "page" || c.Params[0]["location"] != "query" {
				t.Fatalf("params = %v", c.Params)
			}
		}
	}
}

func TestSourceMapSourcesContent(t *testing.T) {
	base := mustURL(t, "https://t/assets/app.js.map")
	body := `{"sources":["webpack:///src/api.ts"],
		"sourcesContent":["export const load = () => fetch('/api/from-map');"]}`
	m := Extract(base, KindSourceMap, []byte(body))
	if !hasEndpoint(m.Endpoints, "GET", "/api/from-map") {
		t.Fatalf("sourcesContent not analyzed: %v", endpointsOf(m.Endpoints))
	}
}

func TestStripJSCommentsKeepsStrings(t *testing.T) {
	src := `const u = "https://t/api/x"; // fetch("/api/commented")
	const v = "not // a comment"; /* fetch("/api/block") */
	fetch("/api/real");`
	out := stripJSComments(src)
	if strings.Contains(out, "/api/commented") || strings.Contains(out, "/api/block") {
		t.Fatalf("comments not stripped: %s", out)
	}
	if !strings.Contains(out, "not // a comment") {
		t.Fatalf("string content was damaged: %s", out)
	}
}

func TestEvidenceWindowsBoundedAndAligned(t *testing.T) {
	// 构造一个远超上限的脚本，锚点分布在中间。
	var b strings.Builder
	for i := 0; i < 400; i++ {
		b.WriteString("var padding_" + strings.Repeat("x", 40) + " = 1;\n")
	}
	b.WriteString("fetch(apiBase + decode(table[3]));\n")
	for i := 0; i < 400; i++ {
		b.WriteString("var tail_" + strings.Repeat("y", 40) + " = 2;\n")
	}
	src := b.String()

	got := EvidenceWindows(src, 8*1024)
	if len(got) > 8*1024 {
		t.Fatalf("window = %d bytes, want <= 8192", len(got))
	}
	if !strings.Contains(got, "decode(table[3])") {
		t.Fatalf("anchor line lost from windows")
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func containsSub(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
