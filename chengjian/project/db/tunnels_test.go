package db

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// tunnels 表 CRUD 需要真实 dev PG(CHENGJIAN_PG_DSN);无 DSN 时按包约定 skip。
func TestTunnelStoreCRUD(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("dev PG 不可用: %v", err)
	}
	defer d.Close()
	ctx := context.Background()
	s := NewTunnelStore(d)

	params, _ := json.Marshal(map[string]any{
		"auth_user": "chengjian", "auth_pass": "deadbeef", "server_pid": 1234,
		"socks_port": 20002, "stage_token": "tok",
	})
	id, err := s.Create(ctx, &TunnelRecord{
		TaskID: 999001, Kind: "socks", ListenHost: "0.0.0.0", ListenPort: 20001,
		ViaSessionID: 3, State: TunnelDeploying, DeployParams: params,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _, _ = d.Exec(`DELETE FROM tunnels WHERE task_id=999001`) }()

	rec, err := s.Get(ctx, id)
	if err != nil || rec == nil {
		t.Fatalf("Get: %v rec=%v", err, rec)
	}
	if rec.State != TunnelDeploying || rec.ListenPort != 20001 || rec.Adapter != "chisel" {
		t.Errorf("行内容不符: %+v", rec)
	}
	var p map[string]any
	if err := json.Unmarshal(rec.DeployParams, &p); err != nil || p["auth_pass"] != "deadbeef" {
		t.Errorf("deploy_params 往返失败: %v %v", err, p)
	}

	// 状态机与 last_check 刷新
	if err := s.UpdateState(ctx, id, TunnelAlive, ""); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	if err := s.UpdateState(ctx, id, "bogus", ""); err == nil {
		t.Error("非法状态应报错")
	}
	alive, err := s.ListAlive(ctx)
	if err != nil {
		t.Fatalf("ListAlive: %v", err)
	}
	found := false
	for _, r := range alive {
		if r.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("ListAlive 未含新隧道")
	}

	// ListByTask
	byTask, err := s.ListByTask(ctx, 999001)
	if err != nil || len(byTask) != 1 {
		t.Errorf("ListByTask = %d 行, %v", len(byTask), err)
	}

	// UpdateDeployParams 回填 remote_pid
	params2, _ := json.Marshal(map[string]any{"remote_pid": 4321})
	if err := s.UpdateDeployParams(ctx, id, 4321, params2); err != nil {
		t.Fatalf("UpdateDeployParams: %v", err)
	}
	rec, _ = s.Get(ctx, id)
	if rec.RemotePID != 4321 {
		t.Errorf("RemotePID = %d, want 4321", rec.RemotePID)
	}

	// 非法 kind
	if _, err := s.Create(ctx, &TunnelRecord{TaskID: 999001, Kind: "gre"}); err == nil {
		t.Error("非法 kind 应报错")
	}

	// 依赖收尾：只列「经该会话部署且未终结」的隧道。已 stopped 的不算依赖，
	// 否则删会话时会对一条已经收干净的隧道重复清理。
	dep, err := s.Create(ctx, &TunnelRecord{
		TaskID: 999001, Kind: "socks", ListenHost: "127.0.0.1", ListenPort: 20002,
		ViaSessionID: 3, State: TunnelDeploying, DeployParams: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Create dependent: %v", err)
	}
	other, err := s.Create(ctx, &TunnelRecord{
		TaskID: 999001, Kind: "socks", ListenHost: "127.0.0.1", ListenPort: 20003,
		ViaSessionID: 4, State: TunnelDeploying, DeployParams: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Create other-session: %v", err)
	}
	deps, err := s.ListActiveByViaSession(ctx, 3)
	if err != nil {
		t.Fatalf("ListActiveByViaSession: %v", err)
	}
	seen := map[int64]bool{}
	for _, r := range deps {
		seen[r.ID] = true
	}
	if !seen[dep] {
		t.Error("应包含经会话 3 部署的未终结隧道")
	}
	if seen[other] {
		t.Error("不应包含经会话 4 部署的隧道")
	}
	if err := s.UpdateState(ctx, dep, TunnelStopped, ""); err != nil {
		t.Fatalf("UpdateState stopped: %v", err)
	}
	deps, err = s.ListActiveByViaSession(ctx, 3)
	if err != nil {
		t.Fatalf("ListActiveByViaSession after stop: %v", err)
	}
	for _, r := range deps {
		if r.ID == dep {
			t.Error("已 stopped 的隧道不应再算依赖")
		}
	}

	// Delete
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rec, _ = s.Get(ctx, id)
	if rec != nil {
		t.Error("Delete 后 Get 应为 nil")
	}
}

// TestTunnelReachDeclarationIsNotObservation 钉住 T-06 的核心区分：
// 写入「可达范围声明」不能把 reach_verified_at 点亮。声明与观测在输出里必须
// 可区分——否则 planner 会把「我以为能到 10.0.0.0/24」当成「已验证能到」。
func TestTunnelReachDeclarationIsNotObservation(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	ctx := t.Context()
	st := NewTunnelStore(d)
	id, err := st.Create(ctx, &TunnelRecord{TaskID: 900000777, Kind: "socks", State: TunnelDeploying})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM tunnels WHERE id=$1`, id) })

	rec, err := st.Get(ctx, id)
	if err != nil || rec == nil {
		t.Fatalf("get: %v %v", rec, err)
	}
	if string(rec.ReachableRanges) != "[]" || string(rec.DNSPolicy) != "{}" {
		t.Fatalf("defaults must be empty declaration: ranges=%s dns=%s", rec.ReachableRanges, rec.DNSPolicy)
	}
	if rec.ReachVerifiedAt != nil {
		t.Fatal("fresh tunnel must not look verified")
	}

	if err := st.SetReachDeclaration(ctx, id,
		json.RawMessage(`["10.0.0.0/24","172.16.5.0/24"]`),
		json.RawMessage(`{"mode":"target","servers":["10.0.0.53"]}`)); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.Get(ctx, id)
	if string(rec.ReachableRanges) == "[]" {
		t.Fatal("declaration not persisted")
	}
	if rec.ReachVerifiedAt != nil {
		t.Fatal("declaration must NOT set reach_verified_at — that field is for observations only")
	}

	// 只有真正探测过的调用方才能点亮它。
	if err := st.MarkReachVerified(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.Get(ctx, id)
	if rec.ReachVerifiedAt == nil {
		t.Fatal("MarkReachVerified must set the timestamp")
	}
	// 观测写进 reach_verified_ranges，**不能**覆盖 reachable_ranges：
	// 声明是「我以为」，实测是「我试过」，后者只覆盖探到的那一小块。
	if string(rec.ReachableRanges) == "[]" {
		t.Fatal("verifying must not erase the declaration — observation is a subset, not a replacement")
	}
	if string(rec.ReachVerifiedRanges) != "[]" {
		t.Fatalf("empty observation must not invent verified ranges, got %s", rec.ReachVerifiedRanges)
	}

	// 带实测范围时只写观测列。
	if err := st.MarkReachVerified(ctx, id, json.RawMessage(`["10.0.0.8/32"]`)); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.Get(ctx, id)
	if string(rec.ReachVerifiedRanges) != `["10.0.0.8/32"]` {
		t.Fatalf("observed ranges not persisted: %s", rec.ReachVerifiedRanges)
	}
	// 比较用语义而不是字节：JSONB 回读会把分隔符归一成 ", "，
	// 拿字节相等去断言等于把「PostgreSQL 的序列化格式」当成契约的一部分。
	if got := jsonList(rec.ReachableRanges); !slices.Equal(got, []string{"10.0.0.0/24", "172.16.5.0/24"}) {
		t.Fatalf("declaration must survive observation: %v", got)
	}
}

// jsonList 把 JSON 数组解成字符串切片，用于「语义相等」断言。
func jsonList(raw json.RawMessage) []string {
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}
