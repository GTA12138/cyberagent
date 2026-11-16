// task_summary.go 任务收官总结与空产出重述（用户反馈的直接修复）。
//
// 问题：任务跑完（或人工续跑后没有新发现）时，系统沉默——漏洞推送只覆盖
// 「有 finding」的情况，没有 finding 的任务、续跑无新产出的轮次，人只能自己
// 翻活动流拼凑「它到底做了什么」。
//
// 方案：一个确定性的总结生成器（不依赖 LLM——任务可能正是因为 LLM 预算
// 耗尽停的，恰恰这时最需要总结），两个触发点：
//   1. 任务落入终态 done（goalless 抽干 / planner 判达标两条路）→ 收官总结卡；
//   2. 人工续跑（chat 直投）整轮正常结束但零写回 → 「本轮无新增产出 +
//      已知态势摘要」卡——没有新东西也要把旧东西主动端出来，不沉默。
//
// 输出通道：任务活动流（kind=text，与待授权通知卡同款，前端已能渲染）。
// 刻意不做 LLM 叙述版：结构化摘要是零成本、零失败面的底线；叙述版将来
// 可作为增量（一次性 Complete 调用，enrich 语义层同款模式），不阻塞本修复。
package server

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"chengjian.local/chengjian/db"
)

// summaryBrief 是 goal/finding 节点 payload 的宽松投影：字段名随写入方不同
// 而异（goal 有 text/desc，finding 有 name/summary），取第一个非空即可。
type summaryBrief struct {
	Name     string
	Severity string
	Text     string
}

func briefOf(payload json.RawMessage) summaryBrief {
	var p struct {
		Name     string `json:"name"`
		Title    string `json:"title"`
		Severity string `json:"severity"`
		Text     string `json:"text"`
		Desc     string `json:"desc"`
		Goal     string `json:"goal"`
		Summary  string `json:"summary"`
	}
	_ = json.Unmarshal(payload, &p)
	b := summaryBrief{Name: p.Name, Severity: p.Severity, Text: p.Text}
	if b.Name == "" {
		b.Name = p.Title
	}
	if b.Text == "" {
		b.Text = p.Desc
	}
	if b.Text == "" {
		b.Text = p.Goal
	}
	if b.Text == "" {
		b.Text = p.Summary
	}
	if len(b.Text) > 80 {
		b.Text = b.Text[:80] + "…"
	}
	if len(b.Name) > 60 {
		b.Name = b.Name[:60] + "…"
	}
	return b
}

// summaryGoal 是 goal 行的总结视图（包级类型：渲染层要做类型断言，
// 匿名 struct 断不过具名切片）。
type summaryGoal struct {
	state string
	brief summaryBrief
}

// taskSummaryDigest 从探索图与内网台账收集总结素材。任何一步失败都缩到
// 空值（总结是增强，不是闸门）。
func (s *Server) taskSummaryDigest(t *Task) map[string]any {
	digest := map[string]any{}
	if t == nil || t.Store == nil {
		return digest
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)

	if stats, err := t.Store.Stats(); err == nil {
		digest["stats"] = stats // kind → count（fact/intent/finding/goal 总量）
	}
	goals := []summaryGoal{}
	if nodes, err := t.Store.ListByKind("goal", 50); err == nil {
		for _, n := range nodes {
			goals = append(goals, summaryGoal{state: n.State, brief: briefOf(n.Payload)})
		}
	}
	digest["goals"] = goals

	findings := []map[string]any{}
	if nodes, err := t.Store.ListByKind("finding", 100); err == nil {
		for _, n := range nodes {
			b := briefOf(n.Payload)
			findings = append(findings, map[string]any{"id": n.ID, "state": n.State, "name": b.Name, "severity": b.Severity})
		}
	}
	digest["findings"] = findings

	// 意图按终态分桶（Stats 只有 kind 总量，没有状态分布）。
	intents := map[string]int{}
	if nodes, err := t.Store.ListByKind("intent", 300); err == nil {
		for _, n := range nodes {
			intents[n.State]++
		}
	}
	digest["intents"] = intents

	recent := []string{}
	if nodes, err := t.Store.ListByKind("fact", 6); err == nil {
		for _, n := range nodes {
			if b := briefOf(n.Payload); b.Text != "" || b.Name != "" {
				recent = append(recent, strings.TrimSpace(b.Name+"："+b.Text))
			}
		}
	}
	digest["recent_facts"] = recent

	// 内网态势（外网任务没有 intranet 键——判断「这是不是内网任务」交给渲染层）。
	if s != nil {
		digest["intranet"] = s.intranetSnapshot(taskID)
	}
	return digest
}

