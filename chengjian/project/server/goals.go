package server

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
)

type goalSpec struct {
	Text      string
	VulnClass string
}

// launchTask runs the shared post-creation sequence for a task created via ANY
// path (HTTP createTask 或 orchestration spawn_task),避免两处复制粘贴:
//  1. seed 根资产,喂给事件驱动 loop;
//  2. 可选种子意图,worker 免等首轮 planner 直接开跑;
//  3. 后台异步做目标分解(发「第 0 轮目标拆解」round + LLM 分解步骤 + 逐条 goal,页面可见),
//     分解完再 engine.Run —— 引擎在 goal 节点就绪后才启动,避免 planner 抢在 goal 之前跑的竞态。
//
// 异步(goroutine)所以调用方立即返回,两条路径行为一致:秒建任务、后台拆目标。
func (s *Server) launchTask(t *Task, seedText string, seedFirstIntent bool) {
	if !s.engine.beginTaskOperation(t.ID) {
		return
	}
	s.seed(t, seedText)
	if seedFirstIntent {
		s.seedFirstIntent(t)
	}
	s.engine.decInflight(t.ID)
	if _, err := s.admitTask(t, "bootstrap"); err != nil {
		log.Printf("[concurrency] task %s 启动失败: %v", t.ID, err)
	}
}

func (s *Server) startTaskEngine(t *Task) {
	ctx := s.engine.execContextFor(s.ctx, t.ID)
	if ctx.Err() != nil || s.engine.IsDeleting(t.ID) {
		return
	}
	s.engine.emitActivity(t, db.Activity{Worker: "planner", Kind: "round",
		Summary: "第 0 轮目标拆解"})
	goals := s.createGoals(ctx, t, func(r db.Activity) {
		s.engine.emitActivity(t, r)
	})
	if ctx.Err() != nil || s.engine.IsDeleting(t.ID) {
		return
	}
	for _, g := range goals {
		summary := g.Text
		if g.VulnClass != "" {
			summary = fmt.Sprintf("[%s] %s", g.VulnClass, g.Text)
		}
		s.engine.emitActivity(t, db.Activity{Worker: "planner", Kind: "text", Summary: summary})
	}
	if ctx.Err() != nil || s.engine.IsDeleting(t.ID) {
		return
	}
	s.engine.Run(s.ctx, t)
}

func (s *Server) occupiesConcurrencySlot(t *Task) bool {
	return s.taskService().occupiesConcurrencySlot(t)
}

func (s *Server) runningTaskCount(excludeID string) int {
	return s.taskService().runningTaskCount(excludeID)
}

// admitTask is the single admission path for new, resumed, rerun and follow-up
// work. It atomically either starts the task or appends it to the persistent FIFO
// queue. mode is bootstrap for a freshly-created task and resume otherwise.
func (s *Server) admitTask(t *Task, mode string) (queued bool, err error) {
	return s.taskService().admitTask(t, mode)
}

// admitPausedTask is the task-control resume path. The paused precondition is
// checked under the same scheduler lock as admission so a concurrent pause,
// dequeue or FIFO promotion cannot leave database and Engine state divergent.
func (s *Server) admitPausedTask(t *Task) (queued bool, err error) {
	return s.taskService().admitPausedTask(t)
}

func (s *Server) admitTaskWhen(t *Task, mode string, requirePaused bool) (queued bool, err error) {
	return s.taskService().admitTaskWhen(t, mode, requirePaused)
}

func (s *Server) hasReadyQueuedTask(excludeID string) bool {
	return s.taskService().hasReadyQueuedTask(excludeID)
}

func (s *Server) startAdmittedTask(t *Task, mode string) {
	s.taskService().startAdmittedTask(t, mode)
}

func (s *Server) reconcileConcurrency() {
	s.taskService().reconcileConcurrency()
}

// reviveTask 让一个已停下的任务重新跑起来:把终态(done/failed/timeout)拉回 running、
// 解除暂停,并(重)启动引擎循环 + 唤醒。已在 running 且未暂停的任务:只剩 Run 里的一次
// Notify,近乎无副作用。用于「主 agent set_goals 新增目标」和「重跑 blocked 意图」两处。
//
// 为什么必须显式复活:planner/worker 循环的终态门(engine.go)会吞掉普通 notify——光改
// 图 + Notify 唤不醒已判完成的任务;重启后终态任务的 goroutine 也可能已不在,故还要 Run。
func (s *Server) reviveTask(t *Task) {
	s.taskService().reviveTask(t)
}

// createGoals materializes the goal node(s) under the task root (rel objective).
// Decomposition is done ENTIRELY by the LLM (the project requires an LLM). There is
// no rule-based fallback splitter — it only ever produced garbage (shredded URLs,
// meaningless 2-way splits). If the LLM yields nothing (an error), the raw task goal
// is used verbatim as a single goal so the task still has something to judge against.
// Returns the seeded specs so callers can emit activity records for them.
// emit, when non-nil, is forwarded to DecomposeGoals so LLM steps are visible in the UI.
func (s *Server) createGoals(ctx context.Context, t *Task, emit func(db.Activity)) []goalSpec {
	if t == nil {
		return nil
	}
	// Use the SAME LLM the task runs on (its pinned profile, else the active profile),
	// NOT agent.FromEnv() — the LLM is configured via the UI (DB profile), not env vars,
	// so FromEnv returned empty and every task silently fell back to the crude rule
	// splitter (which shredded URLs / made meaningless 2-way splits).
	var specs []goalSpec
	var as *db.AssetStore
	if s.m != nil {
		as = s.m.Assets()
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	s.engine.BeginLLMCall(t.ID)
	goalRuntime := s.agentsForTask(t).runtime
	decomposed := agent.DecomposeGoalsWithProvider(agent.WithRuntime(ctx, s.agentRuntime), goalRuntime, s.m.dir, t.Goal, t.Description, as, t.Store, taskID, goalRuntime.nonStreaming(), goalRuntime.maxTokens(), emit)
	s.engine.EndLLMCall(t.ID)
	for _, g := range decomposed {
		if strings.TrimSpace(g.Text) != "" {
			specs = append(specs, goalSpec{Text: g.Text, VulnClass: g.VulnClass})
		}
	}
	if len(specs) == 0 {
		// No decomposed goals (LLM error / no provider): use the raw task goal verbatim
		// as a single goal so the task still has something to judge against. This is the
		// only path that writes here — decomposed goals are already persisted by the tool.
		if g := strings.TrimSpace(t.Goal); g != "" {
			log.Printf("[goals] task %s: LLM 目标拆解无产出，回退为「原始目标作为单目标」", t.ID)
			origin, _ := t.Store.OriginFactID()
			id, _ := t.Store.AddNode(db.KindGoal, map[string]any{"text": g}, 0, "open", "system", nil)
			if origin > 0 && id > 0 {
				_ = t.Store.Link(origin, db.RelSpawns, id) // goal descends from the task root (origin fact)
			}
			specs = []goalSpec{{Text: g}}
		}
	}
	return specs
}
