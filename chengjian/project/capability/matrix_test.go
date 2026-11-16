package capability

import "testing"

func TestMatrixDeclaresEveryProtocol(t *testing.T) {
	want := []Protocol{ProtoSSH, ProtoSMB, ProtoWinRM, ProtoLDAP, ProtoRDP}
	got := map[Protocol]ProtocolSupport{}
	for _, m := range Matrix() {
		got[m.Protocol] = m
	}
	for _, p := range want {
		if _, ok := got[p]; !ok {
			t.Fatalf("协议 %s 必须出现在矩阵里：不列出来的后果是每个调用方各自猜「它应该支持吧」", p)
		}
	}
}

func TestUnimplementedProtocolsSayWhatIsMissing(t *testing.T) {
	for _, m := range Matrix() {
		if m.Implemented {
			if m.Method == "" {
				t.Fatalf("%s 标了 implemented 却没有 method：调用方无从下手", m.Protocol)
			}
			continue
		}
		if m.Missing == "" {
			t.Fatalf("%s 未实现但没写缺什么：下一次评估会重新踩一遍", m.Protocol)
		}
		if m.Method != "" {
			t.Fatalf("%s 未实现却给了 method %q：会造成「文档说有、实际调不通」", m.Protocol, m.Method)
		}
	}
}

func TestExitStatusIsNotABoolean(t *testing.T) {
	// impacket 家族只有本地进程退出码：不能当远端真实退出码。
	for _, method := range []string{"psexec", "wmiexec", "smbexec", "atexec", "dcomexec"} {
		if got := ExitStatusOf(method); got != ExitInferred {
			t.Fatalf("%s 的退出码应标 inferred，实际 %s（当成 real 会让「命令失败但脚本成功」被记成成功）", method, got)
		}
	}
	if got := ExitStatusOf("ssh"); got != ExitReal {
		t.Fatalf("ssh 有远端真实退出码，实际 %s", got)
	}
	if got := ExitStatusOf("winrm"); got != ExitNone {
		t.Fatalf("未知/未实现的 method 必须返回 none，实际 %s", got)
	}
}

func TestLookupMethodCoversImpacketFamily(t *testing.T) {
	m, ok := LookupMethod("wmiexec")
	if !ok || m.Protocol != ProtoSMB {
		t.Fatalf("wmiexec 应落到 SMB 行，got %+v ok=%v", m, ok)
	}
	if _, ok := LookupMethod("telnet"); ok {
		t.Fatal("未声明的 method 不该被 LookupMethod 命中")
	}
}

func TestCancelSemanticsDocumented(t *testing.T) {
	for _, m := range Matrix() {
		if !m.Implemented {
			continue
		}
		if m.Cancel == "" {
			t.Fatalf("%s 已实现却没写 cancel 语义：能不能中途停直接决定超时该怎么给", m.Protocol)
		}
	}
}

func TestMatrixCopyIsIndependent(t *testing.T) {
	m := Matrix()
	m[0].Note = "tampered"
	if Matrix()[0].Note == "tampered" {
		t.Fatal("Matrix 必须返回副本，否则一次读取就能改掉全局声明")
	}
}
