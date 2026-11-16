// prompt_blocks.go 把系统提示词拆成五个职责固定的块（O2 / T-14）。
//
// 问题不是提示词太短，而是它一直在变长却没有边界：角色、红线、工具说明、
// 判据、输出规约依次拼接成一条长字符串。于是三件坏事同时发生：
//
//  1. **改不动**。想收紧「什么算做完」，得在一大段里找到对应句子；找不到就
//     再追加一段，长此以往同一件事有三处说法，互相矛盾时模型挑一处遵守。
//  2. **测不了**。没有稳定边界就无法断言「这条纪律还在」——只能靠人眼读。
//  3. **缓存不住**。易变内容（当前意图、态势）和稳定内容混在一起，system 每
//     轮都变，跨意图复用全失效。
//
// 五个块的分工（顺序即优先级）：
//
//	stable-role     稳定职责：我是谁、红线是什么。跨任务不变，可缓存。
//	current-task    当前任务：这一条意图是什么、开工前要有什么、什么算做完。
//	state           结构化状态：全局态势快照。**放在 user 消息里**，不进 system。
//	criteria        本次场景判据：按攻击面注入的「场景→判据→转向」骨架与纪律。
//	stop-contract   输出与停止合同：写到哪、什么时候停、停之前必须留下什么。
//
// 第三条刻意如此：state 是每轮都变的内容，塞进 system 会让整段 system 失去
// 缓存价值，而且让「稳定职责」和「当前态势」在模型眼里权重相同。状态放在
// user 消息里，模型照样读得到，但它不会被误当成长期纪律。
package agent

import "strings"

// PromptBlockID 是块的稳定标识。字符串常量（不是 iota）是为了让日志、
// 测试与提示词编辑界面能直接引用，且重排不改变含义。
type PromptBlockID string

const (
	// BlockStableRole 是角色与红线：跨任务稳定，可缓存。
	BlockStableRole PromptBlockID = "stable-role"
	// BlockCurrentTask 是当前意图与判据：本次唯一任务。
	BlockCurrentTask PromptBlockID = "current-task"
	// BlockState 是结构化态势：易变，放在 user 消息。
	BlockState PromptBlockID = "state"
	// BlockCriteria 是本次场景判据：按攻击面注入。
	BlockCriteria PromptBlockID = "criteria"
	// BlockStopContract 是输出与停止合同。
	BlockStopContract PromptBlockID = "stop-contract"
)

// PromptBlock 是提示词的一段。
//
// Title 为空表示「不画分隔头」——只用于整段提示词的第一块（角色正文），
// 因为正文自带开头，再加一个标题会把「你是一个…」推到第二行，读起来像
// 目录而不是指令。
type PromptBlock struct {
	ID    PromptBlockID
	Title string
	Body  string
}

// Render 渲染一块。空 Body 返回空串（调用方不必自己判空）。
func (b PromptBlock) Render() string {
	if strings.TrimSpace(b.Body) == "" {
		return ""
	}
	if b.Title == "" {
		return b.Body
	}
	return "\n\n## 【" + b.Title + "】\n" + b.Body
}

// assembleBlocks 按顺序拼接若干块，跳过空块。
func assembleBlocks(blocks ...PromptBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		b.WriteString(blk.Render())
	}
	return b.String()
}

// blockTitles 是块的中文标题，集中一处便于统一措辞。
//
// 单独抽出来是因为这些标题会被测试断言、也会被人在日志里搜——散落在各处的
// 字符串一旦改动就会让「这块还在不在」的断言静默失效。
var blockTitles = map[PromptBlockID]string{
	BlockStableRole:   "职责与红线",
	BlockCurrentTask:  "当前任务",
	BlockState:        "当前态势（只读）",
	BlockCriteria:     "本次场景判据",
	BlockStopContract: "输出与停止合同",
}

// titleOf 取块标题（未知 ID 返回 ID 本身，便于发现拼错）。
func titleOf(id PromptBlockID) string {
	if t, ok := blockTitles[id]; ok {
		return t
	}
	return string(id)
}
