package traffic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func TestIssuedTrafficReferenceRecoversTruncationWithoutGuessing(t *testing.T) {
	tr, _ := openTraffic(t)
	tr.record(newFlow("fixture.test", "GET", "/", nil, []byte("the-recorded-body")))
	raw := onlyExchangeID(t, tr)
	owner := "task/7"
	resolve := func(_ context.Context, source string) (LookupSource, error) {
		if source == "" {
			source = owner
		}
		if source != owner {
			return LookupSource{}, fmt.Errorf("source denied")
		}
		return LookupSource{Name: source, With: func(_ context.Context, f func(*Traffic) error) error { return f(tr) }}, nil
	}
	tools := ScopedTools(resolve)
	call := func(i int, in any) actool.Result {
		b, _ := json.Marshal(in)
		r, err := tools[i].Call(context.Background(), b, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	suffix := raw[strings.LastIndexByte(raw, '-')+1:]
	if r := call(1, map[string]any{"id": suffix}); !r.IsError || !strings.Contains(r.Flatten(), "invalid_reference") {
		t.Fatalf("unissued alias was guessed: %v", r)
	}
	if r := call(0, map[string]any{"host": "fixture.test"}); r.IsError {
		t.Fatal(r)
	}
	for _, alias := range []string{suffix, raw} {
		r := call(1, map[string]any{"id": alias})
		if r.IsError || !strings.Contains(r.Flatten(), "the-recorded-body") || !strings.Contains(r.Flatten(), "id=task/7/"+raw) {
			t.Fatalf("issued alias %q not restored: %v", alias, r)
		}
	}
	owner = "task/8"
	if r := call(1, map[string]any{"id": suffix}); !r.IsError {
		t.Fatal("reference cache escaped its originating task")
	}
}

func TestTrafficGetPagesKeepBlobPointerAndTail(t *testing.T) {
	tr, _ := openTraffic(t)
	body := []byte(strings.Repeat("正文", maxInlineBody/6+100) + "LAST_SECRET_MARKER")
	tr.record(newFlow("fixture.test", "GET", "/", nil, body))
	id := onlyExchangeID(t, tr)
	get := tr.Tools()[1]
	var rawResp string
	_, rawResp, _ = tr.Get(id)
	var all strings.Builder
	for offset := 0; offset < len(rawResp); {
		in, _ := json.Marshal(map[string]any{"id": id, "part": "response", "offset": offset, "length": 512})
		r, err := get.Call(context.Background(), in, nil)
		if err != nil || r.IsError {
			t.Fatalf("read: %v %v", r, err)
		}
		s := r.Flatten()
		if !strings.Contains(s, "body_blob_hash=") {
			t.Fatal("blob pointer lost from a page")
		}
		var next int
		p := strings.Index(s, "next_offset=")
		fmt.Sscanf(s[p:], "next_offset=%d", &next)
		if next <= offset {
			t.Fatal("page did not advance")
		}
		all.WriteString(s)
		offset = next
	}
	// The body itself lives in the blob; verify the pointer reads its tail.
	m := rawBlobPointer.FindStringSubmatch(rawResp)
	if len(m) != 2 {
		t.Fatal("fixture did not spill")
	}
	data, total, err := tr.BlobRange(m[1], int64(len(body)-32), 8192)
	if err != nil || total != int64(len(body)) || !strings.Contains(string(data), "LAST_SECRET_MARKER") {
		t.Fatalf("blob tail inaccessible: %d %v", total, err)
	}
}

func TestTrafficBlobBinaryPageRetainsEveryByte(t *testing.T) {
	data := make([]byte, 8192)
	for i := range data {
		data[i] = byte(i % 256)
	}
	out := trafficBlobPage(data, 8192, 16384, "")
	_, encoded, ok := strings.Cut(out, "\n")
	if !ok || !strings.Contains(out, "next_offset=16384") {
		t.Fatal("missing continuation")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || string(decoded) != string(data) {
		t.Fatal("binary page lost bytes")
	}
}
