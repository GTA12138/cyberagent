# YMH Web 能力源码评估（2026-10-21）

> **【已归档 · 2026-10-23 文档审核】** Web 视角源码评审输入。结论已并入方向文件 §7 的 W1–W4 / O2 工作包定义。
> 本文是历史记录，不是当前状态：文中写的「未做」可能已完成，文中写的「已完成」也可能已被后续改动推翻。
> 现役进度 → [能力评估与演进方案 §7.0](YMH-能力评估与演进方案.md) ｜ 唯一待办源 → [YMH-待办总表](../2026-10-23/YMH-待办总表.md)

本评估只读取源码与公开上游仓库，未读取运行配置、密钥或任务数据，未扫描任何目标，未运行集成验证。以下“缺口”指本次读取的内置源码，不能据此断言用户数据库里没有自定义工具或其他 MCP。源代码静态推断与运行事实须分开。

## 已有能力，应保留和接通

- Worker 单意图执行、资产/事实/漏洞分流写回；漏洞必须有本次真实触发证据：`agent/worker.go:281-291`。
- Bash 与 WebFetch 使用 MITM 与对应 CA；worker 支持任务级代理：`agent/worker.go:473-475,528-532`。
- HTTP 请求/响应、Host 头、全文索引与内容寻址大正文；大包不等于被丢弃：`traffic/traffic.go:761-837,978-991,2328-2445`。
- 漏洞证据独立快照，正文校验后存入证据仓，流量删除不应破坏已固化证据：`evidence/store.go:153-198`。
- 独立反证 verifier，三态结果，正常对照与 WAF/软 404 排除；还支持合并新证据后增量重验：`agent/verifier.go:4-17`、`server/finding_checks.go:146-159`。
- 静态 JS、路由、动态浏览器、mock 前端壳、参数矩阵、WS/SSE 枚举已在 API Recon skill 中存在：`skills/api-recon/SKILL.md:50-76`。
- 资产级粗覆盖与未测资产查询已存在：`db/task_scope.go:278-287,610`。Skill/MCP 延迟解锁也已存在：`server/assembly.go:54-68,111-144`。卡住检测已有确定性实现：`agent/stuck.go:10-35`。

## W1 / P0：统一执行、流量读取和证据的任务上下文

**证据。** Worker 在 `E:/cyberagent/artex-ymh-main/artex-ymh-main/agent/worker.go:475` 取任务代理；`server/taskproxy.go:141` 为每任务创建独立 `traffic.Open`。但 `server/manager.go:918-922` 的 HostTools 始终返回全局 `m.traffic.Tools()`，`server/finding_traffic.go:23` 的证据仓也始终读全局 `m.traffic`。浏览器 MCP 在 `server/manager.go:729` 取全局 ProxyAddr，`server/assembly.go:86` 直接按持久化 MCP 配置建立连接。Verifier 的 ChatAgent 在 `agent/chat.go:118,132-144` 只携带 SessionID 并使用全局代理。

**影响。** 从源码推断，开启 task_proxy 后，worker 新抓到的包可能不在其 traffic_search/get 和漏洞证据绑定的数据源里；浏览器与 verifier 也未必沿用该任务内网出口。未发现跨库读取适配，仍需集成复现确认。流量 ID 目前由秒级时间和实例内 sequence 产生（`traffic/traffic.go:772-774`），多个 store 间也不能只靠现有 ID 保证唯一。

**增量。** 引入 TaskExecutionContext 与 RouteResolver / TrafficResolver，Bash、WebFetch、browser、verifier 和 session 控制面消费同一任务上下文。RouteResolver 按 destination、executor、scope、route version 解析 typed dialer；工具调用固定 route lease，避免任务中两条隧道互相抢出口。证据引用改为 `store_id + exchange_id`，记录 task/intent/tool-call/auth-context/route-version；历史 store 注册持久化。任务路由不能依赖捕获开关。Browser 按任务和身份创建 context，MCP 配置在每次 run 中派生，不改全局记录。已有全局历史流量保留 legacy namespace。

**验收。** 两任务并发访问同一 hostname、各走不同 route；Bash/WebFetch/browser/verifier 的路由一致；检索能找到自己刚发的包；相同 exchange_id 不串库，不允许跨任务错绑；捕获关闭仍能访问批准的内网 route；父 route 失效时子 route 标 degraded，阻止新调用，历史证据照常读。内网评估代理交叉审阅补充：session 控制 route 必须独立声明 parent route，避免用依赖该 session 的 MITM 自己访问自己而成环。

## W2 / P1：增加 Web 认证上下文与多角色会话生命周期

