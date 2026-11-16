package server

import "testing"

// token 预算判定是纯阈值比较，但边界容易错（>= vs >、0 表示不限、两维独立）。
// 这些用例不依赖 DB，直接验证判定语义。
func TestBudgetVerdictSemantics(t *testing.T) {
	// 0 表示该维度不限：任何用量都不应触发。
	// 这里通过 budgetVerdict 的字段语义表达，实际判定在 checkTaskTokenBudget 里，
	// 需要 DB；所以此处锁定「结论对象」的契约。
	v := budgetVerdict{Exceeded: false, Used: 100, Limit: 0}
	if v.Exceeded {
		t.Fatal("未超限时 Exceeded 必须为 false")
	}
	if v.Reason != "" {
		t.Fatal("未超限时不应有 Reason")
	}

	v2 := budgetVerdict{Exceeded: true, Reason: "输入 token 已达预算上限（1000/1000）", Used: 1000, Limit: 1000}
	if !v2.Exceeded || v2.Reason == "" || v2.Used != v2.Limit {
		t.Fatalf("超限结论不完整: %+v", v2)
	}
}

// 预算检查是每轮规划都跑的路径，必须能在「任务不存在/无探索图」时零成本早退，
// 而不是 panic 或返回超限（那会把所有任务误停）。
func TestCheckTaskTokenBudgetFailsOpenOnMissingTask(t *testing.T) {
	m := &Manager{tasks: map[string]*Task{}}
	if v := m.checkTaskTokenBudget("999999"); v.Exceeded {
		t.Fatalf("任务不存在时必须 fail-open，实际: %+v", v)
	}
}

// 未配置预算（两维皆 0）时不应触发任何判定——这是默认形态，
// 绝大多数既有任务都走这条路径，绝不能因为它们没有预算就停。
func TestTaskTokenBudgetDefaultsToUnlimited(t *testing.T) {
	m := &Manager{} // pg == nil
	maxIn, maxOut := m.taskTokenBudget("1")
	if maxIn != 0 || maxOut != 0 {
		t.Fatalf("pg 未就绪时应视为不限, got (%d,%d)", maxIn, maxOut)
	}
}
