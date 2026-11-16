package agent

import (
	"chengjian.local/chengjian/db"
	"context"
	"github.com/Autumn-27/norma/harness"
	"strings"
	"testing"
)

// Configured task routing must not silently become global routing.
func TestWorkerProxyForTask(t *testing.T) {
	w := &Worker{proxyAddr: "http://global:8788", proxyCACert: "/data/traffic/_ca/cert.pem"}

	// 未装 resolver → 全局。
	a, c := w.proxyForTask(7)
	if a != w.proxyAddr || c != w.proxyCACert {
		t.Errorf("无 resolver 应用全局代理, got (%q, %q)", a, c)
	}

	// resolver 返回空 → unavailable.
	w.SetTaskProxyResolver(func(taskID int64) (string, string) { return "", "" })
	a, c = w.proxyForTask(7)
	if a != "" || c != "" {
		t.Errorf("resolver 空结果不能回落全局, got (%q, %q)", a, c)
	}

	// resolver 命中 → 任务实例地址+CA,且按 taskID 传参。
	w.SetTaskProxyResolver(func(taskID int64) (string, string) {
		if taskID != 7 {
			t.Errorf("resolver 应收到任务 id 7, got %d", taskID)
		}
		return "http://task:21100", "/data/traffic-tasks/7/_ca/cert.pem"
	})
	a, c = w.proxyForTask(7)
	if a != "http://task:21100" || c != "/data/traffic-tasks/7/_ca/cert.pem" {
		t.Errorf("resolver 命中应用任务实例, got (%q, %q)", a, c)
	}
}

func TestWorkerProxyFailureStopsBeforeModelOrToolSetup(t *testing.T) {
	w := &Worker{proxyAddr: "http://global:8788"}
	w.SetTaskProxyResolver(func(int64) (string, string) { return "", "" })
	reason, _, err := w.Execute(context.Background(), "fixture", 7, nil, nil, &db.Node{ID: 1}, nil, nil, nil, nil)
	if reason != harness.ReasonModelError || err == nil || !strings.Contains(err.Error(), "记录代理不可用") {
		t.Fatalf("unavailable proxy allowed execution: reason=%s err=%v", reason, err)
	}
}

func TestWorkerProxySourceMatchesResolvedRoute(t *testing.T) {
	for _, tt := range []struct {
		name        string
		taskID      int64
		globalAddr  string
		globalCA    string
		taskAddr    string
		taskCA      string
		withResolve bool
		wantAddr    string
		wantCA      string
		wantSource  string
	}{
		{name: "global proxy", taskID: 7, globalAddr: "http://global:8788", globalCA: "global-ca", wantAddr: "http://global:8788", wantCA: "global-ca", wantSource: "global"},
		{name: "direct global", taskID: 7, wantSource: "global"},
		{name: "resolver failed", taskID: 7, withResolve: true, globalAddr: "http://global:8788", globalCA: "global-ca", wantSource: "task/7"},
		{name: "task proxy", taskID: 7, withResolve: true, globalAddr: "http://global:8788", taskAddr: "http://task:21100", taskCA: "task-ca", wantAddr: "http://task:21100", wantCA: "task-ca", wantSource: "task/7"},
		{name: "non-task run", withResolve: true, taskAddr: "http://invalid:21100", globalAddr: "http://global:8788", wantAddr: "http://global:8788", wantSource: "global"},
		{name: "negative task id", taskID: -1, withResolve: true, taskAddr: "http://invalid:21100", wantSource: "global"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := &Worker{proxyAddr: tt.globalAddr, proxyCACert: tt.globalCA}
			calls := 0
			if tt.withResolve {
				w.SetTaskProxyResolver(func(id int64) (string, string) {
					calls++
					if id != tt.taskID {
						t.Errorf("resolver task=%d want=%d", id, tt.taskID)
					}
					return tt.taskAddr, tt.taskCA
				})
			}
			addr, ca, source := w.proxyForTaskSource(tt.taskID)
			if addr != tt.wantAddr || ca != tt.wantCA || source != tt.wantSource {
				t.Fatalf("resolved (%q,%q,%q), want (%q,%q,%q)", addr, ca, source, tt.wantAddr, tt.wantCA, tt.wantSource)
			}
			if tt.withResolve && tt.taskID > 0 && calls != 1 || tt.taskID <= 0 && calls != 0 {
				t.Fatalf("resolver calls=%d", calls)
			}
		})
	}
}

func TestRunInfoTrafficSourceDoesNotInheritParent(t *testing.T) {
	parent := WithRunInfo(context.Background(), RunInfo{TaskID: 1, TrafficSource: "task/1"})
	child := WithRunInfo(parent, RunInfo{TaskID: 2, IntentID: 20, TrafficSource: "global"})
	if ri := RunInfoFrom(child); ri.TaskID != 2 || ri.TrafficSource != "global" {
		t.Fatalf("child inherited a parent route: %+v", ri)
	}
	if ri := RunInfoFrom(parent); ri.TrafficSource != "task/1" {
		t.Fatal("child mutation affected parent run")
	}
	if ri := RunInfoFrom(context.Background()); ri.TrafficSource != "" {
		t.Fatal("legacy unknown attribution must retain zero source")
	}
	cleared := WithRunInfo(parent, RunInfo{})
	if ri, present := LookupRunInfo(cleared); !present || ri != (RunInfo{}) {
		t.Fatal("explicitly cleared parent attribution must retain its presence")
	}
	if _, present := LookupRunInfo(context.Background()); present {
		t.Fatal("bare context unexpectedly has explicit attribution")
	}
}
