package traffic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func collisionCapture(t *testing.T, body string) *Traffic {
	t.Helper()
	tr, _ := openTraffic(t)
	tr.record(newFlow("fixture.test", "GET", "/", nil, []byte(body)))
	for _, table := range []string{"exchanges", "exchange_bodies"} {
		if _, err := tr.DB().Exec("UPDATE " + table + " SET id='42'"); err != nil {
			t.Fatal(err)
		}
	}
	return tr
}

func TestScopedToolsNeverConfuseCollidingSources(t *testing.T) {
	global, task := collisionCapture(t, "global-body"), collisionCapture(t, "task-body")
	defaultSource := "task/7"
	resolve := func(_ context.Context, requested string) (LookupSource, error) {
		if requested == "" {
			requested = defaultSource
		}
		var tr *Traffic
		switch requested {
		case "global":
			tr = global
		case "task/7":
			tr = task
		default:
			return LookupSource{}, fmt.Errorf("source denied")
		}
		return LookupSource{Name: requested, With: func(_ context.Context, f func(*Traffic) error) error { return f(tr) }}, nil
	}
	tools := ScopedTools(resolve)
	call := func(index int, input string) actool.Result {
		r, err := tools[index].Call(context.Background(), json.RawMessage(input), nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	search := call(0, `{"host":"fixture.test"}`)
	if search.IsError || !strings.Contains(search.Flatten(), `"id":"task/7/42"`) {
		t.Fatalf("search source: %+v", search)
	}
	for _, tc := range []struct{ input, want string }{
		{`{"id":"task/7/42"}`, "task-body"},
		{`{"id":"global/42"}`, "global-body"},
		{`{"id":"42"}`, "global-body"}, // persisted legacy call
	} {
		r := call(1, tc.input)
		if r.IsError || !strings.Contains(r.Flatten(), tc.want) {
			t.Fatalf("%s: %+v", tc.input, r)
		}
	}
	if !call(1, `{"id":"task/8/42"}`).IsError {
		t.Fatal("cross-task read should be denied")
	}
	// Resolve on Call, even when the tool was built before the run changed.
	defaultSource = "global"
	if r := call(0, `{"host":"fixture.test"}`); !strings.Contains(r.Flatten(), `"id":"global/42"`) {
		t.Fatalf("stale source: %s", r.Flatten())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, _ := tools[0].Call(ctx, json.RawMessage(`{"host":"fixture.test"}`), nil); !r.IsError {
		t.Fatal("cancelled call should not query")
	}
}

func TestOpenReaderDoesNotCreateOrUpgradeCapture(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history # 中文")
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tr.record(newFlow("fixture.test", "GET", "/", nil, []byte("historical-body")))
	id := onlyExchangeID(t, tr)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "_ca")); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	_, resp, err := reader.Get(id)
	if err != nil || !strings.Contains(resp, "historical-body") {
		t.Fatalf("history read: %s %v", resp, err)
	}
	if _, err := reader.DB().Exec(`CREATE TABLE forbidden(x)`); err == nil {
		t.Fatal("reader was writable")
	}
	if err := reader.Start(); err == nil {
		t.Fatal("reader must not start proxy")
	}
	if _, err := os.Stat(filepath.Join(dir, "_ca")); !os.IsNotExist(err) {
		t.Fatalf("reader created CA: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	if r, err := OpenReader(context.Background(), missing); err == nil {
		r.Close()
		t.Fatal("reader created nonexistent store")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("failed read created directory")
	}
}

func TestParseTrafficReference(t *testing.T) {
	for _, id := range []string{"task/0/42", "task/01/42", "task/-1/42", "task/7/../42", "global/", "../42", "task/7/42/extra", "42\\bad"} {
		if _, _, err := ParseReference(id, "global"); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
	if source, raw, err := ParseReference("42", "task/7"); err != nil || source != "task/7" || raw != "42" {
		t.Fatalf("explicit default: %q %q %v", source, raw, err)
	}
}
