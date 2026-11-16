package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"chengjian.local/chengjian/db"
)

func decodeSnapshot(t *testing.T, data []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("invalid snapshot JSON: %v", err)
	}
	return out
}

func TestContextSnapshotBoundsProseWithoutChangingIdentityOrInput(t *testing.T) {
	const largeID int64 = 9007199254740993
	original := map[string]any{
		"frontier_open":   1,
		"running_intents": []map[string]any{{"id": largeID, "state": "running", "summary": strings.Repeat("中", 100_000)}},
		"task":            map[string]any{"goal": strings.Repeat("goal", 30_000)},
		"coverage":        map[string]any{"pct": 12.5},
	}
	before, _ := json.Marshal(original)
	first, err := marshalContextSnapshot(original, false)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := marshalContextSnapshot(original, false)
	if !bytes.Equal(first, second) {
		t.Fatal("snapshot projection is nondeterministic")
	}
	if len(first) > contextSnapshotMaxBytes || !utf8.Valid(first) {
		t.Fatal("snapshot exceeded its encoded-byte budget or broke UTF-8")
	}
	out := decodeSnapshot(t, first)
	if out["frontier_open"].(json.Number).String() != "1" || out["_context_snapshot"] == nil {
		t.Fatal("counter or explicit truncation metadata lost")
	}
	intent := out["running_intents"].([]any)[0].(map[string]any)
	if intent["id"].(json.Number).String() != "9007199254740993" || intent["state"] != "running" {
		t.Fatal("projection corrupted an identity or state")
	}
	after, _ := json.Marshal(original)
	if !bytes.Equal(before, after) {
		t.Fatal("projection mutated source state")
	}
	worker := renderWorkerGraphOverview(original)
	if !strings.Contains(worker, `<untrusted-data source="worker-graph-overview">`) || strings.Contains(worker, `"coverage"`) {
		t.Fatal("worker overview lost its data boundary or exposed planner coverage")
	}
	if original["coverage"] == nil {
		t.Fatal("worker rendering deleted coverage from the shared source map")
	}
}

func TestContextSnapshotOmitsAtomicValuesInsteadOfMakingPartialInputs(t *testing.T) {
	assets := []*db.Asset{{ID: 42, URL: "https://example.test/path", Auth: []map[string]any{{"token": strings.Repeat("secret", 10_000)}}}}
	b, err := marshalContextSnapshot(assets, true)
	if err != nil {
		t.Fatal(err)
	}
	out := decodeSnapshot(t, b)
	asset := out["items"].([]any)[0].(map[string]any)
	if asset["url"] != "https://example.test/path" || asset["id"].(json.Number).String() != "42" {
		t.Fatal("short asset address or identity changed")
	}
	if token := asset["auth"].([]any)[0].(map[string]any)["token"]; token != nil {
		t.Fatalf("oversized token was exposed as a partial executable value: %v", token)
	}
}

func TestContextSnapshotBoundsCollectionsDepthAndEscapedBodies(t *testing.T) {
	rows := make([]map[string]any, 600)
	for i := range rows {
		rows[i] = map[string]any{"id": i, "state": "open", "summary": strings.Repeat("\x00\"\\中", 3_000)}
	}
	deep := map[string]any{"leaf": "data"}
	for range 40 {
		deep = map[string]any{"nested": deep}
	}
	b, err := marshalContextSnapshot(map[string]any{"frontier_open": 600, "open_intents": rows, "deep": deep}, false)
	if err != nil {
		t.Fatal(err)
	}
	out := decodeSnapshot(t, b)
	if len(b) > contextSnapshotMaxBytes || out["_context_snapshot"] == nil {
		t.Fatal("adversarial snapshot was not bounded and marked")
	}
	if got := len(out["open_intents"].([]any)); got > contextSnapshotItemCap {
		t.Fatalf("collection count=%d exceeded cap", got)
	}
	if out["frontier_open"].(json.Number).String() != "600" {
		t.Fatal("snapshot turned the shown list length into the frontier total")
	}
}

func TestSnapshotDoesNotTruncateMandate(t *testing.T) {
	full := strings.Repeat("原始意图", 30_000) + "不得执行最后这一项"
	raw, _ := json.Marshal(map[string]string{"summary": full})
	got := renderIntentTask(&db.Node{ID: 17, Payload: raw})
	if !strings.Contains(got, string(raw)) || !strings.Contains(got, "不得执行最后这一项") {
		t.Fatal("authoritative intent was treated as expendable snapshot text")
	}
	if got := renderGraphOverview(map[string]any{"bad": make(chan int)}); got != "" {
		t.Fatal("failed optional snapshot should degrade to on-demand read tools")
	}
}
