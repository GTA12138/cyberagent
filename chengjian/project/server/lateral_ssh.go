// lateral_ssh.go Linux/SSH 横向原语（I3 / T-08）。
//
// 为什么优先做 SSH 而不是补一个 SMB 变体：内网里 Linux 主机的横向几乎只有
// 一条路——ssh。此前 lateral_exec 只支持 impacket 五件套（psexec/wmiexec/
// smbexec/atexec/dcomexec），全是 Windows 协议栈；拿到 Linux 立足点后，
// 系统里明明有 ssh 客户端、有私钥，平台却只能让 agent 用 Bash 手拼
// `ssh -i key user@host cmd`——那又回到「口令进命令行、结果靠模型肉眼读、
// 打成功也不回写验证状态」的老问题。
//
// 三条设计判断：
//
//  1. **不新造工具面。** 能力挂在既有 lateral_exec 的 method=ssh 上，agent 的
//     工作流不变（还是「用哪条凭据打谁、执行什么」）。这符合无感判据：删掉
//     它 agent 照跑，装上它同样的调用多覆盖一类目标。
//
//  2. **HostKeyCallback 必须 InsecureIgnoreHostKey，且要说清为什么。** 渗透
//     目标没有可信 known_hosts，主机指纹本身可能就是我们正在评估的对象；
//     这里做指纹校验只会让工具在第一次连接时必然失败。风险由 RoE 范围校验 +
//     人工审批承担，不由 host key 承担。这一条写进注释，避免下一个人「顺手
//     加固」把它改回去。
//
//  3. **退出码是真实退出码。** SSH 的 exec 通道会回传远端命令的 exit status，
//     这与 impacket 家族「进程退出码 0 就算成功」有本质区别。所以这里能给出
//     exit_status，而协议矩阵里 SSH 的 supports_exit_status=true（见
//     capability/matrix.go）。不要在 SSH 上沿用 impacket 的「无失败签名即成功」
//     判定——那会把「命令跑了但失败了」记成成功。
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/capability"
	"chengjian.local/chengjian/db"
	"github.com/Autumn-27/norma/procguard"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"
)

// lateralSSHMethod 是 SSH 横向的方法名。
const lateralSSHMethod = "ssh"

// sshDialTimeout 是建立 SSH 连接（TCP + 握手 + 认证）的上限。
const sshDialTimeout = 20 * time.Second

// errSSHExecTimeout 是「命令已派发、等回显超时」的哨兵错误。
//
// 这与「连接/认证失败」是两种完全不同的结论：前者命令**可能已经在目标上
// 执行**（超时只说明没等到结果），后者根本没执行。误把前者记成「未执行」
// 会诱发盲目重跑——在目标上重复执行一条已生效的命令，风险比少跑一次大。
var errSSHExecTimeout = errors.New("ssh exec timeout")

// sshResult 是一次 SSH 横向执行的结果。
type sshResult struct {
	Output     string `json:"output"`
	ExitStatus int    `json:"exit_status"`
	// HasExit 表示远端确实回传了退出码。false 时 ExitStatus 无意义
	// （通道在命令结束前被关闭，或远端未回传）。
	HasExit         bool `json:"has_exit_status"`
	OutputTruncated bool `json:"output_truncated"`
}

