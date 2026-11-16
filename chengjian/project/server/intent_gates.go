// intent_gates.go 意图的两道闸：完成判据（done_when）与调度前置（prerequisites）。
//
// 两个判据器早在 planning 包建成（T-11），但此前只在 add_intent 创建瞬间被
// 用过一次：EvaluateDone 全仓无生产调用方、IntentPlan 是死代码。于是：
//   - worker「没出错」就默认 done——哪怕写回 0 条 fact/finding，哪怕它自己
//     声明的 done_when（如 fact:出网能力）一条都没满足；add_intent 的 schema
//     承诺「缺项会回到它面前」，代码从未兑现；
//   - 调度按 priority DESC, id ASC 领意图，prerequisites 完全不参与——下游
//     意图在隧道/凭据/上游事实尚不存在时照样被领取，烧 worker 槽位与 token
//     预算去撞墙（红日 3 式空转）。
//
// 本文件把两个判据器接进引擎，仍不改变 planner-worker 分工：planner 依旧决定
// 「派什么」，引擎只是把「先派谁」排得更聪明、把「假完成」显形给 planner。
//
// 两条纪律（与 planning 包一致）：
//   - unknown/invalid 不算未满足：数据缺失不产生闸门副作用；
//   - 全部候选都不满足前置时仍领取优先级最高的那条：硬拦会让任务停摆
//     （前置可能恰好由这条意图自己产出，或需要 planner 派新意图补齐）。
package server

import (
	"context"
	"encoding/json"
	"log"
	"strconv"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/planning"
)

// intentPlanFields 是意图 payload 里判据字段的投影。AddIntentWithPlan 会把
// 判据同时复制进 payload（db/exploration.go：判据只存列里等于只有 planner
// 知道）——frontier 读回的 Node 只有 payload，从这里取最省一次查询。
type intentPlanFields struct {
	Prerequisites []string `json:"prerequisites"`
	DoneWhen      []string `json:"done_when"`
}

func intentPlanOf(payload json.RawMessage) intentPlanFields {
	var f intentPlanFields
	if len(payload) == 0 {
		return f
	}
	_ = json.Unmarshal(payload, &f)
	return f
}

