// cascade_test.go 多级隧道级联治理的判定测试：
//   - 父断链 → 活跃子隧道标记 error（级联原因）+ LifecycleBroken 事件；
//     已 error / deploying 的子隧道不被覆盖；
//   - 子隧道重拉的父链路闸门（parentAliveForRedeploy）四分支；
//   - Teardown 递归回收子隧道。
package tunnel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"chengjian.local/chengjian/db"
)

// childRec 造一条 ParentTunnelID 指向 parentID 的台账行。
func childRec(id, parentID int64, state string) *db.TunnelRecord {
	raw, _ := json.Marshal(Plan{Kind: KindSocks, SocksPort: 20000 + int(id), ParentTunnelID: parentID, HopDepth: 2})
	return &db.TunnelRecord{ID: id, TaskID: 9, Kind: KindSocks, Adapter: AdapterChisel,
		State: state, DeployParams: raw}
}

func TestCascadeBrokenMarksAliveChildrenOnly(t *testing.T) {
	stale := childRec(3, 1, db.TunnelError)
	stale.Error = "旧失败原因: 上次探测超时"
	s := &lifecycleStore{rows: map[int64]*db.TunnelRecord{
		1: childRec(1, 0, db.TunnelAlive), // 父（单跳）
		2: childRec(2, 1, db.TunnelAlive), // 子：应被级联标 error
		3: stale,                          // 子：已是 error，原因不能被覆盖
		4: childRec(4, 1, db.TunnelDeploying),
		5: childRec(5, 2, db.TunnelAlive), // 孙：本轮不处理（由下一轮巡检接力）
	}}
	m := &Manager{store: s}
	m.cascadeBroken(context.Background(), 1, "平台 server 进程已退出")

	if r := s.rows[2]; r.State != db.TunnelError || !strings.Contains(r.Error, "级联") || !strings.Contains(r.Error, "父隧道 #1") {
		t.Fatalf("alive 子隧道未被级联标记: state=%s error=%q", r.State, r.Error)
	}
	if r := s.rows[3]; r.State != db.TunnelError || !strings.Contains(r.Error, "旧失败原因") {
		t.Fatalf("已 error 的子隧道原因被覆盖: state=%s error=%q", r.State, r.Error)
	}
	if r := s.rows[4]; r.State != db.TunnelDeploying {
		t.Fatalf("deploying 子隧道被抢跑标记: state=%s", r.State)
	}
	if r := s.rows[5]; r.State != db.TunnelAlive {
		t.Fatalf("孙隧道不应在本轮被标记（由下一轮巡检接力）: state=%s", r.State)
	}
}

func TestCascadeBrokenEmitsLifecyclePerChild(t *testing.T) {
	s := &lifecycleStore{rows: map[int64]*db.TunnelRecord{
		1: childRec(1, 0, db.TunnelAlive),
		2: childRec(2, 1, db.TunnelAlive),
		3: childRec(3, 1, db.TunnelAlive),
	}}
	m := &Manager{store: s}
	var events []LifecycleEvent
	m.OnLifecycle = func(ev LifecycleEvent) { events = append(events, ev) }
	m.cascadeBroken(context.Background(), 1, "入口不可连")

	if len(events) != 2 {
		t.Fatalf("应为每条被标记的子隧道各派发一个事件，got %d", len(events))
	}
	for _, ev := range events {
		if ev.Kind != LifecycleBroken || ev.TunnelID == 1 {
			t.Fatalf("事件应为子隧道的 broken，got %+v", ev)
		}
	}
}

func TestParentAliveForRedeployGate(t *testing.T) {
	cases := []struct {
		name    string
		parent  *db.TunnelRecord // nil = 台账里没有这一行
		wantErr string
	}{
		{"父不存在", nil, "已不存在"},
		{"父已回收", &db.TunnelRecord{ID: 1, State: db.TunnelStopped}, "已不存在"},
		{"父断链中", &db.TunnelRecord{ID: 1, State: db.TunnelError}, "等父链路恢复"},
		{"父 alive", &db.TunnelRecord{ID: 1, State: db.TunnelAlive}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := map[int64]*db.TunnelRecord{2: childRec(2, 1, db.TunnelError)}
			if c.parent != nil {
				rows[1] = c.parent
			}
			m := &Manager{store: &lifecycleStore{rows: rows}}
			var p Plan
			if err := json.Unmarshal(rows[2].DeployParams, &p); err != nil {
				t.Fatal(err)
			}
			err := m.parentAliveForRedeploy(context.Background(), &p)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("父 alive 时应放行，got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestTeardownCascadesChildren(t *testing.T) {
	// 父(1) ← 子(2) ← 孙(3)：拆父必须把整条链都拆掉（依赖逆序）。
	s := &lifecycleStore{rows: map[int64]*db.TunnelRecord{
		1: childRec(1, 0, db.TunnelAlive),
		2: childRec(2, 1, db.TunnelAlive),
		3: childRec(3, 2, db.TunnelAlive),
	}}
	m := &Manager{store: s}
	if err := m.TeardownCascade(context.Background(), 1); err != nil {
		t.Fatalf("TeardownCascade 父隧道失败: %v", err)
	}
	for _, id := range []int64{1, 2, 3} {
		if r := s.rows[id]; r.State != db.TunnelStopped {
			t.Fatalf("隧道 %d 未被级联回收: state=%s", id, r.State)
		}
	}
}

func TestTeardownCascadeIsIdempotent(t *testing.T) {
	s := &lifecycleStore{rows: map[int64]*db.TunnelRecord{
		1: childRec(1, 0, db.TunnelAlive),
		2: childRec(2, 1, db.TunnelStopped), // 子已回收：不应再被碰
	}}
	m := &Manager{store: s}
	if err := m.TeardownCascade(context.Background(), 1); err != nil {
		t.Fatalf("TeardownCascade 失败: %v", err)
	}
	if r := s.rows[2]; r.State != db.TunnelStopped {
		t.Fatalf("已 stopped 的子隧道被翻动: state=%s", r.State)
	}
	if r := s.rows[1]; r.State != db.TunnelStopped {
		t.Fatalf("父隧道未回收: state=%s", r.State)
	}
}

func TestCleanupRecoversMultiHopChain(t *testing.T) {
	// Cleanup 走逆 id 序（子先父后——父必须先于子部署，故子 id 更大）：
	// 任务收尾时整条多级链都被回收，不留悬空远端进程。
	s := &lifecycleStore{rows: map[int64]*db.TunnelRecord{
		1: childRec(1, 0, db.TunnelAlive),
		2: childRec(2, 1, db.TunnelAlive),
		3: childRec(3, 2, db.TunnelAlive),
	}}
	m := &Manager{store: s}
	m.Cleanup(context.Background(), 9)
	for _, id := range []int64{1, 2, 3} {
		if r := s.rows[id]; r.State != db.TunnelStopped {
			t.Fatalf("隧道 %d 未被 Cleanup 回收: state=%s", id, r.State)
		}
	}
}
