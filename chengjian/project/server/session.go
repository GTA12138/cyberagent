// session.go 装配立足点会话子系统（内网渗透期 1a,INTRANET-PIVOT-DESIGN.md §4.2):
// 进程内 Registry + DB 恢复 + 四个 host 工具（register_session/session_exec/
// session_read/session_write)。工具全部是普通工具调用——自动过 guard 审批、
// 自动进 activity 留痕，这是相对外部通道的本质优势。
//
// 立足点策略（foothold ladder):webshell 优先为主通道；反弹/隧道后期经主通道投递。
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/capability"
	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/guard"
	"chengjian.local/chengjian/session"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// maxSessionToolOutput 是 session_exec/session_read 返回给模型的输出上限（64KB)。
const maxSessionToolOutput = 64 * 1024

// 启动探活参数（治理假存活：靶场已停、台账仍显示 alive):只探 last_beat 超过
// sessionStaleProbeAge 的 alive 会话（新鲜会话近期刚用过，不必再探）;每会话
// startupProbePerTimeout 超时，最多 startupProbeMaxConc 并发，整体预算
// startupProbeBudget。阈值改动只动 sessionStaleProbeAge 这一个常量。
const (
	sessionStaleProbeAge   = 24 * time.Hour
	startupProbePerTimeout = 8 * time.Second
	startupProbeBudget     = 60 * time.Second
	startupProbeMaxConc    = 8
)

// sessionNeedsStartupProbe 是启动探活的阈值判定（纯函数，可测）:
// alive 且 last_beat 距今超过 age 才需要探活；恰好在边界上不算陈旧。
func sessionNeedsStartupProbe(rec *db.SessionRecord, now time.Time, age time.Duration) bool {
	return rec != nil && rec.Status == db.SessionAlive && now.Sub(rec.LastBeat) > age
}

// initSessions 建会话注册表与落库访问层，并从 DB 恢复存活会话
// (restore_reverse_listeners 模式：启动重建驱动，不逐条重探马型）。恢复后对
// 陈旧（last_beat 超 sessionStaleProbeAge）会话做一次异步启动探活
// (startupProbeStale)：确定失败标 dead，本地超时/取消保留原状态；新鲜会话仍由下次使用时的
// 真实请求验证。失败只记日志，不拖垮启动。
func (s *Server) initSessions() {
	if s.m.pg == nil {
		return
	}
	s.sessStore = db.NewSessionStore(s.m.pg, s.jwtKey)
	s.sessReg = session.NewRegistry()
	recs, err := s.sessStore.List(s.ctx, db.SessionAlive)
	if err != nil {
		log.Printf("[session] 启动恢复读取失败: %v", err)
		return
	}
	restored := 0
	for _, rec := range recs {
		var sec session.Secret
		if err := json.Unmarshal(rec.Secret, &sec); err != nil {
			log.Printf("[session] 会话 %d secret 解析失败，跳过: %v", rec.ID, err)
			continue
		}
		var sh *session.HTTPShell
		switch rec.Kind {
		case "http_php", "http_phpcmd", "http_jsp", "http_aspx", "http_phpenc", "http_jspenc":
			sh, err = session.RestoreHTTPShell(rec.ID, sec)
		case "reverse":
			// 期 5:reverse 会话活在 penelope 进程内存,MCP 接入参数随平台进程
			// 丢失,不可恢复——initReverse 已统一诚实标 dead,这里静默跳过。
			continue
		default:
			log.Printf("[session] 会话 %d 类型 %s 本期不支持恢复，跳过", rec.ID, rec.Kind)
			continue
		}
		if err != nil {
			log.Printf("[session] 会话 %d 恢复失败，跳过: %v", rec.ID, err)
			continue
		}
		s.sessReg.Add(sh)
		restored++
	}
	if len(recs) > 0 {
		log.Printf("[session] 启动恢复：%d/%d 条存活会话已载入注册表", restored, len(recs))
	}
	// 启动卫生：对陈旧（last_beat 超 24h)alive 会话异步探活，确定失败才标 dead。
	// 异步执行不拖启动；整体预算 60s 内完成。
	go s.startupProbeStale(recs)
}

