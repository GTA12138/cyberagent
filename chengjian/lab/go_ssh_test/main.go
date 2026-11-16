// go_ssh_test 用平台同款 Go x/crypto/ssh 路径验证 SSH 靶标：
// 认证 → exec → stdout → exit-status，四条都要过。
//
//	go run . 127.0.0.2:2222 root chengjian123
package main

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

func main() {
	addr := "127.0.0.2:2222"
	user, pass := "root", "chengjian123"
	if len(os.Args) > 3 {
		addr, user, pass = os.Args[1], os.Args[2], os.Args[3]
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		fmt.Println("dial FAIL:", err)
		os.Exit(1)
	}
	defer conn.Close()
	fmt.Println("dial OK:", addr, "as", user)

	bad := 0
	for _, c := range []struct {
		cmd  string
		want int
	}{
		{"whoami && hostname", 0},
		{"cat flag.txt", 0},
		{"nosuchcmd", 127},
		{"false", 1},
	} {
		sess, err := conn.NewSession()
		if err != nil {
			fmt.Println("session FAIL:", err)
			bad++
			continue
		}
		out, err := sess.CombinedOutput(c.cmd)
		status := 0
		if ee, ok := err.(*ssh.ExitError); ok {
			status = ee.ExitStatus()
		} else if err != nil {
			fmt.Printf("  %-22q EXEC FAIL: %v\n", c.cmd, err)
			sess.Close()
			bad++
			continue
		}
		verdict := "OK"
		if status != c.want {
			verdict = "EXIT MISMATCH"
			bad++
		}
		fmt.Printf("  %-22q exit=%d want=%d %s out=%q\n", c.cmd, status, c.want, verdict, clip(string(out)))
		sess.Close()
	}
	if bad > 0 {
		fmt.Println("FAIL:", bad)
		os.Exit(1)
	}
	fmt.Println("PASS: go x/crypto/ssh 与靶标完全互通")
}

func clip(s string) string {
	r := []rune(s)
	if len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}
