package db

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sessionAccessFixture(t *testing.T) (*DB, *SessionStore, *Task, int64) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	owner, err := d.CreateTask("session access fixture", "local fake", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(owner.ID) })
	store := NewSessionStore(d, []byte("session-access-test-key"))
	id, err := store.Create(context.Background(), &SessionRecord{Kind: "fixture", Secret: json.RawMessage(`{}`), CreatedByTask: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), id) })
	return d, store, owner, id
}

func TestSessionAccessLifecycle(t *testing.T) {
	d, store, owner, id := sessionAccessFixture(t)
	ctx := context.Background()
	target, err := d.CreateTaskWithOptions("background only", "fixture", TaskCreateOptions{SourceTaskIDs: []int64{owner.ID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(target.ID) })
	check := func(task int64, action string, want bool) {
		t.Helper()
		got, err := store.CanAccess(ctx, id, task, action)
		if err != nil || got != want {
			t.Fatalf("task=%d action=%s got=%v err=%v want=%v", task, action, got, err, want)
		}
	}
	for _, action := range SessionActions() {
		check(owner.ID, action, true)
		check(target.ID, action, false)
	}
	check(0, SessionActionRead, false)
	if err := store.GrantAccess(ctx, id, target.ID, []string{SessionActionRead}, nil, "manual fixture"); err != nil {
		t.Fatal(err)
	}
	check(target.ID, SessionActionRead, true)
	check(target.ID, SessionActionExec, false)
	grants, err := store.AccessGrants(ctx, id)
	if err != nil || len(grants) != 1 || len(grants[0].Actions) != 1 || grants[0].Actions[0] != SessionActionRead {
		t.Fatalf("grant projection=%+v err=%v", grants, err)
	}
	rows, err := store.ListAccessible(ctx, target.ID, "")
	if err != nil || len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("visible rows=%v err=%v", rows, err)
	}
	// Unrelated corrupt credentials cannot poison this task's filtered list.
	bad, err := store.Create(ctx, &SessionRecord{Kind: "fixture", CreatedByTask: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, bad) })
	if _, err := d.Exec(`UPDATE sessions SET secret='corrupt' WHERE id=$1`, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListAccessible(ctx, target.ID, ""); err != nil {
		t.Fatalf("unrelated secret read: %v", err)
	}
	if err := store.RevokeAccess(ctx, id, target.ID); err != nil {
		t.Fatal(err)
	}
	check(target.ID, SessionActionRead, false)
	if err := store.GrantAccess(ctx, id, target.ID, []string{SessionActionRead}, nil, "explicit regrant"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE session_access_grants SET expires_at=now()-interval '1 second' WHERE session_id=$1 AND task_id=$2`, id, target.ID); err != nil {
		t.Fatal(err)
	}
	check(target.ID, SessionActionRead, false)
	if err := store.GrantAccess(ctx, id, target.ID, []string{SessionActionRead}, nil, "explicit regrant"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE tasks SET archived_at=now() WHERE id=$1`, target.ID); err != nil {
		t.Fatal(err)
	}
	check(target.ID, SessionActionRead, false)
	if _, err := d.Exec(`UPDATE tasks SET archived_at=NULL WHERE id=$1`, target.ID); err != nil {
		t.Fatal(err)
	}
	check(target.ID, SessionActionRead, true)
	if err := store.RevokeAccess(ctx, id, target.ID); err != nil {
		t.Fatal(err)
	}
	check(target.ID, SessionActionRead, false)
	if err := d.DeleteTask(target.ID); err != nil {
		t.Fatal(err)
	}
	grants, err = store.AccessGrants(ctx, id)
	if err != nil || len(grants) != 0 {
		t.Fatalf("deleted task grant retained: %v %v", grants, err)
	}
}