**证据。** 已有加密 credentials 仓，支持 token（`db/credentials.go:25,30,136`）；资产还可附 AuthItem（`db/assets.go:51,66-72`）。但这些不等于浏览器会话管理。内置 endpoint 结构仅 method 与参数（`agent/tools_insert.go:123-125`）；Web 骨架要求双账号 IDOR 对照（`agent/chainskel/web.md:12`），缺少统一 role/tenant、cookie jar、storage state、CSRF 刷新和登录态健康模型。API Recon 明确限定无凭据 mock，不允许真实登录（`skills/api-recon/SKILL.md:21-22,41`），应保持该阶段边界。

**增量。** 在现有 credentials 上加 AuthContext：`credential_refs、principal、role、tenant、origin、storage_state_ref、session_version、expires_at、health_probe、refresh_recipe`。单独增加 web-auth 工作流，使用用户已授权身份做一次登录验证，将浏览器状态安全交给请求工具；匿名、角色 A/B、管理员 context 隔离。刷新加租约，避免并行 worker 互相撤销 token；测试计划显式写 auth_context_ref；身份不可用即 blocked_auth，不能算阴性结果。

**验收。** 本地夹具含过期 cookie、一次性 CSRF、刷新轮换、两角色两 tenant；下游 worker 可复用已验证状态；刷新并发不抢占；失效后不会以 401 证明不存在漏洞；日志与普通提示不暴露 secret。参考 Shannon 的真实浏览器认证预检与下游保存状态机制，按其 AGPL-3.0 许可评估代码复用：[validate-authentication.ts](https://github.com/KeygraphHQ/shannon/blob/main/apps/worker/src/services/validate-authentication.ts)。

## W3 / P1：把原始流量升级为可复现请求与差异实验

**证据。** 内置 traffic 工具是 search/get/blob 三个只读工具（`traffic/traffic.go:2334,2385,2406,2445`）；WebFetch 与 Bash 可发包，但源码内未见统一的“从真实包创建结构化修改→发送→对照”的工具。`traffic_get` 回给模型的请求最多 2500 字符、响应 4000 字符（`traffic/traffic.go:2402`），人工重新组装请求易漏参数和认证。大正文可 blob 读取，应保留而不是声称没有完整包。

**增量。** 加 `request_template_from_traffic`、`request_execute`、`response_compare`。Template 记录原请求 ref、method、URL、content type、参数位置、二进制 body ref、auth context 与 route。修改采用 typed patch；baseline/variant 每次执行自动生成 traffic ref 和 test_attempt_id。保留原始请求，JSON/DOM/状态变化/时序判据分别生成规范化差异；工具 success 仅表示 transport 成功，不能表示漏洞成立。协议差异研究需明确的 raw 模式，不能默认“正常化”破坏重复参数或编码语义。

**验收。** JSON/multipart、重复 query、二进制、CSRF、重定向、本地多角色对照夹具；baseline 与单变量 variant 可回放；自动绑定 evidence refs；内容噪声不造成假差异；跨域重定向每跳使用 scope/route 检查。Strix 已有 repeat_request 和分层 sitemap 可借鉴接口设计：[proxy/tools.py](https://github.com/usestrix/strix/blob/main/strix/tools/proxy/tools.py)。不因此迁移整套 Caido，先接通 YMH 现有仓。

## W4 / P1：把 API Recon 的候选面与真实后端观测分开保留

**证据。** API Recon 已有强方法论，但要求复制参考脚本、针对目标修改、固定阶段顺序（`skills/api-recon/SKILL.md:80-98`）。preload 已对 mock 标记 `source:'mock'`（`scripts/preload.js:257,316`），不能说它完全没有来源。runtime_harvest 在请求发送前先 rec.push，可能直接 stub；forward 时又将真实状态改为 200（`scripts/runtime_harvest.js:153-175`），其 rec 未保留同等明确的 mock/forward/original_status 字段。入资产只留下 method/url/params（`agent/tools_insert.go:123-125`），静态候选、模拟壳触发和真实后端已访问的区别易在交接中丢失。

**增量。** 产物版本化，加入 `discovery_source、observed_stage、mocked、sent_to_backend、original_status、original_body_ref、route、auth_context、traffic_refs、UI_trigger`。静态路径没有 method 时保持 unknown；mock 发现保留为候选，不计算已访问或已测。为成熟脚本提供可配置 runner 和 schema 校验，常见站点免复制；确有目标差异才开放 patch adapter。真实认证侦察走独立阶段，避免破坏原无凭据 Recon 的边界。

**验收。** 含 fake API、真实 401、动态拼接 URL、延迟路由、多页 SPA 的夹具，报告能准确区分 static candidate、mock emitted、backend observed、tested。保留完整原始响应，不将改写 200 当作正常基线；不会因缺 method 自动补 GET。

## W5 / P1：在已有资产覆盖之外增加测试覆盖账本

**证据。** 现有 Coverage 明确是粗资产覆盖，资产锚定任意 fact 即计 tested（`db/task_scope.go:278-280,323-327`；含直接关联来源的实现亦同，`db/task_assets_context.go:48-52`）。这无法区分“看过接口”与“认证/越权/输入验证/业务流程都测过”。目前提示已有覆盖矩阵原则（`agent/chainskel/meta_rules.go:20`），但仍主要靠模型叙述。

**增量。** 新增 `test_attempts`，维度为 endpoint template × auth role/tenant × parameter/operation × risk class；结果区分 planned/running/observed/ruled_out/blocked/not_applicable。保留现有粗资产 coverage 展示，同时增加有证据的 tested ledger。机器观测（发过包、调用工具、终止原因）与 agent 判断（已验证某假设）分列。预算终止、会话失效、工具故障不能写“无漏洞”。Planner 从账本采样高价值缺口，避免重复派工。

**验收。** 只写“站点可访问”fact 不会消掉接口认证/越权任务；超时或断网不会变 clean；相同 role/parameter/risk-class 有去重；角色/版本改变可让特定测试过期。Strix 源码已将 agent_reported 和 machine_observed 分开，并将预算截断标记 incomplete，可直接借鉴语义：[report/coverage.py](https://github.com/usestrix/strix/blob/main/strix/report/coverage.py)。

## W6 / P1：将提示中的强断言改成可检查的假设契约

**证据。** 已有反证 verifier（`agent/verifier.go:8-15`）以及 evidenceFirstRule（`agent/untrusted.go:13`），这是良好基础。但 Web 骨架仍含“唯一判据是某表达式回显某值”（`agent/chainskel/web.md:9`）和固定“先跑四件套”（`:13`）；meta 又写“工具阳性可信”（`agent/chainskel/meta_rules.go:24,31`）。这些强规则容易被固定文本回显、反射、缓存或扫描器误判骗过，与 verifier 的怀疑立场不完全一致。

**增量。** 每类 Web 验证输出 HypothesisContract：前置条件、正常对照、单变量变化、预期行为、排除项、证据 refs、失效条件和预算。提示只加载与 hypothesis 对应的判据；不能凭固定数字/状态码/工具 SUCCESS 下结论。给 negative/inconclusive 同等结构。把现有“一意图通常一条 fact”的摘要策略保留，但多个实验的机器记录分别存 test_attempts，不塞成巨大 fact。既有 deterministic stuck 检测保留，并接 request-level diff 来判断信息增益。

**验收。** 本地“直接反射算式结果文本”“统一 WAF 200”“缓存重放”“工具输出 success 但语义失败”“有信号但认证失效”夹具，模型不能误报；有双角色私有资源标记和稳定差异的真实阳性可通过；阴性结论列出未覆盖维度。Shannon 的 class queue 校验错误不会伪装为零漏洞，这一结果语义值得参考：[exploitation-checker.ts](https://github.com/KeygraphHQ/shannon/blob/main/apps/worker/src/services/exploitation-checker.ts)。

## W7 / P2：让捕获退化、网络失败和长连接能力变成可见状态

**证据。** traffic sink 只在有 Response 时持久化（`traffic/traffic.go:720-724`）；RequestError 仅触发可能的 passthrough（`:731`）；发生代理协议错误会暂时透传并停止记录，主要以日志说明（`:736-745`）。API Recon 已能枚举 WS/SSE（`runtime_harvest.js:105,160-162`），但不应据此声称平台 evidence 具有完整 WS/SSE 消息级验证能力。

**增量。** 增 `network_attempt` 与 capture_status：recorded/metadata_only/passthrough/error/unsupported；保存 DNS/connect/TLS/首字节/总时长、错误类型和证据缺失原因，给模型和 UI 同一视图。长连接采用分段事件产物，先只读观测与字节配额，逐步增加 WebSocket/SSE 消息关联；不强行让 HTTP body 流量表承担所有协议。异步持久化队列要有有界回压和 dropped counters，验证证据以持久化 ack 为准。

**验收。** 本地 TLS 错误、超时、协议异常触发透传、长 SSE、WS、满磁盘夹具均能诊断“目标失败/路由失败/录制失败”；空 traffic_search 不被解释为“未发包”或“目标无此行为”；证据缺失不被补造 ID。实际吞吐收益与队列参数应由本地 benchmark 后确定。

## 建议合并顺序

1. 先完成 W1 的全链路集成测试和最小修复；它是 Web 与内网共同前置。
2. W2 与 W3 一起交付，先有稳定身份和可复现请求，再增检测策略。
3. W4、W5、W6 形成“发现面→假设→实验→证据→覆盖缺口”的闭环；在本地夹具做冻结模型/提示版本的对照评测。
4. W7 逐步上线；先状态可观测，再扩展消息协议，不要先堆更多扫描器。

研究日期：2026-10-21。上游默认分支会变化；正式实现时应 pin commit 和检查许可。未引用 README 自报成绩作为独立有效性证明。
