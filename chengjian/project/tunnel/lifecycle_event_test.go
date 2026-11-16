package tunnel

import (
	"context"
	"encoding/json"
	"testing"

	"chengjian.local/chengjian/db"
)

// notifyLifecycle 必须在 nil 钩子 / 无任务归属时静默，绝不能 panic——
// 它在巡检 goroutine 与 Deploy 主路径上都会被调用。
func TestNotifyLifecycleGuards(t *testing.T) {
	m := &Manager{} // OnLifecycle == nil
	m.notifyLifecycle(LifecycleEvent{Kind: LifecycleDeployed, TunnelID: 1, TaskID: 2}) // 不应 panic

	var nilMgr *Manager
	nilMgr.notifyLifecycle(LifecycleEvent{Kind: LifecycleDeployed, TunnelID: 1, TaskID: 2}) // 不应 panic

	got := []LifecycleEvent{}
	m.OnLifecycle = func(ev LifecycleEvent) { got = append(got, ev) }
	m.notifyLifecycle(LifecycleEvent{Kind: LifecycleDeployed, TunnelID: 1, TaskID: 0})
	if len(got) != 0 {
		t.Fatal("TaskID<=0 的事件不应派发（无任务可归属）")
	}
	m.notifyLifecycle(LifecycleEvent{Kind: LifecycleDeployed, TunnelID: 1, TaskID: 9})
	if len(got) != 1 || got[0].TunnelID != 1 || got[0].TaskID != 9 {
		t.Fatalf("事件未正确派发: %+v", got)
	}
}

// fakeTunnelStore 是最小台账替身，供 health 路径测试。
type fakeTunnelStore struct {
	rec    *db.TunnelRecord
	states []string
}

func (f *fakeTunnelStore) Create(context.Context, *db.TunnelRecord) (int64, error) { return 0, nil }
func (f *fakeTunnelStore) Get(context.Context, int64) (*db.TunnelRecord, error)  { return f.rec, nil }
func (f *fakeTunnelStore) UpdateState(_ context.Context, _ int64, state, _ string) error {
	f.states = append(f.states, state)
	return nil
}
func (f *fakeTunnelStore) UpdateDeployParams(context.Context, int64, int, json.RawMessage) error {
	return nil
}
func (f *fakeTunnelStore) ListActive(context.Context) ([]*db.TunnelRecord, error) {
	return nil, nil
}
func (f *fakeTunnelStore) ListByTask(context.Context, int64) ([]*db.TunnelRecord, error) {
	return nil, nil
}
func (f *fakeTunnelStore) Delete(context.Context, int64) error { return nil }

// 重拉耗尽时必须派发一次 redeploy_failed（且只派一次，靠 exhausted 去重）。
// 这条事件是人看到「自动修不回来了、要人工介入」的唯一信号。
func TestMaybeRedeployExhaustedEmitsOnce(t *testing.T) {
	plan := Plan{Kind: KindSocks, TaskID: 77, Adapter: AdapterChisel, SocksPort: 20801}
	raw, _ := json.Marshal(plan)
	store := &fakeTunnelStore{rec: &db.TunnelRecord{
		ID: 5, TaskID: 77, Kind: KindSocks, Adapter: AdapterChisel,
		State: db.TunnelAlive, DeployParams: raw,
	}}
	var events []LifecycleEvent
	m := &Manager{store: store, OnLifecycle: func(ev LifecycleEvent) { events = append(events, ev) }}

	rs := &redeployState{count: maxRedeploys} // 已达上限
	retry := map[int64]*redeployState{5: rs}
	m.maybeRedeploy(context.Background(), 5, &plan, retry)
	m.maybeRedeploy(context.Background(), 5, &plan, retry) // 第二次应被 exhausted 拦住

	n := 0
	for _, ev := range events {
		if ev.Kind == LifecycleRedeployFailed {
			n++
			if ev.TunnelID != 5 || ev.TaskID != 77 {
				t.Fatalf("事件归属错误: %+v", ev)
			}
			if ev.Attempt != maxRedeploys {
				t.Fatalf("Attempt 应为 %d, got %d", maxRedeploys, ev.Attempt)
			}
		}
	}
	if n != 1 {
		t.Fatalf("重拉耗尽应恰好派发一次 redeploy_failed, got %d", n)
	}
}
