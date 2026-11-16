package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chengjian.local/chengjian/db"
)

// lateralAuthArgs 是「手拼 impacket 最容易错」的那部分逻辑，必须锁死。
// 契约依据 impacket 官方 examples 的共享 target 解析器：认证与目标是**一个
// 操作数**（domain/user:pass@target / domain/user@target），不是分开的参数。
// 旧测试锁定了拆开的形式（"user:pass" + 独立 target），是外部评审确认的
// 接口拼装错误——本组断言已改为官方契约。
func TestLateralAuthArgs(t *testing.T) {
	const target = "10.0.0.8"
	cases := []struct {
		name     string
		cred     *db.CredentialRecord
		wantArgs []string
		wantErr  bool
	}{
		{
			name:     "明文口令 + 域账号 → 单操作数 domain/user:pass@target",
			cred:     &db.CredentialRecord{ID: 1, Username: "administrator", Domain: "CORP", CredType: db.CredPassword, Secret: "Pw0rd"},
			wantArgs: []string{"CORP/administrator:Pw0rd@" + target},
		},
		{
			name:     "明文口令 + 本地账号（无域）",
			cred:     &db.CredentialRecord{ID: 2, Username: "admin", CredType: db.CredPassword, Secret: "pw"},
			wantArgs: []string{"admin:pw@" + target},
		},
		{
			name:     "NT hash 只有一半 → LM 留空 + 目标并入操作数",
			cred:     &db.CredentialRecord{ID: 3, Username: "u", Domain: "D", CredType: db.CredHashNT, Secret: "abcdef1234"},
			wantArgs: []string{"-hashes", ":abcdef1234", "D/u@" + target},
		},
		{
			name:     "NT hash 已含冒号 → 原样",
			cred:     &db.CredentialRecord{ID: 4, Username: "u", CredType: db.CredHashNT, Secret: "lmhash:nthash"},
			wantArgs: []string{"-hashes", "lmhash:nthash", "u@" + target},
		},
		{
			name:     "LM hash 只有一半 → NT 留空（LMHASH:）",
			cred:     &db.CredentialRecord{ID: 5, Username: "u", CredType: db.CredHashLM, Secret: "lmhash"},
			wantArgs: []string{"-hashes", "lmhash:", "u@" + target},
		},
		{
			name:    "票据类型明确不支持（提示改走 Kerberos）",
			cred:    &db.CredentialRecord{ID: 6, Username: "u", CredType: db.CredTicket, Secret: "t"},
			wantErr: true,
		},
		{
			name:    "SSH key 明确不支持",
			cred:    &db.CredentialRecord{ID: 7, Username: "u", CredType: db.CredSSHKey, Secret: "k"},
			wantErr: true,
		},
		{
			name:    "明文类型但 secret 空 → 报错",
			cred:    &db.CredentialRecord{ID: 8, Username: "u", CredType: db.CredPassword, Secret: "  "},
			wantErr: true,
		},
		{
			name:    "NT hash 但 secret 空 → 报错",
			cred:    &db.CredentialRecord{ID: 9, Username: "u", CredType: db.CredHashNT, Secret: ""},
			wantErr: true,
		},
		{
			// 口令含 @ 会破坏单操作数解析（user:pa@ss@target 歧义）——
			// 拒拼而不是生成必错的命令行。
			name:    "口令含 @ → 拒拼（无法安全并入单操作数）",
			cred:    &db.CredentialRecord{ID: 10, Username: "u", CredType: db.CredPassword, Secret: "p@ss"},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args, err := lateralAuthArgs(c.cred, target)
			if c.wantErr {
				if err == nil {
					t.Fatalf("应报错，实际 args=%v", args)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if len(args) != len(c.wantArgs) {
				t.Fatalf("args=%v want=%v", args, c.wantArgs)
			}
			for i := range args {
				if args[i] != c.wantArgs[i] {
					t.Fatalf("args[%d]=%q want %q（完整 %v）", i, args[i], c.wantArgs[i], args)
				}
			}
		})
	}
}

// 域账号必须用 domain/user（impacket 语义），不能用 domain\user——
// 反斜杠会被 shell/参数解析吃掉，是手拼最常见的错误之一。
func TestLateralAuthArgsDomainUsesSlashNotBackslash(t *testing.T) {
	cred := &db.CredentialRecord{ID: 1, Username: "u", Domain: "CORP", CredType: db.CredPassword, Secret: "p"}
	args, err := lateralAuthArgs(cred, "10.0.0.8")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, `CORP\u`) {
		t.Fatalf("不应使用反斜杠域分隔: %v", args)
	}
	if !strings.Contains(joined, "CORP/u") {
		t.Fatalf("应使用 domain/user 形式: %v", args)
	}
}

