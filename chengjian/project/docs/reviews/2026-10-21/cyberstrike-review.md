# CyberStrike 与 YMH：能力复核及可移植设计

> **【已归档 · 2026-10-23 文档审核】** CyberStrike 对照复核输入。可移植结论已并入方向文件 §5（外部能力集合与取舍）与 §7。
> 本文是历史记录，不是当前状态：文中写的「未做」可能已完成，文中写的「已完成」也可能已被后续改动推翻。
> 现役进度 → [能力评估与演进方案 §7.0](YMH-能力评估与演进方案.md) ｜ 唯一待办源 → [YMH-待办总表](../2026-10-23/YMH-待办总表.md)

日期：2026-10-21。方法：只读本地源码与方法论文档；未读取配置密钥、真实任务数据，未执行扫描、利用或工具安装。本报告不把工具名称、提示词能力清单视为运行成功证明。状态中的“已实现”仅表示代码路径存在，未做现场能力验证。

## 结论

保留 YMH 的事件驱动 planner/worker、资产图与探索图、受管工具目录、动态工具加载、轨迹持久化及冷节点压缩。应扩展其执行上下文、结构化 Web 观察模型及可校验的调度语义，不需要换成另一套通用多智能体框架。

CyberStrike 的主要借鉴价值是：把浏览器捕获的原始请求、凭据、UI 行为、业务对象汇总成应用模型，再用统一重放工具进行基线对照和按漏洞类别分派。不能据此推断其内网执行框架优于 YMH；它的内网专家很大部分依靠本机脚本执行、第三方二进制和外接 MCP/Bolt。

## 已核验能力矩阵

| 能力 | CyberStrike 实际状态与证据 | YMH 对照与判断 |
|---|---|---|
| 按领域/漏洞类别分派 | 已实现 Agent 注册和 Task 工具；`agent/agent.ts:424` 内网专家，`:480` Web 编排者，`:517` 分析者，`:744` 起注册 9 类测试者；`tool/task.ts:79` 对明显错分的测试类别执行调度检查 | 已有统一 worker 与 `chain_tags`，无需复制 9 套运行时；更适合增加按 capability/test family 选取的 worker profile |
| Web 应用模型 | 已实现请求模板去重和对象/角色/功能/凭据写入工具；`session/request.ts:241` 解释模板级去重、`:266` 使用 key_hash；`agent/prompt/analyzer/proxy-analyzer/prompt.txt:3` 定义提取对象、角色、功能和对象值的职责 | YMH 已有资产/事实/发现，不应另建孤立全局 Web 数据库；将 Web 实体作为资产类型，保留原始请求与观察关联 |
| 请求重放/对比 | 已实现结构化变异、认证替换、基线对照；`tool/http-replay.ts:178` 凭据替换、`:273` 工具、`:409` 对照路径；`:512` 有原始字节后端 | 应在已有流量和 evidence 上加 typed `replay/compare`，不要继续主要靠模型拼 curl |
| 浏览器探索 | wrapper 和独立进程生命周期已实现；`tool/hackbrowser.ts:22`、`tool/hackbrowser-launcher.ts:72`；实际 crawler 从外部包导入：`hackbrowser-subprocess/hackbrowser-worker.ts:19` | 可借鉴浏览器观察事件格式和进程隔离；浏览器工具可用不等于已覆盖业务流程，必须计算角色与流程覆盖 |
| 多角色访问上下文 | 已实现凭据标记和上下文注入；`tool/task.ts:195` 起注入凭据、`:223` 起推导访问 UI 上下文；Hackbrowser 的认证模式要求人工登录：`tool/hackbrowser.ts:30` | 内网凭据目录与 Web 登录身份不能混成一个无类型字符串；需要 credential_ref + auth_context_kind + subject/role + lifecycle |
| 内网能力 | 专家提示词+本机脚本+外部服务。`tool/winhook/shared.ts:50` 的 run 使用 Bun.spawn；`tool/linuxhook/shared.ts:65` 同类执行；`tool/attack-script.ts:67` 运行本地 python；MCP/Bolt 为外部能力通道 | 工具可执行位置、路由与协议支持才决定能力。YMH 已有 session/tunnel/credential 基础，应先统一执行上下文 |
| Bolt 能力聚合 | 已实现客户端连接和目录读取；`mcp/index.ts:589` 创建 Bolt，`:625` 拼接 `/mcp`，`:643` 使用 StreamableHTTP；`:724` 从远端 `/tools` 聚合目录。远端插件自身不在此路径实现 | 把“已配置”“已连接”“已列出工具”“能力探测通过”“在该 route 可用”分开，不把外接目录项算成内建能力 |
| 延迟工具发现 | 已实现 keyword search/load；`tool/lazy-registry.ts:153` 搜索、`:196` load；但预算超限仅告警，`:203` 分支没有淘汰行为 | YMH 已有更完整的 skill-gated/deferred 机制，`server/assembly.go:54`、`:111`、`:147`；不要把此项列成 YMH 缺失 |
| 方法论与证据校验 | 已实现 coverage note、启发式 chain 和 gates；`session/coverage-note.ts:40` append-only，`methodology/chain.ts:44` detect，`methodology/validation.ts:278` gates | 用作待验证候选和覆盖清单，不能把关键字命中/字数检查或类别百分比当漏洞证明 |
| 调度持久性 | 子会话可恢复，`tool/task.ts:117` 使用 task_id；HTTP ingest 队列是进程内 Promise 链：`session/ingest-queue.ts:8`、`:16`、`:63` | 保留 YMH DB frontier/intent CAS。不得用 Promise 队列替代可恢复领取及执行状态 |

