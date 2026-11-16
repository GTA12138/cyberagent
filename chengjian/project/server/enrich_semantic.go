package server

// enrich_semantic.go 是 S3 语义层（enrich.JSAnalyzer）的生产实现（T-21）。
//
// enrich 包把语义层做成了依赖反转接口：它不 import 任何 LLM 客户端，只在
// 「确定性规则吃不下的材料」上问一句。本文件是那个接口的第一个真实实现。
//
// 通电前必须齐三件，缺一件就不该接（否则等于给一个无法计费、无法判失败、
// 无法测收益的环节通电）：
//
//	① 成本归属：每一次语义调用都要先向 budget.Service 申请准入，用完结算。
//	   语义层跑在补全的后台 worker 池里，不是 agent 的调用路径——如果它走
//	   agent 那条带「预算拒绝即暂停任务」回调的 provider 包装，一次后台补全
//	   被拒就会把正在正常干活的任务暂停掉。所以这里自己 Admit/Settle，
//	   拒绝的后果只是「这一层降级」，不触碰任务状态。
//	② 失败语义：区分「没额度」「没 provider」「模型返回了但不能用」三种。
//	   前两种是环境问题，重试无意义，直接退避；第三种是质量问题，要记日志
//	   但可以继续试。全部失败都不抛给 enrich——它只有「拿到候选」和「没拿到」
//	   两种结果，抛错会让补全主链路以为抓取失败。
//	③ 评测基线：本层能产出什么，由 enrich_semantic_parse.go 的纯函数决定，
//	   而那份纯函数有 golden fixture 单测。这样「接上语义层到底改变了什么」
//	   可以在不烧 token 的前提下被复现和对比。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"chengjian.local/chengjian/budget"
	"chengjian.local/chengjian/enrich"
	"github.com/Autumn-27/norma/llm"
)

const (
	// semanticMaxOutput 是单次语义分析允许模型输出的 token 上限。
	// 候选端点列表是短 JSON，给太多只会让它开始编。
	semanticMaxOutput = 1200
	// semanticMaxInputTokens 是准入时用的输入估算上限。真实输入被
	// enrich.EvidenceWindows 裁到 24 KiB，按 4 字节/token 粗估再加固定开销。
	semanticMaxInputTokens = 24*1024/4 + 1024
	// semanticFailureCooldown：同一任务连续失败到这个次数后，本任务不再尝试
	// 语义层，直到该任务下一次成功为止。理由是环境类失败（没额度、provider
	// 不可用）在短时间内不会自愈，而每份材料都试一遍等于把失败成本乘以材料数。
	semanticFailureCooldown = 2
	// semanticMaxCandidates 是单次返回的候选上限，防止一份混淆 bundle 让
	// 模型吐几百条低置信路径把资产图灌满。
	semanticMaxCandidates = 24
)

// semanticSkipReason 是「这一层为什么没跑」的枚举。它只用于日志与测试断言，
// 不进入 enrich 的返回值——enrich 的契约是「候选 or 空」，不是「错误 or 空」。
type semanticSkipReason string

const (
	semanticOK           semanticSkipReason = ""
	semanticNoTask       semanticSkipReason = "no_task"
	semanticNoProvider   semanticSkipReason = "no_provider"
	semanticNoBudget     semanticSkipReason = "budget_denied"
	semanticCoolingDown  semanticSkipReason = "cooling_down"
	semanticModelFailed  semanticSkipReason = "model_failed"
	semanticUnusableJSON semanticSkipReason = "unusable_output"
)

// WireEnrichSemantic 把语义层接到补全引擎上（T-21 的接线点）。
//
// 放在这里而不是 newManager 里：Manager 不持有 Server，而语义层要读任务 LLM
// 链、要写任务预算，这两样都在 Server 上。反过来把 Server 塞进 Manager 会让
// 「数据层不认识业务层」这条边界破掉。
//
// 失败不阻断启动：语义层是加分层，接不上就是少一层，确定性抽取照常跑。
func (s *Server) WireEnrichSemantic() {
	if s == nil || s.m == nil {
		return
	}
	eng := s.m.Enrich()
	if eng == nil {
		return
	}
	eng.SetJSAnalyzer(NewEnrichSemantic(s))
	log.Printf("[enrich] S3 语义层已通电：按任务预算准入，额度不足只降级不暂停任务")
}

// enrichSemantic 实现 enrich.JSAnalyzer。
type enrichSemantic struct {
	s *Server

	mu       sync.Mutex
	failures map[int64]int // taskID → 连续失败次数
	lastSkip map[int64]semanticSkipReason
}

// NewEnrichSemantic 构造语义层实现。server 启动时注入给 enrich.Engine。
func NewEnrichSemantic(s *Server) enrich.JSAnalyzer { return &enrichSemantic{s: s, failures: map[int64]int{}} }

