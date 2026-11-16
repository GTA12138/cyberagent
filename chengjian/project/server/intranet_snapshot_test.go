package server

import (
	"strings"
	"testing"
)

// intranetSummary 是 planner 决策的主要依据之一：它必须把「台账里有什么」
// 压成「现在能不能打」。这些用例锁定几条最关键的结论措辞。
func TestIntranetSummaryKeyConclusions(t *testing.T) {
	aliveTunnel := []map[string]any{{"id": 1, "state": "alive", "kind": "socks"}}
	deadTunnel := []map[string]any{{"id": 1, "state": "error", "kind": "socks", "error": "回连失败"}}
	aliveSess := []map[string]any{{"id": 1, "status": "alive", "kind": "http_php"}}
	deadSess := []map[string]any{{"id": 1, "status": "dead", "kind": "http_php"}}
	creds := map[string]any{"count": 3, "verified": 1, "by_type": map[string]int{"password": 3}}
	pending := []map[string]any{{"cidr": "10.0.0.0/24", "hits": 2}}

	cases := []struct {
		name     string
		sessions []map[string]any
		tunnels  []map[string]any
		creds    map[string]any
		pending  []map[string]any
		wantSub  []string
	}{
		{
			name:    "隧道 alive 不证明任意目标可达",
			tunnels: aliveTunnel, wantSub: []string{"台账状态 alive", "逐目标验证"},
		},
		{
			name:    "当前页无 alive 入口需核实完整台账",
			tunnels: deadTunnel, wantSub: []string{"无 alive 入口", "tunnel_list", "完整台账"},
		},
		{
			name:     "当前页 dead 不等于全部失效",
			sessions: deadSess, wantSub: []string{"当前展示", "session_list", "翻页核实"},
		},
		{
			name:  "有凭据提示复用",
			creds: creds, wantSub: []string{"凭据库有 3 条", "1 条已验证", "credential_reuse_plan"},
		},
		{
			name:    "有待授权网段必须点出禁止探测",
			pending: pending, wantSub: []string{"1 个网段待人工授权", "批准前不得"},
		},
		{
			name:     "存活会话",
			sessions: aliveSess, wantSub: []string{"1 个存活立足点会话"},
		},
		{
			// Windows 立足点必须改变 planner 派什么意图：命令语法不同。
			name: "Windows 立足点要说明方言",
			sessions: []map[string]any{
				{"id": 1, "status": "alive", "kind": "http_aspx", "platform": "windows"},
			},
			wantSub: []string{"1 个是 Windows 立足点", "cmd/PowerShell", "不要用 sh 语法"},
		},
		{
			// 没有 Windows 立足点时不得凭空出现这句，否则每轮都在花预算。
			name:     "纯 Linux 立足点不提方言",
			sessions: aliveSess, wantSub: []string{"1 个存活立足点会话"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := intranetSummary(c.sessions, c.tunnels, c.creds, c.pending)
			for _, sub := range c.wantSub {
				if !strings.Contains(got, sub) {
					t.Fatalf("summary=%q 缺少关键结论 %q", got, sub)
				}
			}
		})
	}
}

func TestIntranetSummaryEmpty(t *testing.T) {
	if got := intranetSummary(nil, nil, nil, nil); got != "内网作战尚无有效进展" {
		t.Fatalf("空态势应给明确结论, got %q", got)
	}
}

// 纯 Linux 立足点不得出现 Windows 那句：它在每轮 prompt 里，凭空多一句就是
// 纯粹的预算浪费，也会让 planner 误以为目标形态混杂。
func TestIntranetSummaryNoWindowsClauseForLinuxOnly(t *testing.T) {
	sessions := []map[string]any{{"id": 1, "status": "alive", "kind": "http_php", "platform": "linux"}}
	if got := intranetSummary(sessions, nil, nil, nil); strings.Contains(got, "Windows 立足点") {
		t.Fatalf("纯 Linux 立足点不应出现 Windows 提示, got %q", got)
	}
}

// hostOfURL 只给 host：webshell URL 的查询串可能带连接口令，不能进 prompt。
func TestHostOfURLDropsCredentialsAndQuery(t *testing.T) {
	cases := map[string]string{
		"http://10.0.0.5:8080/shell.php?pwd=secret": "10.0.0.5:8080",
		"https://a.example.com/x.php":               "a.example.com",
		"not a url":                                 "",
		"":                                          "",
	}
	for in, want := range cases {
		if got := hostOfURL(in); got != want {
			t.Errorf("hostOfURL(%q)=%q want %q", in, got, want)
		}
	}
	if strings.Contains(hostOfURL("http://h/x?pwd=secret"), "secret") {
		t.Fatal("host 里泄漏了查询串口令")
	}
}

func TestTruncateForSnapshot(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := truncateForSnapshot(long, 200)
	if len([]rune(got)) != 201 { // 200 + 省略号
		t.Fatalf("截断长度异常: %d", len([]rune(got)))
	}
	if got := truncateForSnapshot("short", 200); got != "short" {
		t.Fatalf("短文本不应截断, got %q", got)
	}
}
