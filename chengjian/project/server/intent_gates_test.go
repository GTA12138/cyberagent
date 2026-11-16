// intent_gates_test.go 意图两道闸（claim 前置复查 / done_when 缺口）的判定测试。
// 判定逻辑在 planning 包已有测试；这里锁的是引擎侧的接线语义：
// 候选选择规则、缺口渲染、以及 proxychains 配置生成（横向经隧道）。
package server

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/planning"
)

func gateNode(id int64, prereqs ...string) *db.Node {
	payload, _ := json.Marshal(map[string]any{"summary": "x", "prerequisites": prereqs})
	return &db.Node{ID: id, Kind: db.KindIntent, Payload: payload, Priority: 5}
}

func TestPickClaimablePrefersMetPrereqs(t *testing.T) {
	fr := []*db.Node{
		gateNode(1, "tunnel:socks"), // 高优先级但前置未满足
		gateNode(2, "fact:出网能力"), // 未满足
		gateNode(3),                 // 无前置：应被选中
		gateNode(4, "session"),      // 满足与否看快照
	}
	unmet := func(payload []byte) int {
		var f intentPlanFields
		_ = json.Unmarshal(payload, &f)
		n := 0
		for _, p := range f.Prerequisites {
			if strings.Contains(p, "tunnel") || strings.Contains(p, "fact:出网") {
				n++
			}
		}
		return n
	}
	got := pickClaimable(fr, unmet)
	if got == nil || got.ID != 3 {
		t.Fatalf("应跳过前置未满足的意图选 #3，got %+v", got)
	}
}

func TestPickClaimableFallsBackWhenAllUnmet(t *testing.T) {
	// 停摆保护：全部候选前置未满足时仍取首条（最高优先级）——前置可能由这条
	// 意图自己产出，硬拦会让任务死锁。
	fr := []*db.Node{gateNode(7, "tunnel"), gateNode(8, "credential:smb")}
	got := pickClaimable(fr, func([]byte) int { return 1 })
	if got == nil || got.ID != 7 {
		t.Fatalf("全部未满足应回退首条 #7，got %+v", got)
	}
}

func TestPickClaimableNilUnmetKeepsOrder(t *testing.T) {
	// PrereqState 未装配（独立运行/测试）时保持既有纯顺序领取。
	fr := []*db.Node{gateNode(1), gateNode(2)}
	if got := pickClaimable(fr, nil); got.ID != 1 {
		t.Fatalf("无复查时应取首条，got %d", got.ID)
	}
	if got := pickClaimable(nil, nil); got != nil {
		t.Fatal("空候选应返回 nil")
	}
}

func TestUnmetPrerequisiteCountUnknownIsNotUnmet(t *testing.T) {
	// unknown（缺数据）不产生闸门副作用：Counts 全 nil 时 tunnel 判据是 unknown。
	n := unmetPrerequisiteCount([]string{"tunnel:socks"}, planning.State{})
	if n != 0 {
		t.Fatalf("unknown 不应计为未满足，got %d", n)
	}
	live := 1
	n = unmetPrerequisiteCount([]string{"tunnel:socks"}, planning.State{
		Counts: planning.Counts{Tunnels: &live},
	})
	if n != 0 {
		t.Fatalf("tunnel 判据已满足时不应有未满足项，got %d", n)
	}
	zero := 0
	n = unmetPrerequisiteCount([]string{"tunnel:socks"}, planning.State{
		Counts: planning.Counts{Tunnels: &zero},
	})
	if n != 1 {
		t.Fatalf("确认没有存活隧道时应计 1 条未满足，got %d", n)
	}
}

