package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/enrich"
)

// Only local HTTP fixtures, no LLM or external target. Proves endpoint signal,
// task attribution, real proxy traversal and the recording toggle together.
func TestLocalEnrichmentThroughTaskProxy(t *testing.T) {
	dsn := os.Getenv("CHENGJIAN_PG_DSN")
	if dsn == "" {
		t.Skip("explicit isolated CHENGJIAN_PG_DSN required")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	var requests, upstreamCalls atomic.Int64
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<title>Local fixture</title><form method="POST" action="/submit"><input name="value"></form><script src="/app.js"></script>`)
		case "/app.js":
			w.Header().Set("Content-Type", "application/javascript")
			io.WriteString(w, `fetch('/api/profile', {method: 'GET'});`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()
	target, _ := url.Parse(site.URL)
	forward := httputil.NewSingleHostReverseProxy(target)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Method == http.MethodConnect {
			remote, err := net.DialTimeout("tcp", target.Host, time.Second)
			if err != nil {
				http.Error(w, "local fixture unavailable", 502)
				return
			}
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				remote.Close()
				return
			}
			defer conn.Close()
			defer remote.Close()
			buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			buf.Flush()
			go func() { io.Copy(remote, buf); remote.Close() }()
			io.Copy(conn, remote)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	port := freePort(t)
	m := &Manager{pg: pg, globalProxy: upstream.URL, trafficOn: true}
	m.taskProxy = newTaskProxyManager(t.TempDir(), &fakeTunnelLister{}, m.GlobalProxy, port, port+1)
	defer m.taskProxy.Close()
	e := enrich.New(pg.Assets(), m.enrichProxyForTask, 1)
	defer e.Close()
	for _, recording := range []bool{true, false} {
		task, err := pg.CreateTask("local enrichment fixture", "local only", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer pg.DeleteTask(task.ID)
		if err := m.SetTrafficEnabled(recording); err != nil {
			t.Fatal(err)
		}
		id, err := pg.Assets().UpsertHTTPService(db.UpsertHTTPServiceReq{URL: site.URL, TaskID: task.ID})
		if err != nil {
			t.Fatal(err)
		}
		if n, _, err := pg.Assets().SitesWithoutEndpoints(task.ID, 5); err != nil || n != 1 {
			t.Fatalf("initial missing endpoints = %d, err=%v", n, err)
		}
		addr, ca := m.TaskProxyForTask(task.ID)
		proxyURL, err := url.Parse(addr)
		if err != nil || ca == "" {
			t.Fatal("missing task proxy or TLS trust")
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err := net.DialTimeout("tcp", proxyURL.Host, 100*time.Millisecond)
			if err == nil {
				conn.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("task proxy did not listen")
			}
			time.Sleep(10 * time.Millisecond)
		}
		e.ProbeSite(id, task.ID, site.URL)
		for {
			assets, err := pg.Assets().QueryByTask(task.ID, "endpoint", 10, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(assets) == 2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("endpoint increment = %d, want 2", len(assets))
			}
			time.Sleep(10 * time.Millisecond)
		}
		if n, _, err := pg.Assets().SitesWithoutEndpoints(task.ID, 5); err != nil || n != 0 {
			t.Fatalf("final missing endpoints = %d, err=%v", n, err)
		}
		var captures int
		inst := m.taskProxy.proxies[task.ID]
		if err := inst.tr.DB().QueryRow(`SELECT count(*) FROM exchanges`).Scan(&captures); err != nil {
			t.Fatal(err)
		}
		want := 0
		if recording {
			want = 2
		}
		if captures != want {
			t.Fatalf("recording=%v captures=%d, want %d", recording, captures, want)
		}
		t.Logf("recording=%v: sites_without_endpoints 1 -> 0; endpoints +2; captures=%d", recording, captures)
	}
	if requests.Load() != 4 || upstreamCalls.Load() != 4 {
		t.Fatalf("proxy traversal: target=%d upstream=%d; want 4 each", requests.Load(), upstreamCalls.Load())
	}
}
