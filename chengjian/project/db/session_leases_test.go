package db

import (
	"testing"
	"time"
)

// leaseFixture 建一个最小可用的会话，供租约用例使用。
//
// 只建会话、不建任务：session_leases.task_id 刻意没有外键（任务删除后租约记录
// 仍要能被收尾查询到），而 sessions.created_by_task 也没有外键。所以任务 id
// 用固定的高位假值即可，不必为了一个 id 去凑 tasks 表的 exploration_id 约束。
func leaseFixture(t *testing.T, d *DB) (sessionID, taskA, taskB int64) {
	t.Helper()
	ctx := t.Context()
	if err := d.QueryRowContext(ctx, `
INSERT INTO sessions(kind, url, status) VALUES('http_php','http://example.invalid/x.php','alive')
RETURNING id`).Scan(&sessionID); err != nil {
		t.Fatalf("session: %v", err)
	}
	t.Cleanup(func() {
		// 租约随会话 ON DELETE CASCADE 一起走；显式删一次只是让失败时更好定位。
		_, _ = d.Exec(`DELETE FROM session_leases WHERE session_id=$1`, sessionID)
		_, _ = d.Exec(`DELETE FROM sessions WHERE id=$1`, sessionID)
	})
	return sessionID, 900000001, 900000002
}

// TestSessionLeaseAcquireIsIdempotent 钉住「重复获取不产生新行」——租约是
// 主键 (session_id, task_id)，重复执行 exec 会反复 acquire，若每次插一行，
// 表会随执行次数线性膨胀，而语义上只有一条「正在用」。
func TestSessionLeaseAcquireIsIdempotent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	sid, taskA, _ := leaseFixture(t, d)
	st := NewSessionLeaseStore(d)
	ctx := t.Context()

	for i := 0; i < 3; i++ {
		if err := st.Acquire(ctx, sid, taskA, "exec"); err != nil {
			t.Fatalf("acquire #%d: %v", i, err)
		}
	}
	live, err := st.LiveForTask(ctx, taskA)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0] != sid {
		t.Fatalf("want exactly one live lease for session %d, got %v", sid, live)
	}
}

// TestSessionLeasePurposeAccumulates 钉住 purpose 合并：exec + recon 都发生过时，
// 审计要能同时看到两者，而不是后写的覆盖先写的。
func TestSessionLeasePurposeAccumulates(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	sid, taskA, _ := leaseFixture(t, d)
	st := NewSessionLeaseStore(d)
	ctx := t.Context()

	if err := st.Acquire(ctx, sid, taskA, "exec"); err != nil {
		t.Fatal(err)
	}
	if err := st.Acquire(ctx, sid, taskA, "recon"); err != nil {
		t.Fatal(err)
	}
	var purpose string
	if err := d.QueryRowContext(ctx,
		`SELECT purpose FROM session_leases WHERE session_id=$1 AND task_id=$2`, sid, taskA).Scan(&purpose); err != nil {
		t.Fatal(err)
	}
	if purpose != "exec,recon" {
		t.Fatalf("purpose = %q, want exec,recon", purpose)
	}
}

