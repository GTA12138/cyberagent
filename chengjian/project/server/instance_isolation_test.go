package server

// instance_isolation_test.go 是 T-25 的运行时侧验证：同一进程里两个任务
// 的「每运行时依赖」互不影响。
//
// 静态守卫（agent/deps_isolation_test.go）证明的是「没有包级可变回调」；
// 但那只排除了最坏的一种共享方式。真正要证明的是**闭包绑定正确**：
// 为任务 A 造出来的判据统计，读到的必须是 A 的资源，不能因为两个任务
// 同时存在就串。这类错误的表现极其隐蔽——它不会报错，只会让 A 的判据
// 用 B 的资产算出一个看起来正常的结论。

import (
	"context"
	"os"
	"testing"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/planning"
	"chengjian.local/chengjian/session"
	"chengjian.local/chengjian/tunnel"
)

// TestPlanningCountsAreBoundToTheirOwnTask 证明两个任务的判据统计闭包
// 各读各的，且并发调用不串。
func TestPlanningCountsAreBoundToTheirOwnTask(t *testing.T) {
	dsn := os.Getenv("CHENGJIAN_PG_DSN")
	if dsn == "" {
		t.Skip("需要隔离库：CHENGJIAN_PG_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()

	taskA, err := pg.CreateTask("isolation fixture A", "local only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteTask(taskA.ID)
	taskB, err := pg.CreateTask("isolation fixture B", "local only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteTask(taskB.ID)

	// A 有两个 IP 资产，B 有一个域名资产。数量与类型都不同，
	// 这样「读串了」一定会体现在断言上，而不是碰巧相等。
	for _, ip := range []string{"10.10.0.1", "10.10.0.2"} {
		if _, err := pg.Assets().UpsertIP(db.UpsertIPReq{IP: ip, TaskID: taskA.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: "b.example.com", TaskID: taskB.ID}); err != nil {
		t.Fatal(err)
	}

	// 判据统计要读四个子系统；缺任何一个都会整份降级成 unknown（这是刻意的，
	// 见 planning_counts.go 的纪律）。所以这里把四件都装上——顺带证明
	// 「装配齐全时闭包确实各读各的」而不是「因为没装所以都空」。
	key := []byte("0123456789abcdef0123456789abcdef")
	tunMgr, err := tunnel.NewManager(tunnel.Options{
		Store:    db.NewTunnelStore(pg),
		Sessions: session.NewRegistry(),
		DataDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		m:         &Manager{pg: pg},
		credStore: db.NewCredentialStore(pg, key),
		sessStore: db.NewSessionStore(pg, key),
		tunnels:   tunMgr,
	}
	countsA := s.planningCountsForTask(taskA.ID)
	countsB := s.planningCountsForTask(taskB.ID)

	ctx := context.Background()
	a, b := countsA(ctx), countsB(ctx)
	if a.AssetsByType["ip"] != 2 {
		t.Fatalf("任务 A 应看到 2 个 ip 资产，实际 %v", a.AssetsByType)
	}
	if a.AssetsByType["root_domain"] != 0 {
		t.Fatalf("任务 A 不应看到任务 B 的域名资产，实际 %v", a.AssetsByType)
	}
	if b.AssetsByType["root_domain"] != 1 {
		t.Fatalf("任务 B 应看到 1 个 domain 资产，实际 %v", b.AssetsByType)
	}
	if b.AssetsByType["ip"] != 0 {
		t.Fatalf("任务 B 不应看到任务 A 的 ip 资产，实际 %v", b.AssetsByType)
	}

	// 并发跑一遍：闭包捕获的是 taskID 的副本，不是循环变量或共享字段。
	// 如果哪天有人把 taskID 提成 Server 字段，这条会立刻失败。
	done := make(chan struct{})
	errCh := make(chan string, 2)
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			ca, cb := countsA(ctx), countsB(ctx)
			if ca.AssetsByType["ip"] != 2 || ca.AssetsByType["root_domain"] != 0 {
				errCh <- "并发下任务 A 的计数被串改"
				return
			}
			if cb.AssetsByType["root_domain"] != 1 || cb.AssetsByType["ip"] != 0 {
				errCh <- "并发下任务 B 的计数被串改"
			}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}
}

// TestPlanningCountsDegradeToAllUnknown 钉住「读失败整份返回 unknown」。
//
// 这不是洁癖：部分数据会让判据混出「资产 known、隧道 unknown」的半真状态，
// planner 无从判断哪些结论可信。而全部 unknown 至少是诚实的——它会让
// add_intent 返回「前置条件无法判定」，而不是一个错误的「不满足」。
func TestPlanningCountsDegradeToAllUnknown(t *testing.T) {
	s := &Server{m: &Manager{}} // 无 DB
	got := s.planningCounts(context.Background(), 1)
	if got.AssetsByType != nil || got.Credentials != nil || got.Sessions != nil || got.Tunnels != nil {
		t.Fatalf("无 DB 时必须整份 unknown（全 nil），实际 %+v", got)
	}
	// 显式确认「零值 Counts」在判定里等于 unknown 而不是「0 个」。
	sum := planning.Summarize(planning.Evaluate(
		planning.ParseAll([]string{"asset:ip"}), planning.State{Counts: got}))
	if len(sum.Blocking) != 0 {
		t.Fatalf("unknown 不能变成 unmet（缺数据不等于确认没有）: %+v", sum.Blocking)
	}
	if len(sum.Notes) == 0 {
		t.Fatal("unknown 必须留下可见的说明，否则 planner 不知道这条判据没被判过")
	}
}
