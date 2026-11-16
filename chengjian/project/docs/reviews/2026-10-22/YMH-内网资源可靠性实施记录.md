# YMH 内网资源可靠性实施记录

> **【已完成 · 2026-10-23 文档审核】** 第二轮实施（精确会话授权 / 保守能力合同 / 探活与清理加固 / 上下文分页预算）已交付；其中遗留项已并入待办总表。
> 本文是历史记录，不是当前状态：文中写的「未做」可能已完成，文中写的「已完成」也可能已被后续改动推翻。
> 现役进度 → [能力评估与演进方案 §7.0](../2026-10-21/YMH-能力评估与演进方案.md) ｜ 唯一待办源 → [YMH-待办总表](../2026-10-23/YMH-待办总表.md)

日期：2026-10-22。项目：`E:/cyberagent/artex-ymh-main/artex-ymh-main`。

本轮继续实现内网资源的授权、状态和清理可靠性，并限制新增元数据对上下文的影响。保留 Go/PostgreSQL、双图、planner/worker 和既有 Session 接口。本记录不表示原 12 个工作包全部完成，不包含新增远端 Shell 转发或自动横向移动能力。

## 1. 实际改动

### 资源授权与人工移交

- 六类会话操作分别检查 `exec/read/write/recon/tunnel_probe/tunnel_deploy`，读取数据库权威归属或明确 grant，之后才取注册表驱动。无任务上下文、数据库错误、失效或已撤销授权拒绝操作。
- 普通 `SourceTaskIDs` 只提供背景，不授权资源。新建资源前核实任务存在且未删除、未归档；复用全局 reverse listener 不能隐式改变所属任务，启动串行化。
- 人工移交在同一个数据库事务内提交任务、探索图、父任务关系、单主机 scope 和指定会话的授权；任一步失败全部回滚，提交后才启动任务。IPv6 使用 `/128`。
- 任务只能操作本任务的父 tunnel 和 teardown；校验在生命周期操作前，避免被拒绝的停止请求取消他任务的部署。
- JWT 人工管理接口提供会话授权列表、按动作授权、期限和撤销；这些接口不是 Agent 工具。撤销不依赖解密 secret，损坏凭据不能阻止撤销。
- 自动重部署另检查当前任务与会话授权；拒绝时保留错误状态并阻止新的部署副作用。Teardown 和回滚清理不受这个准入回调限制。

### 保守能力描述

- 新增可选 `CapabilityProvider`，保持既有 Session 接口兼容；未知驱动、缺失驱动及 typed nil 返回 unknown。
- HTTP 与 reverse 驱动区分实现支持和环境确认。能力描述不发网络请求、不执行探活，不从语言或 Kind 推断 OS/架构。
- 如实标注合并输出、无退出码、本地取消不保证远端停止、无持久作业跟踪、未确认二进制完整性。列表返回这些元数据，不能据此视为执行成功或资源授权。

### 探活、心跳与清理

- 取消、本地超时和预算耗尽属于 unknown，不写成 dead。Session 错误保留 typed cause，支持 `errors.Is`。
- 探活写入匹配启动前的状态和成功心跳，防止旧失败覆盖新成功心跳或已删除行。失败操作不再刷新成功心跳；侦察包至少有一项成功才刷新。
- 清理不持注册表锁进行网络操作。并发关闭共享结果；失败保留驱动可重试，关闭期间发现同 ID 被替换时保留新实例。
- reverse 关闭要求上游明确确认；人工删除区分本地遗忘、HTTP 无远端释放事项、上游已确认等结果。缺驱动的人工遗忘不能解释为远端已清理。
- 前端探活明确显示 unknown，不把未完成观察显示为已失连。

### 上下文大小

- `session_list` 默认每页 25 条，最多 50 条；SQL 先筛授权，再分页，只读取非秘密元数据，不解密全量会话。
- 实际编码后的 JSON 上限 64KiB，必要时缩小当前页；`next_offset` 按实际返回条数推进，`count/alive` 明确为当前页统计，`has_more` 提醒继续读取。
- 超长 Kind、语言、宿主和异常大的能力描述整值省略或降为 unknown，标记 `metadata_omitted`，不产生可执行的半截目标。
- 任务阶段判定及 planner 会话快照复用有界元数据查询，纳入有效授权。摘要不再把展示的前五条概括为全部失效，也不把 tunnel alive 当作所有内网目标可达。

上一轮的 64KB 图快照、恢复意图锚定、成功 Skill 恢复解锁仍保留。Offset 基于当前台账；并发新增、撤销或删除时不承诺多次查询组成不可变快照。

## 2. 迁移与兼容

新增 `session_access_grants` 表，含精确动作、来源、过期和撤销时间。启动沿用现有幂等 schema 迁移。没有复制 ARTEX 数据、修改配置秘密或迁移用户运行数据库。

grant 是当前全局资源授权台账，不从旧任务归档恢复；恢复旧快照不能复活已撤销或过期的授权。已归档或已删除任务不能凭 grant 操作资源，真实删除任务/会话时相应 grant 外键级联清理。

旧人工移交没有 grant。本轮不根据背景关系自动补权；这类旧子任务需由已登录操作者在管理接口重新指定会话授权。接口为：

```text
GET    /api/sessions/{id}/grants
PUT    /api/sessions/{id}/grants/{task_id}
       {"actions":["read"],"expires_at":"2026-11-01T00:00:00Z"}
DELETE /api/sessions/{id}/grants/{task_id}
```

`expires_at` 可省略，动作必须明确列出。资源所有者权限来自归属；撤销共享 grant 不改变所有者。人工 API 沿用平台现有 JWT 管理权限，本轮未增加多用户 RBAC 或授权管理 UI。

