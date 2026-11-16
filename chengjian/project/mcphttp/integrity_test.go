package mcphttp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestToolsDiscoverEveryPageAndRejectCursorLoop(t *testing.T) {
	for _, loop := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				ID     int            `json:"id"`
				Params map[string]any `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				return
			}
			name, cursor := "first", "page-two"
			if req.Params["cursor"] == "page-two" {
				name = "second"
				if !loop {
					cursor = ""
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []any{map[string]any{"name": name}}, "nextCursor": cursor}})
		}))
		c := &Client{server: "fixture", url: srv.URL, http: srv.Client()}
		tools, err := c.Tools(context.Background())
		srv.Close()
		if loop {
			if err == nil || !strings.Contains(err.Error(), "cursor") {
				t.Fatalf("loop hidden: %v", err)
			}
			continue
		}
		if err != nil || len(tools) != 2 || tools[1].Name() != "mcp__fixture__second" {
			t.Fatalf("missing second page: tools=%v err=%v", tools, err)
		}
	}
}

func TestMCPRejectsUnrelatedAndOversizedJSON(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if oversized {
				io.WriteString(w, strings.Repeat(" ", maxRPCBody+1))
				return
			}
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":999,"result":{}}`)
		}))
		c := &Client{url: srv.URL, http: srv.Client()}
		_, err := c.call(context.Background(), "fixture", nil)
		srv.Close()
		if err == nil {
			t.Fatal("unrelated or oversized response accepted")
		}
	}
}

func TestLegacyMultilineSSEAndDisconnect(t *testing.T) {
	c := &Client{legacySSE: true, streamDone: make(chan struct{}), streamErr: make(chan error, 1), pending: map[int]chan *rpcResponse{7: make(chan *rpcResponse, 1)}}
	reply := c.pending[7]
	c.readLegacySSE(bufio.NewReader(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\n" + "data: \"id\":7,\"result\":{\"ok\":true}}\n\n")))
	select {
	case got := <-reply:
		if got.ID != 7 {
			t.Fatal("wrong response")
		}
	default:
		t.Fatal("multiline response lost")
	}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := c.legacyRoundTrip(ctx, rpcRequest{ID: 8, Method: "fixture"}, true)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("disconnection not broadcast: %v", err)
		}
	}
}

func TestSSEAggregateLimit(t *testing.T) {
	body := strings.Repeat("data: "+strings.Repeat("x", 1024)+"\n", maxRPCBody/1024+1)
	if _, err := readSSEFrame(bufio.NewReader(strings.NewReader(body))); err == nil {
		t.Fatal("unbounded legacy event")
	}
	if _, err := parseSSE(strings.NewReader(body), 1); err == nil {
		t.Fatal("unbounded streamable event")
	}
}

func TestMCPStructuredAndResourceContentIsRetained(t *testing.T) {
	for _, raw := range []string{
		`{"structuredContent":{"page":"second","items":[1,2]}}`,
		`{"content":[{"type":"resource","resource":{"uri":"fixture://report","text":"embedded evidence"}}]}`,
		`{"content":[{"type":"image","data":"YmluYXJ5","mimeType":"image/png"}]}`,
	} {
		text, isError, err := renderToolResult(json.RawMessage(raw))
		if err != nil || isError || text == "" {
			t.Fatalf("content lost: %q %v", text, err)
		}
		if strings.Contains(raw, "structuredContent") && !strings.Contains(text, `"items":[1,2]`) {
			t.Fatal("structured result lost")
		}
		if strings.Contains(raw, "embedded evidence") && !strings.Contains(text, "embedded evidence") {
			t.Fatal("embedded resource lost")
		}
		if strings.Contains(raw, "YmluYXJ5") && (!strings.Contains(text, "YmluYXJ5") || !strings.Contains(text, "完整")) {
			t.Fatal("unsupported binary block silently dropped")
		}
	}
}

func TestMCPTransportFailurePropagatesForExecutionJournal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "fixture unavailable", 503) }))
	defer srv.Close()
	c := &Client{server: "fixture", url: srv.URL, http: srv.Client()}
	res, err := c.wrap(remoteTool{Name: "effect"}).Call(context.Background(), json.RawMessage(`{}`), nil)
	if err == nil || !res.IsError {
		t.Fatal("transport outcome disguised as an ordinary completed tool result")
	}
}

func TestLegacyWaitingCallHonorsCancellation(t *testing.T) {
	c := &Client{streamDone: make(chan struct{})}
	c.legacyOnce.Do(func() { c.legacyGate = make(chan struct{}, 1) })
	c.legacyGate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.legacyRoundTrip(ctx, rpcRequest{}, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued call ignored cancellation: %v", err)
	}
}

func TestLegacyFinalResponseSurvivesSimultaneousDisconnect(t *testing.T) {
	for i := 0; i < 30; i++ {
		c := &Client{streamDone: make(chan struct{}), streamErr: make(chan error, 1), pending: map[int]chan *rpcResponse{}}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.pendingMu.Lock()
			c.pending[7] <- &rpcResponse{ID: 7, Result: json.RawMessage(`{"ok":true}`)}
			c.pendingMu.Unlock()
			c.streamErr <- io.EOF
			close(c.streamDone)
			w.WriteHeader(http.StatusAccepted)
		}))
		c.http, c.messageURL = srv.Client(), srv.URL
		resp, err := c.legacyRoundTrip(context.Background(), rpcRequest{ID: 7}, true)
		srv.Close()
		if err != nil || resp == nil || resp.ID != 7 {
			t.Fatalf("completed response lost during close: %v", err)
		}
	}
}

func TestLegacyInitializationHonorsCancellation(t *testing.T) {
	for _, phase := range []string{"headers", "endpoint", "initialize"} {
		t.Run(phase, func(t *testing.T) {
			fixtureDone := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.Method == http.MethodGet && phase != "headers" {
					w.Header().Set("Content-Type", "text/event-stream")
					if phase == "initialize" {
						fmt.Fprint(w, "event: endpoint\ndata: /message\n\n")
					}
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-fixtureDone:
				}
			}))
			defer srv.Close()
			defer close(fixtureDone)
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				c, err := NewSSE(ctx, "fixture", srv.URL, nil, false)
				if c != nil {
					_ = c.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled initialization succeeded")
				}
			case <-time.After(time.Second):
				srv.CloseClientConnections()
				t.Fatal("initialization ignored cancellation")
			}
		})
	}
}

func TestMalformedMCPResultCannotBecomeCompletedExecution(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"unexpected":"evidence"}`, `{"content":null,"structuredContent":null}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req rpcRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, body)
		}))
		c := &Client{server: "fixture", url: srv.URL, http: srv.Client()}
		res, err := c.wrap(remoteTool{Name: "effect"}).Call(context.Background(), json.RawMessage(`{}`), nil)
		srv.Close()
		if err == nil || !res.IsError {
			t.Fatalf("malformed result accepted: %s", body)
		}
	}
}

func TestToolDiscoveryDistinguishesMissingFromEmpty(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"tools":null}`, `{"tools":[]}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req rpcRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, body)
		}))
		c := &Client{server: "fixture", url: srv.URL, http: srv.Client()}
		_, err := c.Tools(context.Background())
		srv.Close()
		if (err == nil) != (body == `{"tools":[]}`) {
			t.Fatalf("missing catalog confused with empty catalog: body=%s err=%v", body, err)
		}
	}
}
