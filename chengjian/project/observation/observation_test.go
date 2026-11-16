package observation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPromoteOnlyUpgradesAndNeverSkipsObservation 钉住两条不能松的规则：
//   - 只升不降（重测未复现不该抹掉已有的强证据）
//   - inferred 不能直接变 verified（推断确认推断 ≠ 有人真的连过）
func TestPromoteOnlyUpgradesAndNeverSkipsObservation(t *testing.T) {
	cases := []struct {
		cur, want, got Evidence
		changed       bool
		why           string
	}{
		{EvidenceInferred, EvidenceObserved, EvidenceObserved, true, "推断被实测取代"},
		{EvidenceObserved, EvidenceVerified, EvidenceVerified, true, "实测被独立探测确认"},
		{EvidenceVerified, EvidenceObserved, EvidenceVerified, false, "降级不允许"},
		{EvidenceObserved, EvidenceInferred, EvidenceObserved, false, "推断不能覆盖实测"},
		{EvidenceInferred, EvidenceVerified, EvidenceInferred, false, "跳过 observed 的验证不允许"},
		{EvidenceVerified, EvidenceVerified, EvidenceVerified, false, "同级无变化"},
		{Evidence("bogus"), EvidenceObserved, Evidence("bogus"), false, "未知等级拒绝参与比较"},
	}
	for _, c := range cases {
		got, changed := Promote(c.cur, c.want)
		if got != c.got || changed != c.changed {
			t.Fatalf("%s: Promote(%q,%q) = (%q,%v)，期望 (%q,%v)",
				c.why, c.cur, c.want, got, changed, c.got, c.changed)
		}
	}
}

// TestListenerScopeSeparatesLoopbackFromRemote 是 I3 验收里最硬的一条：
// 「loopback-only 服务不会变成远程可达」。判错的代价是把一个只在本机可用的
// 端口当成远程入口，于是 planner 派一条注定失败的横向意图。
func TestListenerScopeSeparatesLoopbackFromRemote(t *testing.T) {
	cases := []struct {
		addr    string
		scope   ListenerScope
		remote  bool
	}{
		{"127.0.0.1", ScopeLocalOnly, false},
		{"127.0.0.1:445", ScopeLocalOnly, false},
		{"[::1]:445", ScopeLocalOnly, false},
		{"::1", ScopeLocalOnly, false},
		{"localhost", ScopeLocalOnly, false},
		{"0.0.0.0", ScopeAnyInterface, true},
		{"*", ScopeAnyInterface, true},
		{"::", ScopeAnyInterface, true},
		{"[::]", ScopeAnyInterface, true},
		{"10.1.2.3", ScopeHostOnly, true},
		{"10.1.2.3:445", ScopeHostOnly, true},
		{"", ScopeUnknown, false},
		{"not-an-address", ScopeUnknown, false},
	}
	for _, c := range cases {
		if got := ClassifyListener(c.addr); got != c.scope {
			t.Fatalf("ClassifyListener(%q) = %q，期望 %q", c.addr, got, c.scope)
		}
		if got := RemoteReachable(c.addr); got != c.remote {
			t.Fatalf("RemoteReachable(%q) = %v，期望 %v", c.addr, got, c.remote)
		}
	}
}

// TestUnknownListenerScopeDoesNotDefaultToReachable 单独钉住「认不出就别猜」。
// 猜成 any_interface 会造出一个假入口；猜成 local_only 会漏掉真入口。
// 两种猜测都是编造，所以第三种答案是 unknown。
func TestUnknownListenerScopeDoesNotDefaultToReachable(t *testing.T) {
	if RemoteReachable("weird-binding-spec") {
		t.Fatal("无法识别的绑定地址不能被当成远程可达")
	}
	if ClassifyListener("weird-binding-spec") != ScopeUnknown {
		t.Fatal("无法识别的绑定地址必须报 unknown，不能回落成任何具体范围")
	}
}

// TestEdgeExpiryDiffersByKind 钉住「不同事实的新鲜度不同」。
// 全部用同一个 TTL 会让 ARP 表看起来和网卡配置一样新鲜。
func TestEdgeExpiryDiffersByKind(t *testing.T) {
	now := time.Now()
	neighbor := Edge{Kind: EdgeNeighbor}.WithTTL(now)
	nic := Edge{Kind: EdgeNIC}.WithTTL(now)
	if !neighbor.Fresh(now.Add(time.Minute)) {
		t.Fatal("刚观测的邻居边应新鲜")
	}
	if neighbor.Fresh(now.Add(time.Hour)) {
		t.Fatal("一小时前的邻居边必须过期（ARP 表几分钟就变）")
	}
	if !nic.Fresh(now.Add(time.Hour)) {
		t.Fatal("一小时前的网卡边应仍新鲜（网卡配置变化慢）")
	}
	// 没有有效期的边不能当新鲜事实——零值 ValidUntil 是「没人算过」，不是「永久有效」。
	if !(Edge{Kind: EdgeNIC}).Stale(now) {
		t.Fatal("缺有效期的边必须视为过期")
	}
}

func TestEdgeValidateRejectsUnsourcedEdges(t *testing.T) {
	ok := Edge{TaskID: 1, ObservationID: 9, Kind: EdgeListener, From: "10.0.0.5", To: "10.0.0.5:445",
		Evidence: EvidenceObserved}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法边被拒: %v", err)
	}
	noSource := ok
	noSource.ObservationID = 0
	if err := noSource.Validate(); err == nil {
		t.Fatal("没有来源观测的边必须拒绝（无法复核）")
	}
	badKind := ok
	badKind.Kind = "guess"
	if err := badKind.Validate(); err == nil {
		t.Fatal("未知边类型必须拒绝")
	}
	badEvidence := ok
	badEvidence.Evidence = "pretty-sure"
	if err := badEvidence.Validate(); err == nil {
		t.Fatal("未知证据等级必须拒绝（自由文本正是要修的问题）")
	}
}