## 3. 验证与独立审查

源码基线：`.build/intranet-stability-baseline-20261022`，508 个 Go/SQL/module 文件，不含配置、密钥或任务数据。

三个子代理分别参与能力实现、状态实现、独立授权审查和后续交叉复核。发现的测试挂起、心跳测试提前被授权拒绝、TEXT[] 读取兼容、损坏 secret 阻止撤销、reverse owner 隐式改变等问题已经修正。

首轮全新 PG 全包回归中，新模块用例通过，但全套未通过，失败日志保留为 `first-full-pg.log`。第二轮暴露一处测试后台任务清理竞态，保留为 `second-full-pg.log`。扩大验收共修正四处既有测试问题：

- deny 规则测试直接写 DB 后没有使已预热的规则缓存失效；修正 fixture 并增加规则实际命中断言，未改生产放行策略。
- 工具接线测试错误要求 worker Bash 描述不带既有 UA 指引；改为同时核对 worker 必须包含、planner 不得混入，未改生产工具说明。
- 凭据 CRUD 测试先关闭 DB，后运行删除 cleanup，遗留使用另一测试密钥的行；修正清理顺序并检查清理错误，未改生产解密规则。
- 继承关系只读 API 测试启动了恢复任务，未等待后台写入结束就移除临时目录。改用预取消 context，先逐个停止并等待恢复的 runtime，再等待归档、删除 fixture、关闭 DB，最后清理目录。保留原关系删除与继承内容断言。

最终第三轮全新 PostgreSQL 回归通过：`go test -p 1 ./... -count=1 -timeout 180s`，24 个有测试的包全部通过，另 2 个包无测试。包括 `db` 32.526s、`server` 36.614s、`session` 4.015s、`tunnel` 0.089s。此前两轮的失败日志保留，没有删除失败用例或放宽生产错误处理。

其余通过的检查：`go vet ./agent ./db ./session ./tunnel ./server`、前端 `tsc --noEmit --incremental false`、Next 静态构建，以及带 `embedui` 的 Windows 候选程序构建。最终全包回归后只更新文档，没有继续改动生产源码。

全新 PostgreSQL 集群由 `.build/run-intranet-stability-pg.ps1` 创建，只监听随机本地端口；同时隔离 `ARTEX_CONFIG` 和 `ARTEX_PG_DSN`。最终集群为 `final-20261022-c`，结束后停止成功，`pg_ctl status` 确认 `no server running`。此前两轮及最小复现集群也已停止，未触碰用户运行数据库。日志位于 `.build/intranet-stability-review/`。

独立审查详见 [独立复核记录](独立复核记录.md)。Go/SQL/module 文件相对本轮基线共变更 41 个，另修改 2 个前端源码文件，并重新生成嵌入式静态资源；`go.mod`、`go.sum` 未修改。完整源码清单位于 `.build/intranet-stability-review/changed-source-files.txt`。

主要改动位置：

| 模块 | 实现入口 | 主要验证 |
| --- | --- | --- |
| 精确授权及事务移交 | `db/session_access.go`、`db/schema.sql`、`db/tasks.go`、`server/resource_access.go`、`server/handoff.go`、`server/session_grants.go` | 撤销/过期/归档/数据库错误拒绝、跨任务驱动调用计数、最终 grant 插入失败整事务回滚、IPv6 scope |
| 保守能力合同 | `session/capabilities.go`、`session/capabilities_http.go`、`server/session_capabilities.go` | 旧接口兼容、typed nil、描述无网络或执行副作用 |
| 探活及清理 | `db/session_health.go`、`server/session_health.go`、`session/registry_cleanup.go`、`server/intranet.go` | 取消为 unknown、旧观察不覆盖新心跳、失败不刷心跳、清理重试/并发/实例替换/上游明确确认 |
| 列表与摘要预算 | `db/session_page.go`、`server/session.go`、`server/intranet_snapshot.go` | 60 条连续分页无遗漏、超长原子字段完整省略、JSON 预算缩页不跳项、损坏 secret 不阻断元数据读取 |
| 自动重部署准入 | `tunnel/health.go`、`server/tunnel_redeploy_access.go` | 当前持久 plan 校验、拒绝或取消时零驱动调用、允许时确实进入驱动、清理不被部署准入阻断 |

## 4. 保留边界

- 授权检查是操作准入，不是完整运行租约。撤销不停止已经执行的远端作业；授权检查后才发生的撤销不会回溯取消本次调用。
- 探活快照不是完整观察版本台账；重复失败不建立全序。
- 关闭期间已有执行仍可能继续；关闭驱动与数据库删除不是同一原子操作，会话未建立持久清理待办表。
- listener 启动串行和所属任务保护不等于完整进程代际状态机。
- 目的地选路、DNS/协议路由合同、完整观察来源、共享引用/运行租约、任务终态 drain 仍待后续设计与实现。
- 测试使用本地 fake/fixture；真实目标、真实模型、真实远端进程未做端到端联调。未将普通并发测试写成 race 检测通过。

## 5. 交付状态

Windows 候选程序：`.build/intranet-stability/artex.exe`，版本 `ymh-intranet-stability-20261022`，包含本轮重新构建的静态前端。

SHA256：`6bfbd42f43fce5b3ce46928715571ba4c8ed2de5e3fd8b467ef6aa012d645376`。

本轮不替换项目根目录 `artex.exe`，不重启用户服务；源码、候选构建与运行实例的状态分别记录。根程序 SHA256 仍为 `74d60516ef802e101950172da94306fbf278bf2d8eb29015a224291bb329bd36`。