// resolveImpacketScript：两种部署形态都要解析到可执行 argv，绝不依赖
// 服务工作目录（旧实现跑裸脚本名，是评审确认的路径缺陷）。
func TestResolveImpacketScript(t *testing.T) {
	dir := t.TempDir()
	// 形态一：venv 安装（bin/wmiexec console script 直接可执行）。
	venvBin := filepath.Join(dir, "wmiexec")
	if err := os.WriteFile(venvBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	argv, err := resolveImpacketScript(dir, "wmiexec")
	if err != nil || len(argv) != 1 || argv[0] != venvBin {
		t.Fatalf("venv 形态应直接执行: %v %v", argv, err)
	}
	// 形态二：源码 clone（examples/wmiexec.py 走 python3 + 绝对路径）。
	dir2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir2, "examples"), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir2, "examples", "wmiexec.py")
	if err := os.WriteFile(scriptPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	argv2, err := resolveImpacketScript(dir2, "wmiexec.py")
	if err != nil || len(argv2) != 2 || argv2[0] != "python3" || !filepath.IsAbs(argv2[1]) {
		t.Fatalf("源码形态应 python3+绝对路径: %v %v", argv2, err)
	}
	// 两种形态都没有 → 明确报错（带修复指引），不是静默跑裸名。
	if _, err := resolveImpacketScript(t.TempDir(), "wmiexec"); err == nil || !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("缺脚本应报错带指引, got %v", err)
	}
}

// SSH 派发后超时必须能被调用方识别（errors.Is 哨兵），不能与「没执行」混记。
func TestSSHExecTimeoutSentinel(t *testing.T) {
	wrapped := errSSHExecTimeoutWrapped()
	if !errors.Is(wrapped, errSSHExecTimeout) {
		t.Fatalf("哨兵必须可被 errors.Is 识别: %v", wrapped)
	}
	if errors.Is(errors.New("SSH 握手/认证失败"), errSSHExecTimeout) {
		t.Fatal("非超时错误不应命中哨兵")
	}
}

func errSSHExecTimeoutWrapped() error {
	// 与 sshRun 超时分支同构的包装链。
	return errors.Join(contextDeadlineErr, errSSHExecTimeout)
}

var contextDeadlineErr = errors.New("context deadline exceeded")

// 失败签名识别：impacket 有时退出码 0 但把错误打在 stdout。
func TestLateralFailureSignature(t *testing.T) {
	fails := []string{
		"[-] Logon failure: unknown user name or bad password",
		"rpc_s_access_denied",
		"STATUS_LOGON_FAILURE",
		"Access is denied.",
		"Traceback (most recent call last):",
		"connection refused",
	}
	for _, s := range fails {
		if !lateralFailureSignature(s) {
			t.Errorf("应判失败: %q", s)
		}
	}
	ok := []string{
		"Microsoft Windows [Version 10.0.19045.3693]",
		"nt authority\\system",
		"C:\\Windows\\system32> whoami",
	}
	for _, s := range ok {
		if lateralFailureSignature(s) {
			t.Errorf("不应判失败: %q", s)
		}
	}
}

// 方法白名单必须只收已知 impacket 脚本，不能任意拼接。
func TestLateralMethodsWhitelist(t *testing.T) {
	for _, m := range []string{"psexec", "wmiexec", "smbexec", "atexec", "dcomexec"} {
		if _, ok := lateralMethods[m]; !ok {
			t.Fatalf("应支持 %q", m)
		}
	}
	for _, m := range []string{"", "bash", "cmd", "python", "evil"} {
		if _, ok := lateralMethods[m]; ok {
			t.Fatalf("不应支持 %q（白名单外）", m)
		}
	}
}