上述 CyberStrike 路径均相对 `E:/cyberagent/CyberStrike/packages/cyberstrike/src/`。

## YMH 已有基础与真正的增量

1. `E:/cyberagent/artex-ymh-main/artex-ymh-main/agent/planner.go:21` 已是事件驱动 planner；`:46` 已有跨唤醒 todo。todo 仍为进程内 map，不能把它当完整的持久工作流语义。
2. `E:/cyberagent/artex-ymh-main/artex-ymh-main/agent/tools.go:1071` 已强制意图 parent 是 fact/finding，`:1125` 已保存血缘；因此“给意图增加来源”不是新功能。真正缺少的是机器可判定的 prerequisites 与不满足时的等待语义。
3. `E:/cyberagent/artex-ymh-main/artex-ymh-main/db/exploration.go:359` 已有 blocked 重开，`:374` 把 blocked 视为终结状态；`agent/terminalreason.go:37` 主要把模型/API 错误归到 blocked。应区分 transport_error 与 unmet_prerequisite，不能声称现在没有 blocked。
4. `E:/cyberagent/artex-ymh-main/artex-ymh-main/server/engine.go:1351` 领取路径当前是 Frontier(20) 后 ClaimIntent；`db/exploration.go:1025` 排序依靠 priority/FIFO。运行前依赖校验、预算预约和执行环境快照是建议增量。
5. `E:/cyberagent/artex-ymh-main/artex-ymh-main/agent/planner.go:298`、`:460`、`:463` 要求目标未成且没有 open/running 意图就必须派单。这能防停摆，也会在没有新前提、所有有效方向都已尝试时诱发同义重派。应允许显式 `awaiting_dependency` / `needs_input` / `exhausted_with_evidence`，而不是只要求更强的“继续”提示词。
6. YMH 已有模型错误重试、暂停恢复与 transcript 延续：`server/engine.go:1144`、`:1205`；已有 stuck 提示：`:1106`；已有 cold compaction：`agent/compaction.go:68`。建议增量必须在这些能力上收敛，不重复造轮子。

## 七项建议及验收

### 1. 统一执行上下文，先保证每次动作发生在正确位置

新增 `TaskExecutionContext`，最少包括 task/intent/run、executor、route_ref/version、auth_ref、traffic_store_ref、artifact namespace 和 budget_ref。Resolver 在领取时生成不可变 ExecutionSnapshot，shell、WebFetch、browser、session HTTP、host tools 和 evidence 全部使用同一个快照。不要把凭据明文放入 planner 或任务摘要，只传类型化引用；实际执行器按需取用。

执行期间固定路由；切换路由必须产生环境版本变化。重试应重新 resolve，并比较 route/auth/executor 版本，防止把旧环境的负面结果或缓存结论用于新环境。能力标注至少区分 local/remote、HTTP/SOCKS/native、OS、协议和依赖版本。

