// task_summary_test.go 收官总结渲染的判定测试：有漏洞/无漏洞两形态、
// 意图状态分桶、内网摘要拼接、截断。
package server

import (
	"strings"
	"testing"
)

func summaryDigestFixture() map[string]any {
	return map[string]any{
		"goals": []summaryGoal{
			{state: "met", brief: summaryBrief{Name: "拿到域管", Text: "以域管权限读取域内任意主机文件"}},
			{state: "open", brief: summaryBrief{Name: "横向到核心段", Text: ""}},
		},
		"findings": []map[string]any{
			{"id": int64(11), "state": "confirmed", "name": "SQL注入 /api/users", "severity": "high"},
			{"id": int64(12), "state": "confirmed", "name": "越权删除订单", "severity": "medium"},
			{"id": int64(13), "state": "dismissed", "name": "疑似 XSS", "severity": "low"},
		},
		"stats":   map[string]int{"fact": 47, "intent": 20},
		"intents": map[string]int{"done": 12, "blocked": 2, "exhausted": 1, "open": 0},
		"recent_facts": []string{
			"立足点：目标 10.0.0.8 可出网 443",
			"凭据：CORP\\admin 的 NTLM 已离线提取",
		},
		"intranet": map[string]any{"summary": "有 1 条 alive 隧道（内网可达）；有 2 个网段待人工授权"},
	}
}

func TestRenderTaskSummaryWithFindings(t *testing.T) {
	out := renderTaskSummary(summaryDigestFixture(), 4000)
	for _, want := range []string{
		"目标：2 个（达成 1 / 未达 1）",
		"✔ [met] 拿到域管",
		"漏洞：共 3 条（确认 2，dismissed 1）",
		"[high] SQL注入 /api/users (#11)",
		"事实 47 条",
		"完成 12 / 受阻 2 / 预算耗尽 1",
		"内网态势：有 1 条 alive 隧道",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("总结缺 %q:\n%s", want, out)
		}
	}
}

func TestRenderTaskSummaryZeroFindings(t *testing.T) {
	// 用户反馈的核心场景：任务跑完没有任何漏洞——「阴性也是结论」要明说，
	// 并把事实摘要端出来，不能沉默。
	d := map[string]any{
		"stats":        map[string]int{"fact": 5},
		"intents":      map[string]int{"done": 3},
		"recent_facts": []string{"端口全关", "WAF 拦截注入类 payload"},
	}
	out := renderTaskSummary(d, 4000)
	if !strings.Contains(out, "漏洞：0 条") || !strings.Contains(out, "阴性也是结论") {
		t.Fatalf("零漏洞形态必须明说阴性结论:\n%s", out)
	}
	if !strings.Contains(out, "端口全关") {
		t.Fatalf("事实摘要应出现在总结里:\n%s", out)
	}
}

func TestRenderTaskSummaryTruncates(t *testing.T) {
	out := renderTaskSummary(summaryDigestFixture(), 60)
	// maxLen 字节 + "…"（3 字节 UTF-8）。
	if len(out) > 63 || !strings.HasSuffix(out, "…") {
		t.Fatalf("应按 maxLen 截断并带省略号, got %d bytes: %q", len(out), out)
	}
}