// AnalyzeCandidates 是 enrich 唯一会调的入口。
func (e *enrichSemantic) AnalyzeCandidates(ctx context.Context, req enrich.AnalyzeRequest) ([]enrich.Candidate, error) {
	if e == nil || e.s == nil {
		return nil, nil
	}
	if req.TaskID <= 0 {
		// 没有任务归属就没有账单主体。给不出成本归属的调用一律不花。
		e.noteSkip(0, semanticNoTask)
		return nil, nil
	}
	if strings.TrimSpace(req.Code) == "" {
		e.noteSkip(req.TaskID, semanticUnusableJSON)
		return nil, nil
	}
	if e.coolingDown(req.TaskID) {
		e.noteSkip(req.TaskID, semanticCoolingDown)
		return nil, nil
	}

	prov, model, ok := e.providerFor(req.TaskID)
	if !ok {
		// 没有可用 provider 是环境问题：记一次失败并退避，不要每份材料都去解析一遍。
		e.noteFailure(req.TaskID, semanticNoProvider)
		return nil, nil
	}

	completion := llm.CompletionRequest{
		System:    []string{semanticSystemPrompt},
		Messages:  []llm.Message{llm.UserText(buildSemanticInput(req))},
		MaxTokens: semanticMaxOutput,
		Thinking:  "disabled", // 抽取任务：思考只烧钱不提高召回
	}

	// ① 成本归属：先申请准入，拿到 reservation 才允许发请求。
	res, err := e.admit(ctx, req.TaskID)
	if err != nil {
		var exceeded *budget.ExceededError
		var reserved *budget.ReservedError
		if errors.As(err, &exceeded) || errors.As(err, &reserved) {
			// 额度不足不是故障，是边界：本层降级，任务照常跑。
			log.Printf("[enrich-semantic] task %d 语义层跳过：token 预算不足（%v）", req.TaskID, err)
			e.noteFailure(req.TaskID, semanticNoBudget)
			return nil, nil
		}
		log.Printf("[enrich-semantic] task %d 语义层准入失败: %v", req.TaskID, err)
		e.noteFailure(req.TaskID, semanticNoProvider)
		return nil, nil
	}

	msg, _, usage, callErr := prov.Complete(ctx, completion)
	// 结算用独立的短超时 context：模型调用被取消不能连带取消记账。
	e.settle(res, usage, callErr == nil)
	if callErr != nil {
		if ctx.Err() != nil {
			return nil, nil // 任务在收尾，不是失败
		}
		log.Printf("[enrich-semantic] task %d 语义层调用失败 (model=%s): %v", req.TaskID, model, callErr)
		e.noteFailure(req.TaskID, semanticModelFailed)
		return nil, nil
	}

	cands, dropped := parseSemanticCandidates(msg.Text(), req.SourceURL)
	if len(cands) == 0 {
		// 模型回了话但没有一条能用——这是质量问题不是环境问题。不记失败，
		// 但要留痕：否则「语义层开着但从不产出」这件事会完全隐形。
		log.Printf("[enrich-semantic] task %d 语义层无可采候选 url=%s dropped=%d digest=%s",
			req.TaskID, req.SourceURL, len(dropped), shortDigest(msg.Text()))
		e.noteSkip(req.TaskID, semanticUnusableJSON)
		return nil, nil
	}
	e.noteSuccess(req.TaskID)
	if len(dropped) > 0 {
		log.Printf("[enrich-semantic] task %d 丢弃 %d 条不可采候选（跨源/无 method/非法路径），保留 %d 条 url=%s",
			req.TaskID, len(dropped), len(cands), req.SourceURL)
	}
	return cands, nil
}

// providerFor 按 agent 角色同一套优先级（绑定 → 任务链 → 全局）解析 provider。
//
// 刻意取 current() 的**未包装**结果：current() 返回的是裸 provider，
// budget 包装只发生在 streamHooks() 里。本层自己 Admit，所以不能再用那层
// 包装——那层的拒绝回调会暂停整个任务。
func (e *enrichSemantic) providerFor(taskID int64) (llm.Provider, string, bool) {
	if e.s == nil || e.s.m == nil || e.s.m.pg == nil {
		return nil, "", false
	}
	rt := &taskLLMRuntime{s: e.s, taskID: strconv.FormatInt(taskID, 10), agentKey: "enrich"}
	sel, err := rt.current()
	if err != nil || sel.provider == nil {
		if err != nil {
			log.Printf("[enrich-semantic] task %d 解析 provider 失败: %v", taskID, err)
		}
		return nil, "", false
	}
	model := ""
	if cfg, ok := rt.activeCfg(); ok {
		model = cfg.Model
	}
	return sel.provider, model, true
}

