// Package capability 把「会话能做什么」从**展示字段**变成**决策输入**。
//
// 为什么需要单独一层：`session.Capabilities` 已经如实描述了驱动实现的路径，
// 但它的全部消费者只有两处——会话列表和前端详情。也就是说能力描述只被
// 「显示」，从未被「据此做决定」。I2 要的是反过来：选命令包、选远端路径、
// 决定要不要发这条命令之前，先问能力。
//
// 本包只做两件事，都不碰目标：
//
//  1. Contract：把 session.Capabilities + 方言（驱动自报的平台）压成一份可
//     直接判读的合同。它区分 supported / unsupported / unknown 三态，且**不
//     把 unknown 当成 supported**——这是全仓反复踩过的坑。
//  2. CheckCommand：在发包之前判断「这条命令和这个方言对不对得上」。
//
// 边界（勿放宽）：
//   - 只读元数据，绝不 Test/Exec/探活。CheckCommand 是纯字符串判定。
//   - 只拦**高置信**的不匹配。宁可不拦，也不要误拦一条其实能跑的命令——误拦
//     会让 agent 换着花样重试，比让它看到目标返回的真实报错更糟。
//   - 不把「能力未知」渲染成「能力缺失」。未知只降低置信度，不改变是否放行。
package capability

import (
	"strings"

	"chengjian.local/chengjian/session"
)

// Op 是会被前置检查的动作。
type Op string

const (
	OpExec  Op = "exec"
	OpRead  Op = "read"
	OpWrite Op = "write"
)

// Contract 是一条会话当前可据以行动的结论。
//
// 三态语义与 session.CapabilityState 一致：
//   - Supported：驱动明确实现，且调用路径存在；
//   - Unsupported：驱动明确不支持——**前置检查应直接拒绝**，不要发包后再失败；
//   - Unknown：没人声明过。既不等于支持，也不等于不支持，前置检查放行但要在
//     结果里标注「未验证」。
type Contract struct {
	Driver   string
	Platform string // "linux" / "windows" / ""（未知）
	// PlatformKnown 为 false 时，任何按方言做的判断都不成立。
	PlatformKnown bool

	Exec      session.CapabilityState
	ReadFile  session.CapabilityState
	WriteFile session.CapabilityState

	// ExitCode：能否从返回值判断命令成败。Unsupported 时「无报错」不等于成功。
	ExitCode session.CapabilityState
	// SeparateStreams：stdout/stderr 是否分离。merged 时「stderr 为空」无意义。
	SeparateStreams bool
	// CanDetach：目标侧能否后台常驻（隧道/长跑作业的前提）。Unsupported 时
	// 不应承诺「进程还在跑」。
	CanDetach session.CapabilityState
	// PersistentJobs：是否有独立的作业回执/状态接口（不是 shell 的 & 语法）。
	PersistentJobs session.CapabilityState

	// Unverified 列出「描述为 unknown」的项，用于让调用方在结果里如实标注，
	// 而不是让 agent 把沉默当保证。
	Unverified []string
}

// For 由一条会话推出合同。s 为 nil 时返回全 unknown 的保守合同。
func For(s session.Session) Contract {
	if s == nil {
		return fromCapabilities(session.UnknownCapabilities(), "")
	}
	caps := session.DescribeCapabilities(s)
	plat := session.DescribePlatform(s)
	return fromCapabilities(caps, plat)
}

func fromCapabilities(caps session.Capabilities, platform string) Contract {
	c := Contract{
		Driver:          caps.Driver,
		Platform:        platform,
		PlatformKnown:   platform != "",
		Exec:            caps.Operations.Exec.DriverSupport,
		ReadFile:        caps.Operations.ReadFile.DriverSupport,
		WriteFile:       caps.Operations.WriteFile.DriverSupport,
		ExitCode:        caps.Execution.ExitCode,
		SeparateStreams: caps.Execution.OutputStreams == session.OutputStreamsSeparate,
		PersistentJobs:  caps.Execution.PersistentJobTracking,
	}
	// CanDetach 恒为 unknown，且刻意不拿 RemoteStopOnCancel 推导：
	// 「取消本地调用能停掉远端进程」与「进程能在目标侧脱离会话存活」是两个
	// 问题。能杀不等于能脱离；混用会让 agent 以为后台作业是可管理的。
	// 真正的 detach 证据只能来自目标侧观察（进程仍在、会话已断），不在这里。
	c.CanDetach = session.CapabilityUnknown

	c.Unverified = unverifiedOf(c)
	return c
}

func unverifiedOf(c Contract) []string {
	var out []string
	add := func(name string, st session.CapabilityState) {
		if st == session.CapabilityUnknown {
			out = append(out, name)
		}
	}
	add("exec", c.Exec)
	add("read_file", c.ReadFile)
	add("write_file", c.WriteFile)
	add("exit_code", c.ExitCode)
	add("persistent_jobs", c.PersistentJobs)
	add("detach", c.CanDetach)
	if !c.PlatformKnown {
		out = append(out, "platform")
	}
	return out
}

