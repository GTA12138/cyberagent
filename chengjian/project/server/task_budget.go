// task_budget.go keeps the planner preflight check for early feedback.
// Every model attempt is additionally gated by model_budget.go and the durable
// task-wide reservation ledger; settlement calls share the same limit.
package server

import (
	"context"
	"log"
	"strconv"
	"time"

	"chengjian.local/chengjian/agent"
)

// budgetVerdict 是一次预算检查的结论。
type budgetVerdict struct {
	Exceeded bool
	// Reason 是给人和 planner 看的一句话（超了才非空）。
	Reason string
	// Used / Limit 便于写进 activity。
	Used  int64
	Limit int64
}

func tokenBudgetVerdict(input, output, maxIn, maxOut int64) budgetVerdict {
	if maxIn > 0 && input >= maxIn {
		return budgetVerdict{Exceeded: true, Reason: "输入 token 已达预算上限（" + strconv.FormatInt(input, 10) + "/" + strconv.FormatInt(maxIn, 10) + "）", Used: input, Limit: maxIn}
	}
	if maxOut > 0 && output >= maxOut {
		return budgetVerdict{Exceeded: true, Reason: "输出 token 已达预算上限（" + strconv.FormatInt(output, 10) + "/" + strconv.FormatInt(maxOut, 10) + "）", Used: output, Limit: maxOut}
	}
	return budgetVerdict{Used: input + output}
}

// checkTaskTokenBudget 是 engine 视角的预算检查入口。
// 从持久化预算账本读取已结算用量；在途预留不触发暂停。
// engine 每轮规划前调用；未配置预算时是零成本早退。
//
// 尚无账本的旧任务临时沿用 activity 估算；首个调用会导入 llm_usage。
func (m *Manager) checkTaskTokenBudget(taskID string) budgetVerdict {
	t, ok := m.Task(taskID)
	if !ok || t == nil || t.ExpID <= 0 {
		return budgetVerdict{} // 任务句柄不在 → 不做预算判定（fail-open，不误杀）
	}
	maxIn, maxOut := m.taskTokenBudget(taskID)
	if maxIn <= 0 && maxOut <= 0 {
		return budgetVerdict{}
	}
	numericID, _ := strconv.ParseInt(taskID, 10, 64)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := m.pg.ModelBudgetStatus(ctx, numericID)
	if err == nil && status != nil && status.Tracking {
		return tokenBudgetVerdict(status.Spent.Input, status.Spent.Output, maxIn, maxOut)
	}
	u, err := t.Store.TokenTotal()
	if err != nil {
		log.Printf("[budget] 任务 %s token 用量读取失败，跳过预算判定: %v", taskID, err)
		return budgetVerdict{}
	}
	return tokenBudgetVerdict(int64(u.InputTokens), int64(u.OutputTokens), maxIn, maxOut)
}

// taskTokenBudget 读任务级 token 预算（0 = 不限）。读失败按不限处理（fail-open）。
func (m *Manager) taskTokenBudget(taskID string) (maxIn, maxOut int64) {
	if m.pg == nil {
		return 0, 0
	}
	_ = m.pg.QueryRow(`SELECT COALESCE(max_input_tokens,0), COALESCE(max_output_tokens,0) FROM tasks WHERE id=$1`, taskID).
		Scan(&maxIn, &maxOut)
	return maxIn, maxOut
}

// noteTaskBudgetExceeded records the advisory preflight failure.
func (m *Manager) noteTaskBudgetExceeded(taskID string, v budgetVerdict) {
	log.Printf("[budget] 任务 %s token 预算耗尽：%s", taskID, v.Reason)
}

// pauseForBudget 暂停任务并把原因标记为 budget_exhausted。
// 已有工具写回保留；后续模型收尾调用仍受任务预算约束。
func (m *Manager) pauseForBudget(taskID string, e *Engine) {
	if e == nil {
		return
	}
	if err := m.SetTaskPaused(taskID, true); err != nil {
		log.Printf("[budget] persist pause task %s: %v", taskID, err)
	}
	e.Pause(taskID, agent.AbortBudgetExhausted)
}