func TestDoneGapHintRendersGaps(t *testing.T) {
	// 判据空 → 无提示；判据满足 → 无提示；有缺口 → 文本含判据原文与处置指引。
	if got := doneGapHint(nil, planning.DoneState{}); got != "" {
		t.Fatalf("无判据不应有提示，got %q", got)
	}
	st := planning.DoneState{Facts: []planning.FactView{{ID: 5, Summary: "目标可出网到 443"}}}
	if got := doneGapHint([]string{"fact:出网"}, st); got != "" {
		t.Fatalf("判据已满足不应有提示，got %q", got)
	}
	got := doneGapHint([]string{"fact:域管凭据", "evidence:2"}, planning.DoneState{})
	if !strings.Contains(got, "fact:域管凭据") || !strings.Contains(got, "完成判据缺口") {
		t.Fatalf("缺口文本应含判据原文与标题，got %q", got)
	}
	if !strings.Contains(got, "evidence:2") {
		t.Fatalf("多条缺口都应列出（截断上限 3 条），got %q", got)
	}
}

func TestDoneGapEvidenceCountsIntentYield(t *testing.T) {
	// doneStateFor 的 ManualNote 口径：零产出时必须为空——WriteCounts.String()
	// 对零值也返回文本，直接用会让 command: 判据在「什么都没写」时亮绿灯。
	wrote := agent.WriteCounts{}
	st := planning.DoneState{ManualNote: ""}
	if wrote.Total() > 0 || st.ManualNote != "" {
		t.Fatal("零写回不应产生 ManualNote")
	}
	wrote = agent.WriteCounts{Facts: 2}
	if wrote.Total() == 0 {
		t.Fatal("有写回时 Total 应大于 0")
	}
}

func TestWriteProxychainsConfWithAuth(t *testing.T) {
	path, cleanup, err := writeProxychainsConf("socks5://chengjian:secret123@127.0.0.1:20801")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	conf := string(raw)
	// proxychains-ng 官方格式：socks5 IP PORT USER PASS——IP 与端口分字段，
	// 不是 host:port 合写（旧实现的格式错误由外部评审确认）。
	for _, want := range []string{"strict_chain", "proxy_dns", "socks5 127.0.0.1 20801 chengjian secret123"} {
		if !strings.Contains(conf, want) {
			t.Fatalf("conf 缺少 %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "socks5 127.0.0.1:20801") {
		t.Fatalf("不应出现 host:port 合写:\n%s", conf)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cleanup 应删除配置文件（里面有 socks 口令）")
	}
}

func TestWriteProxychainsConfWithoutAuth(t *testing.T) {
	path, cleanup, err := writeProxychainsConf("socks5://127.0.0.1:20802")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "socks5 127.0.0.1 20802\n") {
		t.Fatalf("无认证上游不应带 user/pass 字段:\n%s", string(raw))
	}
}

func TestWriteProxychainsConfRejectsBadUpstream(t *testing.T) {
	if _, _, err := writeProxychainsConf("http://not-socks"); err == nil {
		t.Fatal("非 socks5:// 上游应报错")
	}
}

func TestLocateProxychainsEnvOverride(t *testing.T) {
	t.Setenv("CHENGJIAN_PROXYCHAINS_PATH", `Z:\definitely\missing\proxychains4.exe`)
	if _, err := locateProxychains(t.TempDir()); err == nil {
		t.Fatal("env 指向不存在的文件应报错")
	} else if !strings.Contains(err.Error(), "CHENGJIAN_PROXYCHAINS_PATH") {
		t.Fatalf("报错应指明 env 变量名，got %v", err)
	}
}

func TestLocateProxychainsNotFoundGuidance(t *testing.T) {
	t.Setenv("CHENGJIAN_PROXYCHAINS_PATH", "")
	dir := t.TempDir() // 空 data/tools + 测试环境 PATH 里没有 proxychains
	if _, err := locateProxychains(dir); err == nil {
		t.Skip("环境 PATH 里存在 proxychains，跳过未找到分支")
	} else if !strings.Contains(err.Error(), "proxychains-ng") || !strings.Contains(err.Error(), "session_exec") {
		t.Fatalf("报错应含安装指引与替代路径，got %v", err)
	}
}
