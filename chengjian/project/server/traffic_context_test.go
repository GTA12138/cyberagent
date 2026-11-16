package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/traffic"
)

func TestTrafficToolsExplicitEmptyRunDoesNotRestoreAssemblyTask(t *testing.T) {
	tr, err := traffic.Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	m := &Manager{traffic: tr, trafficOn: true}
	assembled := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: 7, IntentID: 70, TrafficSource: "task/7"})
	tools := m.scopedTrafficTools(assembled)
	if len(tools) == 0 {
		t.Fatal("fixture did not enable traffic tools")
	}
	call := func(ctx context.Context) (string, bool) {
		r, err := tools[0].Call(ctx, json.RawMessage(`{"host":"fixture.test"}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		return r.Flatten(), r.IsError
	}
	if out, failed := call(context.Background()); !failed || !strings.Contains(out, "任务流量历史不可用") {
		t.Fatalf("absent run attribution should retain assembly task selection: %q error=%v", out, failed)
	}
	cleared := agent.WithRunInfo(assembled, agent.RunInfo{})
	if out, failed := call(cleared); failed || !strings.Contains(out, "无匹配流量") {
		t.Fatalf("explicit empty run must use global: %q error=%v", out, failed)
	}
	globalTask := agent.WithRunInfo(assembled, agent.RunInfo{TaskID: 8, TrafficSource: "global"})
	if out, failed := call(globalTask); failed || !strings.Contains(out, "无匹配流量") {
		t.Fatalf("explicit fallback route must use global: %q error=%v", out, failed)
	}
}
