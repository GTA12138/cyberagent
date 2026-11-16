package traffic

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestRouteFeedbackConcurrentHTTPExchanges(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%v", secure), func(t *testing.T) { testRouteFeedbackExchanges(t, secure) })
	}
}

func testRouteFeedbackExchanges(t *testing.T, secure bool) {
	t.Setenv("CHENGJIAN_PROXY_AUTH", "off")
	newServer := httptest.NewServer
	scheme := "http"
	if secure {
		newServer = httptest.NewTLSServer
		scheme = "https"
	}
	firstArrived := make(chan struct{})
	releaseFirst := make(chan struct{}, 1)
	backend1 := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(firstArrived)
		select {
		case <-releaseFirst:
		case <-r.Context().Done():
		}
		fmt.Fprint(w, "first")
	}))
	defer backend1.Close()
	defer func() {
		select {
		case releaseFirst <- struct{}{}:
		default:
		}
	}()
	backend2 := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "second") }))
	defer backend2.Close()
	up1 := routeTestProxy(t, backend1.URL)
	defer up1.Close()
	up2 := routeTestProxy(t, backend2.URL)
	defer up2.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	tr, err := Open(t.TempDir(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	var selections atomic.Int32
	observed := make(chan int, 4)
	tr.SetRouteSelector(func(string) (string, func(error), error) {
		n := int(selections.Add(1))
		up := up1.URL
		if n == 2 {
			up = up2.URL
		}
		return up, func(err error) {
			if err == nil {
				observed <- n
			}
		}, nil
	})
	go func() { _ = tr.Start() }()
	ready := false
	for i := 0; i < 100; i++ {
		c, e := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if e == nil {
			c.Close()
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("proxy did not start")
	}
	pu, _ := url.Parse("http://" + addr)
	transport := &http.Transport{Proxy: http.ProxyURL(pu), DisableKeepAlives: true, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx, "GET", scheme+"://example.invalid/first", nil)
		res, e := client.Do(req)
		if e == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}()
	select {
	case <-firstArrived:
	case <-time.After(3 * time.Second):
		t.Fatal("first request not routed")
	}
	res, err := client.Get(scheme + "://example.invalid/second")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	select {
	case n := <-observed:
		if n != 2 {
			t.Fatalf("second completion credited to %d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("missing feedback")
	}
	releaseFirst <- struct{}{}
	<-done
	select {
	case n := <-observed:
		if n != 1 {
			t.Fatalf("first completion credited to %d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("missing first feedback")
	}
}

func routeTestProxy(t *testing.T, target string) *httptest.Server {
	t.Helper()
	u, _ := url.Parse(target)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("expected CONNECT, got %s", r.Method)
			http.Error(w, "bad method", 400)
			return
		}
		remote, err := net.DialTimeout("tcp", u.Host, time.Second)
		if err != nil {
			http.Error(w, "dial failed", 502)
			return
		}
		defer remote.Close()
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { io.Copy(remote, client); remote.Close() }()
		io.Copy(client, remote)
	}))
}

func TestRouteChoiceRemainsBoundAfterHookReplacement(t *testing.T) {
	tr := &Traffic{}
	oldCalls, newCalls := 0, 0
	tr.SetRouteSelector(func(string) (string, func(error), error) {
		return "http://127.0.0.1:10001", func(error) { oldCalls++ }, nil
	})
	req, _ := http.NewRequest("GET", "http://example.invalid", nil)
	u, err := tr.resolveRequestUpstream(req)
	if err != nil {
		t.Fatal(err)
	}
	tr.SetRouteSelector(func(string) (string, func(error), error) {
		return "http://127.0.0.1:10002", func(error) { newCalls++ }, nil
	})
	u2, err := tr.resolveRequestUpstream(req)
	if err != nil || u.String() != u2.String() {
		t.Fatal("one exchange changed upstream")
	}
	req.Context().Value(routeChoiceKey{}).(*routeChoice).report(nil)
	if oldCalls != 1 || newCalls != 0 {
		t.Fatal("feedback used current hook instead of chosen route")
	}
}
