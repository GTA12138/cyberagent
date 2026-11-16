# YMH 内网能力与链路评审（2026-10-21）

> **【已归档 · 2026-10-23 文档审核】** 内网视角源码评审输入。结论已并入方向文件 §7 的 S0 / I1–I4 / O1–O2 工作包定义与 §7.0 进度表，本文不再单独维护。
> 本文是历史记录，不是当前状态：文中写的「未做」可能已完成，文中写的「已完成」也可能已被后续改动推翻。
> 现役进度 → [能力评估与演进方案 §7.0](YMH-能力评估与演进方案.md) ｜ 唯一待办源 → [YMH-待办总表](../2026-10-23/YMH-待办总表.md)

本报告仅审阅源码和公开项目文档。未读取配置秘密、真实任务数据或数据库，未对任何目标发起探测。下文“缺口”指源码接线或模型语义；没有运行真实内网端到端环境，不把静态推断写成实测故障。

## 结论

YMH 内网能力的基础已经是“会话 + 被动侦察 + 受管隧道 + 任务代理 + 凭据实体 + 探索图”，并非只有提示词。当前最该扩展的是执行上下文、按目标路由、证据存储解析、资源生命周期和能力契约，而不是先增加隧道种类或再写一组横向命令提示词。

实际路径分两条：

1. 控制目标：worker → session 工具 → Session Registry → HTTPShell 或 Penelope MCP 适配器 → 目标侧执行。HTTP 会话显式由平台直连，reverse 的会话在受管 Penelope 进程中；SSH 仍只有契约/注释预留。
2. 访问内网：worker 的 Bash HTTP/WebFetch → 每任务 MITM → 任务当前最新 alive SOCKS → Chisel/Suo5 → 内网目标。Bash 的原生 TCP/UDP、忽略代理环境变量的程序不自动获得这条路径。Browser MCP、session HTTP 控制面、流量检索和证据绑定目前也没有统一接入该上下文。

现有能力应保留：session 的 AES-GCM 落库、随机哨兵、每会话串行锁与每 worker 临时命名空间；session_recon 的逐条执行和结构化回库；tunnel 的 probe/plan/deploy/health、随机令牌、逐层回滚、真实 SOCKS 验证、指数退避重拉、端口台账；stage 的硬 TTL 与一次性下载；pending_scope 的人工范围扩展；reverse 启动后诚实标死。不能将这些已实现能力描述为待从零建设。

## 证据与八项建议

### 1. P0：执行上下文与流量存储解析统一

证据：`E:/cyberagent/artex-ymh-main/artex-ymh-main/agent/worker.go:475` 选择任务代理；`server/taskproxy.go:141` 为任务独立 traffic.Open，拥有独立 SQLite 索引和正文树。但 `server/manager.go:922` 的 HostTools 恒返全局 traffic.Tools，`server/finding_traffic.go:23` 的 evidenceStore 恒收全局 traffic，最终 `evidence/store.go:168` 只读取这个固定 Traffic。`server/manager.go:729` Browser MCP 也取全局代理。没有发现任务实例聚合查询或委派读取兜底。`server/manager.go:971` 又用抓包开关阻断 TaskProxyForTask，因此“关闭记录”会连带影响内网出口。

改法：引入 TaskExecutionContext（task、intent、executor、route、route_version、traffic_store_id、identity_ref）；TrafficResolver 解析当前与历史证据存储；TrafficRef 改为 store_id + exchange_id，并验证任务所有权。Browser、WebFetch、Bash、查询、证据快照、归档都消费同一上下文。网络路由与 capture 开关分开。路由切换后保留旧 store 可读，不把检索工具简单指向“当前代理实例”了事。

验收：两个并发任务访问相同 host，经各自不同出口，三类客户端路径一致、证据可绑定且不串任务；关闭 capture 后路由仍有效；切换/关闭实例后旧流量仍可供报告与归档读取。

### 2. P0：从“最新 SOCKS”改为按目的地解析路径

证据：`server/taskproxy.go:70` 的 pickUpstream 选 ID 最大 alive SOCKS；`tunnel/plan.go:22` Plan 只有 via_session 与 callback 平台地址，没有 parent_route/executor；`server/intranet.go:651` 所有拓扑边都以 platform 为起点。多条隧道可以存在，但尚不是具备逐跳依赖、目标亲和和路径版本的多跳路由器。

改法：新增 Route/ReachabilityEdge，记录 source executor/session、destination、协议、DNS 位置、parent_route、scope_version、验证时间与状态。RouteResolver 在单次工具调用前按 destination 选路径，给 intent/call 发 route lease；任务代理内部按目标分流，避免新增一条隧道就切换整个任务。保留显式指定路径；无已验证路径时返回不可达原因，内网任务默认不自动回落直连。拓扑必须由运行时相同的路径模型投影。