// sshAuth 从凭据构造 SSH 认证方法。
//
// 只接受两类：明文口令与私钥。票据/令牌类没有 SSH 语义，直接报错并说清
// 「该用哪条路」，而不是静默降级成口令空串去撞一次必然失败的认证。
func sshAuth(cred *db.CredentialRecord) ([]ssh.AuthMethod, error) {
	switch cred.CredType {
	case db.CredPassword:
		if strings.TrimSpace(cred.Secret) == "" {
			return nil, fmt.Errorf("凭据 #%d 是明文类型但 secret 为空", cred.ID)
		}
		return []ssh.AuthMethod{ssh.Password(cred.Secret)}, nil
	case db.CredSSHKey:
		key := strings.TrimSpace(cred.Secret)
		if key == "" {
			return nil, fmt.Errorf("凭据 #%d 是 SSH 私钥类型但 secret 为空", cred.ID)
		}
		// 私钥可能带口令：先按无口令解析，失败再尝试用 secret 之外的
		// 用户名作为口令没有意义——所以这里只支持无口令私钥，并明确报错。
		// 带口令的私钥请把口令写进 Domain 字段？不行，那不是它的语义。
		// 结论：带口令私钥需人工先用 ssh-keygen -p 去口令，或改用口令凭据。
		signer, err := ssh.ParsePrivateKey([]byte(key))
		if err != nil {
			var passErr *ssh.PassphraseMissingError
			if errors.As(err, &passErr) {
				return nil, fmt.Errorf("凭据 #%d 的 SSH 私钥带口令（passphrase），平台不支持交互式解密："+
					"请先用 ssh-keygen -p -N '' 去口令后重新登记，或改用口令凭据", cred.ID)
			}
			return nil, fmt.Errorf("凭据 #%d 的私钥无法解析: %w", cred.ID, err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	case db.CredHashNT, db.CredHashLM, db.CredHashSHA1:
		return nil, fmt.Errorf("凭据 #%d 是 hash 类型：SSH 不接受 hash 认证（那是 Windows 协议栈的做法）。"+
			"Linux 目标请用明文口令或 SSH 私钥；Windows 目标请用 method=wmiexec/psexec", cred.ID)
	case db.CredTicket, db.CredToken:
		return nil, fmt.Errorf("凭据 #%d 是 %s 类型：SSH 不使用票据/令牌。票据请走 Kerberos 流程，令牌请走对应 API", cred.ID, cred.CredType)
	}
	return nil, fmt.Errorf("凭据 #%d 类型 %q 不支持 SSH 横向", cred.ID, cred.CredType)
}

// sshDialer 返回建立 TCP 连接的拨号器。
//
// proxyURL 为空 = 直连（平台出口）；非空 = 经 socks（通常是任务隧道），
// 这是「横向目标只在隧道另一侧」时的必需能力。它与隧道可达性探测共用同一套
// socks 解析（parseSocksURL），避免两处对 `socks5://user:pass@host:port`
// 的理解出现分歧。
func sshDialer(proxyURL string) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	if strings.TrimSpace(proxyURL) == "" {
		d := &net.Dialer{Timeout: sshDialTimeout}
		return d.DialContext, nil
	}
	u, err := parseSocksURL(proxyURL)
	if err != nil {
		return nil, err
	}
	var auth *proxy.Auth
	if u.user != "" {
		auth = &proxy.Auth{User: u.user, Password: u.pass}
	}
	pd, err := proxy.SOCKS5("tcp", u.addr, auth, &net.Dialer{Timeout: sshDialTimeout})
	if err != nil {
		return nil, fmt.Errorf("构造 socks 拨号器失败: %w", err)
	}
	cd, ok := pd.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("socks 拨号器不支持 context（内部错误）")
	}
	return cd.DialContext, nil
}

// sshTargetAddr 把 target 归一成 host:port（默认 22）。
func sshTargetAddr(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("target 不能为空")
	}
	if host, port, err := net.SplitHostPort(target); err == nil {
		if host == "" {
			return "", fmt.Errorf("target %q 缺少主机", target)
		}
		if p, perr := strconv.Atoi(port); perr != nil || p <= 0 || p > 65535 {
			return "", fmt.Errorf("target %q 的端口非法", target)
		}
		return net.JoinHostPort(host, port), nil
	}
	return net.JoinHostPort(target, "22"), nil
}

