# YMH 资源租约、路由声明与认证账本实施记录

日期：2026-10-23（跨至 10-24 凌晨），Asia/Taipei。项目根：`E:/cyberagent/artex-ymh-main/artex-ymh-main`。

本轮接续 [`YMH-待办总表.md`](YMH-待办总表.md) 的 P1 段。**开工前先做了一次全量审核**，因为工作区里已经有一批并行加入的改动（Runtime 实例化、模型预算账本、任务归档、证据版本、前端四页），必须先确认它们的状态再动手——否则会在一个编译不过或语义已变的基线上继续叠。

---

## 0. 开工审核（先看再改）

### 0.1 发现并修复的编译断点

**`server/conversations.go` 编译失败**：

```
server\conversations.go:496:36: undefined: sql
server\conversations.go:504:11: undefined: llmrec
```

`conversationRunContext` 新增了「从 findings 反查来源任务 → 给会话上下文带上 `llmrec` 任务标签」的逻辑，但漏了两个导入。补齐 `database/sql` 与 `github.com/Autumn-27/artex/llmrec` 后 `go build ./...` 通过。

这不是设计问题，但值得记一笔：**新加的跨模块接线（findings → conversations → llmrec）让 server 包多了一层对 `llmrec` 的依赖**，而 `llmrec` 原本只被 `agent` 与 `server/intercept.go` 使用。

### 0.2 并行改动的状态判定

| 改动 | 判定 | 依据 |
| --- | --- | --- |
| `agent.Runtime`（实例回调替代包级变量） | 已落地 | `agent/runtime.go`、`runtime_test.go` |
| 模型预算账本（`task_model_budget` / `_calls`） | 已落地，**有 1 个真实缺陷**（见 §1） | `db/model_budget.go`、`budget/budget.go` |
| 任务归档 / 恢复 | 已落地，**测试与守卫冲突**（见 §2） | `db/task_archives*.go` |
| 证据版本与 finding 重验 | 已落地 | `db/finding_checks.go`、`finding_merges.go` |
| `evals/` 首批 6 场景 | 已落地 | `evals/scenarios.json` |
| 前端四页修复 | 已落地 | `independent-design-review.md` |

**结论：这批改动的主体是好的，但有两条测试在隔离库上稳定失败**。它们与本轮待办无关，却是「全部完成」的前置——一条红着的基线无法用来验证新功能。

### 0.3 一个环境事实（影响验证方式）

`scripts/test.ps1` 在本机跑不起来，原因不在仓库：Windows 环境里同时存在 `http_proxy` 与 `HTTP_PROXY` 两个键，`Start-Process` 直接抛 `ArgumentException`。即使绕过这一点，脚本末尾的 `pg_ctl stop` 退出码判断也会把成功的停库判成失败。

因此本轮验证改为：**同一条命令内**完成「起隔离库 → 跑 `go test` → 停库 → 删库」，且每次用新的数据库名（`.build/pgverify` 里的旧数据不能复用——复用会让 `TestCompanyScopeAttribution` 这类依赖「当前公司集合」的用例因残留行而失败，且看起来像代码 bug）。

---

## 1. 修复：预算历史导入漏掉 cache token

### 1.1 现象

`TestModelBudgetSettlementAndRestart` 稳定失败：

```
model_budget_test.go:97: &{1000 100 {140 25} {0 0} 0 0 true} <nil>
```

期望 `Spent.Input = 165`，实际 `140`。差额 25 = `cache_read 20 + cache_write 5`。

### 1.2 根因

`ReserveModelTokens` 在首次建账时做一次性历史导入：

```sql
-- 改前
INSERT INTO task_model_budget(task_id,input_spent,output_spent)
SELECT $1, COALESCE(sum(input_tokens::bigint),0), COALESCE(sum(output_tokens::bigint),0)
FROM llm_usage WHERE task_id=$2
```

`cache_read` / `cache_write` 没有计入。

### 1.3 为什么这是真缺陷而不是测试写错

缓存 token 在供应商账单上打折，但它们**确实占用上下文预算**。漏掉它们的方向是单向的：长上下文 + 高缓存命中的任务会系统性「看起来没花钱」，而这类任务恰恰是 `max_input_tokens` 最该拦住的。也就是说，这个偏差不是随机的，它偏向于放过最贵的场景。

