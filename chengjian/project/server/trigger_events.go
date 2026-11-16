package server

import (
	"fmt"
	"chengjian.local/chengjian/db"
)

// Render a committed event using the recipient's configuration at event time.
// Manual callers keep the original storedTrigger payload shape.
func renderStoredTrigger(it storedTrigger) (triggeredRun, error) {
	item := triggeredRun{agentKey: it.AgentKey, title: it.Title, message: it.Message, taskID: it.TaskID, taskDesc: it.TaskDesc, taskGoal: it.TaskGoal, mergeable: it.Mergeable}
	if it.SourceKind == "" {
		return item, nil
	}
	e, tr := it.SourceEvent, it.Trigger
	if e == nil || tr == nil || tr.AgentKey != it.AgentKey {
		return item, fmt.Errorf("invalid trigger event snapshot")
	}
	item.taskDesc, item.taskGoal = e.TaskDesc, e.TaskGoal
	switch it.SourceKind {
	case "created":
		item.title = fmt.Sprintf("任务创建触发 · task#%d", e.TaskID)
		item.message = tr.TaskCreateMessage + "\n\n【本次由任务创建触发】"
	case "timeout":
		item.title = fmt.Sprintf("超时触发 · task#%d", e.TaskID)
		item.message = tr.TaskTimeoutMessage + "\n\n【本次由任务超时触发】"
	case "finding":
		item.title = fmt.Sprintf("finding 触发 · task#%d", e.TaskID)
		item.message = tr.FindingMessage + fmt.Sprintf("\n\n【本次由任务发现 finding 触发】\n发现: [%s/%s] %s", e.VulnClass, e.Severity, e.Summary)
		if it.AgentKey == db.FindingVerifierAgentKey {
			item.verifyEvent = e
		}
	case "goal":
		item.title = fmt.Sprintf("目标触发 · task#%d", e.TaskID)
		item.message = tr.GoalMessage + fmt.Sprintf("\n\n【本次由任务完成目标触发】\n达成目标: %s", e.Summary)
	case "tool":
		tag := ""
		if e.ToolIsErr {
			tag = "[error] "
		}
		item.title = fmt.Sprintf("工具触发 · %s · task#%d", e.Tool, e.TaskID)
		item.message = tr.ToolCallMessage + fmt.Sprintf("\n\n【本次由工具调用触发】\n工具: %s\n入参: %s\n返回: %s%s", e.Tool, trunc(e.ToolInput, 1500), tag, trunc(e.ToolOutput, 1500))
	case "interval":
		item.title = "定时触发 · " + e.FiredAt.Format("15:04")
		item.message = tr.IntervalMessage + "\n\n【本次为定时触发】" + e.FiredAt.Format(" 2006-01-02 15:04:05 MST")
	default:
		return item, fmt.Errorf("unknown trigger event kind %q", it.SourceKind)
	}
	return item, nil
}
