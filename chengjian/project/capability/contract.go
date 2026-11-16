// contract.go 工具结果的三段式合同（O2 / T-15 前半段）。
//
// 问题：工具的返回此前是「ok + note + 一堆字段」。这带来三个具体麻烦：
//
//  1. **失败与没执行分不开。** `ok:false` 既可能是「命令跑了但失败了」，也可能是
//     「根本没连上」。前者说明凭据/路径对、只是命令错；后者说明凭据或网络有问题。
//     两者下一步该做的事完全相反，混在一个布尔里，agent 只能靠读 note 猜。
//  2. **被拦下和失败分不开。** capability 门禁拒绝、RoE 越界、尝试预算用尽——
//     这些都是**没执行**，不是执行失败。记成失败会污染失败率统计，也会让 agent
//     反复重试一个它本来就做不了的动作。
//  3. **结论与证据混在一起。** note 里既有「为什么失败」也有「这意味着什么」，
//     下游（planner、报告、复盘）无法分别使用。
//
// 三段式把这三件事拆开：
//
//	execution_status  执行到了哪一步（succeeded/failed/not_executed/blocked/partial）
//	failure_reason    失败归因（auth/network/capability/precondition/timeout/environment/unknown）
//	assessment        这次结果对计划意味着什么（一句人话，给 agent 读）
//
// 关键约束：**status=succeeded 时 reason 必须是 none；status=not_executed/blocked
// 时 reason 不能是 unknown。** 这两条不是洁癖——它们正是「不把没执行记成失败」
// 和「不把失败记成成功」的可执行版本，所以在 Validate 里强制。
package capability

import (
	"fmt"
	"strings"
)

// ExecutionStatus 表示一次工具调用执行到了哪一步。
type ExecutionStatus string

const (
	// StatusSucceeded 动作完成且达到预期（对「执行类」工具＝命令退出码 0）。
	StatusSucceeded ExecutionStatus = "succeeded"
	// StatusFailed 动作执行了，但结果不符合预期（命令跑了、返回非零/报错）。
	StatusFailed ExecutionStatus = "failed"
	// StatusNotExecuted 动作**没有开始执行**（连不上、认证不过、目标不可达）。
	StatusNotExecuted ExecutionStatus = "not_executed"
	// StatusBlocked 动作被平台主动拦下（能力缺失、RoE 越界、预算用尽、人工拒绝）。
	StatusBlocked ExecutionStatus = "blocked"
	// StatusPartial 动作执行了一部分（多目标里部分成功、输出被截断到无法判定）。
	StatusPartial ExecutionStatus = "partial"
)

// FailureReason 是失败归因。unknown 只允许与 failed/partial 搭配。
type FailureReason string

const (
	ReasonNone          FailureReason = "none"
	ReasonAuth          FailureReason = "auth"          // 凭据无效/权限不足
	ReasonNetwork       FailureReason = "network"       // 不可达、超时、DNS
	ReasonCapability    FailureReason = "capability"    // 目标平台不支持该动作
	ReasonPrecondition  FailureReason = "precondition"  // 前置条件未满足（无隧道/无会话/无凭据）
	ReasonBudget        FailureReason = "budget"        // 预算/尝试次数用尽
	ReasonTimeout       FailureReason = "timeout"       // 执行超时
	ReasonEnvironment   FailureReason = "environment"   // 平台自身环境问题（缺工具、脚本报错）
	ReasonUnknown       FailureReason = "unknown"
)

// ToolResult 是工具结果的三段式合同。
type ToolResult struct {
	ExecutionStatus ExecutionStatus `json:"execution_status"`
	FailureReason   FailureReason   `json:"failure_reason"`
	// Assessment 是「这对计划意味着什么」，一句话，给 agent 直接读。
	Assessment string `json:"assessment,omitempty"`
	// Evidence 是支撑结论的原始片段（可为空）。与 Assessment 分开：前者是事实，
	// 后者是解释；把解释当事实引用是这类系统最常见的错误来源。
	Evidence string `json:"evidence,omitempty"`
}