验收：同任务两不可互通网段同时调用不会抢出口；父 route 故障使子 route 进入 degraded 并阻止新调用；修复局部路径后恢复；路径无环，路由解释可显示真正的每一跳。

### 3. P0/P1：会话控制面也使用独立的路由引用

证据：`session/http_shell.go:99` DirectHTTPClient 中明确 Proxy:nil；加密会话也复用此客户端；`tunnel/suo5.go:339` payload 探测客户端同样直连。因此“worker 可经 SOCKS 访问深层地址”不代表“深层地址可登记为 Session 并持续管理”。这属于真正执行接线问题。

改法：SessionRecord 增 control_route_ref，驱动通过 typed Dialer/TransportFactory 创建连接；以父 route 消费 Session 控制流。不要把当前任务 MITM 无条件用于所有 Session 请求，否则当该 MITM 的上游依赖被控 Session 时可能出现路由环。Suo5 控制/探测也经相同 route 引用。第一阶段保留本地 server executor，之后再加远程 executor，不必立即落地整套分布式节点。

验收：一个仅能经第一条已建立路径访问的第二层测试 Session 可登记、Exec、Read/Write、恢复；父路径断开时给出 dependency_unavailable；禁止自依赖和环；不会因失败偷偷改为平台直连。

### 4. P0：资源所有权和结构化操作检查前置

证据：`server/session.go:540` lookupSession 仅按全局 Registry ID 查找，无 task 参数或所有权检查；`server/tunnel.go:458` teardown 直接按 tunnel ID 操作。`guard/guard.go:67` 只从 Bash/shell_open/shell_send 提取命令，`guard/roe.go:156` 只支持命令和 WebFetch URL；session_exec 的目标及命令没有进入通用范围抽取。register_session 与 portfwd 虽有局部范围检查，不能覆盖已有资源的每次使用。`intercept/intercept.go:149` 默认纳入 session 工具，说明拦截系统存在，不能说没有 guard。

改法：工具先解析 OperationEnvelope（task、resource、executor、target、effect、scope_version），统一校验资源所有权/显式共享授权和目的地范围，再执行或审批。lookupSession 改为 context-aware；tunnel teardown 同样约束。新增结构化检查补足命令正则，保留既有拦截规则和人工审批。

验收：任务 A 引用任务 B 的 Session/Tunnel ID 会失败；显式共享授权后才可用；scope 收缩后旧 Session 不自动保留新操作权限；任务动作的审计能说明执行位置和目标，而不是只有空 command。

### 5. P1：依赖资源回收与远端作业状态诚实化

证据：`tunnel/health.go:232` 已有 Cleanup，但全仓 Go 检索未找到调用者；`server/engine.go:301` 删除/归档处理关闭任务 MITM，未见隧道 Cleanup 接线。`server/intranet.go:432` 删除 Session 先 Registry.Remove/Close，未先处理依赖隧道。`tunnel/health.go:200` 和 `:215` 远端不可清理仅日志，最终仍 stopped。`server/reverse.go:660` 明确 timeout 只终止平台等待，目标命令可能继续运行。stage 已有 TTL，不能把所有资源笼统说成无 TTL。

改法：把 Session/Tunnel/RemoteJob/Stage 纳入 ResourceLease 与依赖图；任务 cancel/timeout/delete/archive 调用受管资源 draining；先子资源后父资源；cleanup_pending、local_closed、remote_cleanup_verified 分开。取消不支持的驱动返回 cancel_unsupported/unknown，禁止误报“已停止”。服务器重启对账，清理失败保留可重试条目与有界预算，避免健康巡检重新部署已结束任务资源。

验收：取消任务、删除会话、服务重启、网络中断四类故障注入，资源状态与实际进程/监听一致；暂不可清理有留痕和重试；终止任务不被巡检重新激活；remote timeout 后下次调用不会误认前条任务已结束。

### 6. P1：OS/架构/协议能力契约和适配器选择

证据：`server/session_recon.go:31` 命令包固定 Linux，虽然 recon 解析器支持 Windows 输出；`tunnel/plan.go:123` 固定 /tmp，`tunnel/deploy.go:293` 使用 POSIX 下载命令、后续 chmod，管理器只取同一个 chiselPath。`server/reverse.go:168` 文件路径能力本期面向 Unix；SSH 在 `server/session.go:74` 恢复分支中没有驱动。因此工具名称和解析器覆盖面不等于可运行的跨平台能力。