### 1.4 改法

```sql
SELECT $1,
       COALESCE(sum(input_tokens::bigint + cache_read::bigint + cache_write::bigint),0),
       COALESCE(sum(output_tokens::bigint),0)
FROM llm_usage WHERE task_id=$2
```

只改导入一侧。**没有**顺手改 `SettleModelTokens`：结算时传入的 `used` 来自 provider 的真实回包，那里 cache 已经是分开统计的，硬塞进去会让同一笔钱被算两次。

---

## 2. 修复：归档守卫与测试互相矛盾

### 2.1 现象

`TestTaskArchiveDatabaseRoundTrip` 失败：

```
task archive state does not allow this operation:
model calls have unresolved reservations; inspect task budget pending_calls before retrying archive
```

### 2.2 判断

守卫在 `db/task_archives.go:572`（`requireModelBudgetSettled`），检查是否还有 `state='reserved'` 的调用。测试在归档前**故意**留了一条未结算的预留，期待「归档把它一起快照进去」。

两者不可能同时对。**守卫是对的**：归档快照一旦漏掉 in-flight 预留，恢复之后账本就少一笔钱，而且事后无法察觉——这正是「不可核验的账」最难查的形态。

### 2.3 改法（改测试，不改守卫）

把测试从「留一条 reserved 然后直接归档」改成三段：

1. 留一条 reserved；
2. **断言快照被拒绝**（`errors.Is(err, ErrTaskArchiveState)`）——把守卫的契约写进测试；
3. 结算这条预留，再走原来的归档 + 快照断言。

`task_model_budget_calls` 仍是 3 行，原有用例意图（归档必须包含预算账本）不变，额外多了一条「不许带着未结算调用归档」的断言。

**注意一个容易写错的地方**：守卫在**快照**阶段生效，不在 `QueueTaskArchive`。第一次改的时候我把断言放在排队处，结果拿到 `nil`——排队只是登记意图，真正读账本的是快照。这条差异已写进测试注释。

---

## 3. T-09 会话租约（共享会话不被一个任务误删）

### 3.1 为什么不能复用 `session_access_grants`

| | `session_access_grants` | `session_leases` |
| --- | --- | --- |
| 语义 | **准入**：这个任务被允许对这个会话做哪些动作 | **存活**：这个任务此刻正在用它 |
| 生命周期 | 可以长期有效，人工授予 | 随任务终态释放 |
| 合并的后果 | 一个早已结束的任务因为 grant 还在，**永久**阻止别人删会话 | — |

### 3.2 实现

`db/session_leases.go`：

- `Acquire(ctx, sessionID, taskID, purpose)`：`ON CONFLICT DO UPDATE`，幂等；`purpose` 用逗号累积（`exec,recon`），只进审计不参与判定。
- `ReleaseTask(ctx, taskID)`：任务终态的唯一释放出口。
- `OtherHolders(ctx, sessionID, excludeTaskID)`：删会话前的检查。
- `LiveForTask`：审计用。

两个刻意的取舍：

1. **`task_id` 不加外键**。任务删除后租约记录仍要能被收尾查询到；加了外键就得先删租约再删任务，而收尾路径正是在任务已经删掉之后跑的。
2. **零 id 直接返回 nil**。`sessionID<=0 || taskID<=0` 时不落行——否则平台自检这类调用会造出一条 `task_id=0` 的租约，它永远没人释放，于是那个会话再也删不掉。

### 3.3 接线

| 位置 | 动作 |
| --- | --- |
| `server/session.go` `session_exec` | 执行前 `noteSessionLease(..., "exec")` |
| `server/session_recon.go` | 侦察前 `noteSessionLease(..., "recon")` |
| `server/intranet.go` `intranetDeleteSession` | 删前查 `OtherHolders`，非空返回 **409**，需显式 `force=1` |
| 任务终态 | 见 §4 |

`noteSessionLease` 是 **best-effort**：写失败只记日志，不拒绝执行。理由是执行本身已经发生——为一条簿记记录失败而拒绝执行，等于让 agent 白跑一轮，代价高于「少一条租约」。代价是删会话的保护弱一档，所以这里必须留日志，让「为什么没拦住」可追。

