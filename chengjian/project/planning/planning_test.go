package planning

import (
	"strings"
	"testing"
)

func intp(v int) *int { return &v }

func TestParseRejectsEmptyAndUnknown(t *testing.T) {
	if _, err := Parse("   "); err == nil {
		t.Fatal("空判据必须报错：静默跳过会让『写了判据但没生效』无法察觉")
	}
	if _, err := Parse("拿到 shell"); err == nil {
		t.Fatal("自然语言判据必须报错：它无法被判定")
	}
	r, err := Parse("Tunnel:socks")
	if err != nil {
		t.Fatalf("大小写应归一: %v", err)
	}
	if r.Kind != ReqTunnel || r.Arg != "socks" {
		t.Fatalf("解析结果错: %+v", r)
	}
}

func TestParseAllKeepsInvalidAsVisible(t *testing.T) {
	reqs := ParseAll([]string{"fact:出网可达", "", "胡写一条", "session"})
	if len(reqs) != 3 {
		t.Fatalf("空项应被忽略，其余三条保留，got %d", len(reqs))
	}
	if reqs[1].Kind != "" {
		t.Fatal("非法判据必须保留在结果里，不能静默丢弃")
	}
	checks := Evaluate(reqs, State{})
	if checks[1].Status != StatusInvalid {
		t.Fatalf("非法判据应判 invalid，got %s", checks[1].Status)
	}
	if checks[1].Detail == "" {
		t.Fatal("invalid 必须带原因")
	}
}

func TestEvaluateUnknownIsNotUnmet(t *testing.T) {
	// 调用方没给会话/凭据数据：必须判 unknown，不能判 unmet。
	checks := Evaluate(ParseAll([]string{"session", "credential:smb", "asset:domain", "tunnel"}), State{})
	for _, c := range checks {
		if c.Status != StatusUnknown {
			t.Fatalf("%s 缺数据时应为 unknown，got %s（把『没查』当『没有』会把提示变成噪音）", c.Requirement, c.Status)
		}
		if c.Blocking() {
			t.Fatalf("%s 判 unknown 不能算 blocking", c.Requirement)
		}
	}
}

func TestEvaluateConfirmedZeroIsUnmet(t *testing.T) {
	st := State{Counts: Counts{Sessions: intp(0), Tunnels: intp(0)}}
	checks := Evaluate(ParseAll([]string{"session", "tunnel"}), st)
	for _, c := range checks {
		if c.Status != StatusUnmet {
			t.Fatalf("%s 确认没有时应为 unmet，got %s", c.Requirement, c.Status)
		}
		if !c.Blocking() {
			t.Fatalf("%s unmet 应算 blocking", c.Requirement)
		}
	}
}

func TestEvaluateFactByKeyword(t *testing.T) {
	st := State{Facts: []FactView{{ID: 7, Summary: "目标 10.0.0.5 可出网（TCP 443 回连成功）"}}}
	checks := Evaluate(ParseAll([]string{"fact:出网", "fact:不出网"}), st)
	if checks[0].Status != StatusMet {
		t.Fatalf("关键词命中应判 met，got %s", checks[0].Status)
	}
	if !strings.Contains(checks[0].Detail, "#7") {
		t.Fatalf("met 必须能指认证据，got %q", checks[0].Detail)
	}
	if checks[1].Status != StatusUnmet {
		t.Fatalf("关键词未命中应判 unmet，got %s", checks[1].Status)
	}
}

func TestEvaluateTunnelByKind(t *testing.T) {
	kinds := map[string]int{"socks": 2}
	st := State{Counts: Counts{Tunnels: intp(2), TunnelKinds: kinds}}
	checks := Evaluate(ParseAll([]string{"tunnel:socks", "tunnel:portfwd"}), st)
	if checks[0].Status != StatusMet {
		t.Fatalf("有 socks 隧道应 met，got %s", checks[0].Status)
	}
	if checks[1].Status != StatusUnmet {
		t.Fatalf("没有 portfwd 应 unmet，got %s", checks[1].Status)
	}
}

func TestEvaluateIntentDone(t *testing.T) {
	st := State{DoneItems: []string{"确认目标出网能力", "枚举内网网段"}}
	checks := Evaluate(ParseAll([]string{"intent_done:出网", "intent_done:横向"}), st)
	if checks[0].Status != StatusMet {
		t.Fatalf("已完成意图匹配应 met，got %s", checks[0].Status)
	}
	if checks[1].Status != StatusUnmet {
		t.Fatalf("未完成应 unmet，got %s", checks[1].Status)
	}
}

func TestSummarizeSeparatesBlockingFromUnknown(t *testing.T) {
	st := State{Counts: Counts{Sessions: intp(0)}}
	checks := Evaluate(ParseAll([]string{"session", "credential:smb", "fact:x"}), st)
	s := Summarize(checks)
	if s.Unmet != 2 { // session + fact:x
		t.Fatalf("unmet 计数错: %+v", s)
	}
	if s.Unknown != 1 { // credential 无数据
		t.Fatalf("unknown 计数错: %+v", s)
	}
	if len(s.Blocking) != 2 {
		t.Fatalf("blocking 只应包含 unmet，got %v", s.Blocking)
	}
	if len(s.Notes) != 1 {
		t.Fatalf("notes 只应包含 unknown/invalid，got %v", s.Notes)
	}
}

func TestEvaluateDoneGaps(t *testing.T) {
	crits := ParseDoneAll([]string{"fact:版本", "finding:high", "evidence:2", "command", ""})
	if len(crits) != 4 {
		t.Fatalf("空项应忽略，got %d", len(crits))
	}
	gaps := EvaluateDone(crits, DoneState{
		Facts:    []FactView{{ID: 1, Summary: "nginx 版本 1.18.0"}},
		EvidenceN: 1,
	})
	if len(gaps) != 3 {
		t.Fatalf("应剩 3 条未满足（finding/evidence/command），got %d: %+v", len(gaps), gaps)
	}
	// 补足后应全部通过。
	gaps = EvaluateDone(crits, DoneState{
		Facts:      []FactView{{ID: 1, Summary: "nginx 版本 1.18.0"}},
		Findings:   []FactView{{ID: 2, Summary: "high SQL 注入"}},
		EvidenceN:  2,
		ManualNote: "curl 返回 500 且堆栈泄露路径",
	})
	if len(gaps) != 0 {
		t.Fatalf("判据全满足时不应有 gap，got %+v", gaps)
	}
}

func TestEvaluateDoneEmptyCriteriaHasNoGaps(t *testing.T) {
	if gaps := EvaluateDone(nil, DoneState{}); len(gaps) != 0 {
		t.Fatalf("没有判据 = 没有额外要求，不该报 gap，got %+v", gaps)
	}
}

func TestEvaluateDoneCommandRequiresManualNote(t *testing.T) {
	gaps := EvaluateDone(ParseDoneAll([]string{"command"}), DoneState{})
	if len(gaps) != 1 || !strings.Contains(gaps[0].Reason, "命令输出") {
		t.Fatalf("command 类判据没有输出时必须报 gap，got %+v", gaps)
	}
}

func TestEvaluateDoneBadEvidenceCount(t *testing.T) {
	// evidence:abc 无法解析成数字：退回默认 1，而不是当成 0 条永远不满足。
	gaps := EvaluateDone(ParseDoneAll([]string{"evidence:abc"}), DoneState{EvidenceN: 1})
	if len(gaps) != 0 {
		t.Fatalf("非法条数应退回默认 1，got %+v", gaps)
	}
}