// startupProbeStale 对陈旧 alive 会话并发探活：成功刷新 last_beat（状态归
// alive),确定失败标 dead；取消或本地预算耗尽视为 unknown，不改状态。
// 只探注册表内会话，状态写入须匹配探活前快照，防旧结果覆盖新成功心跳。
func (s *Server) startupProbeStale(recs []*db.SessionRecord) {
	ctx, cancel := context.WithTimeout(s.ctx, startupProbeBudget)
	defer cancel()
	sem := make(chan struct{}, startupProbeMaxConc)
	var wg sync.WaitGroup
	var aliveN, deadN, unknownN, staleN atomic.Int64
	probed := 0
	now := time.Now()
	for _, rec := range recs {
		if !sessionNeedsStartupProbe(rec, now, sessionStaleProbeAge) {
			continue
		}
		sess, ok := s.sessReg.Get(rec.ID)
		if !ok {
			continue
		}
		probed++
		wg.Add(1)
		go func(id int64, expected db.SessionHealth, sess session.Session) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				unknownN.Add(1)
				return
			}
			defer func() { <-sem }()
			pctx, pcancel := context.WithTimeout(ctx, startupProbePerTimeout)
			if pctx.Err() != nil {
				pcancel()
				unknownN.Add(1)
				return
			}
			err := sess.Test(pctx)
			state := classifySessionProbe(pctx.Err(), err)
			pcancel()
			if state == sessionProbeUnknown {
				unknownN.Add(1)
				return
			}
			dctx, dcancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
			defer dcancel()
			updated, uerr := s.sessStore.ApplyProbe(dctx, id, expected, state == sessionProbeAlive)
			if uerr != nil {
				log.Printf("[session] 启动探活：会话 %d 状态落库失败：%v", id, uerr)
				unknownN.Add(1)
				return
			}
			if !updated {
				staleN.Add(1)
				return
			}
			if state == sessionProbeDead {
				deadN.Add(1)
				return
			}
			aliveN.Add(1)
		}(rec.ID, db.SessionHealth{Status: rec.Status, LastBeat: rec.LastBeat}, sess)
	}
	wg.Wait()
	if probed > 0 {
		log.Printf("[session] 启动探活：%d 活 %d 死 %d 未确定 %d 过期结果（共探 %d 条陈旧会话）",
			aliveN.Load(), deadN.Load(), unknownN.Load(), staleN.Load(), probed)
	}
}

// sessionRoECheck 在登记会话前校验目标 URL 的 host 是否在本任务 RoE 范围内：
// strict 拦截、warn 告警放行、范围未登记/范围内直接放行。返回告警文本（可空）
// 与拦截错误（可空）。非任务运行（无 task scope 可查）按未登记范围放行。
func (s *Server) sessionRoECheck(ctx context.Context, rawURL string) (string, error) {
	ri := agent.RunInfoFrom(ctx)
	if ri.TaskID <= 0 || s.m.pg == nil {
		return "", nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "", nil // 提取不出目标 = 无目标,不拦截（与 guard RoE 保守取向一致）
	}
	mode, _, _ := s.m.pg.GetSetting(settingRoEEnforcement)
	if strings.EqualFold(strings.TrimSpace(mode), "off") {
		return "", nil
	}
	rows, err := s.m.pg.Assets().ListTaskScope(ri.TaskID)
	if err != nil {
		return "", nil // 读取失败按未登记范围处理（fail-open,与 guard 一致）
	}
	verdict := guard.NewScopeMatcher(scopeRulesFromRows(rows)).Check(u.Hostname())
	if verdict != guard.ScopeOut {
		return "", nil
	}
	msg := fmt.Sprintf("RoE 授权范围检查：webshell 目标 %s 不在本任务(task %d)已登记范围内。"+
		"如需放行：把目标加入任务范围(add_task_scope)，或在系统设置把 roe_enforcement 改为 warn/off。",
		u.Hostname(), ri.TaskID)
	if strings.EqualFold(strings.TrimSpace(mode), "strict") {
		return "", fmt.Errorf("%s", msg)
	}
	return "⚠ " + msg, nil // warn：告警放行
}

// --- host 工具（默认绑 worker;seed 进 tools 表，按 agent 过滤） ---

func sessionPerm(context.Context, json.RawMessage, permission.Context) permission.Decision {
	return permission.Allowed()
}