409 的正文带持有者列表（`#7, #12`），不是只说「被占用」——**让人一眼看出是谁在用**，否则只能盲猜。

---

## 4. T-10 任务终态按依赖逆序释放

### 4.1 顺序为什么不能反

```
任务 → 会话（存活声明）→ 隧道（部署依赖会话）
```

收尾必须**先隧道、后租约**。先放租约等于先宣布「我不再用它了」，再去拆建在它上面的东西——中间窗口里别人可以合法地把会话删掉，于是隧道拆不掉、记录悬空。

### 4.2 实现

`server/resource_release.go`：

- `releaseTaskResources(taskID)`：隧道按 id **倒序**拆（后建的更可能依赖先建的：先打通 socks，再在其上布 portfwd），最后释放租约。
- `stopTaskAndRelease(taskID)`：`Engine.StopTask` + 收尾，作为删除/归档路径的唯一入口。
- 独立 `context.Background()` + 90s 超时：收尾不能挂在请求 ctx 上——客户端断开或已返回 202 之后请求 ctx 会被取消，而远端进程还在。

### 4.3 接线

`server_mgmt.go` 删除路径两处（含「PG 已提交」的 committed-error 分支）、`task_archives.go` 归档路径。收尾结果写进 `DeleteTaskResult.TaskResourceRelease`（指针 + `omitempty`，什么都没做时不塞空对象）；归档路径把失败项写进 `AppendTaskArchiveWarning`。

### 4.4 为什么失败只报告不回滚

收尾发生在 PostgreSQL 已提交之后。那时回滚不可能，静默吞掉更糟——所以失败项进响应/警告，让「有隧道没拆掉」可见。

---

## 5. T-11 意图 inflight 唯一键

### 5.1 约束

```sql
CREATE UNIQUE INDEX uq_expnodes_intent_live
  ON exploration_nodes(exploration_id, lower(btrim(payload->>'summary')))
  WHERE kind='intent' AND state IN ('open','running');
```

先去重再加索引，且只在索引不存在时扫一遍（`DO $$ ... IF NOT EXISTS (pg_indexes) ...`），避免每次启动都全表 UPDATE。

### 5.2 写侧：报错而不是静默复用

`AddIntent` 命中唯一冲突时，返回 `ErrIntentAlreadyLive` **并带上已有意图的 id**：

```
intent already live: 已存在同类意图 #42，不要重复派发
```

为什么不是静默返回旧 id：静默复用会让 planner 以为「新方向已派」，从而不再推进；显式报错它才能决定「改成更具体的方向」还是「直接等那个意图出结果」。

`liveIntentID` 的谓词与索引**完全一致**（`lower(btrim(...))` + 同样的 state 集合）。不一致会查出一个「索引认为不冲突」的 id，把错误信息指向错误的意图。

### 5.3 一处必要的测试调整

`db/activity_page_test.go` 原来连续插 25 条 `summary: "i"` 只为测分页。新约束下这会直接失败——改成 `i-00..i-24`。这是「新不变量 → 旧用例必须表达它」的正常代价，不是放宽约束。

---

## 6. T-05 会话控制通道路由

### 6.1 问题

第二层会话往往位于只有经隧道才可达的网段。控制请求（probe/exec/read/write）直连时平台根本连不上它——会话登记得再对也用不了，「拿到立足点」和「能操作立足点」之间断了一截。

### 6.2 实现

- `session/http_shell.go`：`SetControlProxy(proxyURL)` / `ControlProxyURL()`。
- `session.ProbeVia` / `ProbeEncVia`：**探针本身也走这条路**——否则「目标只在隧道另一侧」的会话永远探测失败，路由等于没配。
- `server/session_control_route.go`：`controlRouteUpstream(tunnelID, sessionID)`。
- `register_session` 新增 `control_tunnel_id`。

### 6.3 三个刻意的限制

1. **只接受 alive 的 socks 隧道**。portfwd 是「固定目标的一条通道」，不能承载任意控制请求。
2. **防环**：`tunnel.via_session_id == sessionID` 时拒绝。这不是性能问题，是死锁——隧道一断会话就再也够不着，而重建隧道又需要这个会话。
3. **路由模式下拒绝 `lang=auto`**。auto 判型会连发 php/sh/aspx 三个探针，无法统一走隧道；让它半路失败会得到「探测失败」这种误导性错误。实践中「经隧道才可达的马」总是已知类型（自己传上去的）。

