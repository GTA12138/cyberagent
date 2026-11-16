package session

import (
	"context"
	"strings"
	"time"
)

// 目标侧平台的两个已知形态，取值与 HTTPShell.platform / Secret.Platform 一致。
// 用字符串而不是枚举，是为了让台账里既有的 "linux"/"windows" 原样可用。
const (
	PlatformLinux   = "linux"
	PlatformWindows = "windows"
)

// DescribePlatform 只回答「驱动自己知道的目标形态」，不做推断、不碰目标。
//
// 未知（未实现 HostPlatformProvider、或自报了一个我们不认识的取值）一律返回
// ""。调用方必须显式处理未知，不能把 "" 当成 linux——把「不知道」悄悄降级成
// 「按 Unix 办」正是这个包要避免的误读。也不认识的值不猜，理由同上。
//
// 与 Capabilities().Runtime.OS 的区别：Runtime 是**观测**结果，本函数是**驱动
// 元数据**（例如 aspx 马必然对 cmd.exe 说话）。两者不能互相冒充。
func DescribePlatform(s Session) string {
	if s == nil || nilSession(s) {
		return ""
	}
	p, ok := s.(HostPlatformProvider)
	if !ok {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(p.HostPlatform())) {
	case PlatformWindows, "win", "win32", "win64":
		return PlatformWindows
	case PlatformLinux, "unix", "darwin", "bsd":
		return PlatformLinux
	default:
		return ""
	}
}

// PlatformForSession 是「这个立足点按什么方言说话」的统一判定。
//
// 证据优先级：驱动自报（DescribePlatform）> 马型推断（PlatformForKind）。
// 之所以保留马型推断，是因为有些适配器结构上不知道平台（例如 penelope 反转
// shell 拿到的是 PTY，不区分 OS），此时没有比马型更好的信号；返回 linux 是
// 既有行为，不是新假设。
//
// 这是全系统唯一的平台判定入口：隧道部署、侦察命令包、agent 视图都必须走这里，
// 否则同一件事会有两份判据，改一处漏一处。
func PlatformForSession(s Session) string {
	if p := DescribePlatform(s); p != "" {
		return p
	}
	if s == nil || nilSession(s) {
		return PlatformLinux
	}
	return PlatformForKind(s.Kind())
}

// PlatformForKind 按马型推方言。aspx/asp 跑在 IIS 上，必为 Windows；其余
// （php/jsp/reverse）按 Unix 处理。
//
// 判据是马型而非探测结果：部署隧道、被动侦察本身就是探测前的第一步，此时
// 没有更可靠的信号。判错时的正确做法是**在目标上确认方言**（见
// DetectShellDialect），而不是在这里加更多猜测。
func PlatformForKind(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	if strings.Contains(k, "aspx") || strings.Contains(k, "asp") || strings.Contains(k, "windows") {
		return PlatformWindows
	}
	return PlatformLinux
}

// dialectProbe 是一条在两种解释器下都会「成功」、但回显不同的命令：
//
//	sh  ：$((40+2)) 展开成 42        → 回显含 42
//	cmd ：$((40+2)) 原样输出（cmd 不做算术展开）→ 回显含 "$(("
//
// 单条命令判方言，比「换个命令包再打五条」便宜得多。与 http_shell.go 的
// echoProbeCommand 同一原理，但这里要的是判方言而不是验通道。
const dialectProbe = `echo __A$((40+2))__ 2>&1`

// DetectShellDialect 用一条哨兵回显在目标侧实测方言，返回
// PlatformLinux / PlatformWindows / ""（判不出来，例如会话已死或输出被吞）。
//
// 只给「驱动结构上不知道平台」的适配器兜底用（典型是反转 shell）。已知平台
// 的会话不该调它——那是在花一次目标侧请求去确认已知事实。
func DetectShellDialect(ctx context.Context, s Session, timeout time.Duration) string {
	if s == nil || nilSession(s) {
		return ""
	}
	out, _, err := s.Exec(ctx, dialectProbe, timeout)
	if err != nil && strings.TrimSpace(out) == "" {
		return ""
	}
	switch {
	case strings.Contains(out, "__A42__"):
		return PlatformLinux
	case strings.Contains(out, "$(("):
		return PlatformWindows
	default:
		return ""
	}
}