// renderTaskSummary 把素材渲染成确定性文本（纯函数）。maxLen 是正文字节上限。
func renderTaskSummary(digest map[string]any, maxLen int) string {
	var b strings.Builder
	w := func(format string, args ...any) {
		if b.Len() > maxLen {
			return
		}
		fmt.Fprintf(&b, format, args...)
	}

	if goals, ok := digest["goals"].([]summaryGoal); ok && len(goals) > 0 {
		met := 0
		for _, g := range goals {
			if g.state == "met" {
				met++
			}
		}
		w("目标：%d 个（达成 %d / 未达 %d）\n", len(goals), met, len(goals)-met)
		for _, g := range goals {
			mark := "○"
			if g.state == "met" {
				mark = "✔"
			}
			if txt := strings.TrimSpace(g.brief.Name + " " + g.brief.Text); txt != "" {
				w("  %s [%s] %s\n", mark, g.state, txt)
			}
		}
	}
	if findings, ok := digest["findings"].([]map[string]any); ok && len(findings) > 0 {
		confirmed := []map[string]any{}
		other := map[string]int{}
		for _, f := range findings {
			if st, _ := f["state"].(string); st == "confirmed" {
				confirmed = append(confirmed, f)
			} else {
				other[st]++
			}
		}
		w("漏洞：共 %d 条（确认 %d", len(findings), len(confirmed))
		for st, n := range other {
			w("，%s %d", st, n)
		}
		w("）\n")
		for i, f := range confirmed {
			if i >= 10 {
				w("  …（其余 %d 条见漏洞列表）\n", len(confirmed)-10)
				break
			}
			id, _ := f["id"].(int64)
			w("  - [%v] %s (#%d)\n", f["severity"], f["name"], id)
		}
	} else {
		w("漏洞：0 条（本任务未确认漏洞——阴性也是结论，见下方事实摘要）\n")
	}
	if stats, ok := digest["stats"].(map[string]int); ok {
		if intents, ok := digest["intents"].(map[string]int); ok {
			w("探索：事实 %d 条；意图 完成 %d / 受阻 %d / 预算耗尽 %d / 未开始 %d\n",
				stats["fact"], intents["done"], intents["blocked"], intents["exhausted"], intents["open"])
		} else {
			w("探索：事实 %d 条\n", stats["fact"])
		}
	}
	if recent, ok := digest["recent_facts"].([]string); ok && len(recent) > 0 {
		w("最近事实：\n")
		for _, r := range recent {
			w("  · %s\n", r)
		}
	}
	if intra, ok := digest["intranet"].(map[string]any); ok && len(intra) > 0 {
		if sum, ok := intra["summary"].(string); ok && sum != "" {
			w("内网态势：%s\n", sum)
		}
	}
	out := b.String()
	if len(out) > maxLen {
		out = out[:maxLen] + "…"
	}
	return strings.TrimRight(out, "\n")
}

// emitTaskSummary 生成并写一张收官总结卡（任务终态时调用）。
func (s *Server) emitTaskSummary(t *Task, reason string) {
	if s == nil || t == nil {
		return
	}
	digest := s.taskSummaryDigest(t)
	body := renderTaskSummary(digest, 4000)
	nFindings := 0
	if findings, ok := digest["findings"].([]map[string]any); ok {
		nFindings = len(findings)
	}
	headline := fmt.Sprintf("任务收官总结（%s）：%d 条漏洞记录、详见正文", reason, nFindings)
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
		Summary: headline, Detail: body})
}

// emitIdleRecap 人工续跑零写回时的「已知态势重述」卡——没有新发现也要
// 主动把已有结论端出来，而不是沉默（用户原始反馈的场景）。
func (s *Server) emitIdleRecap(t *Task) {
	if s == nil || t == nil {
		return
	}
	digest := s.taskSummaryDigest(t)
	body := renderTaskSummary(digest, 2500)
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
		Summary: "本轮执行完成，无新增事实/漏洞产出——以下是任务已知态势摘要（上一步的结论都在这里）",
		Detail:  body})
}
