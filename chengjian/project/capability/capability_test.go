package capability

import (
	"context"
	"testing"
	"time"

	"chengjian.local/chengjian/session"
)

// capSession 是可控的能力/平台桩。所有方法除 Capabilities/HostPlatform 外
// 都 panic：合同推导绝不允许顺带探目标。
type capSession struct {
	driver string
	plat   string
	caps   func(*session.Capabilities)
}

func (*capSession) ID() int64                  { return 1 }
func (*capSession) Kind() string               { panic("Kind called") }
func (*capSession) Test(context.Context) error { panic("Test called") }
func (*capSession) Exec(context.Context, string, time.Duration) (string, string, error) {
	panic("Exec called")
}
func (*capSession) ReadFile(context.Context, string) ([]byte, error) { panic("ReadFile called") }
func (*capSession) WriteFile(context.Context, string, []byte) error  { panic("WriteFile called") }
func (*capSession) Close(context.Context) error                      { return nil }

func (s *capSession) Capabilities() session.Capabilities {
	c := session.UnknownCapabilities()
	c.Driver = s.driver
	if s.caps != nil {
		s.caps(&c)
	}
	return c
}

func (s *capSession) HostPlatform() string { return s.plat }

func TestForReadsDriverFactsWithoutTouchingTarget(t *testing.T) {
	s := &capSession{driver: "http_shell", plat: "windows", caps: func(c *session.Capabilities) {
		c.Operations.Exec.DriverSupport = session.CapabilitySupported
		c.Operations.ReadFile.DriverSupport = session.CapabilitySupported
		c.Execution.ExitCode = session.CapabilityUnsupported
		c.Execution.OutputStreams = session.OutputStreamsMerged
	}}
	ct := For(s)
	if ct.Driver != "http_shell" || ct.Platform != session.PlatformWindows || !ct.PlatformKnown {
		t.Fatalf("合同基本字段不符: %+v", ct)
	}
	if ct.ExitCode != session.CapabilityUnsupported {
		t.Errorf("ExitCode 应保留 unsupported，得到 %q", ct.ExitCode)
	}
	if ct.SeparateStreams {
		t.Error("merged 流不应被读成 separate")
	}
	// detach 恒为 unknown：不能拿「能杀远端进程」冒充「能后台常驻」。
	if ct.CanDetach != session.CapabilityUnknown {
		t.Errorf("CanDetach 必须恒为 unknown，得到 %q", ct.CanDetach)
	}
}

// 未知项必须被列出来，否则调用方会把沉默当保证。
func TestForListsUnverifiedFields(t *testing.T) {
	ct := For(&capSession{driver: "legacy"}) // 全 unknown，平台也未知
	want := map[string]bool{
		"exec": true, "read_file": true, "write_file": true,
		"exit_code": true, "persistent_jobs": true, "detach": true, "platform": true,
	}
	got := map[string]bool{}
	for _, u := range ct.Unverified {
		got[u] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("Unverified 缺少 %q：%v", k, ct.Unverified)
		}
	}
	if ct.PlatformKnown {
		t.Error("空平台不应被当成已知")
	}
}

// 已声明的项不应出现在 Unverified 里——否则「未知」这个标记会失去意义。
func TestForDeclaredFieldsAreNotUnverified(t *testing.T) {
	ct := For(&capSession{driver: "d", plat: "linux", caps: func(c *session.Capabilities) {
		c.Operations.Exec.DriverSupport = session.CapabilitySupported
		c.Execution.ExitCode = session.CapabilityUnsupported
	}})
	for _, u := range ct.Unverified {
		if u == "exec" || u == "exit_code" || u == "platform" {
			t.Fatalf("已声明的 %q 不应出现在 Unverified：%v", u, ct.Unverified)
		}
	}
}

// 只有「明确不支持」才拦。unknown 一律放行——把未知当缺失会让所有还没声明
// 能力的驱动（当前是大多数）直接不可用。
func TestCheckOnlyBlocksExplicitUnsupported(t *testing.T) {
	unsupported := For(&capSession{driver: "d", caps: func(c *session.Capabilities) {
		c.Operations.Exec.DriverSupport = session.CapabilityUnsupported
	}})
	if ok, reason := unsupported.Check(OpExec); ok || reason == "" {
		t.Fatalf("明确不支持应拒绝并给原因，得到 ok=%v reason=%q", ok, reason)
	}
	// 未声明的动作仍是 unknown → 放行。
	if ok, _ := unsupported.Check(OpRead); !ok {
		t.Error("unknown 的 read 不应被拦")
	}

	unknown := For(&capSession{driver: "d"})
	for _, op := range []Op{OpExec, OpRead, OpWrite} {
		if ok, _ := unknown.Check(op); !ok {
			t.Errorf("全 unknown 合同不应拦 %s", op)
		}
	}
	if ok, _ := (Contract{}).Check(Op("bogus")); !ok {
		t.Error("未知动作不应被拦")
	}
}