模块：新增 `agent/executioncontext.go`、`server/execution_resolver.go`；接入 `server/assembly.go`、`server/engine.go`，以及现有 session/tunnel/traffic/evidence/browser 装配。具体全局 store 引用问题由 Web/内网审阅交叉确认。

验收：两个任务使用不同代理、凭据和流量库并发运行，所有动作及证据仅落本任务；一个路由失效不把另一个任务切走；browser/HTTP/session/shell 的 trace 都包含相同 snapshot_id；退出与恢复不生成跨任务引用。

### 2. 建立 Web 观察模型和结构化对照

模型增加 endpoint_template、parameter_schema、auth_context、business_object、workflow_transition；Observation 必须引用 request/response artifact、capture time、executor/route/auth context、解析器版本。已有资产图管理对象和关系，探索图管理假设与结论，原始流量留在 evidence，不把大响应塞进图节点。

新增 `http_replay`/`http_compare` 作为确定性工具：输入捕获请求引用、字段级变更和认证引用；输出差异与证据引用，工具不直接宣布漏洞。重要业务操作先标注副作用/是否可重试。对照允许顺序执行，不能照搬 CyberStrike compare 的两个并发请求（`tool/http-replay.ts:427`）；会话更新、CSRF/token 轮换和有状态流程通常需要顺序与隔离环境。

模块：扩展 `traffic/`、`evidence/`、`db/`，新增 `agent/web_observation.go`、`server/http_replay.go`；复用已有 finding traffic binding。

验收：本地回放夹具覆盖重定向、Cookie/CSRF 轮换、JSON/multipart 和大响应；基线请求与变体的原始字节/语义差异可追溯；相同路径不同角色/参数结构不被错误合并；不可重试动作不会因网络重试被重复提交。

### 3. 用假设账本驱动测试，补 typed prerequisites

新增 hypothesis 记录：claim、support/contradict evidence_refs、status（proposed/testing/supported/refuted/inconclusive）、next_discriminating_test、环境版本、更新时间/有效期。事实保留 observed/inferred 的区别；假设未验证不升级成 finding。

`intentItem` 增加 `hypothesis_id`、`test_family`、`prerequisites[]`、`expected_observation`、`execution_snapshot_id`、`retest_condition`。前置条件是 typed predicate，例如 asset.exists、session.alive、route.reaches、auth.valid、observation.present；不用自由文本“前一步已完成”承担调度职责。保持现有 fact/finding 血缘，前置条件是可执行性，不代替知识来源。

ClaimIntent 在事务内同时检查 prerequisites、预算预约和 inflight 去重。未满足时记录 awaiting_dependency 及缺项，只有匹配的依赖变更才唤醒。模型/API blocked 仍沿用现有错误恢复流程。成功状态必须有 expected_observation/evidence，不能以工具退出码 0 自动判链路完成。

模块：`agent/tools.go:1062`、`db/exploration.go:1063`、`server/engine.go:1351` 及新 DB 表/迁移。若采用统一新状态，应同步更新 coldgraph 的 settled 判定。

验收：依赖未满足的下游完全不执行；依赖变化只唤醒相关下游；路由或凭据失效会撤销相关可执行性；无新信息的 heartbeat 不新建同义意图；进程重启仍保留待等原因与下一步测试。

### 4. 在现有 frontier 上增加去重和成本调度

去重键包括 task + endpoint/service identity + auth role + test family + parameter schema + route/executor fingerprint + test version，并有 inflight 唯一约束。失败结果只在对应环境指纹和有效期内抑制重复；新增凭据、版本变化或新证据可以有条件复测。

沿用 priority/FIFO 作为基础，加入信息增益估计、目标相关性、执行成本、依赖解锁数、近期重复惩罚；分数的初版用可解释规则，不需要额外 LLM judge 每次排序。保留人工优先级最高、kill/steer/cancel 和现有 stuck detector。

模块：`db/exploration.go:1025` 的 frontier 查询、`agent/tools.go:1074` 创建路径、`server/engine.go:1351` 领取路径。

验收：同一测试并发提交只跑一次；环境不变的重复阴性结果不占新 worker；一条低成本前置检查能先于依赖它的高成本工作；人工投递和终止的响应延迟不退化。