// sessionTools 返回立足点工具(期 1a 四个 + 加密马生成 + 期 2 被动侦察 + 台账只读)。
// sessReg 为 nil(DB 未就绪)时不提供。
func (s *Server) sessionTools() []actool.CoreTool {
	if s.sessReg == nil || s.sessStore == nil {
		return nil
	}
	return []actool.CoreTool{
		s.sessionListTool(), // 只读台账（planner 也可见）
		s.registerSessionTool(),
		s.sessionExecTool(),
		s.sessionReadTool(),
		s.sessionWriteTool(),
		s.generateWebshellTool(), // 自研加密马生成(phpenc/jspenc)
		s.sessionReconTool(),     // 期 2 被动侦察(只读命令包 + recon 解析)
	}
}

// sessionListTool 列本任务的立足点会话台账（只读）。
//
// 为什么需要：planner 的 graph_overview 里有 intranet.sessions 投影（限 5 条、
// 只有 id/kind/host/status），但那是「有没有立足点」的概览。要判断「这条会话
// 能不能用、上次心跳多久前、是哪种马型」就得看完整台账。没有这个工具，
// planner 会出现「看得到 sessions 却查不了详情」的悬空。
func (s *Server) sessionListTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "session_list",
		Description: "【只读】列本任务的立足点会话台账：id / 马型（http_php/http_jsp/http_aspx/reverse）/ " +
			"宿主 / 存活状态 / 上次心跳 / 创建它的意图。只列本任务创建或获得明确授权的会话。能力字段区分驱动支持与环境未确认。" +
			"platform 是驱动自报的目标方言（linux/windows），据此决定命令语法：windows 用 cmd/PowerShell" +
			"（dir/type/ipconfig，无 chmod/setsid//tmp），linux 用 sh；platform 缺失=驱动没自报（如反转 shell），" +
			"此时用 session_recon 或一条 uname -s / cmd /c ver 现场确认，不要默认 linux。" +
			"capabilities.runtime.os 是【观测】字段，当前无驱动上报、恒为 unknown，不代表平台未知。" +
			"granted_actions 只在「非本任务创建、靠授权访问」的会话上出现：里面没有 exec 就不要派执行类意图，先申请授权。" +
			"默认每页25条，最多50条且JSON不超过64KiB；count/alive只统计当前页。has_more为true时用next_offset继续，勿据单页断言全部失活。" +
			"metadata_omitted表示字段完整省略，不能把省略当成空目标。要执行命令用 session_exec。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "每页条数，默认25；输出预算可能缩小本页"},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "分页偏移，默认0；继续时传上页next_offset"},
			},
		},
		ReadOnly:    func(json.RawMessage) bool { return true },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("session_list 必须在任务上下文内使用（非任务运行无归属）"), nil
			}
			limit, offset := 25, 0
			var args map[string]json.RawMessage
			if len(in) > 0 {
				if err := json.Unmarshal(in, &args); err != nil || args == nil {
					return actool.Errorf("session_list 参数必须为合法JSON对象"), nil
				}
			}
			if raw, ok := args["limit"]; ok {
				if strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, &limit) != nil || limit < 1 || limit > 50 {
					return actool.Errorf("session_list limit必须为1到50的整数"), nil
				}
			}
			if raw, ok := args["offset"]; ok {
				if strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, &offset) != nil || offset < 0 {
					return actool.Errorf("session_list offset必须为非负整数"), nil
				}
			}
			recs, moreRows, err := s.sessStore.ListAccessiblePage(ctx, ri.TaskID, "", limit, offset)
			if err != nil {
				return actool.Errorf("会话台账读取失败：" + err.Error()), nil
			}
			items := []map[string]any{}
			// 授权来的会话必须带上「能做什么」：只给存在、不给权限，worker 会
			// 对着只有 read 授权的会话派 exec，执行时才被拒。一次查询覆盖整页。
			// 查询失败时整列省略——把读库错误说成「无授权」是用假信息换缺失信息。
			grants, grantsErr := s.sessStore.GrantsForTask(ctx, ri.TaskID)
			for _, r := range recs {
				item := map[string]any{
					"id":              r.ID,
					"status":          r.Status,
					"last_beat":       r.LastBeat.Format(time.RFC3339),
					"created_by_task": r.CreatedByTask,
				}
				if r.CreatedByTask != ri.TaskID && grantsErr == nil {
					acts := grants[r.ID]
					if acts == nil {
						acts = []string{}
					}
					item["granted_actions"] = acts
				}
				omitted := append([]string(nil), r.MetadataOmitted...)
				if r.Kind != "" {
					item["kind"] = r.Kind
				}
				if r.URL != "" {
					host := hostOfURL(r.URL)
					if len(host) <= 256 {
						item["host"] = host
					} else {
						omitted = append(omitted, "host")
					}
				}
				capabilities := s.sessionCapabilities(r.ID)
				// A future third-party provider must not grow an unbounded list
				// item. Omit the whole snapshot rather than truncate atomic data.
				if encoded, err := json.Marshal(capabilities); err != nil || len(encoded) > 2048 {
					capabilities = session.UnknownCapabilities()
					omitted = append(omitted, "capabilities")
				}
				item["capabilities"] = capabilities
				// platform 是驱动自报的方言（linux/windows），不是已验证的 OS 事实。
				// 必须和 capabilities.runtime.os 并存且分开：后者是观测字段，当前
				// 无驱动上报，恒为 unknown。此前只给 capabilities，agent 读到
				// runtime.os=unknown 就以为「不知道目标是什么系统」，于是对 Windows
				// 立足点照发 id/uname/ls——五条全失败，还被记成「会话可能已死」。
				if p := s.sessionPlatform(r.ID); p != "" {
					item["platform"] = p
				}
				if r.Lang != "" {
					item["lang"] = r.Lang
				}
				if r.CreatedByIntent > 0 {
					item["created_by_intent"] = r.CreatedByIntent
				}
				if r.HostAssetID > 0 {
					item["host_asset_id"] = r.HostAssetID
				}
				if len(omitted) > 0 {
					item["metadata_omitted"] = omitted
				}
				items = append(items, item)
			}
			for {
				alive := 0
				for _, it := range items {
					if it["status"] == db.SessionAlive {
						alive++
					}
				}
				hasMore := moreRows || len(items) < len(recs)
				out := map[string]any{
					"count": len(items), "alive": alive, "items": items,
					"limit": limit, "offset": offset, "has_more": hasMore, "next_offset": offset + len(items),
				}
				encoded, err := json.Marshal(out)
				if err != nil {
					return actool.Errorf("会话元数据编码失败"), nil
				}
				if len(encoded) <= maxSessionToolOutput {
					return actool.Text(string(encoded)), nil
				}
				if len(items) <= 1 {
					return actool.Errorf("单条会话元数据超出输出预算，请检查能力提供者"), nil
				}
				items = items[:len(items)-1]
			}
		},
	})
}