### 6.4 不泄露凭据

`ControlProxyURL()` 只回 `scheme://host:port`。suo5 的代理口令内嵌在 URL 里，整串回显会随注册输出进模型上下文和审计日志。

---

## 7. T-06 可达范围与 DNS 策略声明

### 7.1 核心区分

| 字段 | 含义 |
| --- | --- |
| `reachable_ranges` / `dns_policy` | **声明**：我以为这条隧道能到哪些网段 |
| `reach_verified_at` | **观测**：真的探测过 |

声明不点亮 `reach_verified_at`。混用会让「声明」在输出里看起来像「已验证」，而**本地 socks 端口打开 ≠ 所有网段可达**——这是这条待办被单独列出来的原因。

`tunnel_list` 里两者分字段输出（`reachable_ranges` + `reach_verified: bool`），且只有声明过的隧道才出现这两个键——避免「未声明」被读成「空 = 什么都到不了」。

### 7.2 未做

**没有实现自动可达性探测**。`MarkReachVerified` 存在但没有调用方，接口留给后续的 `tunnel_probe` 扩展。这一条明确记在待办总表里，不假装完成。

---

## 8. T-07 认证结果账本

### 8.1 表

`auth_attempts`：`task_id / credential_id / principal / service / target / route / auth_method / outcome / failure_signature / via_session_id / evidence`。

`outcome` 五态：`success / failure / error / blocked / unknown`。**保留 `unknown` 是有意的**——「发出去了但无法判定」既不能当成功也不能当失败，压成任何一边都会污染统计。

### 8.2 写侧

`lateral_exec` 三个出口都写账：

| 出口 | outcome | signature |
| --- | --- | --- |
| 超时 | `error` | `timeout` |
| 成功 | `success` | — |
| 有失败签名 | `failure` | 命中的短语 |
| 无签名但退出码非 0 | `error` | `exit_nonzero` |

最后一行的区分很重要：没有失败签名但退出码非 0，多半是环境问题（python 缺失、脚本报错）。记成 `failure` 会污染「凭据无效」的统计，让 planner 错误地放弃一条本来有效的凭据。

`lateralFailureSignature` 顺手重构成 `lateralFailureReason` 返回具体签名（保留布尔包装给旧调用方）——**签名进账本才能识别「同一失败原因反复试」**。

### 8.3 读侧：尝试预算

同一凭据 × 同一目标 × 同一服务，30 分钟内同签名失败 ≥ 5 次 → **不发包**，直接返回：

```
尝试预算已用尽（未发包）：凭据 #7 对 10.9.9.9 的 wmiexec 在 30 分钟内已失败 5 次，
最近失败签名 "logon failure"。继续试同一组合不会改变结果，且可能触发账户锁定。
```

取 5 的理由：低于 3 会在正常排错时误拦（第一次格式错、第二次路径错都是合理尝试），高于 8 就失去防止账户锁定的意义。

### 8.4 未做

`HasSuccess` 已实现但没有接进 `credential_reuse_plan` 的打分。现在 `verified` 仍由 `credStore.SetVerified` 的布尔位驱动，账本是**旁路证据**而非唯一真相。这一条记在待办里。

---

## 9. T-20 静态收割覆盖报告

`harvest_static.py --coverage` 产出 `coverage.json`：

- `scripts.seen / fetched / failed`（失败分三类：`returned_html`=SPA 路由回落、`http_4xx`=服务端不给、`empty_or_too_large`=超限放弃）
- `endpoints.method_known / method_unknown`
- `endpoints_only_in_sourcemap`（只在还原源码里出现、bundle 里没有的端点）
- `not_covered`（动态交互面 / 需登录态 / 客户端计算的路径）

**刻意没做自动点 Tab / 弹窗 / 分页**——那需要浏览器，本脚本是纯 HTTP 静态收割。与其假装覆盖了动态面，不如把「静态面覆盖到哪、哪块没碰到」如实报出来：否则下游会把「api_static.txt 只有 12 条」误读成「这个站只有 12 个接口」。

