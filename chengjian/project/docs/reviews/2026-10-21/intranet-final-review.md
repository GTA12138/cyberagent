# 统一方案内网交叉复核（2026-10-21）

> **【已归档 · 2026-10-23 文档审核】** 内网交叉复核记录，结论为「方向可接受，七项须补合同与验收」。七项均已写入方向文件正文，复核已完成。
> 本文是历史记录，不是当前状态：文中写的「未做」可能已完成，文中写的「已完成」也可能已被后续改动推翻。
> 现役进度 → [能力评估与演进方案 §7.0](YMH-能力评估与演进方案.md) ｜ 唯一待办源 → [YMH-待办总表](../2026-10-23/YMH-待办总表.md)

已完整阅读 `E:/cyberagent/artex-ymh-main/artex-ymh-main/docs/reviews/2026-10-21/YMH-能力评估与演进方案.md`。未修改主报告。结论是方向可接受，但第 1 项必须进入 P0 的 S0；第 2—5 项应明确合同和验收，避免实现时再次由模型或隐式默认值补语义。

## 七项结论

### 1. 必须补：资源操作授权属于 S0 的 P0

S0 已写流量跨任务隔离，I4 已写共享引用，却没有明确 Session/Tunnel 每次操作的 task 授权。`server/session.go:540` 只按全局 ID 查 Registry；`server/tunnel.go:458` teardown 只传 tunnel ID；`guard/guard.go:67` 的 cmd 提取不覆盖 session_exec，`guard/roe.go:143` 的目标抽取也不解析资源 ID。因此建议新增 F09，并在 S0 加 OperationEnvelope（task/intent/resource/executor/target/protocol/effect/scope_revision），执行前依次检查资源 grant、目标范围、路径/能力和租约。

不能只加 `CreatedByTask == currentTask`：现有 `server/handoff.go:121` 创建只读继承的子任务，继续使用原 Session，严格比较会破坏合法移交。需要显式 ResourceGrant（谁可 use/read/stop/share、授予范围/版本/有效期）与 ResourceReference（谁仍依赖资源）分开；新 handoff 先提交 grant/ref 与初始 scope，再启动子任务。移交不自动复制父任务全部 scope，不因知道 ID 就拥有操作权限。管理员全局操作也应有明确管理上下文和审计。

验收补充：A 无 grant 不能操作 B 的 Session/Tunnel；合法 handoff 后子任务可用但权限受限；撤销 grant/scope 后新调用失败；父任务终止不误停子任务仍引用资源；子任务无权以 teardown 绕过共享所有者。此项不能推迟到 I4 全部完成后才补。

### 2. 必须明确：I1 的 reachable ranges 是候选，不是可达事实

I1 已正确保留父路径、DNS、协议和版本，且禁止控制通道自依赖。请补：route 声明的范围只是候选选择器；可达 Observation 必须带 source executor/session、destination/service、protocol、DNS location、tested_at/valid_until 与原始 evidence。一个验证目标通了不证明整个 CIDR 所有服务可达；既有 ESTABLISHED 不证明任意来源、双向或未来可达。

“保留现有默认路由作兼容”应限于未绑定路径的 legacy 调用；已有快照或显式 route 的调用不可因失效回落默认。control_route_id 的父路径必须在 Session/Tunnel 建依赖前被验证无环。并行分支网段的选择发生在目的地拨号处，不能通过不断热切换整个任务 MITM 来伪装多路径。验收加相同域名不同 DNS 视角、无匹配路由、父失效与版本陈旧。

### 3. 必须明确：I4 的引用、授权、租约和清理是四种状态

I4 所列状态和依赖逆序释放方向正确。建议限定 ResourceLease 的含义：活动调用占用、共享引用、管理所有权分别持久化；lease 过期代表需要对账，不代表远端进程已停止。授权撤销可阻止新调用，但已有引用保留不等于继续授权执行；授权和生命周期应由不同判据处理。

任务终态/stop 请求提交时先原子标记 desired_state=stopped，再 drain、回收；health/redeploy 必须读取 desired_state，防清理与自动重拉竞态。local_closed、remote_cleanup_pending 和 remote_verified_clean 应可分别表达。清理失败用有界重试/对账，父控制会话暂不可达时不能删除唯一清理路径和元数据。共享资源只有最后有效引用释放或显式管理员处置才回收。F07 的事实与优先级基本可接受；最小终止合同在 A，完整依赖回收在 B2。

### 4. 必须补：I2/O2 的 timeout 与 cancel 结果不能仅用 transport_error 表达

`server/reverse.go:660` 已明确超时只结束平台等待，目标命令可能继续跑。当前 Session.Exec 契约也没有统一 exit_code/job_id/cancel 回执。建议 I2 能力增加 cancellation_semantics、supports_exit_status、streaming、job_tracking；O2 结果中保留 started/running/completed/unknown/cancel_requested/cancel_confirmed，而 success/negative 等是领域判定，二者不要揉成一个枚举。

不支持终止的驱动返回 cancel_unsupported 或 execution_state_unknown，禁止自动重试可能有副作用的命令；后续输出要按 job/call 隔离，不可混入下一条。W2/W4 的业务状态测试同样适用：重试前检查前置状态、回执和清理结果。验收增加目标命令继续运行、平台 timeout、重复投递和迟到结果。

### 5. 可接受并补一句：I3 与 W1 共用主体/认证引用，观察保留原文而不只 hash