// registerSessionTool 让 worker 把拿到的 webshell 登记为平台管理的立足点会话：
// 先无害探针探测马型，成功才落库登记；失败返回探针回显摘要，不伪造成功。
func (s *Server) registerSessionTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "register_session",
		Description: "【立足点主通道】把已拿到的 webshell 登记为平台管理的会话。" +
			"马型:PHP eval 一句话(php)、PHP 命令执行马(phpcmd:POST 参数直接是 shell 命令、马体直接 exec 回显,无 eval)、jsp / aspx / auto(自动探测,eval 探针与 cmd 探针分开判型);自研加密马(phpenc/jspenc,由 generate_webshell 生成)," +
			"加密马全密文通信、每会话独立密钥,登记时必须传 secret=generate_webshell 返回的 key_b64。" +
			"先向 URL 发无害探针（明文马 echo 哨兵；加密马 op=probe 加密探针）探测马型与可用执行函数，探测成功才登记，" +
			"返回 session_id、马型（eval/cmd/jsp/aspx/enc)、可用执行函数、disable_functions 指纹与内建 LD_PRELOAD 绕过可用性；" +
			"失败返回明确原因（区分马不可达/探针协议不匹配/疑似被拦，附回显摘要，不伪造）。登记时目标 host 会过 RoE 任务范围校验。" +
			"立足点策略：webshell 优先为主通道；后续 session_exec/session_read/session_write 都经它在目标侧执行，注意 OPSEC。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":      strParam("webshell 完整 URL，含协议（必填）"),
				"password": strParam("连接密码（POST 字段名，默认 cmd；加密马不需要）"),
				"lang":     strParam("马语言：php / phpcmd(PHP 命令执行马,POST 参数直接是 shell 命令) / jsp / aspx / auto（明文马，默认 auto）;phpenc / jspenc（自研加密马）"),
				"secret":   strParam("加密马密钥（仅 phpenc/jspenc 必填：generate_webshell 返回的 key_b64)"),
				"control_tunnel_id": map[string]any{"type": "integer", "description": "（可选）控制通道经哪条 socks 隧道出去。目标只在隧道另一侧可达时必须给：不给则探针/执行都直连，必然连不上。经隧道登记时必须显式指定 lang（auto 判型无法统一走隧道）"},
			},
			"required": []any{"url"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				URL             string `json:"url"`
				Password        string `json:"password"`
				Lang            string `json:"lang"`
				Secret          string `json:"secret"`
				ControlTunnelID int64  `json:"control_tunnel_id"`
			}
			if err := s.requireResourceTask(ctx); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			_ = json.Unmarshal(in, &a)
			a.URL = strings.TrimSpace(a.URL)
			if a.URL == "" {
				return actool.Errorf("url 为必填参数"), nil
			}
			warn, roeErr := s.sessionRoECheck(ctx, a.URL)
			if roeErr != nil {
				return actool.Errorf(roeErr.Error()), nil
			}
			var sh *session.HTTPShell
			var info session.ProbeInfo
			var err error
			// 控制通道路由（I1）：先解析出口，再探测——探针本身也要走这条路，
			// 否则「目标只在隧道另一侧」的会话永远探测失败，路由等于没配。
			// sessionID 传 0：此时会话还没落库，新 id 不可能被隧道引用，防环检查
			// 留到登记之后由会话侧保证（见 controlRouteUpstream 的注释）。
			controlUpstream, cerr := s.controlRouteUpstream(ctx, a.ControlTunnelID, 0)
			if cerr != nil {
				return actool.Errorf(cerr.Error()), nil
			}
			if a.Lang == "phpenc" || a.Lang == "jspenc" {
				// 模型常把加密马密钥本能地放进 password(一句话马的连接密钥语义),
				// secret 为空时接受 password 作为别名,避免可预期的误用循环。
				if strings.TrimSpace(a.Secret) == "" {
					a.Secret = strings.TrimSpace(a.Password)
				}
				if a.Secret == "" {
					return actool.Errorf("加密马(" + a.Lang + "）登记需要密钥：把 generate_webshell 返回的 key_b64 放进 secret(或 password)参数"), nil
				}
				sh, info, err = session.ProbeEncVia(ctx, a.URL, a.Lang, a.Secret, controlUpstream)
			} else if controlUpstream != "" {
				sh, info, err = session.ProbeVia(ctx, a.URL, a.Password, a.Lang, controlUpstream)
			} else {
				sh, info, err = session.Probe(ctx, a.URL, a.Password, a.Lang)
			}
			if err != nil {
				return actool.Errorf("探测失败：" + err.Error()), nil
			}
			secret, _ := json.Marshal(sh.Secret())
			ri := agent.RunInfoFrom(ctx)
			rec := &db.SessionRecord{
				Kind: info.Kind, URL: a.URL, Secret: secret, Lang: info.Lang,
				Status: db.SessionAlive, CreatedByTask: ri.TaskID, CreatedByIntent: ri.IntentID,
			}
			id, err := s.sessStore.Create(ctx, rec)
			if err != nil {
				return actool.Errorf("会话落库失败：" + err.Error()), nil
			}
			sh.SetID(id)
			s.sessReg.Add(sh)
			// webshell 本身登记为 endpoint 资产（复用 insert_assets 路径）;best-effort。
			var assetID int64
			if s.m.Assets() != nil {
				if aid, aerr := s.m.Assets().UpsertEndpoint(db.UpsertEndpointReq{
					URL: a.URL, Method: "POST", TaskID: ri.TaskID,
				}); aerr == nil {
					assetID = aid
					s.sessStore.SetHostAsset(ctx, id, aid)
				}
			}
			out := map[string]any{
				"session_id": id, "kind": info.Kind, "lang": info.Lang,
				"variant": info.Variant, "funcs": info.Funcs, "platform": info.Platform,
				"shell_type": shellTypeLabel(info), "message": registerMessage(info),
			}
			// 控制通道路由要回显：否则 agent 不知道「这次是直连还是经隧道」，
			// 后续执行失败时会去猜是马死了还是出口错了。空串 = 直连。
			if cp := sh.ControlProxyURL(); cp != "" {
				out["control_route"] = cp
			}
			// 立足点登记是内网作战的起点事件：写进任务活动流，让人和 planner 都能
			// 看到「什么时候拿到的立足点」。此前只落 sessions 表，活动流一片空白。
			s.noteSessionRegistered(ri.TaskID, id, info.Kind, hostOfURL(a.URL))
			// on_foothold 半自动移交（pending_handoff.go）：立足点出现即登记「待
			// 移交内网」并写通知卡。执行侧仍须人工批准——内网授权边界不因发现
			// 侧自动化而放松。人不再需要盯着会话台账等这一刻。
			s.notePendingHandoff(ri.TaskID, id, hostOfURL(a.URL))
			if assetID > 0 {
				out["host_asset_id"] = assetID
			}
			// disable_functions 指纹与内建绕过可用性（复盘 R1)：如实上报，不伪造。
			if strings.HasPrefix(info.Lang, "php") && info.Variant != "cmd" {
				out["disable_functions"] = info.DisableFunctions
				out["mail"], out["putenv"] = info.Mail, info.Putenv
				if info.Bypass != "" {
					out["bypass"], out["bypass_lib"] = info.Bypass, info.BypassLib
				}
				if info.BypassNote != "" {
					out["bypass_note"] = info.BypassNote
				}
			}
			if len(info.Funcs) == 0 && strings.HasPrefix(info.Lang, "php") && info.Variant != "cmd" {
				if info.Bypass != "" {
					out["warning"] = "目标 PHP 禁用了全部命令执行函数（disable_functions);session_exec 直执行不可用，文件读写仍可用；" +
						"已登记 LD_PRELOAD 绕过（.so 已投递 " + info.BypassLib + "),session_exec 接通绕过执行是后续项"
				} else {
					out["warning"] = "目标 PHP 禁用了全部命令执行函数（disable_functions);session_exec 不可用，文件读写仍可用"
				}
			}
			if warn != "" {
				out["roe_warning"] = warn
			}
			return jsonResult(out)
		},
	})
}

