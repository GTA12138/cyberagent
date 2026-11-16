package server

import "testing"

// 补全出口必须按任务解析;任务级解析不到时回落全局出口(与 worker 同一套回落
// 语义),而不是「不发」——但回落要留日志,这是内网补全静默失效的唯一线索。
func TestEnrichProxyForTask(t *testing.T) {
	m := &Manager{
		tasks:       map[string]*Task{"7": {ID: "7"}},
		globalProxy: "http://global.example:8080",
	}

	// 任务级解析器没装配(taskProxy=nil)→ 回落全局出口。
	if got := m.enrichProxyForTask(7); got != m.globalProxy {
		t.Fatalf("taskProxy 未装配时应回落全局出口 %q, got %q", m.globalProxy, got)
	}
	// 无归属任务 → 同样走全局出口(与旧行为一致)。
	if got := m.enrichProxyForTask(0); got != m.globalProxy {
		t.Fatalf("taskID=0 应走全局出口 %q, got %q", m.globalProxy, got)
	}

	// 无全局出口且无任务出口 → 空(直连),不能拼出非法地址。
	empty := &Manager{tasks: map[string]*Task{}}
	if got := empty.enrichProxyForTask(7); got != "" {
		t.Fatalf("两边都没有出口时应返回空(直连), got %q", got)
	}
}

// Capture off must preserve task routing, CA trust and upstream selection.
func TestEnrichProxyForTaskWithoutRecording(t *testing.T) {
	port := freePort(t)
	m := &Manager{
		tasks:       map[string]*Task{"7": {ID: "7"}},
		globalProxy: "http://global.example:8080",
		trafficOn:   false,
	}
	m.taskProxy = newTaskProxyManager(t.TempDir(), &fakeTunnelLister{}, m.GlobalProxy, port, port)
	m.taskProxy.SetRecordEnabled(false)
	defer m.taskProxy.Close()
	addr, ca := m.TaskProxyForTask(7)
	if addr == "" || addr == m.globalProxy || ca == "" {
		t.Fatalf("capture off lost task proxy or TLS trust")
	}
	if got := m.enrichProxyForTask(7); got != addr {
		t.Fatal("enrichment did not use the task proxy")
	}
	inst := m.taskProxy.proxies[7]
	if inst.tr.RecordEnabled() || len(m.HostTools()) != 0 {
		t.Fatal("capture off must disable recording and traffic tools")
	}
	m.taskProxy.SetIntranetResolver(func(int64) bool { return true })
	m.RefreshTaskProxy(7)
	if got := m.TaskProxyModeForTask(7); got != string(UpstreamBlocked) {
		t.Fatalf("capture off lost blocked mode: %s", got)
	}
}

func TestTaskByNumericID(t *testing.T) {
	m := &Manager{tasks: map[string]*Task{"7": {ID: "7"}}}
	if got := m.taskByNumericID(7); got == nil || got.ID != "7" {
		t.Fatalf("应取到任务 7, got %v", got)
	}
	if got := m.taskByNumericID(8); got != nil {
		t.Fatalf("未加载的任务应返回 nil, got %v", got)
	}
	if got := m.taskByNumericID(0); got != nil {
		t.Fatalf("taskID=0 应返回 nil, got %v", got)
	}
}