I3 已覆盖绑定地址、来源、置信度、有效期、逐目标/服务认证和失败预算；正确保留现有 CredentialStore。请明确 AuthContext/Principal/AuthResult 与 W1 使用同一 identity_ref 命名和版本，不另建 Web/内网各一套身份真值。route 变化可使 transport 失败失效，但不必无条件让已验证账号本身失效；认证结论的适用性与传输观察分别判断。

Observation 的 hash 只能证明内容一致，无法替代可读取的原文；应存 artifact_ref + hash + 已解析字段 + parser_version，权限受 grant/task 限制。原文留存、必要脱敏和摘要不含 secret 的边界可复用现有 evidence；无需把明文凭据复制进观察。此项对应 I3/W1，其他设计可接受。

### 6. 可接受并加强验收：W1—W4、O1/O2、E1

Web 四包作为内网身份/路径的消费方逻辑一致：W1 模板与动态认证、W2 baseline/对照、W3 覆盖、W4 独立验证都已阅读并接受。W4 标 P2 应理解为新增 ExecutionRecipe/白盒扩展；现有独立证据核验在 S0/P1 不能后移。W3 的 negative/blocked/inconclusive 必须带 scope/route/identity 版本，不能把未知路线格子算作已完成测试。

O1 的准入、前提变化唤醒、inflight 去重与阴性结论有效期正确；共享资源的租约/授权检查需纳入原子领取与执行前复查，不把意图 Claim CAS 当作 ResourceLease。O2 预算服务正确，建议明确 reserve/consume/refund，以及取消/超时的保守结算，避免中断使请求预算返还后被重复消费。

E1 的固定条件、成对对比、至少 3 次初筛、不把目标当成绩均可接受。补四类硬性 fixture：跨任务资源 ID、合法 handoff 与撤销、stop/redeploy 竞态、远端 timeout 未停止。确定性 resolver/状态机测试每次必跑；LLM 重复实验用于效果评价，不能代替资源授权与路由正确性门槛。

### 7. 数据/迁移/粗估可接受为草估，但须列出阶段 A 的最小边界

原文已正确保留双图、允许 payload 渐进、反对双可写真值和 exactly-once 副作用承诺。请补 ResourceGrant/ResourceReference/ResourceLease 的逻辑实体；legacy Session 不能凭“能查询到 ID”直接生成全任务共享授权，应由 existing handoff/任务关系或人工确认迁移，并对有歧义项只读或要求显式授权。共享资源的归档删除按引用计数和授权规则处理，不能按单 task_id 直接级联。

store_id 需要稳定命名空间，归档后不可复用；legacy exchange ID 用明确 store 前缀，迁移禁止遍历猜测。resolver 影子模式只比较决策，不执行第二次请求或副作用。切换与回退应有 schema_version、幂等 backfill、校验统计和可恢复 checkpoint。

32—54 人日作为初始范围没有实测速度依据，不应进一步写成承诺。S0 的 Browser 隔离、历史证据解析、归档迁移、资源授权与最小停止语义同时做，6—10 人日偏紧；建议把 A 明确为“现有本地执行器 + 固定少数工具 + 新运行和 legacy 只读兼容”，全量历史迁移/全部工具适配另列，先用一次垂直切片重新估计。B2 的 10—16 人日限于现有驱动、能力声明与 unsupported 判断，不包含新增 SSH/Windows 完整执行器或所有协议支持。依赖顺序与先 A 再闭环方向可接受。

## 各工作包审阅状态

| 包/事项 | 状态 | 复核范围 |
|---|---|---|
| S0 | 必须补后接受 | 第 1 项；流量和执行快照本身可接受 |
| W1 | 接受，补统一身份引用 | 第 5 项 |
| W2 | 接受，补执行状态/预算结算 | 第 4、6 项 |
| W3 | 接受，补覆盖前提版本 | 第 6 项 |
| W4 | 接受，明确 P2 范围 | 第 4、6 项 |
| I1 | 必须细化可达事实和兼容回落 | 第 2 项 |
| I2 | 必须补 cancel/远端 job 语义 | 第 4 项 |
| I3 | 接受，补 artifact/身份共用 | 第 5 项 |
| I4 | 必须明确状态及 desired-state 防重拉 | 第 1、3 项 |
| O1 | 接受，补资源 grant/lease 准入 | 第 1、6 项 |
| O2 | 接受，补执行状态与预算结算 | 第 4、6 项 |
| E1 | 接受，补四类硬性 fixture | 第 6 项 |
| 数据/迁移 | 必须补授权/引用与 legacy 边界 | 第 7 项 |
| 顺序/粗估 | 方向接受，注明 A/B2 边界再校准 | 第 7 项 |

这是静态设计交叉复核；未跑真实内网或基准，不替代后续验收。

## 修订后二次确认

已重新阅读主报告修订内容，前述必须修改项均已落实，内网视角接受统一方案：

1. F09、OperationEnvelope、S0 的 owner/grant 检查与 handoff 授权/引用分离已写入 P0；I1 同时明确候选范围、带来源可达观察、控制路由无环和指定路径不得回落。
2. I2 的作业/取消状态、I3 的 artifact_ref 与共用身份模型、I4 的 desired_state/generation 防重拉，以及 E1 的四类资源/恢复样本已覆盖主要风险；O2 与数据边界区分执行状态、判定与预算消费。
3. 迁移的 legacy 授权依据、歧义只读、稳定 store_id、幂等回填与 A/B2 的 MVP/粗估限制已补齐。全部工作包均已复核，无新增阻塞意见；这表示设计通过交叉复核，不表示实现或端到端验收已经完成。
