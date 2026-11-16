package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func expectedBashUANote() string {
	if runtime.GOOS == "windows" {
		return strings.ReplaceAll(bashUANote, "$CHENGJIAN_UA", "$env:CHENGJIAN_UA")
	}
	return bashUANote
}

func TestCustomHTTPProxyFailureNeverSendsDirect(t *testing.T) {
	var hits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, "unexpected") }))
	defer origin.Close()
	for _, spec := range []httpExec{{URL: origin.URL, UseRecordingProxy: true}, {URL: origin.URL, Proxy: "http://%invalid"}} {
		raw, _ := json.Marshal(spec)
		r, err := (&Server{}).runHTTPTool(context.Background(), raw, nil, nil)
		if err != nil || !r.IsError || hits.Load() != 0 {
			t.Fatalf("proxy failure escaped: %v %v hits=%d", r, err, hits.Load())
		}
	}
}

func TestCustomHTTPRetainsTailAndDeclaresReadLimit(t *testing.T) {
	for _, n := range []int{40000, (2 << 20) + 20} {
		body := strings.Repeat("x", n) + "TAIL_MARKER"
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		raw, _ := json.Marshal(httpExec{URL: origin.URL})
		r, err := (&Server{}).runHTTPTool(context.Background(), raw, nil, &actool.ToolContext{OutputDir: t.TempDir()})
		origin.Close()
		if err != nil || r.IsError {
			t.Fatalf("HTTP failed: %v %v", r, err)
		}
		var result struct {
			BodyFile     string `json:"body_file"`
			ReadComplete bool   `json:"read_complete"`
		}
		if err := json.Unmarshal([]byte(r.Flatten()), &result); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(result.BodyFile)
		if err != nil || string(data) != body[:min(len(body), 2<<20)] || result.ReadComplete != (len(body) <= 2<<20) {
			t.Fatalf("lost bytes or false completeness: n=%d err=%v complete=%t", n, err, result.ReadComplete)
		}
	}
}
