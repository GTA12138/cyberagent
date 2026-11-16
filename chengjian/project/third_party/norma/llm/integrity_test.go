package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamRequiresTerminalEvent(t *testing.T) {
	for _, tc := range []struct {
		format            Format
		partial, terminal string
	}{
		{FormatOpenAI, `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n", "data: [DONE]"},
		{FormatAnthropic, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}` + "\n\n", `data: {"type":"message_stop"}`},
		{FormatOpenAIResponses, `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n", `data: {"type":"response.completed","response":{"status":"completed"}}`},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			for _, complete := range []bool{false, true} {
				body := tc.partial
				if complete {
					body += tc.terminal
				} // terminal line deliberately has no newline
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
				p, e := NewProvider(Config{Format: tc.format, BaseURL: srv.URL, APIKey: "fixture", Model: "fixture", MaxRetries: -1, EmptyResponseRetries: -1})
				if e != nil {
					t.Fatal(e)
				}
				var gotErr error
				var stopped bool
				for ev, err := range p.Stream(context.Background(), CompletionRequest{}) {
					if err != nil {
						gotErr = err
					}
					stopped = stopped || ev.Type == SEMessageStop
				}
				srv.Close()
				if complete && (gotErr != nil || !stopped) {
					t.Fatalf("complete: stopped=%v err=%v", stopped, gotErr)
				}
				if !complete && (!errors.Is(gotErr, ErrIncompleteStream) || stopped) {
					t.Fatalf("partial falsely completed: stopped=%v err=%v", stopped, gotErr)
				}
			}
		})
	}
}

func TestSSEScannerBoundsAndFinalLine(t *testing.T) {
	scan := newSSEScanner(strings.NewReader("data: first\ndata: final"))
	_, data, err := scan.next()
	if err != nil || data != "first\nfinal" {
		t.Fatalf("last bytes lost: %q %v", data, err)
	}
	for _, body := range []string{"data: " + strings.Repeat("x", maxSSEEventBytes), strings.Repeat("data: "+strings.Repeat("x", 1024)+"\n", maxSSEEventBytes/1024+1)} {
		_, _, err = newSSEScanner(strings.NewReader(body)).next()
		if !errors.Is(err, ErrResponseLimit) {
			t.Fatalf("unbounded SSE: %v", err)
		}
	}
	if _, err = readResponse(io.LimitReader(repeatingReader{}, maxLLMResponseBytes+1)); !errors.Is(err, ErrResponseLimit) {
		t.Fatalf("unbounded JSON: %v", err)
	}
}

type repeatingReader struct{}

func (repeatingReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 'x'
	}
	return len(b), nil
}

func TestInterleavedToolCallsKeepTheirOwnArguments(t *testing.T) {
	for _, format := range []Format{FormatOpenAI, FormatOpenAIResponses} {
		t.Run(string(format), func(t *testing.T) {
			body := ""
			if format == FormatOpenAI {
				body = sse(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Read","arguments":"{\"file_path\":\""}},{"index":1,"id":"b","function":{"name":"Read","arguments":"{\"file_path\":\""}}]}}]}`, ``,
					`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"first\"}"}},{"index":1,"function":{"arguments":"second\"}"}}]},"finish_reason":"tool_calls"}]}`, ``, `data: [DONE]`, ``)
			} else {
				body = sse(`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"item-a","type":"function_call","call_id":"a","name":"Read"}}`, ``,
					`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"item-b","type":"function_call","call_id":"b","name":"Read"}}`, ``,
					`data: {"type":"response.function_call_arguments.delta","item_id":"item-a","delta":"{\"file_path\":\"first\"}"}`, ``,
					`data: {"type":"response.function_call_arguments.delta","item_id":"item-b","delta":"{\"file_path\":\"second\"}"}`, ``,
					`data: {"type":"response.completed","response":{"status":"completed"}}`, ``)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer srv.Close()
			p, _ := NewProvider(Config{Format: format, BaseURL: srv.URL, APIKey: "fixture", Model: "fixture"})
			msg := collect(t, p, CompletionRequest{})
			uses := msg.ToolUses()
			if len(uses) != 2 || uses[0].ID != "a" || uses[1].ID != "b" || string(uses[0].Input) != `{"file_path":"first"}` || string(uses[1].Input) != `{"file_path":"second"}` {
				t.Fatalf("crossed tool arguments: %+v", uses)
			}
		})
	}
}

func TestOpenAIStreamErrorBodyNotEmptySuccess(t *testing.T) {
	for _, body := range []string{`data: {"error":{"message":"fixture failure"}}` + "\n\ndata: [DONE]\n\n", "data: {broken\n\ndata: [DONE]\n\n"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		p, _ := NewProvider(Config{Format: FormatOpenAI, BaseURL: srv.URL, APIKey: "fixture", Model: "fixture", EmptyResponseRetries: -1})
		var got error
		for _, err := range p.Stream(context.Background(), CompletionRequest{}) {
			if err != nil {
				got = err
			}
		}
		srv.Close()
		if got == nil {
			t.Fatal("invalid/error frame reported success")
		}
	}
}

func TestAnthropicMalformedFrameCannotDisappear(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "data: {broken\n\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Format: FormatAnthropic, BaseURL: srv.URL, APIKey: "fixture", Model: "fixture", MaxRetries: -1})
	var got error
	for _, err := range p.Stream(context.Background(), CompletionRequest{}) {
		if err != nil {
			got = err
		}
	}
	if got == nil {
		t.Fatal("malformed data frame silently treated as successful stream")
	}
}

func TestNonStreamingMissingTerminalCannotComplete(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parse func([]byte) (Message, string, Usage, error)
		body  string
	}{
		{"anthropic-null", parseAnthropicResponse, `null`},
		{"anthropic-partial", parseAnthropicResponse, `{"content":[{"type":"text","text":"partial"}]}`},
		{"chat-partial", parseOpenAIResponse, `{"choices":[{"message":{"content":"partial"}}]}`},
		{"responses-null", parseResponsesResponse, `null`},
		{"responses-partial", parseResponsesResponse, `{"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := tc.parse([]byte(tc.body)); err == nil {
				t.Fatal("response without completion metadata succeeded")
			}
		})
	}
}
