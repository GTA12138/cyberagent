package server

import (
	"strings"
	"testing"

	"chengjian.local/chengjian/session"
)

// Windows 立足点必须用 Windows 命令包：此前只发 Linux 命令，aspx/asp 会话
// 五条侦察全失败，而 recon 解析器其实早已支持 ipconfig/route print/netstat -ano。
//
// 断言链路走「马型 → 方言 → 命令包」：马型到方言由 session.PlatformForKind
// 决定（全系统唯一判据），本包只按方言选包。这样测试同时锁住了两件事——
// aspx 仍是 Windows，以及侦察不再自己搞一份 kind 匹配。
func TestReconCommandPackSelectsWindowsForASPX(t *testing.T) {
	for _, kind := range []string{"http_aspx", "asp", "ASPX", "http_asp"} {
		plat := session.PlatformForKind(kind)
		if plat != session.PlatformWindows {
			t.Fatalf("kind=%q 应判为 windows，得到 %q", kind, plat)
		}
		pack := reconCommandPack(plat)
		if !containsCmdPrefix(pack, "ipconfig /all") {
			t.Fatalf("kind=%q 应选 Windows 命令包（含 ipconfig /all），实际 %v", kind, cmds(pack))
		}
		if containsCmdPrefix(pack, "ip -o -4 addr show") {
			t.Fatalf("kind=%q 不应混入 Linux 命令", kind)
		}
	}
}

func TestReconCommandPackSelectsLinuxForPHPAndJSP(t *testing.T) {
	for _, kind := range []string{"http_php", "http_phpcmd", "http_jsp", "reverse", ""} {
		plat := session.PlatformForKind(kind)
		if plat != session.PlatformLinux {
			t.Fatalf("kind=%q 应判为 linux，得到 %q", kind, plat)
		}
		pack := reconCommandPack(plat)
		if !containsCmdPrefix(pack, "ip -o -4 addr show") {
			t.Fatalf("kind=%q 应选 Linux 命令包，实际 %v", kind, cmds(pack))
		}
		if containsCmdPrefix(pack, "ipconfig /all") {
			t.Fatalf("kind=%q 不应混入 Windows 命令", kind)
		}
	}
}

// 未知方言取值不得被当成 Windows：包选择必须显式比较 PlatformWindows，
// 而不是「不是 linux 就当 windows」。
func TestReconCommandPackDefaultsToLinuxForUnknownPlatform(t *testing.T) {
	for _, plat := range []string{"", "weird", "darwin"} {
		if containsCmdPrefix(reconCommandPack(plat), "ipconfig /all") {
			t.Fatalf("platform=%q 不应选 Windows 命令包", plat)
		}
	}
}

// 两个命令包必须覆盖同一组 key，否则解析侧会出现「有键无值」的静默空洞。
func TestReconCommandPacksShareKeys(t *testing.T) {
	want := map[string]bool{"nics": true, "routes": true, "neighbors": true, "hosts": true, "conns": true}
	got := map[string]bool{}
	for _, c := range reconCommands {
		got[c.key] = true
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("Linux 命令包缺 key %q", k)
		}
	}
	gotWin := map[string]bool{}
	for _, c := range reconCommandsWindows {
		gotWin[c.key] = true
	}
	for k := range want {
		if !gotWin[k] {
			t.Fatalf("Windows 命令包缺 key %q", k)
		}
	}
}

func containsCmdPrefix(pack []struct {
	key string
	cmd string
}, want string) bool {
	for _, c := range pack {
		if strings.HasPrefix(c.cmd, want) {
			return true
		}
	}
	return false
}

func cmds(pack []struct {
	key string
	cmd string
}) []string {
	out := make([]string, 0, len(pack))
	for _, c := range pack {
		out = append(out, c.cmd)
	}
	return out
}