func TestCheckCommandWindows(t *testing.T) {
	win := For(&capSession{driver: "d", plat: "windows"})
	for _, cmd := range []string{"id", "uname -a", "ls -la /tmp", "cat /etc/passwd", "chmod +x a", "ps aux", "ifconfig -a"} {
		if v, reason := CheckCommand(win, cmd); v != VerdictMismatch || reason == "" {
			t.Errorf("windows 下 %q 应判方言不匹配，得到 %v %q", cmd, v, reason)
		}
	}
	// 两边都有、或有别名的命令绝不能拦。
	for _, cmd := range []string{"dir", "type C:\\x", "whoami", "netstat -ano", "curl http://x", "ipconfig /all", "cmd /c ver", "powershell -c 1"} {
		if v, _ := CheckCommand(win, cmd); v != VerdictOK {
			t.Errorf("windows 下 %q 不应被判不匹配", cmd)
		}
	}
}

func TestCheckCommandLinux(t *testing.T) {
	lin := For(&capSession{driver: "d", plat: "linux"})
	for _, cmd := range []string{"ipconfig /all", "icacls C:\\x", "certutil -urlcache", "powershell -c 1", "tasklist", "findstr x", "schtasks /query", "reg query HKLM"} {
		if v, _ := CheckCommand(lin, cmd); v != VerdictMismatch {
			t.Errorf("linux 下 %q 应判方言不匹配", cmd)
		}
	}
	for _, cmd := range []string{"id", "ls -la", "cat /etc/passwd", "ps aux", "ip addr", "grep -r x ."} {
		if v, _ := CheckCommand(lin, cmd); v != VerdictOK {
			t.Errorf("linux 下 %q 不应被判不匹配", cmd)
		}
	}
}

// 平台未知时任何方言判断都不成立：必须放行，不能猜。
func TestCheckCommandUnknownPlatformNeverBlocks(t *testing.T) {
	ct := For(&capSession{driver: "reverse"}) // 无 HostPlatform
	for _, cmd := range []string{"id", "ipconfig /all", "icacls x", "ls"} {
		if v, _ := CheckCommand(ct, cmd); v != VerdictOK {
			t.Errorf("平台未知时不应拦 %q", cmd)
		}
	}
}

func TestFirstToken(t *testing.T) {
	cases := map[string]string{
		"ls -la":                    "ls",
		"  id  ":                    "id",
		"/bin/ls -l":                "ls",
		"FOO=1 ls":                  "ls",
		"FOO=1 BAR=2 id":            "id",
		"env FOO=1 uname -a":        "uname",
		"nohup cat /etc/passwd":     "cat",
		"time ps aux":               "ps",
		"command ls":                "ls",
		"LS -la":                    "ls",
		"ICACLS C:\\x":              "icacls",
		"":                          "",
		"; ls":                      "",
		"&& id":                     "",
		"1=2 ls":                    "ls", // 数字开头的也算赋值形态（不影响判定）
		"powershell -c \"id\"":      "powershell",
		"bash -c \"ls\"":            "bash",
		"wmic process list brief":   "wmic",
		"  CMD /c ver":              "cmd",
		"certutil -urlcache -f u o": "certutil",
	}
	for in, want := range cases {
		if got := firstToken(in); got != want {
			t.Errorf("firstToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// 包装命令（bash -c "ls"）必须放行：那是有意显式换方言，不是错误。
func TestCheckCommandAllowsExplicitWrapper(t *testing.T) {
	win := For(&capSession{driver: "d", plat: "windows"})
	if v, _ := CheckCommand(win, `bash -c "ls -la"`); v != VerdictOK {
		t.Error("显式 bash -c 包装不应被拦")
	}
	if v, _ := CheckCommand(win, `wsl ls -la`); v != VerdictOK {
		t.Error("显式 wsl 包装不应被拦")
	}
}