// shellTypeLabel 给模型的人类可读马型标签。
func shellTypeLabel(info session.ProbeInfo) string {
	switch {
	case strings.HasSuffix(info.Lang, "enc"):
		return "enc"
	case info.Lang == "phpcmd" || info.Variant == "cmd":
		return "cmd"
	case info.Lang == "php":
		return "eval"
	default:
		return info.Lang
	}
}

// registerMessage 是登记结论的明示文案（复盘 R1：命令马登记成功必须明示）。
func registerMessage(info session.ProbeInfo) string {
	switch shellTypeLabel(info) {
	case "cmd":
		return "已登记为命令执行马（cmdshell):session_exec 直接透传 shell 命令（哨兵同步），文件读写走 shell base64 通道"
	case "enc":
		return "已登记为加密会话（" + info.Lang + ")：全密文通信"
	case "eval":
		if info.DisableFunctions {
			if info.Bypass != "" {
				return "已登记为 PHP eval 马；disable_functions 全禁，session_exec 直执行不可用，已确认 LD_PRELOAD 绕过可用"
			}
			return "已登记为 PHP eval 马；disable_functions 全禁，session_exec 不可用，仅文件读写"
		}
		return "已登记为 PHP eval 马（" + info.Variant + " 族）"
	default:
		return "已登记为 " + info.Lang + " 会话"
	}
}