// sshRun 经 SSH 在目标上执行一条命令。
//
// 返回值区分三种情况（调用方据此写认证账本，不能混为一谈）：
//   - err == nil 且 HasExit：命令执行完毕，ExitStatus 是远端真实退出码；
//   - err == nil 但 !HasExit：命令跑过但远端没回传退出码（通道提前关闭）；
//   - err != nil：连接/认证/通道失败——这**不是**命令失败，是没执行。
func sshRun(ctx context.Context, cred *db.CredentialRecord, target, command string, timeout time.Duration, proxyURL string) (sshResult, error) {
	var out sshResult
	if strings.TrimSpace(command) == "" {
		return out, fmt.Errorf("command 不能为空")
	}
	addr, err := sshTargetAddr(target)
	if err != nil {
		return out, err
	}
	auths, err := sshAuth(cred)
	if err != nil {
		return out, err
	}
	dial, err := sshDialer(proxyURL)
	if err != nil {
		return out, err
	}
	if timeout <= 0 {
		timeout = lateralDefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg := &ssh.ClientConfig{
		User: cred.Username,
		Auth: auths,
		// 见文件头第 2 条：渗透目标没有可信 known_hosts，指纹校验只会让
		// 第一次连接必然失败。风险由 RoE + 人工审批承担。
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         sshDialTimeout,
	}

	conn, err := dial(runCtx, "tcp", addr)
	if err != nil {
		return out, fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	// One cancellation guard covers handshake, channel open and command execution.
	// Closing the underlying connection also unblocks NewSession and pending I/O.
	defer conn.Close()
	operationDone := make(chan struct{})
	defer close(operationDone)
	go func() {
		select {
		case <-operationDone:
		case <-runCtx.Done():
			_ = conn.Close()
		}
	}()
	handshakeDeadline := time.Now().Add(sshDialTimeout)
	if deadline, ok := runCtx.Deadline(); ok && deadline.Before(handshakeDeadline) {
		handshakeDeadline = deadline
	}
	_ = conn.SetDeadline(handshakeDeadline)
	// ssh.NewClientConn 接管连接生命周期；失败时必须自己关，否则泄漏 fd。
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		if runCtx.Err() != nil {
			err = runCtx.Err()
		}
		return out, fmt.Errorf("SSH 握手/认证失败: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(clientConn, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		if runCtx.Err() != nil {
			err = runCtx.Err()
		}
		return out, fmt.Errorf("建立 SSH 会话失败: %w", err)
	}
	defer sess.Close()

	type execOut struct {
		b   []byte
		err error
	}
	done := make(chan execOut, 1)
	var buffer procguard.Buffer
	sess.Stdout, sess.Stderr = &buffer, &buffer
	go func() {
		e := sess.Run(command)
		done <- execOut{buffer.Bytes(), e}
	}()

	select {
	case <-runCtx.Done():
		_ = conn.Close() // Do not block on a remote signal acknowledgement after cancellation.
		out.Output = string(buffer.Bytes())
		// 带哨兵：调用方据此把「已派发、等不到结果」与「没执行」区分开。
		return out, fmt.Errorf("SSH 执行超时（%s）: %w：命令可能已在目标执行，只是未等到回显", timeout, errors.Join(runCtx.Err(), errSSHExecTimeout))
	case r := <-done:
		if runCtx.Err() != nil {
			return out, fmt.Errorf("SSH 执行已取消：%w；远端命令可能已执行", errors.Join(runCtx.Err(), errSSHExecTimeout))
		}
		out.Output = string(r.b)
		out.OutputTruncated = buffer.Overflow
		if out.OutputTruncated {
			return out, fmt.Errorf("SSH 输出超过 8MiB，结果不完整：%w；命令已经派发，不能认定未执行", errSSHExecTimeout)
		}
		var exitErr *ssh.ExitError
		if errors.As(r.err, &exitErr) {
			// 命令跑完了但返回非零：这是**命令**的结论，不是连接的失败。
			out.ExitStatus = exitErr.ExitStatus()
			out.HasExit = true
			return out, nil
		}
		if r.err != nil {
			return out, fmt.Errorf("SSH 通道错误: %w", r.err)
		}
		// CombinedOutput 返回 nil 表示退出码 0，但没有 ExitError 就拿不到
		// 显式状态——SSH 协议下退出码 0 是默认值，所以这里可以确定是 0。
		out.ExitStatus = 0
		out.HasExit = true
		return out, nil
	}
}

// sshFailureSignature 识别 SSH 输出里的常见失败原因。
//
// 与 impacket 的 lateralFailureReason 分开：SSH 的错误措辞完全不同，混用一张
// 表会让两边都漏。这里的签名只用于**认证类**失败（决定要不要计入尝试预算），
// 命令本身的失败由退出码表达，不靠文本猜。
func sshFailureSignature(out string) (string, bool) {
	low := strings.ToLower(out)
	for _, sig := range []string{
		"permission denied", "authentication failed", "no supported authentication methods",
		"too many authentication failures", "host key verification failed",
	} {
		if strings.Contains(low, sig) {
			return sig, true
		}
	}
	return "", false
}

// contractFields 把三段式合同铺平进工具输出（T-15）。
//
// 单独一个函数是为了让所有横向路径用同一套字段名：agent 与下游脚本不需要
// 为 ssh / impacket / 隧道探测各写一份解析。
func contractFields(r capability.ToolResult) map[string]any {
	out := map[string]any{
		"execution_status": string(r.ExecutionStatus),
		"failure_reason":   string(r.FailureReason),
	}
	if r.Assessment != "" {
		out["assessment"] = r.Assessment
	}
	return out
}

// lateralReasonForSignature 把 impacket 的失败签名映射成合同归因。
//
// 映射的用处是让「凭据无效」与「网络不通」在统计上分开：前者换凭据，后者换
// 隧道。此前两者都是 ok:false，agent 只能读输出猜。
func lateralReasonForSignature(sig string) capability.FailureReason {
	switch sig {
	case "logon failure", "access is denied", "rpc_s_access_denied",
		"status_logon_failure", "status_account_locked_out",
		"authentication failed", "invalid credentials":
		return capability.ReasonAuth
	case "connection refused", "no route to host", "timed out":
		return capability.ReasonNetwork
	case "traceback (most recent call last)":
		return capability.ReasonEnvironment
	}
	return capability.ReasonUnknown
}

// runLateralSSH 是 lateral_exec 的 SSH 分支。
//
// 认证账本的写法与 impacket 分支一致（四出口、区分 error 与 failure），但判定
// 依据不同：SSH 有真实退出码，所以「命令失败」与「连不上/认证不过」必须分开
// 记——前者说明凭据可用（只是命令错了），后者才说明凭据有问题。混在一起会让
// 「同一凭据反复失败」的预算统计把「命令打错」也算成认证失败。
func (s *Server) runLateralSSH(ctx context.Context, ri agent.RunInfo, cred *db.CredentialRecord,
	target, command string, timeout time.Duration) (map[string]any, error) {

	// 目标只经隧道可达时，SSH 必须走同一套选路（与 HTTP 流量共享判断）。
	// blocked（内网期无存活隧道）直接拒绝而不是直连——此前 UpstreamForTaskHost
	// 在「实例未建」窗口返回空串，SSH 会从平台出口直连内网目标。
	proxyURL := ""
	if s.m != nil && s.m.taskProxy != nil {
		host := target
		if h, _, err := net.SplitHostPort(target); err == nil {
			host = h
		}
		route := s.m.taskProxy.RouteForTaskHost(ri.TaskID, host)
		if route.Mode == UpstreamBlocked {
			return map[string]any{
				"ok": false, "method": lateralSSHMethod, "target": target,
				"credential_id": cred.ID, "username": cred.Username,
				"error": "任务处于内网期但没有存活隧道（上游已阻断），已拒绝直连",
				"note": "命令未在目标上执行，本次也没有任何包发出。先恢复链路（tunnel_list 查看状态，" +
					"等自动重拉或重新 tunnel_deploy），隧道 alive 后再重试本动作。",
			}, nil
		}
		if route.Mode == UpstreamTunnel {
			proxyURL = route.Upstream
		}
	}

	res, runErr := sshRun(ctx, cred, target, command, timeout, proxyURL)
	if runErr != nil {
		// 三种失败要分开记，结论完全不同：
		//  1. 认证拒绝 → 凭据问题（failure）；
		//  2. 连接/握手失败 → 没执行（error）；
		//  3. **命令已派发但等回显超时 → 命令可能已执行**（error + partial），
		//     不能记成「未执行」——那会诱发盲目重跑已生效的命令（评审 P0-4）。
		execTimedOut := errors.Is(runErr, errSSHExecTimeout)
		sig := ""
		outcome := db.AuthOutcomeError
		if s2, ok := sshFailureSignature(runErr.Error()); ok {
			outcome, sig = db.AuthOutcomeFailure, s2
		}
		s.recordAuthAttempt(ctx, db.AuthAttempt{
			TaskID: ri.TaskID, CredentialID: cred.ID, Principal: cred.Username,
			Service: lateralSSHMethod, Target: target, AuthMethod: cred.CredType,
			Outcome: outcome, FailureSignature: sig,
			Evidence: truncateForSnapshot(runErr.Error(), 600),
		})
		s.appendIntranetActivity(ri.TaskID, "lateral_exec",
			fmt.Sprintf("SSH 横向未取得结果：%s → %s", cred.Username, target),
			fmt.Sprintf("credential_id=%d target=%s method=ssh err=%s", cred.ID, target, runErr.Error()))
		out := map[string]any{
			"ok": false, "method": lateralSSHMethod, "target": target,
			"credential_id": cred.ID, "username": cred.Username,
			"error": runErr.Error(),
		}
		if execTimedOut {
			// 认证已通过、命令已派发：凭据本身是好的（握手完成才走得到这里）。
			out["timed_out"] = true
			out["note"] = "超时只说明没等到回显，命令**可能已在目标上执行**：不要判定凭据无效，" +
				"也不要盲目重跑同一条命令（若已生效，重跑是重复副作用）。先核对目标侧状态或换只读命令确认。"
			if c, cerr := capability.NewToolResult(capability.StatusPartial, capability.ReasonTimeout,
				"SSH 认证已通过、命令已派发但未在超时内取得回显；执行状态未知。",
				truncateForSnapshot(runErr.Error(), 400)); cerr == nil {
				for k, v := range contractFields(c) {
					out[k] = v
				}
			}
		} else {
			out["note"] = "这是连接/认证层面的失败，命令没有执行。若错误是认证拒绝，说明凭据不适用于该目标；" +
				"若是超时或不可达，先确认目标经哪条隧道可达（tunnel_reach 可验证）。"
			// 三段式：没执行 ≠ 执行失败。这一步没在目标上发生任何事，所以不能
			// 推出「凭据无效」之外的任何结论。
			if c, cerr := capability.NewToolResult(capability.StatusNotExecuted,
				capability.ClassifyTransportError(runErr),
				"命令未在目标上执行；这不能证明凭据有效，也不能证明目标不可用（可能只是这一次没连上）。",
				truncateForSnapshot(runErr.Error(), 400)); cerr == nil {
				for k, v := range contractFields(c) {
					out[k] = v
				}
			}
		}
		if proxyURL != "" {
			out["via"] = "socks" // 只说明走了隧道，不暴露凭据
		}
		out["output"], out["read_complete"] = res.Output, false
		return out, nil
	}

	// 命令执行了：凭据一定有效（认证已通过），所以标 verified。
	if verr := s.credStore.SetVerified(ctx, cred.ID, true); verr != nil {
		s.appendIntranetActivity(ri.TaskID, "lateral_exec",
			fmt.Sprintf("凭据 #%d 验证状态回写失败：%v", cred.ID, verr), "")
	}
	s.recordAuthAttempt(ctx, db.AuthAttempt{
		TaskID: ri.TaskID, CredentialID: cred.ID, Principal: cred.Username,
		Service: lateralSSHMethod, Target: target, AuthMethod: cred.CredType,
		Outcome:  db.AuthOutcomeSuccess,
		Evidence: truncateForSnapshot(res.Output, 600),
	})
	s.appendIntranetActivity(ri.TaskID, "lateral_exec",
		fmt.Sprintf("SSH 横向执行成功：%s → %s（退出码 %d）", cred.Username, target, res.ExitStatus),
		fmt.Sprintf("credential_id=%d target=%s method=ssh exit=%d", cred.ID, target, res.ExitStatus))

	out := map[string]any{
		"ok":                  res.ExitStatus == 0,
		"method":              lateralSSHMethod,
		"target":              target,
		"credential_id":       cred.ID,
		"username":            cred.Username,
		"output":              res.Output,
		"read_complete":       !res.OutputTruncated,
		"exit_status":         res.ExitStatus,
		"has_exit_status":     res.HasExit,
		"credential_verified": true,
		"note": "SSH 有真实退出码：exit_status!=0 表示命令执行了但失败（凭据是好的），" +
			"不是认证失败。凭据已标记为已验证。",
	}
	// 三段式：命令执行了。退出码 0 → succeeded；非 0 → failed 但**归因为 none**
	// （执行成功、命令自身失败），这正好与「认证失败」区分开。
	status, reason := capability.StatusSucceeded, capability.ReasonNone
	if res.ExitStatus != 0 {
		status = capability.StatusFailed
	}
	if c, cerr := capability.NewToolResult(status, reason,
		fmt.Sprintf("SSH 认证通过、命令已在目标执行（退出码 %d）。", res.ExitStatus),
		truncateForSnapshot(res.Output, 400)); cerr == nil {
		for k, v := range contractFields(c) {
			out[k] = v
		}
	}
	if proxyURL != "" {
		out["via"] = "socks"
	}
	return out, nil
}
