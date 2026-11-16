package server

import (
	"context"
	"errors"
	"os"
	"testing"

	"chengjian.local/chengjian/db"
)

// TestReleaseTaskResourcesEndToEnd 端到端验证「任务终态按依赖逆序收尾」。
//
// 此前只有「空收尾不污染响应」与「无数据库时 no-op」两条单测，真正要保证的
// 语义（倒序拆、跳过已收、失败保留并报告、租约最后放）一条都没验过。补法
// 不是造一条真隧道——那需要真实远端；而是把收尾执行器换成可观测的替身，
// 走的仍是 releaseTaskResources 的真实代码路径，数据库也是真的。
func TestReleaseTaskResourcesEndToEnd(t *testing.T) {
	dsn := os.Getenv("CHENGJIAN_PG_DSN")
	if dsn == "" {
		t.Skip("explicit isolated CHENGJIAN_PG_DSN required")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()

	task, err := pg.CreateTask("release e2e", "local fixtures", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteTask(task.ID)

	// 三条隧道：一条已经收过（stopped，必须跳过），一条会拆失败（必须保留
	// 在 Failures 里而不是静默忽略），一条正常。
	tstore := db.NewTunnelStore(pg)
	first, err := tstore.Create(context.Background(), &db.TunnelRecord{
		TaskID: task.ID, Kind: "socks", State: db.TunnelAlive,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := tstore.Create(context.Background(), &db.TunnelRecord{
		TaskID: task.ID, Kind: "portfwd", State: db.TunnelAlive,
	})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := tstore.Create(context.Background(), &db.TunnelRecord{
		TaskID: task.ID, Kind: "socks", State: db.TunnelStopped,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 一条真实会话 + 两条租约（租约对 sessions 有外键，必须建真会话）。
	sessStore := db.NewSessionStore(pg, []byte("release-e2e-master-key"))
	sessID, err := sessStore.Create(context.Background(), &db.SessionRecord{
		Kind: "http_php", URL: "http://127.0.0.1:1/shell.php", Lang: "php",
		Status: db.SessionAlive, CreatedByTask: task.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	leaseStore := db.NewSessionLeaseStore(pg)
	if err := leaseStore.Acquire(context.Background(), sessID, task.ID, "exec"); err != nil {
		t.Fatal(err)
	}
	if err := leaseStore.Acquire(context.Background(), sessID, task.ID+900000, "recon"); err != nil {
		t.Fatal(err)
	}

	// 替身执行器：记录调用顺序，并让 second 拆失败。
	var order []int64
	failOn := second
	s := &Server{
		m: &Manager{pg: pg, dir: t.TempDir()},
		tunnelTeardownOverride: func(_ context.Context, id int64) error {
			order = append(order, id)
			if id == failOn {
				return errors.New("远端清理未确认")
			}
			return nil
		},
	}

	rel := s.releaseTaskResources(task.ID)

	// 1) 倒序：后建的先拆。依赖方向是「后建的更可能依赖先建的」，所以逆序。
	if len(order) != 2 {
		t.Fatalf("应只拆两条未终结隧道（stopped 跳过），实际 %v", order)
	}
	if order[0] != second || order[1] != first {
		t.Fatalf("必须按 id 倒序拆，实际 %v（期望 [%d %d]）", order, second, first)
	}
	for _, id := range order {
		if id == stopped {
			t.Fatal("已 stopped 的隧道不该再拆一次")
		}
	}

	// 2) 成功的那条进 Tunnels，失败的那条进 Failures——不能因为一条失败就
	//    把整次收尾判成失败，也不能把失败当成功。
	if len(rel.Tunnels) != 1 || rel.Tunnels[0] != first {
		t.Fatalf("成功拆除的隧道应只含 #%d，实际 %v", first, rel.Tunnels)
	}
	if len(rel.Failures) != 1 {
		t.Fatalf("拆不掉的那条必须留在 Failures 里，实际 %v", rel.Failures)
	}
	if rel.Empty() {
		t.Fatal("有拆除动作或失败时不能报 Empty（否则响应里看不到收尾发生过）")
	}

	// Unknown teardown retains the task's session leases until a successful retry.
	if rel.Leases != 0 {
		t.Fatalf("unconfirmed teardown released leases: %d", rel.Leases)
	}
	holders, err := leaseStore.OtherHolders(context.Background(), sessID, 0)
	if err != nil || len(holders) != 2 {
		t.Fatalf("leases must survive failure: %v %v", holders, err)
	}
	failOn = 0
	retry := s.releaseTaskResources(task.ID)
	if len(retry.Failures) != 0 || retry.Leases != 1 {
		t.Fatalf("retry cleanup: %+v", retry)
	}
	otherTask := task.ID + 900000
	holders, err = leaseStore.OtherHolders(context.Background(), sessID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || holders[0] != otherTask {
		t.Fatalf("别的任务的租约必须保留（且只剩它），实际 %v", holders)
	}
	if n, err := leaseStore.ReleaseTask(context.Background(), otherTask); err != nil || n != 1 {
		t.Fatalf("另一个任务释放自己的租约应成功: n=%d err=%v", n, err)
	}
	if holders, _ = leaseStore.OtherHolders(context.Background(), sessID, 0); len(holders) != 0 {
		t.Fatalf("全部释放后不该还有持有者，实际 %v", holders)
	}
}
