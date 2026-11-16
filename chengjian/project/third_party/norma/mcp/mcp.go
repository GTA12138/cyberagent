// Package mcp integrates Model Context Protocol tools (FR-10). It speaks
// JSON-RPC 2.0 over a newline-delimited stdio transport to external MCP servers
// and also supports in-process servers (Go functions). Every MCP tool is
// adapted to a tool.CoreTool named mcp__<server>__<tool> (FR-10.2/10.3).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

// ToolName builds the three-segment MCP tool name.
func ToolName(server, name string) string { return "mcp__" + server + "__" + name }

// --- in-process server ---

// InProcessTool is a Go-native tool exposed under the MCP naming convention.
type InProcessTool struct {
	Name        string
	Description string
	Schema      map[string]any
	Handler     func(ctx context.Context, args json.RawMessage) (string, error)
}

// InProcessServer adapts a set of in-process tools to CoreTools (FR-10.3).
func InProcessServer(server string, tools []InProcessTool) []tool.CoreTool {
	out := make([]tool.CoreTool, 0, len(tools))
	for _, it := range tools {
		it := it
		out = append(out, tool.Build(tool.Spec{
			Name:        ToolName(server, it.Name),
			Description: it.Description,
			Schema:      it.Schema,
			Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
				return permission.AskUser("call MCP tool " + ToolName(server, it.Name) + "?")
			},
			Run: func(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
				text, err := it.Handler(ctx, in)
				if err != nil {
					return tool.Errorf("Error: " + err.Error()), nil
				}
				return tool.Text(tool.Capture(tc, text)), nil
			},
		}))
	}
	return out
}

// --- JSON-RPC transport ---

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message) }

// conn is a JSON-RPC 2.0 connection over a newline-delimited byte stream. It is
// transport-agnostic (stdio, pipe, socket) for testability.
type conn struct {
	r         io.Reader
	w         io.Writer
	mu        sync.Mutex
	nextID    int
	pending   map[int]chan rpcResponse
	closeCh   chan struct{}
	once      sync.Once
	writeGate chan struct{}
	readErr   error
}

func newConn(r io.Reader, w io.Writer) *conn {
	c := &conn{r: r, w: w, pending: map[int]chan rpcResponse{}, closeCh: make(chan struct{}), writeGate: make(chan struct{}, 1)}
	go c.readLoop(r)
	return c
}

func (c *conn) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue // notifications / non-response frames are ignored
		}
		if resp.ID == 0 {
			continue
		}
		c.mu.Lock()
		ch := c.pending[resp.ID]
		delete(c.pending, resp.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
	c.mu.Lock()
	c.readErr = sc.Err()
	c.mu.Unlock()
	c.close()
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.closeCh)
		if w, ok := c.w.(io.Closer); ok {
			_ = w.Close()
		}
		if r, ok := c.r.(io.Closer); ok {
			_ = r.Close()
		}
	})
}
func (c *conn) closedError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil {
		return fmt.Errorf("mcp: connection closed before complete response: %w", c.readErr)
	}
	return fmt.Errorf("mcp: connection closed; in-flight outcome may be unknown")
}

func (c *conn) notify(method string, params any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.writeContext(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *conn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-c.closeCh:
		return nil, c.closedError()
	default:
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	err := c.writeContext(ctx, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closeCh:
		select {
		case resp := <-ch:
			if resp.Error != nil {
				return nil, resp.Error
			}
			return resp.Result, nil
		default:
		}
		return nil, c.closedError()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (c *conn) writeContext(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closeCh:
		return c.closedError()
	case c.writeGate <- struct{}{}:
	}
	select {
	case <-c.closeCh:
		<-c.writeGate
		return c.closedError()
	default:
	}
	done := make(chan error, 1)
	go func() {
		defer func() { <-c.writeGate }()
		n, e := c.w.Write(data)
		if e == nil && n != len(data) {
			e = io.ErrShortWrite
		}
		done <- e
	}()
	select {
	case err := <-done:
		if err != nil {
			c.close()
		}
		return err
	case <-ctx.Done():
		c.close()
		return ctx.Err()
	case <-c.closeCh:
		return c.closedError()
	}
}

// --- client ---

// Client is a connection to one MCP server.
type Client struct {
	server string
	conn   *conn
	closer io.Closer
}

type remoteTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// initialize performs the MCP handshake.
func (c *Client) initialize(ctx context.Context) error {
	_, err := c.conn.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "norma", "version": "0.2"},
	})
	if err != nil {
		return err
	}
	return c.conn.notify("notifications/initialized", map[string]any{})
}

