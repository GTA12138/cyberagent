package server

import (
	"strings"
	"testing"

	"chengjian.local/chengjian/db"
)

// sshAuth 必须按凭据类型分流：hash 类在 SSH 上没有语义，静默降级会白撞一次认证。
func TestSSHAuthRejectsHashCredentials(t *testing.T) {
	_, err := sshAuth(&db.CredentialRecord{ID: 1, CredType: db.CredHashNT, Secret: "aad3b435b51404ee:31d6cfe0d16ae931"})
	if err == nil {
		t.Fatal("NT hash 不能用于 SSH：必须报错并说清该走哪条路")
	}
	if !strings.Contains(err.Error(), "wmiexec") {
		t.Fatalf("错误信息应指向正确方法，got %v", err)
	}
}

func TestSSHAuthAcceptsPassword(t *testing.T) {
	auths, err := sshAuth(&db.CredentialRecord{ID: 2, CredType: db.CredPassword, Secret: "hunter2"})
	if err != nil || len(auths) != 1 {
		t.Fatalf("明文口令应产生一条认证方法: %v %d", err, len(auths))
	}
}

func TestSSHAuthRejectsPassphraseKey(t *testing.T) {
	// 带口令的私钥：必须明确报「不支持交互式解密」，而不是给出一个会失败的 signer。
	_, err := sshAuth(&db.CredentialRecord{ID: 3, CredType: db.CredSSHKey, Secret: "-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-key\n-----END OPENSSH PRIVATE KEY-----"})
	if err == nil {
		t.Fatal("无法解析的私钥必须报错")
	}
}

func TestSSHTargetAddrDefaultsTo22(t *testing.T) {
	cases := map[string]string{
		"10.0.0.5":        "10.0.0.5:22",
		"10.0.0.5:2222":   "10.0.0.5:2222",
		" host.example ":  "host.example:22",
	}
	for in, want := range cases {
		got, err := sshTargetAddr(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q → %q，期望 %q", in, got, want)
		}
	}
	if _, err := sshTargetAddr("10.0.0.5:99999"); err == nil {
		t.Fatal("非法端口必须报错")
	}
	if _, err := sshTargetAddr("   "); err == nil {
		t.Fatal("空 target 必须报错")
	}
}

func TestSSHFailureSignatureOnlyMatchesAuthErrors(t *testing.T) {
	// 命令本身的失败不该被当成认证失败——否则尝试预算会把「命令打错」也算进去。
	if _, hit := sshFailureSignature("bash: foo: command not found"); hit {
		t.Fatal("命令不存在不是认证失败")
	}
	if sig, hit := sshFailureSignature("Permission denied (publickey,password)."); !hit || sig == "" {
		t.Fatal("认证拒绝必须被识别")
	}
}

func TestSSHDialerRejectsBadProxyURL(t *testing.T) {
	if _, err := sshDialer("socks5://127.0.0.1"); err == nil {
		t.Fatal("缺端口的 socks 地址必须报错，而不是退化成直连")
	}
	if _, err := sshDialer(""); err != nil {
		t.Fatalf("空代理应表示直连: %v", err)
	}
}