---

## 10. T-14 修正强断言

| 位置 | 改前 | 改后 |
| --- | --- | --- |
| `agent/chainskel/web.md` SSTI | 「唯一判据是 `{{7*7}}` 回显 49」 | 必须做对照：`{{7*7}}` vs `{{7*8}}` 回显 49→56 才算表达式进了模板；单独一个 49 可能是页面原有数字/缓存/原样回显 |
| `agent/chainskel/web.md` JWT | 「先跑 alg=none、RS→HS、kid/jku 四件套」 | 先做**改签对照**（改一位签名重放）：仍被接受=没校验；被拒=有校验，再逐个试且一次只改一个变量。单次被拒不等于安全 |
| `meta_rules.go`（planner/worker 两处） | 「工具阳性可信、阴性不关门」 | 工具阳性**也要**一条独立对照才升级为事实（版本匹配/单次命中都可能误报）；阴性不关门（保留） |

三条改的是同一类错误：**把「一个信号」当成「一个结论」**。`49` 是一个信号，`改签被拒` 是一个信号，「工具说有洞」也是一个信号——它们都需要一条对照才能变成事实。

---

## 11. T-12 评测场景扩充（6 → 12）

`evals/scenarios.json` 升到 v2：

| 编号 | 状态 | 内容 |
| --- | --- | --- |
| E01–E05 | automated | 原有五项 |
| **E06** | **automated**（原 gap） | 共享会话不被误删 → 4 条租约用例 |
| **E07** | automated | 重复意图不派两次 → 2 条去重用例 |
| **E08** | partial | 任务终态依赖逆序释放（单测覆盖空收尾/无库 no-op；端到端需真实隧道） |
| **E09** | automated | 控制通道路由与防环 → 4 条用例 |
| **E10** | automated | 声明不得伪装成观测 |
| **E11** | automated | 认证账本与尝试预算 |
| **E12** | manual | 静态收割覆盖报告 |

`notes` 里写清了三态定义与「没有测试支撑的场景不写 automated」的规则——**那会让「有 eval」变成自我安慰**。

---

## 12. 验证

### 12.1 方式

同一条命令内完成「起隔离库（新库名）→ `go build` / `go vet` / `go test ./...` → 停库 → 删库」。用新库名而不是复用 `.build/pgverify`：复用会因残留行让 `TestCompanyScopeAttribution` 这类用例误失败，看起来像代码 bug。

### 12.2 结果

```
go build ./...                    # 通过
go vet ./...                      # 通过
go test ./... -count=1 -p 1       # 26 个包 ok，0 FAIL，0 SKIP
```

`-p 1` 是必要的：`db` 包的用例共享同一套表，并发跑会互相清表。

**过程中出现的两次假失败，原因都不是代码**，记下来免得下次误判：

1. 第一次用 `.build/pgverify` 里**已存在**的数据目录跑，`TestCompanyScopeAttribution` 报
   `in-CIDR ip (insert-time) company_id = 27, want 124`——上一个用例留下的公司行还在，
   CIDR 规则命中了旧行。换成新库名后通过。
2. 第二次 `TestTaskArchiveDatabaseRoundTrip` 报 `budget ledger restore mismatch`——是
   §2 的改动让账本算术变了（多了一笔已知结算），期望值没跟着改。这不是「测试挡住
   了 bug」，是「测试还在描述旧状态」，所以改的是期望值 + 注释说明算术来源。

新增测试 14 条：租约 4、认证账本 1、意图去重 2、资源收尾 5、控制通道路由 3（含
`ProbeVia` 的 auto 拒绝）、隧道声明 1。

---

## 13. 已知残留（明确未做）

| 项 | 现状 | 为什么没做 |
| --- | --- | --- |
| 自动可达性探测（T-06 后半） | `MarkReachVerified` 无调用方 | 需要在目标侧跑探测命令并解析结果，属独立工作；先立声明与观测的字段边界，避免「声明冒充观测」 |
| `HasSuccess` 接入凭据复用打分（T-07 后半） | 账本是旁路证据 | 要改 `credential_reuse_plan` 的评分口径，与 T-16 的 `identity_ref` 模型耦合，应一起做 |
| `harvest_static` 动态交互覆盖（T-20 后半） | 仅静态面 | 需要浏览器运行时，是「新工具」而非「原地升级」，与无感判据冲突 |
| 隧道收尾的端到端验证（E08） | 仅单测 | 需要真实隧道台账 + 远端进程 |
| `scripts/test.ps1` 在本机不可用 | 环境问题（重复代理键） | 属本机环境，不是仓库缺陷；已在 §0.3 记录绕过方式 |

