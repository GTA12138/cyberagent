package tunnel

// 隧道生命周期事件（内网态势留痕）。
//
// 为什么需要：隧道状态变化此前只有 log.Printf + tunnels.state 一列。任务活动流
// 里是空白的——人类复盘看不到「什么时候建的、什么时候断的、自动重拉了几次、
// 最后死在哪一层」。planner 也拿不到任何触发信号，只能靠 fact 文本间接推断。
//
// 与 OnStateChange 的分工：那个钩子只负责「重算任务 MITM 上游」，签名是
// func(taskID)；这个钩子带事件类型与原因，专供人类可读的时序记录。两者分开是
// 为了避免把一个纯副作用回调改造成半个业务事件总线。
type LifecycleKind string

const (
	// LifecycleDeployed 部署完成且通过隧道内决定性验证。
	LifecycleDeployed LifecycleKind = "deployed"
	// LifecycleBroken 巡检发现已断（进程没了 / 入口不可连 / 决定性拨测失败）。
	LifecycleBroken LifecycleKind = "broken"
	// LifecycleRedeployed 自动重拉成功。
	LifecycleRedeployed LifecycleKind = "redeployed"
	// LifecycleRedeployFailed 自动重拉失败（含重试耗尽）。
	LifecycleRedeployFailed LifecycleKind = "redeploy_failed"
	// LifecycleTornDown 已回收。
	LifecycleTornDown LifecycleKind = "torn_down"
)

// LifecycleEvent 是一次隧道生命周期事件。Reason 只在失败/断链时有值；
// Attempt 只在重拉相关事件里有意义（1-based）。
type LifecycleEvent struct {
	Kind      LifecycleKind
	TunnelID  int64
	TaskID    int64
	Adapter   string
	SocksPort int
	Reason    string
	Attempt   int
}

// notifyLifecycle 是内部派发：nil 钩子时静默（测试与未装配场景）。
func (m *Manager) notifyLifecycle(ev LifecycleEvent) {
	if m == nil || m.OnLifecycle == nil || ev.TaskID <= 0 {
		return
	}
	m.OnLifecycle(ev)
}