### 5. 工具目录从“有名字”升级为“此环境可用”

复用现有 ToolResolve/deferred/skill gates，增加 capability 描述、输入输出类型、执行位置、协议支持、依赖版本、healthcheck、失败分类和 parser version。Profile 仅组合已有 worker、工具与提示词片段，不复制完整 Agent 运行循环。

工具查询先按 execution snapshot 和 prerequisites 过滤，再做关键词/语义相关性。schema 使用真实 token 预算并限制活跃工具数，按 run/task 隔离；新版本工具更新后失效旧 schema。CyberStrike `lazy-registry.ts:116` 对已知工具直接 continue，更新/删除同步也不完善，不能原样移植。

验收：不可达的远端或不支持当前 OS/协议的工具不会出现在可执行候选；超预算能真正拒绝/卸载；工具升级后 schema 正确刷新；两个任务的工具加载互不干扰。

### 6. 预算与证据服务共享，不写成每次调用的局部变量

预算细分 task/host/credential/protocol，包含请求数、并发、令牌、耗时与认证失败数；超限进入可解释等待/收尾。每次真实尝试计费，重试也计数；token bucket、breaker 状态在同一 task/host 下共享并可恢复。

CyberStrike governor 原语本身存在（`replay/governor.ts:41`、`:117`、`:156`），但普通 `http_replay` 的 `sendGoverned` 每次在 `tool/http-replay.ts:209`、`:210` 新建预算及 breaker，不提供所声称的跨调用 session/host 约束。YMH 应把服务挂在 ExecutionContext 引用上。

验收：四个并发 worker 共用一个主机预算；重启不会清零已消费额度；429/认证失败触发正确的局部退避而不阻塞其他资产；预算耗尽后仍能完成事实/证据写回。

### 7. 用场景集评估设计，并缩短提示词

固定授权实验环境与录制夹具，分 Web（角色/对象/流程）、网络（执行位置/路由/会话健康）、编排（依赖/重复/恢复）三类。指标至少包括已确认结果精确率、可复现率、可解释覆盖率、重复率、请求/结果成本、恢复成功率及 evidence 完整性；不要只比较“找到了多少问题”。

提示词改为：稳定角色职责 + 当前 execution snapshot + 本意图假设/前提/期望证据 + 动态选取方法论。删除“任何时候都必须继续派单”的硬底线，改为“没有可执行新方向则明确等待原因/缺失前提”。保留现有不信任目标内容、证据优先与只做本意图的边界。

验收：同一模型/预算/场景与当前版本做 A/B；每项改动均由固定失败场景验证。先通过执行隔离和证据一致性，再谈检出率提升。Prompt 与 profile 都有版本号，观察与结果可追溯到版本。

## 不应照搬

- 大量工具描述、WSTG 文本和 48 KB 请求/响应广播到每个测试者（CyberStrike `tool/task.ts:21`）：会增加重复成本及敏感数据暴露；应采用摘要 + 可定位 artifact 段落，必要时显式读取。
- “每个 endpoint 扇出所有专家”及强制完成类别覆盖百分比：适用性应由应用模型和证据确定，blocked/不适用不算“安全”。
- 本机 shell hook 当通用远程执行器：它不会自动知道 pivot、route、credential 与目标位置；更不能仅因命令名称存在而宣称 AD 链路闭环。
- 隐蔽执行、持久化、数据外传模块不是提升现有框架可靠性的前提；本方案不移植这些功能。
- Promise 内存队列、全局工具 map、局部 budget/breaker：它们不满足 YMH 的多任务隔离与恢复目标。
- 启发式 chain detect 或证据字数 gate 当验证器：只应产生待验证候选与审查提示，确认仍依赖真实可回溯证据。

## 实施顺序

P0：ExecutionContext/Resolver + 任务隔离回归 + planner 等待/耗尽语义修正。

P1：Web observation + replay/compare + hypothesis/prerequisites + 跨 worker 预算。

P2：profile/capability 过滤 + 语义去重/成本排序 + 可复现 benchmark 与提示词 A/B。

P0 的一致上下文是 Web 和内网共用依赖。先做它，后续重放、浏览器、会话执行和验证都能复用同一套任务隔离、路由、凭据与证据契约。
