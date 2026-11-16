package capability

import (
	"errors"
	"testing"
)

func TestSucceededCannotCarryFailureReason(t *testing.T) {
	if _, err := NewToolResult(StatusSucceeded, ReasonAuth, "打成功了", ""); err == nil {
		t.Fatal("succeeded + auth 必须被拒：把成功记成有原因，下游会当成部分失败")
	}
	if _, err := NewToolResult(StatusSucceeded, ReasonNone, "打成功了", ""); err != nil {
		t.Fatalf("合法的成功结果被拒: %v", err)
	}
}

func TestNotExecutedRequiresConcreteReason(t *testing.T) {
	for _, bad := range []FailureReason{ReasonNone, ReasonUnknown} {
		if _, err := NewToolResult(StatusNotExecuted, bad, "", ""); err == nil {
			t.Fatalf("not_executed + %s 必须被拒：「不知道为什么没执行」是漏掉的归因，不是结论", bad)
		}
	}
	if _, err := NewToolResult(StatusNotExecuted, ReasonAuth, "认证被拒", ""); err != nil {
		t.Fatalf("明确归因应通过: %v", err)
	}
}

func TestExecutedDistinguishesNotExecuted(t *testing.T) {
	// 这是三段式存在的核心理由：只有 executed 才能对目标下结论。
	succeeded, _ := NewToolResult(StatusSucceeded, ReasonNone, "", "")
	failed, _ := NewToolResult(StatusFailed, ReasonUnknown, "", "")
	notExec, _ := NewToolResult(StatusNotExecuted, ReasonNetwork, "", "")
	blocked, _ := NewToolResult(StatusBlocked, ReasonCapability, "", "")
	if !succeeded.Executed() || !failed.Executed() {
		t.Fatal("成功与失败都是「执行过」")
	}
	if notExec.Executed() || blocked.Executed() {
		t.Fatal("没执行/被拦下不能算执行过——那会让「凭据有效」这类结论凭空成立")
	}
	if failed.OK() {
		t.Fatal("failed 不能报 OK")
	}
}

func TestClassifyTransportErrorPrefersAuth(t *testing.T) {
	// 「permission denied」在认证与网络两边都常见，必须归认证：
	// 归错的代价是 agent 去修一个没坏的东西。
	cases := map[string]FailureReason{
		"Permission denied (publickey,password).":                 ReasonAuth,
		"ssh: handshake failed: read tcp: i/o timeout":            ReasonTimeout,
		"dial tcp 10.0.0.5:22: connectex: No connection could be made because the target machine actively refused it.": ReasonNetwork,
		"python3: executable file not found in $PATH":             ReasonEnvironment,
		"something bizarre happened":                              ReasonUnknown,
	}
	for msg, want := range cases {
		if got := ClassifyTransportError(errors.New(msg)); got != want {
			t.Fatalf("%q → %s，期望 %s", msg, got, want)
		}
	}
	if got := ClassifyTransportError(nil); got != ReasonNone {
		t.Fatalf("nil 应为 none，got %s", got)
	}
}

func TestInvalidStatusRejected(t *testing.T) {
	if _, err := NewToolResult(ExecutionStatus("done"), ReasonNone, "", ""); err == nil {
		t.Fatal("未知 status 必须被拒")
	}
	if _, err := NewToolResult(StatusFailed, FailureReason("oops"), "", ""); err == nil {
		t.Fatal("未知 reason 必须被拒")
	}
}