改法：Session.Capabilities 返回 platform/arch/executors/filesystem/read/write/cancel/stream/native_tcp/pty 等；Manifest 描述二进制版本、平台架构和哈希；recon/deploy 根据能力选择有界命令集/投递器，缺能力时明确拒绝。原生 TCP/UDP 应通过具备协议声明的执行器，不依靠 HTTP_PROXY 环境变量暗示支持。

验收：Linux 与 Windows 本地模拟驱动可选择正确命令、路径和二进制；不支持的组合执行前报错；代理兼容与原生协议能力显式展示；版本/架构不匹配不会通过“部署成功”判断。

### 7. P1：凭据从静态先验升级为带认证证据的主体关系

证据：`db/credentials.go:60` 已有加密存储，`:212` 有 verified 更新；但 `server/credentials.go:45` 描述让模型 verify=true 重新登记，实际 `:49` schema 与 `:61` struct 没有 verified 输入。`db/credential_score.go:80` 只要 cred.Domain 非空就给每个候选加分，没有验证候选属于该域；top 10 不结合 route 可达性、候选授权或服务认证结果。该工具自己也称“成功率先验”，不能说它已实证认证成功。

改法：保留 Credential 引用，新增 Principal、AuthContext 和 AuthObservation（target、service、principal、credential_ref、route_version、time、outcome、evidence_ref）。verified 不再是跨所有目标的全局布尔结论；认证适用性先按协议、域、可达性、授权过滤，再按实证与先验排序；记录失败类别和尝试预算。Agent 默认拿引用，按需受控使用，避免秘密反复进入上下文。

验收：不同域/不同服务候选不会因为一个 domain 字符串统一升权；一次成功只影响相应目标/服务；历史失败可解释且限制重复；verify 描述与工具 schema 一致；secret 不出现在检索摘要。

### 8. P1：被动侦察区分本地监听、已观察连接和可达性推断

证据：`recon/recon.go:56` Listener 只有 Port/Proto，没有 BindAddr；`server/session_recon.go:162` 取第一块非 loopback NIC，再 `:187` 将所有监听记为该 IP 的服务，容易把仅回环监听标成外部接口服务。`server/session_recon.go:228` 写 fact 的 evidence 只是固定命令包说明，`:229` 统一 observed，但 detail 包含推断，不持久化逐条原始输出引用。

改法：Observation 保存 session、executor、command/tool_call、output artifact、parsed fields、时间和 source_ref；推断另记录 inferred、依据和失效条件。Listener 保留绑定地址/接口/proto，服务标记 local_only/unknown_exposure，不能从监听推定任意来源可访问。Reachability 必须带 source、destination、protocol、tested_at，避免把既有 ESTABLISHED 连接当永久双向可达。图的每个关键关系可追溯到原始证据。

验收：多网卡、回环监听、IPv6、过期 ARP、单向连接等固定样本保留真实语义；事实置信级与推断分离；拓扑与 planner 不据未经验证的本地监听推导外部可达。

## 提示词与编排配合

保留已有内网纪律和 pivot 骨架，用机器状态替代重复扩写口号：每次意图输入含 execution_location、selected_route/version、available_capabilities、identity_ref、prerequisite_observations、success_check、stop_condition、cleanup_contract。planner 只提候选意图，确定性层筛掉授权/路由/协议/资源条件不满足者，再由 worker 执行；证据验证更新状态后才解锁下一条。并发约束按 Session/Route/Principal/资源划分，不靠全任务统一提高 worker 数量。

这一结构可借鉴 CALDERA 的 executors、requirements、facts/relationships、cleanup 与 watchdog 的明确契约，不照搬 C2 或整个命令库。官方资料：[构建 Agents](https://caldera.readthedocs.io/en/latest/How-to-Build-Agents.html)、[构建 Planners](https://caldera.readthedocs.io/en/5.0.0/How-to-Build-Planners.html)。其中联系人/执行器/知识来源/停止条件和清理是可复用的工程分层；YMH 的资产/探索双图及 LLM 自适应规划可保留。

## 建议实施顺序

1. 先打通执行上下文、任务流量/证据解析、网络与 capture 解耦，并建立双任务并发与故障验收。
2. 再做按目标路径、Session 控制路由、能力与资源所有权契约；保留单机 executor 兼容。
3. 然后完善资源依赖回收、跨平台适配、认证观察和带来源的网络关系。
4. 以上稳定后，复用 `E:/cyberagent/artex-ymh-main/artex-ymh-main/DISTRIBUTED-DESIGN.md:95` 的 node 亲和思路增远程执行器；该设计文件存在，但仓库未见 ToolTransport/node 协议实现，不应把它当作已有分布式运行时。
