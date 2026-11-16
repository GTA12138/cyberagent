package traffic

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestStreamingCapturePreservesWireAndRecordsLimit(t *testing.T) {
	for _, size := range []int{(6 << 20) + 137, maxStreamCapture + 137} {
		tr, _ := openTraffic(t)
		f := newFlow("stream.test", "GET", "/large", nil, nil)
		f.Stream = true
		s := &sink{t: tr}
		body := bytes.Repeat([]byte("z"), size)
		reader := s.StreamResponseModifier(f, bytes.NewReader(body))
		wire, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(wire, body) {
			t.Fatal("capture changed network bytes", err)
		}
		id := onlyExchangeID(t, tr)
		var raw string
		if err := tr.db.QueryRow(`SELECT metadata FROM capture_state WHERE id=?`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var state captureState
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			t.Fatal(err)
		}
		if state.Response.Received != int64(size) || state.Response.Retained != int64(min(size, maxStreamCapture)) || state.Response.Complete != (size <= maxStreamCapture) {
			t.Fatalf("wrong completeness: %s", raw)
		}
		var hash string
		if err := tr.db.QueryRow(`SELECT resp_blob FROM exchange_bodies WHERE id=?`, id).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		page, total, err := tr.BlobRange(hash, int64(min(size, maxStreamCapture)-9), 9)
		if err != nil || total != int64(min(size, maxStreamCapture)) || string(page) != "zzzzzzzzz" {
			t.Fatal("captured tail unavailable", err, total)
		}
		tr.Close()
	}
}

func TestProxyLargeAndSSECaptureIntegration(t *testing.T) {
	for _, contentType := range []string{"application/octet-stream", "text/event-stream"} {
		body := strings.Repeat("data: captured-event\n\n", (6<<20)/22) + "TAIL"
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			io.WriteString(w, body)
		}))
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := listener.Addr().String()
		listener.Close()
		tr, err := Open(t.TempDir(), addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { tr.Close() })
		go func() { _ = tr.Start() }()
		up := false
		for i := 0; i < 100; i++ {
			conn, e := net.DialTimeout("tcp", addr, 50*time.Millisecond)
			if e == nil {
				conn.Close()
				up = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !up {
			t.Fatal("proxy did not start")
		}
		proxyURL, _ := url.Parse(tr.ProxyAddr())
		transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		transport.CloseIdleConnections()
		origin.Close()
		if err != nil || string(wire) != body {
			t.Fatal("stream changed", err)
		}
		id := onlyExchangeID(t, tr)
		if !strings.Contains(tr.captureMetadata(id), `"complete":true`) {
			t.Fatal("stream absent/incomplete")
		}
		tr.Close()
	}
}