// generateWebshellTool 生成自研加密 webshell 马体（phpenc/jspenc):AES-256-GCM
// 全密文通信 + 随机填充（PHP 无 GCM 时马体运行时降级 CBC+HMAC)，每会话独立随机
// 密钥，变量/函数名每份随机化。工具只在平台侧渲染模板，不触碰目标。
func (s *Server) generateWebshellTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "generate_webshell",
		Description: "生成自研加密 webshell 马体（phpenc/jspenc)：请求/响应全密文（AES-256-GCM,PHP 无 GCM 时马体自动降级 CBC+HMAC)、随机填充杀流量签名、" +
			"每会话独立随机密钥、变量名随机化；可选轻度混淆（字符串拆分/拼接，不用 eval(base64) 高特征形态）。" +
			"返回马体内容 content、建议文件名 filename、密钥 key_b64 与密钥摘要。" +
			"用法：把 content 以上述文件名上传到目标（经已有立足点或文件上传漏洞），然后 register_session(lang=phpenc|jspenc, secret=key_b64) 登记为加密会话。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"lang":      strParam("马体语言：php / jsp（必填）"),
				"obfuscate": map[string]any{"type": "boolean", "description": "PHP 马体轻度语法变形（字符串拆分/拼接），默认 false"},
				"note":      strParam("备注（随生成结果返回，便于审计对照，可选）"),
			},
			"required": []any{"lang"},
		},
		ReadOnly:    func(json.RawMessage) bool { return true }, // 只读：平台侧渲染，无外部副作用
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				Lang      string `json:"lang"`
				Obfuscate bool   `json:"obfuscate"`
				Note      string `json:"note"`
			}
			_ = json.Unmarshal(in, &a)
			gen, err := session.GenerateShell(a.Lang, a.Obfuscate, a.Note)
			if err != nil {
				return actool.Errorf("马体生成失败：" + err.Error()), nil
			}
			return jsonResult(gen)
		},
	})
}

