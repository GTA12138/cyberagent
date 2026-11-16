package db

import (
	"errors"
	"testing"
)

// TestAddIntentRejectsDuplicateLiveIntent 钉住 T-11：
// 同一探索下、summary 相同、仍处于 open/running 的意图只能有一条。
// 返回的是 ErrIntentAlreadyLive 且带上已有 id——不是静默复用，因为静默复用
// 会让 planner 以为「新方向已派」，从而不再推进。
func TestAddIntentRejectsDuplicateLiveIntent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	expID, err := d.CreateExploration("dedup", "intent dedup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) })
	es := d.Exploration(expID)

	first, err := es.AddIntent(map[string]any{"summary": "枚举 /api 下的端点"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	// 大小写与首尾空白都算同一个意图——索引用的是 lower(btrim(...))。
	second, err := es.AddIntent(map[string]any{"summary": "  枚举 /API 下的端点  "}, 5, nil, "planner")
	if !errors.Is(err, ErrIntentAlreadyLive) {
		t.Fatalf("want ErrIntentAlreadyLive, got %v", err)
	}
	if second != first {
		t.Fatalf("error must carry the existing intent id: got %d, want %d", second, first)
	}

	// 意图结束后，同样的方向可以重新派（状态变了，唯一索引不再覆盖）。
	if err := es.SetNodeState(first, "done"); err != nil {
		t.Fatalf("finish intent: %v", err)
	}
	third, err := es.AddIntent(map[string]any{"summary": "枚举 /api 下的端点"}, 5, nil, "planner")
	if err != nil {
		t.Fatalf("re-adding after terminal state must succeed: %v", err)
	}
	if third == first {
		t.Fatal("re-added intent must be a new node")
	}
}

// TestAddIntentAllowsDistinctSummaries 防止约束过宽：不同方向必须都能派。
func TestAddIntentAllowsDistinctSummaries(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	expID, err := d.CreateExploration("dedup2", "distinct")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) })
	es := d.Exploration(expID)
	for _, s := range []string{"测 IDOR", "测 SSTI", "测 SSRF"} {
		if _, err := es.AddIntent(map[string]any{"summary": s}, 1, nil, "planner"); err != nil {
			t.Fatalf("distinct intent %q rejected: %v", s, err)
		}
	}
}