// TestSessionLeaseReleaseAndOtherHolders 是 T-09 的核心断言：
// 别的任务持有租约时 OtherHolders 必须报出来（删会话要拦），
// 释放之后必须为空（删会话要放行）。
func TestSessionLeaseReleaseAndOtherHolders(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	sid, taskA, taskB := leaseFixture(t, d)
	st := NewSessionLeaseStore(d)
	ctx := t.Context()

	if err := st.Acquire(ctx, sid, taskA, "exec"); err != nil {
		t.Fatal(err)
	}
	holders, err := st.OtherHolders(ctx, sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || holders[0] != taskA {
		t.Fatalf("holders = %v, want [%d]", holders, taskA)
	}
	// 排除自己后不该再看到自己（任务自己删自己的会话不该被自己拦）。
	if holders, err := st.OtherHolders(ctx, sid, taskA); err != nil || len(holders) != 0 {
		t.Fatalf("excluding self: holders=%v err=%v", holders, err)
	}
	// 第二个任务也来用 → 两个持有者。
	if err := st.Acquire(ctx, sid, taskB, "exec"); err != nil {
		t.Fatal(err)
	}
	holders, _ = st.OtherHolders(ctx, sid, 0)
	if len(holders) != 2 {
		t.Fatalf("holders = %v, want 2", holders)
	}
	// 释放 A → 只剩 B。
	if n, err := st.ReleaseTask(ctx, taskA); err != nil || n != 1 {
		t.Fatalf("release A: n=%d err=%v", n, err)
	}
	holders, _ = st.OtherHolders(ctx, sid, 0)
	if len(holders) != 1 || holders[0] != taskB {
		t.Fatalf("after releasing A: holders=%v, want [%d]", holders, taskB)
	}
}

// TestSessionLeaseIgnoresZeroIDs 钉住「没有任务维度不造租约」：
// 平台自检这类调用若凭空造一条 task_id=0 的租约，那条记录永远没人释放，
// 于是该会话再也删不掉。
func TestSessionLeaseIgnoresZeroIDs(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	st := NewSessionLeaseStore(d)
	ctx := t.Context()
	if err := st.Acquire(ctx, 0, 12345, "exec"); err != nil {
		t.Fatal(err)
	}
	if err := st.Acquire(ctx, 12345, 0, "exec"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM session_leases WHERE task_id=0 OR session_id=0`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("zero-id leases leaked: %d", n)
	}
}

// TestAuthAttemptLedger 覆盖 T-07 的读侧三问：成功有没有记录、失败能不能统计、
// 同一签名能不能识别。
func TestAuthAttemptLedger(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	ctx := t.Context()
	const credID = int64(987654321)
	const target = "10.9.9.9"
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM auth_attempts WHERE credential_id=$1`, credID)
	})
	st := NewAuthAttemptStore(d)

	if ok, err := st.HasSuccess(ctx, credID, "wmiexec", target); err != nil || ok {
		t.Fatalf("empty ledger must not claim success: ok=%v err=%v", ok, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := st.Record(ctx, AuthAttempt{
			TaskID: 1, CredentialID: credID, Principal: "administrator",
			Service: "wmiexec", Target: target, AuthMethod: CredHashNT,
			Outcome: AuthOutcomeFailure, FailureSignature: "logon failure",
		}); err != nil {
			t.Fatal(err)
		}
	}
	n, sig, err := st.RecentFailures(ctx, credID, "wmiexec", target, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || sig != "logon failure" {
		t.Fatalf("failures = %d/%q, want 2/logon failure", n, sig)
	}
	// 换一个服务不该串味：service 是谓词的一部分。
	if n, _, _ := st.RecentFailures(ctx, credID, "psexec", target, time.Hour); n != 0 {
		t.Fatalf("failures leaked across services: %d", n)
	}
	if _, err := st.Record(ctx, AuthAttempt{
		TaskID: 1, CredentialID: credID, Principal: "administrator",
		Service: "wmiexec", Target: target, AuthMethod: CredHashNT,
		Outcome: AuthOutcomeSuccess,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.HasSuccess(ctx, credID, "wmiexec", target); err != nil || !ok {
		t.Fatalf("success not recorded: ok=%v err=%v", ok, err)
	}
	// 成功之后失败计数仍然保留（历史不能因为后来成功就被抹掉）。
	if n, _, _ := st.RecentFailures(ctx, credID, "wmiexec", target, time.Hour); n != 2 {
		t.Fatalf("history lost after success: %d", n)
	}
	rows, err := st.ListByTask(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("list by task = %d rows, want 3", len(rows))
	}
	if rows[0].Outcome != AuthOutcomeSuccess {
		t.Fatalf("list must be newest-first, got %q", rows[0].Outcome)
	}
}