// sessionExecTool 经会话在目标侧执行命令（目标侧执行 = 网络位置在立足点）。
func (s *Server) sessionExecTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "session_exec",
		Description: "经已登记的立足点会话在【目标侧】执行一条 shell 命令（立足点主通道，webshell 优先）。" +
			"复杂命令（含引号/换行）自动 base64 落盘到目标临时文件再执行，用完清理；临时文件按调用 worker 的" +
			"task_intent 前缀命名空间隔离，同会话执行串行（防多 worker 输出串台）。" +
			"返回 stdout/stderr;stderr 与 stdout 合并（webshell 通道无法分离）;输出超 64KB 截断标注。" +
			"命令在目标机器上运行，注意 OPSEC（日志/进程/网络留痕）。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "integer", "description": "register_session 返回的会话 id"},
				"command":    strParam("要在目标侧执行的 shell 命令"),
				"timeout":    map[string]any{"type": "integer", "description": "超时秒数（可选，默认 15)"},
			},
			"required": []any{"session_id", "command"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				SessionID int64  `json:"session_id"`
				Command   string `json:"command"`
				Timeout   int    `json:"timeout"`
			}
			_ = json.Unmarshal(in, &a)
			sess, err := s.lookupSession(ctx, a.SessionID, db.SessionActionExec)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			// 能力前置检查（I2）：驱动明确声明不支持执行时直接返回能力缺失，
			// 不发包。此前这里会照发，然后由驱动层失败——agent 收到的是目标侧
			// 形态的错误，很容易被记成「会话失效 / 命令被拦」，而真实原因是
			// 这个通道根本不支持执行。
			ct := capability.For(sess)
			if ok, reason := ct.Check(capability.OpExec); !ok {
				return actool.Errorf("capability_missing: " + reason + "。改用 session_read / session_write，或换一个支持执行的立足点。"), nil
			}
			// 方言预检：高置信不匹配时**不发包**，直接给出该方言下的等价命令。
			// 判据只看首个 token（见 capability.CheckCommand），误判率低；
			// 判不出来就放行，绝不因为「未知」而拦。
			if v, reason := capability.CheckCommand(ct, a.Command); v == capability.VerdictMismatch {
				return actool.Errorf("dialect_mismatch（未发包）: " + reason), nil
			}
			timeout := time.Duration(a.Timeout) * time.Second
			// per-worker 命名空间（复盘 R5)：目标侧临时文件按 task_intent 前缀
			// 注入，多 worker 共用同一会话不再互相覆盖落盘脚本；驱动内
			// per-session 锁保证同会话执行串行，防输出串台。
			ri := agent.RunInfoFrom(ctx)
			// 租约（I4）：登记「本任务正在用这个会话」。删会话时据此拦住
			// 「另一个任务还在跑，却被单方面掐断」。best-effort，见 noteSessionLease。
			s.noteSessionLease(ctx, a.SessionID, "exec")
			execCtx := session.WithTmpPrefix(ctx, session.WorkerTmpPrefix(ri.TaskID, ri.IntentID))
			stdout, stderr, err := sess.Exec(execCtx, a.Command, timeout)
			if err != nil {
				return actool.Errorf("执行失败：" + err.Error()), nil
			}
			s.sessStore.Touch(ctx, a.SessionID)
			out := map[string]any{"stdout": truncateOutput(stdout), "stderr": truncateOutput(stderr)}
			if len(stdout) > maxSessionToolOutput || len(stderr) > maxSessionToolOutput {
				out["truncated"] = true
			}
			// 空输出 + 拿不到退出码，是 agent 最容易误判成「成功」的形态。
			// 只在这个形态下补一句，正常路径不加字节。
			if stdout == "" && stderr == "" && ct.ExitCode != session.CapabilitySupported {
				out["note"] = "命令返回空输出，且本通道无法取退出码（" + string(ct.ExitCode) + "）：" +
					"空输出不等于成功，也可能是失败但无回显。要判定成败请显式带哨兵：" +
					"linux 用 `; echo __RC=$?`，windows 用 `& echo __RC=!ERRORLEVEL!`（需 cmd /v:on）。"
			}
			return jsonResult(out)
		},
	})
}

