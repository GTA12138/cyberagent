package enrich

import (
	"net/http"
	"net/url"
	"testing"
)

// 任务 id 必须能随请求走到 transport 的 Proxy func——这是「按任务选出口」的唯一
// 传递通道,断了就退回全局出口。
func TestWithTaskIDRoundTrip(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://a.example/", nil)
	if got := taskIDOf(req); got != 0 {
		t.Fatalf("未打标记的请求应读到 0, got %d", got)
	}
	stamped := withTaskID(req, 42)
	if got := taskIDOf(stamped); got != 42 {
		t.Fatalf("打标记后应读到 42, got %d", got)
	}
	// 原请求不受影响(WithContext 返回副本)。
	if got := taskIDOf(req); got != 0 {
		t.Fatalf("原请求不应被改写, got %d", got)
	}
	// taskID<=0 视为「无归属」,不打标记。
	if got := taskIDOf(withTaskID(req, 0)); got != 0 {
		t.Fatalf("taskID=0 不应打标记, got %d", got)
	}
}

// 同一份 Transport 对不同任务返回不同上游:这是 T-22 的核心断言。
// 用全局地址时这个测试会失败——两个任务会拿到同一个 proxy。
func TestBuildClientSelectsProxyPerTask(t *testing.T) {
	perTask := map[int64]string{
		7: "http://127.0.0.1:9107",
		9: "http://127.0.0.1:9109",
	}
	client := buildClient(func(taskID int64) string { return perTask[taskID] })
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("client 应装配 http.Transport 且带 Proxy func")
	}
	get := func(taskID int64) string {
		req, _ := http.NewRequest("GET", "http://target.example/", nil)
		req = withTaskID(req, taskID)
		u, err := tr.Proxy(req)
		if err != nil {
			t.Fatalf("proxy(taskID=%d) 报错: %v", taskID, err)
		}
		if u == nil {
			return ""
		}
		return u.String()
	}
	if got := get(7); got != perTask[7] {
		t.Fatalf("任务 7 应走 %s, got %q", perTask[7], got)
	}
	if got := get(9); got != perTask[9] {
		t.Fatalf("任务 9 应走 %s, got %q", perTask[9], got)
	}
	// 无归属任务 → 由 resolver 决定(server 侧回落全局),这里 resolver 返回空 → 直连。
	if got := get(0); got != "" {
		t.Fatalf("无归属任务应直连, got %q", got)
	}
}

// resolver 返回空 = 直连,不能变成「拼一个空 URL 报错」。
func TestBuildClientEmptyProxyIsDirect(t *testing.T) {
	client := buildClient(func(int64) string { return "" })
	tr := client.Transport.(*http.Transport)
	req, _ := http.NewRequest("GET", "http://target.example/", nil)
	u, err := tr.Proxy(withTaskID(req, 3))
	if err != nil {
		t.Fatalf("空地址不应报错: %v", err)
	}
	if u != nil {
		t.Fatalf("空地址应表示直连, got %v", u)
	}
}

// nil resolver 时不装 Proxy func(保持直连语义),不能 panic。
func TestBuildClientNilResolver(t *testing.T) {
	client := buildClient(nil)
	tr := client.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Fatal("nil resolver 不应装配 Proxy func")
	}
	// 合法地址必须能被解析——这是 resolver 契约的一半。
	if _, err := url.Parse("http://127.0.0.1:9107"); err != nil {
		t.Fatal(err)
	}
}

// 补全抓取必须上报给 tarpit,否则补全是预算外通道。
func TestNoteFetchObserver(t *testing.T) {
	type rec struct {
		taskID int64
		host   string
		n      int
	}
	var got []rec
	e := &Engine{}
	e.SetFetchObserver(func(taskID int64, host string, n int) {
		got = append(got, rec{taskID, host, n})
	})
	e.noteFetch(5, "a.example", 1)
	if len(got) != 1 || got[0] != (rec{5, "a.example", 1}) {
		t.Fatalf("应上报一次抓取, got %v", got)
	}
	// 无任务上下文/空 host/非正数次数都不上报——避免把无归属抓取算进某个任务。
	e.noteFetch(0, "a.example", 1)
	e.noteFetch(5, "", 1)
	e.noteFetch(5, "a.example", 0)
	if len(got) != 1 {
		t.Fatalf("非法参数不应上报, got %v", got)
	}
	// 未注入 observer 时静默跳过。
	var bare Engine
	bare.noteFetch(5, "a.example", 1)
	var nilEngine *Engine
	nilEngine.noteFetch(5, "a.example", 1)
}