// GrantsForTask 是 agent 视图的授权投影：它必须和 CanAccess 用同一套谓词，
// 否则「列表说能 exec、执行时被拒」这类信息不同步会重新出现。
func TestGrantsForTaskMatchesEnforcement(t *testing.T) {
	d, store, owner, id := sessionAccessFixture(t)
	ctx := context.Background()
	target, err := d.CreateTaskWithOptions("grant projection", "fixture", TaskCreateOptions{SourceTaskIDs: []int64{owner.ID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(target.ID) })

	// 无授权时不出现该会话。
	got, err := store.GrantsForTask(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[id]; ok {
		t.Fatalf("无授权时不应出现会话 %d：%v", id, got)
	}

	if err := store.GrantAccess(ctx, id, target.ID, []string{SessionActionRead}, nil, "fixture"); err != nil {
		t.Fatal(err)
	}
	got, err = store.GrantsForTask(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if acts := got[id]; len(acts) != 1 || acts[0] != SessionActionRead {
		t.Fatalf("授权投影=%v，期望只有 read", acts)
	}
	if allowed, _ := store.CanAccess(ctx, id, target.ID, SessionActionExec); allowed {
		t.Fatalf("投影有 read 但执行侧却允许 exec，两者谓词不一致")
	}

	// 过期后投影必须同步消失（与执行侧一致）。
	if _, err := d.Exec(`UPDATE session_access_grants SET expires_at=now()-interval '1 second' WHERE session_id=$1 AND task_id=$2`, id, target.ID); err != nil {
		t.Fatal(err)
	}
	got, err = store.GrantsForTask(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[id]; ok {
		t.Fatalf("过期授权不应出现在投影里：%v", got)
	}
	if allowed, _ := store.CanAccess(ctx, id, target.ID, SessionActionRead); allowed {
		t.Fatalf("过期授权在投影里消失、执行侧却仍允许")
	}

	// 撤销后同理。
	if err := store.GrantAccess(ctx, id, target.ID, []string{SessionActionExec}, nil, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeAccess(ctx, id, target.ID); err != nil {
		t.Fatal(err)
	}
	got, err = store.GrantsForTask(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[id]; ok {
		t.Fatalf("已撤销授权不应出现在投影里：%v", got)
	}
}

func TestSessionHandoffAtomic(t *testing.T) {
	d, store, owner, id := sessionAccessFixture(t)
	child, err := d.CreateTaskWithOptions("handoff fixture", "fixture", TaskCreateOptions{SourceTaskIDs: []int64{owner.ID}, SessionHandoff: &SessionHandoffOptions{SessionID: id, SourceTaskID: owner.ID, HostIP: "2001:db8::1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(child.ID) })
	if child.ParentRef != strconv.FormatInt(owner.ID, 10) {
		t.Fatalf("missing parent: %+v", child)
	}
	persisted, err := d.GetTask(child.ID)
	if err != nil || persisted == nil || persisted.ParentRef != child.ParentRef {
		t.Fatalf("parent not persisted: %+v %v", persisted, err)
	}
	scope, err := d.Assets().ListTaskScope(child.ID)
	if err != nil || len(scope) != 1 || scope[0].Net != "2001:db8::1/128" || scope[0].Source != "manual" {
		t.Fatalf("single-host IPv6 scope=%+v err=%v", scope, err)
	}
	for _, action := range SessionActions() {
		allowed, err := store.CanAccess(context.Background(), id, child.ID, action)
		if err != nil || !allowed {
			t.Fatalf("handoff action %s: %v %v", action, allowed, err)
		}
	}
	// Fail at the final grant INSERT, after parent and scope writes. The whole
	// new task and exploration must disappear, not become a runnable partial task.
	if _, err := d.Exec(`CREATE OR REPLACE FUNCTION fixture_reject_session_grant() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture grant rejected'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TRIGGER fixture_reject_session_grant BEFORE INSERT ON session_access_grants FOR EACH ROW WHEN (NEW.session_id=` + strconv.FormatInt(id, 10) + `) EXECUTE FUNCTION fixture_reject_session_grant()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DROP TRIGGER IF EXISTS fixture_reject_session_grant ON session_access_grants; DROP FUNCTION IF EXISTS fixture_reject_session_grant()`)
	})
	var beforeTasks, beforeExplorations int
	if err := d.QueryRow(`SELECT (SELECT count(*) FROM tasks),(SELECT count(*) FROM explorations)`).Scan(&beforeTasks, &beforeExplorations); err != nil {
		t.Fatal(err)
	}
	_, err = d.CreateTaskWithOptions("must rollback", "fixture", TaskCreateOptions{SourceTaskIDs: []int64{owner.ID}, SessionHandoff: &SessionHandoffOptions{SessionID: id, SourceTaskID: owner.ID, HostIP: "127.0.0.1"}})
	if err == nil || !strings.Contains(err.Error(), "fixture grant rejected") {
		t.Fatalf("expected failure at final grant insert, got %v", err)
	}
	var afterTasks, afterExplorations int
	if err := d.QueryRow(`SELECT (SELECT count(*) FROM tasks),(SELECT count(*) FROM explorations)`).Scan(&afterTasks, &afterExplorations); err != nil {
		t.Fatal(err)
	}
	if beforeTasks != afterTasks || beforeExplorations != afterExplorations {
		t.Fatalf("partial handoff persisted: %d/%d -> %d/%d", beforeTasks, beforeExplorations, afterTasks, afterExplorations)
	}
}

func TestSessionGrantRejectsInvalidInputWithoutDatabase(t *testing.T) {
	store := &SessionStore{}
	for _, actions := range [][]string{nil, {"*"}, {SessionActionRead, SessionActionRead}} {
		if err := store.GrantAccess(context.Background(), 1, 2, actions, nil, "fixture"); err == nil {
			t.Fatalf("accepted %v", actions)
		}
	}
	past := time.Now().Add(-time.Second)
	if err := store.GrantAccess(context.Background(), 1, 2, []string{SessionActionRead}, &past, "fixture"); err == nil {
		t.Fatal("accepted expired grant")
	}
}