// sessionReadTool 经会话读目标侧文件（大文件分块）。
func (s *Server) sessionReadTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "session_read",
		Description: "经立足点会话读取目标侧文件（大文件自动 512KB 分块）。" +
			"文本返回 content（超 64KB 截断标注）;二进制返回 content_base64。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "integer", "description": "会话 id"},
				"path":       strParam("目标侧文件绝对路径"),
			},
			"required": []any{"session_id", "path"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				SessionID int64  `json:"session_id"`
				Path      string `json:"path"`
			}
			_ = json.Unmarshal(in, &a)
			sess, err := s.lookupSession(ctx, a.SessionID, db.SessionActionRead)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if ok, reason := capability.For(sess).Check(capability.OpRead); !ok {
				return actool.Errorf("capability_missing: " + reason + "。改用 session_exec 走命令通道读取。"), nil
			}
			data, err := sess.ReadFile(ctx, strings.TrimSpace(a.Path))
			if err != nil {
				return actool.Errorf("读取失败：" + err.Error()), nil
			}
			s.sessStore.Touch(ctx, a.SessionID)
			out := map[string]any{"size": len(data)}
			truncated := false
			if len(data) > maxSessionToolOutput {
				data = data[:maxSessionToolOutput]
				truncated = true
			}
			if utf8.Valid(data) {
				out["content"] = string(data)
			} else {
				out["content_base64"] = base64.StdEncoding.EncodeToString(data)
			}
			if truncated {
				out["truncated"] = true
			}
			return jsonResult(out)
		},
	})
}

// sessionWriteTool 经会话写目标侧文件（大文件分块 append)。
func (s *Server) sessionWriteTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "session_write",
		Description: "经立足点会话向目标侧写文件（二进制安全，大文件自动 512KB 分块 append)。" +
			"写文件是高危写操作，注意 OPSEC（落盘有被查杀引擎发现的检测面）。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id":     map[string]any{"type": "integer", "description": "会话 id"},
				"path":           strParam("目标侧文件绝对路径"),
				"content":        strParam("文本内容（与 content_base64 二选一）"),
				"content_base64": strParam("二进制内容的 base64（与 content 二选一，优先）"),
			},
			"required": []any{"session_id", "path"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				SessionID     int64  `json:"session_id"`
				Path          string `json:"path"`
				Content       string `json:"content"`
				ContentBase64 string `json:"content_base64"`
			}
			_ = json.Unmarshal(in, &a)
			sess, err := s.lookupSession(ctx, a.SessionID, db.SessionActionWrite)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if ok, reason := capability.For(sess).Check(capability.OpWrite); !ok {
				return actool.Errorf("capability_missing: " + reason + "。改用 session_exec 走命令通道落盘（base64 解码写文件）。"), nil
			}
			data := []byte(a.Content)
			if a.ContentBase64 != "" {
				data, err = base64.StdEncoding.DecodeString(a.ContentBase64)
				if err != nil {
					return actool.Errorf("content_base64 解码失败：" + err.Error()), nil
				}
			}
			if err := sess.WriteFile(ctx, strings.TrimSpace(a.Path), data); err != nil {
				return actool.Errorf("写入失败：" + err.Error()), nil
			}
			s.sessStore.Touch(ctx, a.SessionID)
			return jsonResult(map[string]any{"bytes": len(data), "path": a.Path})
		},
	})
}

// truncateOutput 截断超长输出并标注。
func truncateOutput(s string) string {
	if len(s) <= maxSessionToolOutput {
		return s
	}
	return s[:maxSessionToolOutput] + fmt.Sprintf("\n…[输出截断：共 %d 字节，仅返回前 %d]", len(s), maxSessionToolOutput)
}

// seedSessionTools 把会话工具 seed 进 tools 表。
// 写类工具只绑 worker；只读的 session_list 额外绑 planner（见 plannerReadableIntranetTools）。
func (s *Server) seedSessionTools() {
	for _, t := range s.sessionTools() {
		schema, _ := json.Marshal(t.InputSchema())
		agents, _ := json.Marshal(toolAgentBindings(t.Name()))
		if err := s.m.PG().SeedTool(t.Name(), t.Description(), schema, agents); err != nil {
			log.Printf("[tools] seed %s 失败： %v", t.Name(), err)
		}
	}
}