// Tools lists the server's tools and adapts them to CoreTools.
func (c *Client) Tools(ctx context.Context) ([]tool.CoreTool, error) {
	var out []tool.CoreTool
	cursor := ""
	seenCursors, seenNames := map[string]bool{}, map[string]bool{}
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.conn.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var res struct {
			Tools      []remoteTool `json:"tools"`
			NextCursor string       `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, err
		}
		if res.Tools == nil {
			return nil, fmt.Errorf("mcp: tools/list is missing a tools array; discovery incomplete")
		}
		for _, rt := range res.Tools {
			if rt.Name == "" || seenNames[rt.Name] {
				return nil, fmt.Errorf("mcp: empty or duplicate tool name in paginated list")
			}
			seenNames[rt.Name] = true
			out = append(out, c.wrap(rt))
			if len(out) > 10000 {
				return nil, fmt.Errorf("mcp: tools/list exceeds 10000 tools; discovery incomplete")
			}
		}
		if res.NextCursor == "" {
			return out, nil
		}
		if seenCursors[res.NextCursor] {
			return nil, fmt.Errorf("mcp: repeated tools/list cursor; discovery incomplete")
		}
		seenCursors[res.NextCursor] = true
		cursor = res.NextCursor
	}
	return nil, fmt.Errorf("mcp: tools/list exceeds 100 pages; discovery incomplete")
}

// renderToolResult keeps structured results and embedded resources visible.
// Unsupported blocks retain their complete JSON through the normal output spill
// path rather than silently turning a successful result into an empty string.
func renderToolResult(raw json.RawMessage) (string, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", false, err
	}
	hasContent := fields["content"] != nil && strings.TrimSpace(string(fields["content"])) != "null"
	hasStructured := fields["structuredContent"] != nil && strings.TrimSpace(string(fields["structuredContent"])) != "null"
	if !hasContent && !hasStructured {
		return "", false, fmt.Errorf("mcp: result is missing content and structuredContent; outcome unknown")
	}
	var res struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Resource struct {
				Text string `json:"text"`
				URI  string `json:"uri"`
			} `json:"resource"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", false, err
	}
	var b strings.Builder
	unsupported := false
	for _, blk := range res.Content {
		switch blk.Type {
		case "text":
			b.WriteString(blk.Text)
		case "resource":
			if blk.Resource.Text != "" {
				fmt.Fprintf(&b, "\n[resource %s]\n%s", blk.Resource.URI, blk.Resource.Text)
			} else {
				unsupported = true
			}
		default:
			unsupported = true
		}
	}
	if len(res.Structured) > 0 && string(res.Structured) != "null" {
		if b.Len() > 0 {
			b.WriteString("\n[structuredContent]\n")
		}
		b.Write(res.Structured)
	}
	if unsupported {
		b.WriteString("\n[MCP 返回了图像、二进制或其他内容块，文本预览不能完整呈现；完整原始 JSON（含所有内容块）如下，长结果会保存到输出文件，可用 Read 继续读取。]\n")
		b.Write(raw)
	}
	return b.String(), res.IsError, nil
}

func (c *Client) wrap(rt remoteTool) tool.CoreTool {
	schema := rt.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	full := ToolName(c.server, rt.Name)
	return tool.Build(tool.Spec{
		Name:        full,
		Description: rt.Description,
		Schema:      schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.AskUser("call MCP tool " + full + "?")
		},
		Run: func(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
			var args any
			if len(in) > 0 {
				_ = json.Unmarshal(in, &args)
			}
			raw, err := c.conn.call(ctx, "tools/call", map[string]any{"name": rt.Name, "arguments": args})
			if err != nil {
				return tool.Errorf("Error: " + err.Error()), err
			}
			text, isError, err := renderToolResult(raw)
			if err != nil {
				return tool.Errorf("Error: bad MCP response: " + err.Error()), err
			}
			return tool.Result{Content: []llm.ContentBlock{llm.TextBlock(tool.Capture(tc, text))}, IsError: isError}, nil
		},
	})
}

// Close shuts down the client's transport.
func (c *Client) Close() error {
	if c.conn != nil {
		c.conn.close()
	}
	if c.closer != nil {
		return c.closer.Close()
	}
	return nil
}
