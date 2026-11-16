package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStdioToolsDiscoverAllPages(t *testing.T) {
	cReader, sWriter := io.Pipe()
	sReader, cWriter := io.Pipe()
	defer cReader.Close()
	defer sWriter.Close()
	defer sReader.Close()
	defer cWriter.Close()
	go func() {
		sc := bufio.NewScanner(sReader)
		enc := json.NewEncoder(sWriter)
		for sc.Scan() {
			var req struct {
				ID     int            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == 0 {
				continue
			}
			result := map[string]any{}
			if req.Method == "tools/list" {
				name := "first"
				if req.Params["cursor"] == "second-page" {
					name = "second"
				} else {
					result["nextCursor"] = "second-page"
				}
				result["tools"] = []any{map[string]any{"name": name}}
			}
			if enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) != nil {
				return
			}
		}
	}()
	c, err := newClientWithConn(context.Background(), "fixture", cReader, cWriter, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := c.Tools(context.Background())
	if err != nil || len(tools) != 2 || tools[1].Name() != "mcp__fixture__second" {
		t.Fatalf("stdio tool discovery incomplete: tools=%v err=%v", tools, err)
	}
}

func TestStdioCancellationReleasesPendingAndBlockedWrites(t *testing.T) {
	for _, blockedWrite := range []bool{false, true} {
		reader, serverWriter := io.Pipe()
		defer reader.Close()
		defer serverWriter.Close()
		var writer io.Writer = io.Discard
		if blockedWrite {
			serverReader, clientWriter := io.Pipe()
			defer serverReader.Close()
			defer clientWriter.Close()
			writer = clientWriter
		}
		c := newConn(reader, writer)
		defer c.close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, err := c.call(ctx, "fixture", nil)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancellation lost: %v", err)
		}
		c.mu.Lock()
		pending := len(c.pending)
		c.mu.Unlock()
		if pending != 0 {
			t.Fatalf("canceled call retained %d pending channels", pending)
		}
	}
}

func TestMCPStructuredAndBinaryContentRetained(t *testing.T) {
	text, isError, err := renderToolResult(json.RawMessage(`{"structuredContent":{"ok":true}}`))
	if err != nil || isError || text != `{"ok":true}` {
		t.Fatalf("structured result lost: %q %v", text, err)
	}
	text, isError, err = renderToolResult(json.RawMessage(`{"content":[{"type":"image","data":"fixture"}]}`))
	if err != nil || isError || !strings.Contains(text, `"data":"fixture"`) {
		t.Fatalf("binary result silently lost: %q %v", text, err)
	}
}

func TestMalformedMCPResultIsNotSuccess(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"unexpected":true}`, `{"content":null,"structuredContent":null}`} {
		if _, _, err := renderToolResult(json.RawMessage(body)); err == nil {
			t.Fatalf("invalid result accepted: %s", body)
		}
	}
}

func TestStdioDiscoveryDistinguishesMissingFromEmpty(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"tools":null}`, `{"tools":[]}`} {
		clientReader, serverWriter := io.Pipe()
		serverReader, clientWriter := io.Pipe()
		c := &Client{conn: newConn(clientReader, clientWriter)}
		go func() {
			defer serverReader.Close()
			defer serverWriter.Close()
			var req rpcRequest
			if json.NewDecoder(serverReader).Decode(&req) == nil {
				_ = json.NewEncoder(serverWriter).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": json.RawMessage(body)})
			}
		}()
		_, err := c.Tools(context.Background())
		_ = c.Close()
		if (err == nil) != (body == `{"tools":[]}`) {
			t.Fatalf("missing catalog confused with empty catalog: body=%s err=%v", body, err)
		}
	}
}