// nodeSummaries 把一类节点压成判据视图（ID + summary）。
func nodeSummaries(nodes []*db.Node) []planning.FactView {
	out := make([]planning.FactView, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		var p struct {
			Summary string `json:"summary"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		out = append(out, planning.FactView{ID: n.ID, Summary: p.Summary})
	}
	return out
}

// factState 构建 planning.State：facts/intent_done 判据的输入。
// 任何读取失败都缩到空集（判据返回 unknown/基于已有内容的结论），闸门
// fail-open——判据系统的职责是「把已知缺口显形」，不是拦任务。
func factState(store *db.ExplorationStore) planning.State {
	st := planning.State{}
	if store == nil {
		return st
	}
	if facts, err := store.ListByKind("fact", 200); err == nil {
		st.Facts = nodeSummaries(facts)
	}
	if intents, err := store.ListByKind("intent", 120); err == nil {
		for _, n := range intents {
			if n == nil || n.State != "done" {
				continue
			}
			var p struct {
				Summary string `json:"summary"`
			}
			_ = json.Unmarshal(n.Payload, &p)
			st.DoneItems = append(st.DoneItems, p.Summary)
		}
	}
	return st
}

// enginePrereqState 是引擎侧的 planning.State 构建器（claim 时复查前置用）。
// 组装处把它注入 ExecutionService.PrereqState；nil = 不复查（独立运行/测试）。
func (s *Server) enginePrereqState(ctx context.Context, taskID int64, store *db.ExplorationStore) planning.State {
	if s == nil || store == nil {
		return planning.State{}
	}
	st := factState(store)
	st.Counts = s.planningCounts(ctx, taskID)
	return st
}

// unmetPrerequisiteCount 返回明确未满足（unmet）的前置条数。
// unknown/invalid 不计（planning 包的纪律：缺数据不产生闸门副作用）。
func unmetPrerequisiteCount(prereqs []string, st planning.State) int {
	if len(prereqs) == 0 {
		return 0
	}
	return planning.Summarize(planning.Evaluate(planning.ParseAll(prereqs), st)).Unmet
}

// doneStateFor 构建「做完没有」的判定输入。
//
// EvidenceN 的口径：该意图实际产出并落图的节点数（FactsYielded 的 fact +
// 本次写回的 finding）。「证据」在这个系统里的载体就是 fact/finding，
// 用别的口径（比如活动条数）会把日志噪声记成证据。
// ManualNote 只有在真有产出时才非空：WriteCounts.String() 对零值也返回
// 文本，直接用会让 command: 类判据在「什么都没写」时也亮绿灯。
func doneStateFor(store *db.ExplorationStore, intentID int64, wrote agent.WriteCounts) planning.DoneState {
	var st planning.DoneState
	if store == nil {
		return st
	}
	if facts, err := store.ListByKind("fact", 200); err == nil {
		st.Facts = nodeSummaries(facts)
	}
	if findings, err := store.ListByKind("finding", 200); err == nil {
		st.Findings = nodeSummaries(findings)
	}
	if yielded, err := store.FactsYielded(intentID); err == nil {
		st.EvidenceN = len(yielded) + wrote.Findings
	}
	if wrote.Total() > 0 {
		st.ManualNote = wrote.String()
	}
	return st
}

// doneGapHint 渲染完成判据缺口的提示文本；无判据或无缺口返回空串。
// 文本面向 planner（hint 节点）：给出「哪条判据、为什么没满足、该怎么办」，
// 让它决定是重派同方向、派补缺方向，还是接受「判据写错了」修订目标。
func doneGapHint(doneWhen []string, st planning.DoneState) string {
	if len(doneWhen) == 0 {
		return ""
	}
	gaps := planning.EvaluateDone(planning.ParseDoneAll(doneWhen), st)
	if len(gaps) == 0 {
		return ""
	}
	out := "【完成判据缺口】意图正常结束（state=done），但它自己声明的 done_when 有 " +
		strconv.Itoa(len(gaps)) +
		" 条未满足——「跑完」不等于「做完」，请勿按已覆盖处理："
	for i, g := range gaps {
		if i >= 3 {
			out += "；…"
			break
		}
		out += "；" + g.Criterion + "（" + g.Reason + "）"
	}
	out += "。处置：据缺口重派或派补缺意图（如缺 fact:关键词 就派能产出该事实的方向）；" +
		"若判据本身写得不现实，用新意图的 done_when 修正，不要硬凑。"
	return out
}

// noteDoneGaps 在意图以 done 收场后核对其 done_when（intent_gates 的引擎接线）。
//
// 留痕走两条既有通道（与停滞检测同款，engine.go runIntent 的 onStuck 注释）：
//   1. hint 节点挂探索图——planner 每轮 graph overview 必读 hints，这是缺口
//      进 planner 视野的既有通道；
//   2. system activity 进 worker trace——复盘时能看到「哪条意图、何时被标缺口」。
//
// 刻意不改 intent 终态（done 仍是 done）：判据是软约束（planning 包纪律 2），
// 引擎的职责是把「跑完≠做完」显形，改状态会让「判据写错」变成「意图失败」，
// 反而诱发无意义重试。
func (e *Engine) noteDoneGaps(t *Task, intent *db.Node, wrote agent.WriteCounts, worker string) {
	if e == nil || t == nil || t.Store == nil || intent == nil {
		return
	}
	doneWhen := intentPlanOf(intent.Payload).DoneWhen
	if len(doneWhen) == 0 {
		return
	}
	hint := doneGapHint(doneWhen, doneStateFor(t.Store, intent.ID, wrote))
	if hint == "" {
		return
	}
	iid := intent.ID
	if _, err := t.Store.AddNode(db.KindHint, map[string]any{"text": hint}, 0, "active", "system", nil); err != nil {
		log.Printf("[worker %s] 意图 #%d 完成判据缺口 hint 写图失败: %v", worker, iid, err)
	}
	e.emitActivity(t, db.Activity{NodeID: &iid, Worker: worker, Kind: "system",
		Summary: "完成判据未满足（done_when 缺口），已挂 hint 供规划者处置", Detail: hint})
}
