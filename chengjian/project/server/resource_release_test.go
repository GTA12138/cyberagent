package server

import (
	"strings"
	"testing"
)

// TestTaskResourceReleaseEmpty 钉住「什么都没做不污染响应」：
// 删除一个没有隧道、也没有租约的任务时，响应里不该出现 resource_release。
func TestTaskResourceReleaseEmpty(t *testing.T) {
	if !(TaskResourceRelease{}).Empty() {
		t.Fatal("zero value must be Empty")
	}
	if (TaskResourceRelease{Leases: 1}).Empty() {
		t.Fatal("released lease must make it non-empty")
	}
	if (TaskResourceRelease{Failures: []string{"x"}}).Empty() {
		t.Fatal("failure must make it non-empty even with no tunnels/leases")
	}
	if (TaskResourceRelease{Tunnels: []int64{1}}).Empty() {
		t.Fatal("torn down tunnel must make it non-empty")
	}
}

// TestStopTaskAndReleaseWithoutDatabase 钉住「数据库不可用时收尾是 no-op 而不是崩溃」。
// 这条路径在关闭流程与测试环境里都会走到：s.m 可能为 nil。
func TestStopTaskAndReleaseWithoutDatabase(t *testing.T) {
	s := &Server{}
	if rel := s.stopTaskAndRelease("123"); !rel.Empty() {
		t.Fatalf("no database must be a no-op, got %+v", rel)
	}
	// 非数值 id 必须跳过而不是按 0 去查——按 0 查会命中所有 task_id=0 的记录。
	if rel := s.releaseTaskResources(0); !rel.Empty() {
		t.Fatalf("task 0 must be a no-op, got %+v", rel)
	}
	if rel := s.releaseTaskResources(-1); !rel.Empty() {
		t.Fatalf("negative task must be a no-op, got %+v", rel)
	}
}

// TestJoinInt64s 是 409 正文的可读性保证：必须能看出「是谁在用」。
func TestJoinInt64s(t *testing.T) {
	cases := []struct {
		in   []int64
		want string
	}{
		{nil, ""},
		{[]int64{7}, "#7"},
		{[]int64{7, 12}, "#7, #12"},
	}
	for _, c := range cases {
		if got := joinInt64s(c.in); got != c.want {
			t.Fatalf("joinInt64s(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseTaskIDRejectsNonNumeric 说明：收尾只对真实任务 id 生效。
// 这里用已有 parseTaskID（task_llm.go）验证语义，避免再造一份解析逻辑。
func TestParseTaskIDRejectsNonNumeric(t *testing.T) {
	if _, err := parseTaskID("abc"); err == nil {
		t.Fatal("non-numeric id must error")
	}
	if _, err := parseTaskID("0"); err == nil {
		t.Fatal("zero id must error")
	}
	id, err := parseTaskID("42")
	if err != nil || id != 42 {
		t.Fatalf("parseTaskID(42) = %d, %v", id, err)
	}
}

// TestControlRouteUpstreamRejectsWithoutDB 说明：没有数据库时不能静默按「直连」处理。
// 静默直连的后果是「以为走了隧道，其实从平台出口发出去」——这比报错更危险。
func TestControlRouteUpstreamRejectsWithoutDB(t *testing.T) {
	s := &Server{}
	_, err := s.controlRouteUpstream(t.Context(), 9, 0)
	if err == nil {
		t.Fatal("must not silently fall back to direct when tunnel cannot be resolved")
	}
	if !strings.Contains(err.Error(), "数据库未就绪") {
		t.Fatalf("unexpected error: %v", err)
	}
	// tunnelID=0 表示「不路由」，必须返回空串且无错误（这是唯一允许的直连路径）。
	up, err := s.controlRouteUpstream(t.Context(), 0, 0)
	if err != nil || up != "" {
		t.Fatalf("tunnel 0 must mean direct: up=%q err=%v", up, err)
	}
}