---

## 14. 变更清单

**新增**

| 文件 | 作用 |
| --- | --- |
| `db/session_leases.go` | 会话租约（T-09） |
| `db/auth_attempts.go` | 认证尝试账本（T-07） |
| `server/resource_release.go` | 任务终态依赖逆序收尾（T-10） |
| `server/session_control_route.go` | 控制通道路由解析与防环（T-05） |
| `db/session_leases_test.go` | 4 条租约用例 + 1 条账本用例 |
| `db/exploration_intent_dedup_test.go` | 2 条去重用例 |
| `server/resource_release_test.go` | 5 条收尾用例 |
| `session/control_route_test.go` | 3 条路由用例 |

**修改**

| 文件 | 改动 |
| --- | --- |
| `server/conversations.go` | 补 `database/sql`、`llmrec` 导入（修复编译断点） |
| `db/model_budget.go` | 历史导入计入 cache token |
| `db/task_archives_test.go` | 归档守卫断言前置到快照阶段 |
| `db/schema.sql` | `session_leases`、`auth_attempts`、`tunnels` 三列、意图唯一索引与去重 |
| `db/tunnels.go` | 记录结构扩三列 + `SetReachDeclaration` / `MarkReachVerified` / `jsonOr` |
| `db/exploration.go` | `ErrIntentAlreadyLive` + `AddIntent` 幂等报错 + `liveIntentID` |
| `db/activity_page_test.go` | 分页用例改用不同 summary |
| `server/lateral_exec.go` | 认证账本三出口 + 尝试预算 + `lateralFailureReason` |
| `server/session.go` | 租约登记 + `control_tunnel_id` + `control_route` 回显 |
| `server/session_recon.go` | 侦察租约登记 |
| `server/intranet.go` | 删会话前的租约检查（409 + force） |
| `server/server_mgmt.go` | 删除路径改用 `stopTaskAndRelease` |
| `server/task_archives.go` | 归档路径收尾 + 警告 |
| `server/manager.go` | `DeleteTaskResult.TaskResourceRelease` |
| `server/tunnel.go` | `reachable_ranges` / `dns_policy` 参数与输出 |
| `session/http_shell.go` | `SetControlProxy` / `ControlProxyURL` / `ProbeVia` |
| `session/http_shell_enc.go` | `ProbeEncVia` + `finishEncProbe` 拆分 |
| `agent/chainskel/web.md` | SSTI / JWT 判据改成对照式 |
| `agent/chainskel/meta_rules.go` | 「工具阳性可信」→「需独立对照」 |
| `skills/api-recon/scripts/harvest_static.py` | `--coverage` 模式 |
| `evals/scenarios.json` | v1 → v2，6 → 12 场景 |

---

## 15. 交付状态

| 待办 | 状态 |
| --- | --- |
| T-09 共享会话租约 | **完成** |
| T-10 任务终态依赖逆序释放 | **完成**（端到端验证待真实隧道） |
| T-11 意图 inflight 唯一键 | **完成** |
| T-05 会话控制通道路由 | **完成** |
| T-06 可达范围 / DNS 策略 | **部分**（声明与观测边界完成，自动探测未做） |
| T-07 认证结果账本 | **部分**（账本 + 尝试预算完成，`HasSuccess` 未接入打分） |
| T-20 harvest coverage | **部分**（静态面覆盖报告完成，动态交互未做） |
| T-14 强断言修正 | **完成**（提示词拆五块未做，见待办） |
| T-12 评测场景 | **部分**（6 → 12，完整 26 未做） |
| 预算 cache 计量缺陷 | **修复**（审核时发现） |
| 归档守卫测试冲突 | **修复**（审核时发现） |
| `conversations.go` 编译断点 | **修复**（审核时发现） |