// NewToolResult 构造并校验一个三段式结果。
func NewToolResult(status ExecutionStatus, reason FailureReason, assessment, evidence string) (ToolResult, error) {
	r := ToolResult{ExecutionStatus: status, FailureReason: reason, Assessment: assessment, Evidence: evidence}
	if err := r.Validate(); err != nil {
		return r, err
	}
	return r, nil
}

// Validate 强制三条不变量。
func (r ToolResult) Validate() error {
	switch r.ExecutionStatus {
	case StatusSucceeded, StatusFailed, StatusNotExecuted, StatusBlocked, StatusPartial:
	default:
		return fmt.Errorf("非法 execution_status %q", r.ExecutionStatus)
	}
	switch r.FailureReason {
	case ReasonNone, ReasonAuth, ReasonNetwork, ReasonCapability, ReasonPrecondition,
		ReasonBudget, ReasonTimeout, ReasonEnvironment, ReasonUnknown:
	default:
		return fmt.Errorf("非法 failure_reason %q", r.FailureReason)
	}
	if r.ExecutionStatus == StatusSucceeded && r.FailureReason != ReasonNone {
		return fmt.Errorf("succeeded 不能带失败归因（%q）：把成功记成有原因，下游会当成部分失败", r.FailureReason)
	}
	// 没执行/被拦下必须有明确归因。unknown 在这里是禁止的——「我不知道为什么没执行」
	// 说明调用方没做归因，那是漏掉的逻辑，不是一种结论。
	if (r.ExecutionStatus == StatusNotExecuted || r.ExecutionStatus == StatusBlocked) &&
		(r.FailureReason == ReasonNone || r.FailureReason == ReasonUnknown) {
		return fmt.Errorf("%s 必须给出明确归因（auth/network/capability/precondition/budget/timeout/environment），实际 %q",
			r.ExecutionStatus, r.FailureReason)
	}
	return nil
}

// Executed 报告动作是否真的在目标上发生了。
//
// 这是三段式最直接的用处：只有 true 的情况下，「凭据有效」这类结论才成立。
// not_executed/blocked 都不能推出任何关于目标的结论——它们说的是平台侧的事。
func (r ToolResult) Executed() bool {
	return r.ExecutionStatus == StatusSucceeded || r.ExecutionStatus == StatusFailed || r.ExecutionStatus == StatusPartial
}

// OK 报告动作是否达成预期（partial 不算达成）。
func (r ToolResult) OK() bool { return r.ExecutionStatus == StatusSucceeded }

// ClassifyTransportError 把一次「没执行」的错误归到 auth / network / timeout /
// environment 四类里。
//
// 判定顺序刻意先认证后网络：`permission denied` 在两边都常见，而认证失败的可
// 行动结论（换凭据）与网络失败（换隧道/换路）完全不同；归错类的代价是 agent
// 去修一个没坏的东西。
func ClassifyTransportError(err error) FailureReason {
	if err == nil {
		return ReasonNone
	}
	s := strings.ToLower(err.Error())
	for _, p := range []string{"permission denied", "authentication failed", "invalid credentials",
		"logon failure", "no supported authentication", "too many authentication"} {
		if strings.Contains(s, p) {
			return ReasonAuth
		}
	}
	for _, p := range []string{"timeout", "timed out", "deadline exceeded"} {
		if strings.Contains(s, p) {
			return ReasonTimeout
		}
	}
	for _, p := range []string{"connection refused", "no route to host", "network is unreachable",
		"i/o timeout", "connection reset", "no such host", "actively refused"} {
		if strings.Contains(s, p) {
			return ReasonNetwork
		}
	}
	for _, p := range []string{"executable file not found", "not found in $path", "no such file or directory",
		"traceback", "module not found"} {
		if strings.Contains(s, p) {
			return ReasonEnvironment
		}
	}
	return ReasonUnknown
}
