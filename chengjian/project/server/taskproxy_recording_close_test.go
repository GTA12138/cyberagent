package server

import (
	"context"
	"chengjian.local/chengjian/traffic"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestCaptureOffDuringTaskClose(t *testing.T) {
	port := freePort(t)
	m := newTaskProxyManager(t.TempDir(), &fakeTunnelLister{}, nil, port, port)
	defer m.Close()
	addr, _ := m.ForTask(context.Background(), 1)
	if addr == "" {
		t.Fatal("proxy not created")
	}
	inst := m.proxies[1]
	parsed, _ := url.Parse(addr)
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", parsed.Host, 20*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	leased := make(chan struct{})
	release := make(chan struct{})
	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		_ = m.withTraffic(context.Background(), 1, func(*traffic.Traffic) error {
			close(leased)
			<-release
			return nil
		})
	}()
	<-leased
	closeDone := make(chan struct{})
	go func() { m.CloseTask(1); close(closeDone) }()
	defer func() { close(release); <-leaseDone; <-closeDone }()
	for {
		m.mu.Lock()
		closing := m.closing[1] != nil
		m.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CloseTask did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	m.SetRecordEnabled(false)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "local capture test") }))
	defer local.Close()
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(parsed)}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get(local.URL + "/after-capture-off")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var captures int
	if err := inst.tr.DB().QueryRow("SELECT count(*) FROM exchanges").Scan(&captures); err != nil {
		t.Fatal(err)
	}
	if captures != 0 {
		t.Fatalf("SetRecordEnabled(false) returned but closing task still persisted %d exchange(s)", captures)
	}
}