// admit 向任务预算申请一次非 completion 开销的准入。
func (e *enrichSemantic) admit(ctx context.Context, taskID int64) (budget.Reservation, error) {
	svc := e.s.budgetService()
	if svc == nil {
		return budget.Reservation{}, errors.New("task budget store unavailable")
	}
	return svc.Admit(ctx, taskID, budget.Tokens{Input: semanticMaxInputTokens, Output: semanticMaxOutput})
}

// settle 结算一次语义调用。结算失败只记日志：把它当成调用失败会掩盖真实原因。
func (e *enrichSemantic) settle(res budget.Reservation, usage llm.Usage, complete bool) {
	svc := e.s.budgetService()
	if svc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Settle(ctx, res, usage, complete); err != nil {
		log.Printf("[enrich-semantic] 结算语义层用量失败 (reservation=%s): %v", res.ID, err)
	}
}

func (e *enrichSemantic) coolingDown(taskID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.failures[taskID] >= semanticFailureCooldown
}

func (e *enrichSemantic) noteFailure(taskID int64, why semanticSkipReason) {
	if taskID <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failures == nil {
		e.failures = map[int64]int{}
	}
	e.failures[taskID]++
	if e.lastSkip == nil {
		e.lastSkip = map[int64]semanticSkipReason{}
	}
	e.lastSkip[taskID] = why
}

func (e *enrichSemantic) noteSuccess(taskID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.failures, taskID)
	delete(e.lastSkip, taskID)
}

func (e *enrichSemantic) noteSkip(taskID int64, why semanticSkipReason) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastSkip == nil {
		e.lastSkip = map[int64]semanticSkipReason{}
	}
	e.lastSkip[taskID] = why
}

// SkipReason 供测试与运维查询「这个任务的语义层为什么没产出」。
func (e *enrichSemantic) SkipReason(taskID int64) semanticSkipReason {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastSkip[taskID]
}

// Forgot 在任务结束时清掉该任务的计数，避免长跑进程里 map 无限增长。
func (e *enrichSemantic) Forgot(taskID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.failures, taskID)
	delete(e.lastSkip, taskID)
}

func shortDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// buildSemanticInput 把证据窗口拼成一段可读输入。
//
// 刻意把 signals 放在最前面：模型最容易忽略的是「我在找什么」，而这份输入
// 的主体是一大段压缩过的 JS。把意图写在开头比写在结尾召回率高得多。
func buildSemanticInput(req enrich.AnalyzeRequest) string {
	var b strings.Builder
	b.WriteString("来源 URL：")
	b.WriteString(req.SourceURL)
	b.WriteString("\n确定性规则已命中的信号：")
	if len(req.Signals) == 0 {
		b.WriteString("（无，说明是靠语义判断）")
	} else {
		b.WriteString(strings.Join(req.Signals, ", "))
	}
	b.WriteString("\n\n以下是同一份材料里按命中位置裁剪出的证据窗口（`/* … */` 表示中间省略）：\n\n")
	b.WriteString(req.Code)
	return b.String()
}

// semanticSystemPrompt 是语义层的合同。它只被允许做一件事：把「确定性规则
// 认不出的端点」读出来。任何扩张（跨域、编造 method、猜参数值）都会把
// 假端点写进资产图，比漏掉更贵——漏掉只是少一条，编造会让后续所有基于
// 资产图的判断都建在假前提上。
const semanticSystemPrompt = `你在读一份 Web 前端的静态材料（JS bundle / sourcemap / 配置），任务是从中找出【代码里真实存在】的 HTTP 端点。

只输出 JSON，不要任何解释文字，格式：
[{"method":"GET","url":"/api/user/list","params":[{"location":"query","name":"page","type":"integer"}],"why":"简短依据"}]

硬性规则（违反任何一条都会被丢弃，且不计入你的产出）：
1. **url 只能是路径**（以 / 开头）或与来源同源的绝对 URL。跨域地址一律不要输出——写出去也会被丢弃。
2. **method 必须有依据**：代码里明确写了 method（fetch(...,{method:'POST'})、axios.post、XHR.open('PUT',...) 等）才能填。看不出 method 就**不要输出这一条**，不要猜 GET。
3. **url 必须是代码里出现的字面量或可静态拼接出的路径**。含运行时变量的（如 ` + "`/api/user/${id}`" + `）只保留能确定的前缀，且把变量部分写成 {id} 这类占位符；无法确定前缀就不要输出。
4. **params 只填代码里能看到的参数名**（查询串字面量、body 对象字面量的 key、header 名）。类型不确定填 "string"。看不到就不要编。
5. 不要输出静态资源（.js/.css/.png/.woff/.map 等）、不要输出已经是页面导航的路径（如 "/"、"/login" 这种 HTML 路由），除非代码里明确以 XHR/fetch 方式请求它。
6. 最多 24 条，按置信度从高到低。宁少勿滥：一条编造的端点会让后续所有基于资产图的判断建在假前提上。
7. 没有任何可确定的端点时输出 []。`

var _ enrich.JSAnalyzer = (*enrichSemantic)(nil)