// Check 判断某动作是否被驱动明确拒绝。
//
// 返回 (allowed, reason)：
//   - Unsupported → false + 明确原因（调用方应直接返回 capability_missing，
//     不要发包，更不要把失败记成「目标不通」）；
//   - Supported / Unknown → true（Unknown 不拦，理由见包注释）。
func (c Contract) Check(op Op) (bool, string) {
	var st session.CapabilityState
	switch op {
	case OpExec:
		st = c.Exec
	case OpRead:
		st = c.ReadFile
	case OpWrite:
		st = c.WriteFile
	default:
		return true, ""
	}
	if st == session.CapabilityUnsupported {
		return false, "驱动 " + c.Driver + " 明确声明不支持 " + string(op) +
			"；这是能力缺失，不是目标侧故障，重试同一条命令不会成功"
	}
	return true, ""
}

// Verdict 是命令预检的结论。
type Verdict string

const (
	// VerdictOK：方言与命令相符，或无法判定（未知不拦）。
	VerdictOK Verdict = "ok"
	// VerdictMismatch：高置信不匹配——命令在对方言下必然不存在。
	VerdictMismatch Verdict = "dialect_mismatch"
)

// unixOnlyHeads 是「Windows cmd/PowerShell 下必然不存在」的高置信命令头。
//
// 只收**确凿**的：这些都是 Unix 专有工具，Windows 自带组件里没有同名命令。
// 刻意不收四类：
//   - `dir`/`type`/`whoami`/`netstat`/`curl`/`ping`：两边都有，或常有别名；
//   - `bash`/`sh`/`wsl`/`grep`/`sed`/`awk`：常随 Git Bash/WSL 一起出现，
//     而且 `bash -c "ls"` 正是我们要**推荐**的显式换方言写法；
//   - `service`：Windows 上也可能存在同名可执行文件。
//
// 误拦一条其实能跑的命令，比不拦更糟——那会让 agent 换着花样重试。
var unixOnlyHeads = map[string]bool{
	"id": true, "uname": true, "ls": true, "cat": true, "chmod": true,
	"chown": true, "ps": true, "ifconfig": true, "sudo": true, "systemctl": true,
	"apt": true, "apt-get": true, "yum": true, "dnf": true,
	"useradd": true, "passwd": true, "crontab": true, "ssh-keygen": true,
}

// windowsOnlyHeads 是「Linux/Unix sh 下必然不存在」的高置信命令头。
var windowsOnlyHeads = map[string]bool{
	"icacls": true, "certutil": true, "powershell": true, "pwsh": true,
	"wmic": true, "reg": true, "cmd": true, "cmd.exe": true,
	"tasklist": true, "taskkill": true, "ipconfig": true, "findstr": true,
	"xcopy": true, "robocopy": true, "schtasks": true, "net1": true,
}

// shellWrappers 是「显式切换解释器」的包装。出现它们说明调用方**有意**换方言，
// 预检必须放行——这正是我们在不匹配提示里推荐的写法。
var shellWrappers = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "wsl": true, "busybox": true,
	"python": true, "python3": true, "perl": true,
}

// CheckCommand 在发包前判断命令与方言是否相符。
//
// 只看**首个 token**（去掉前导的 env 赋值与 nohup/time 之类透明包装）。不做
// shell 解析、不看管道后续——那些地方误判率高，收益低。判不出来就是 VerdictOK。
//
// 返回值 reason 面向 agent，必须能据以改写命令（给出该方言下的等价命令），
// 而不是只说「不匹配」。
func CheckCommand(c Contract, command string) (Verdict, string) {
	if !c.PlatformKnown {
		return VerdictOK, ""
	}
	head := firstToken(command)
	if head == "" || shellWrappers[head] {
		return VerdictOK, ""
	}
	switch c.Platform {
	case session.PlatformWindows:
		if unixOnlyHeads[head] {
			return VerdictMismatch, "该会话是 windows 方言（cmd/PowerShell），没有 `" + head +
				"`。改用对应命令：ls→dir、cat→type、id/whoami→whoami、uname→ver、ps→tasklist、" +
				"ip addr→ipconfig、chmod→（Windows 无此概念）。若目标确实装了 Git Bash/WSL，" +
				"用 `bash -c \"...\"` 或 `wsl ...` 显式走它。"
		}
	case session.PlatformLinux:
		if windowsOnlyHeads[head] {
			return VerdictMismatch, "该会话是 linux 方言（sh），没有 `" + head +
				"`。改用对应命令：ipconfig→ip addr、tasklist→ps aux、findstr→grep、" +
				"certutil→curl/wget、icacls→ls -l、schtasks→crontab。"
		}
	}
	return VerdictOK, ""
}

// firstToken 取命令的首个可执行 token。
//
// 跳过前导的环境变量赋值（`FOO=1 cmd`）、`env`、`nohup`、`time` 这类透明包装，
// 让 `FOO=1 ls` 也能被判成 `ls`。遇到重定向/分隔符即停止（那是空命令）。
func firstToken(command string) string {
	fields := strings.Fields(strings.TrimSpace(command))
	for _, f := range fields {
		// 环境变量赋值：NAME=value
		if i := strings.IndexByte(f, '='); i > 0 && isShellName(f[:i]) {
			continue
		}
		switch f {
		case "env", "nohup", "time", "command", "exec":
			continue
		case "&", "&&", "|", "||", ";", ">", ">>", "<":
			return ""
		}
		// 取 basename，`/bin/ls` 与 `ls` 等价。
		if j := strings.LastIndexByte(f, '/'); j >= 0 {
			f = f[j+1:]
		}
		return strings.ToLower(f)
	}
	return ""
}

// isShellName 判断 s 是否像一个合法的环境变量名（全 [A-Za-z0-9_]）。
func isShellName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}