func TestObservationValidateRequiresReadableArtifact(t *testing.T) {
	base := Observation{TaskID: 1, Kind: "recon:nics", ArtifactRef: "obs/ab/abc.txt",
		ArtifactHash: "abc", Confidence: QualityComplete}
	if err := base.Validate(); err != nil {
		t.Fatalf("合法观测被拒: %v", err)
	}
	noRef := base
	noRef.ArtifactRef = ""
	if err := noRef.Validate(); err == nil {
		t.Fatal("没有 artifact_ref 的观测必须拒绝：hash 不能替代可读回的原文")
	}
	noQuality := base
	noQuality.Confidence = ""
	if err := noQuality.Validate(); err == nil {
		t.Fatal("缺少采集质量的观测必须拒绝")
	}
}

func TestDedupeKeepsStrongestEvidence(t *testing.T) {
	in := []Edge{
		{Kind: EdgeListener, From: "10.0.0.5", To: "10.0.0.5:445", Evidence: EvidenceObserved},
		{Kind: EdgeListener, From: "10.0.0.5", To: "10.0.0.5:445", Evidence: EvidenceVerified},
		{Kind: EdgeListener, From: "10.0.0.5", To: "10.0.0.5:22", Evidence: EvidenceInferred},
		{Kind: EdgeNeighbor, From: "10.0.0.5", To: "10.0.0.9", Evidence: EvidenceObserved},
	}
	out := Dedupe(in)
	if len(out) != 3 {
		t.Fatalf("应去重成 3 条，实际 %d", len(out))
	}
	if out[0].Kind != EdgeListener || out[0].To != "10.0.0.5:22" {
		t.Fatalf("排序应按 kind→from→to，实际首条 %+v", out[0])
	}
	var verified int
	for _, e := range out {
		if e.To == "10.0.0.5:445" {
			if e.Evidence != EvidenceVerified {
				t.Fatalf("同一条边应保留最强证据 verified，实际 %q", e.Evidence)
			}
			verified++
		}
	}
	if verified != 1 {
		t.Fatalf("445 那条边应只剩 1 条，实际 %d", verified)
	}
}

func TestArtifactRoundTripAndTamperDetection(t *testing.T) {
	dir := t.TempDir()
	store := NewArtifactStore(dir)
	content := []byte("eth0: 10.0.0.5/24\n")

	ref, hash, size, err := store.Put(content)
	if err != nil {
		t.Fatalf("写入原文失败: %v", err)
	}
	if size != int64(len(content)) {
		t.Fatalf("字节数不符: %d != %d", size, len(content))
	}
	got, verified, err := store.Read(ref, hash)
	if err != nil {
		t.Fatalf("读回原文失败: %v", err)
	}
	if !verified {
		t.Fatal("带 hash 读回必须校验通过")
	}
	if string(got) != string(content) {
		t.Fatalf("读回内容不一致: %q", got)
	}

	// 内容寻址：同样的内容第二次写入复用同一份文件。
	ref2, hash2, _, err := store.Put(content)
	if err != nil || ref2 != ref || hash2 != hash {
		t.Fatalf("相同内容应复用同一 ref: %q/%q vs %q/%q (%v)", ref2, hash2, ref, hash, err)
	}

	// 篡改后读回必须报错，不能把「内容已经不是当时那段」当成证据返回。
	path := filepath.Join(dir, filepath.FromSlash(ref))
	if err := os.WriteFile(path, []byte("eth0: 10.0.0.6/24\n"), 0o600); err != nil {
		t.Fatalf("篡改原文失败: %v", err)
	}
	if _, _, err := store.Read(ref, hash); err == nil {
		t.Fatal("内容与 hash 不一致时必须报错")
	} else if !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("错误信息应说明是校验失败: %v", err)
	}
}

func TestArtifactMissingIsNotEmpty(t *testing.T) {
	store := NewArtifactStore(t.TempDir())
	_, _, err := store.Read("obs/ff/deadbeef.txt", "deadbeef")
	if err == nil {
		t.Fatal("原文缺失必须报错")
	}
	// 关键区分：缺失 ≠ 空输出。把缺失读成空会让「证据被清理」变成
	// 「目标上什么都没有」这条假事实。
	if !strings.Contains(err.Error(), "不能当成空输出") {
		t.Fatalf("错误信息必须点明缺失不等于空: %v", err)
	}
}

func TestArtifactRejectsPathTraversal(t *testing.T) {
	store := NewArtifactStore(t.TempDir())
	for _, ref := range []string{"../etc/passwd", "/etc/passwd", "obs/../../secret"} {
		if _, _, err := store.Read(ref, ""); err == nil {
			t.Fatalf("非法 ref 必须拒绝: %q", ref)
		}
	}
}

func TestArtifactEmptyContentIsStillEvidence(t *testing.T) {
	store := NewArtifactStore(t.TempDir())
	ref, hash, size, err := store.Put(nil)
	if err != nil {
		t.Fatalf("空输出写入失败: %v", err)
	}
	if size != 0 {
		t.Fatalf("空输出字节数应为 0，实际 %d", size)
	}
	got, verified, err := store.Read(ref, hash)
	if err != nil || !verified {
		t.Fatalf("空输出应可读回且校验通过: %v / %v", err, verified)
	}
	if len(got) != 0 {
		t.Fatalf("空输出读回应为空，实际 %q", got)
	}
}
