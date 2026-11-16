# 统一方案最终交叉复核：编排与 CyberStrike 对照

> **【已归档 · 2026-10-23 文档审核】** 编排与 CyberStrike 对照复核记录，12 个工作包逐一意见已写入方向文件正文，复核已完成。
> 本文是历史记录，不是当前状态：文中写的「未做」可能已完成，文中写的「已完成」也可能已被后续改动推翻。
> 现役进度 → [能力评估与演进方案 §7.0](YMH-能力评估与演进方案.md) ｜ 唯一待办源 → [YMH-待办总表](../2026-10-23/YMH-待办总表.md)

日期：2026-10-21。审阅对象：`E:/cyberagent/artex-ymh-main/artex-ymh-main/docs/reviews/2026-10-21/YMH-能力评估与演进方案.md` 当前草案。

已完整阅读 S0、W1、W2、W3、W4、I1、I2、I3、I4、O1、O2、E1 全部 12 个工作包，以及前置发现、外部能力集合、数据模型、迁移与投入估计。没有修改主报告；以下为应纳入的修改与可接受结论。

## 1. 通过总体路线；修正一个外部能力分类

保留 YMH 的 Go/PG/双图/并发 worker，增量加入确定性准入、执行快照和证据合同，方向正确。草案已经避免把 deferred tools、blocked、恢复、stuck、compaction 列为缺失，也没有引用过时的 PentestGPT 三会话 PTT 作为当前架构。

主报告第 124 行把 Bolt/winhook/linuxhook 合称“外部能力依赖”不精确：Bolt 是远端 MCP 服务接入；winhook/linuxhook 是 CyberStrike 仓库内的内置脚本工具包装，实际依赖本机执行环境/系统能力/第三方命令。它们不等于通用远程 session 执行器，也不能仅由内网专家名称推断端到端 AD 能力。建议改成这三个层次。依据：`E:/cyberagent/CyberStrike/packages/cyberstrike/src/tool/winhook/shared.ts:50`、`E:/cyberagent/CyberStrike/packages/cyberstrike/src/tool/linuxhook/shared.ts:65`、`E:/cyberagent/CyberStrike/packages/cyberstrike/src/mcp/index.ts:625`。

## 2. W1/W2 必须明确有状态实验的隔离和顺序

W1 身份过期/CSRF 轮换与 W2 差分判据均可接受；但 W2 第 204 行只列 baseline/变更/对照，尚未写明运行顺序。

应补：默认先 baseline，再变体，再只读验证业务断言；仅当工具合同声明请求无副作用且身份上下文独立时可并行。涉及状态变更时，使用可重置 fixture、隔离业务对象或显式 setup/teardown；若 auth/CSRF/业务版本在实验中改变，记录新 step attempt 并重做基线或输出 inconclusive，不把两个不同状态的请求直接比较。重放模板需声明副作用、可重试性及业务前置状态。

不能照搬 CyberStrike `E:/cyberagent/CyberStrike/packages/cyberstrike/src/tool/http-replay.ts:427` 的并发 baseline/variant 作为通用方案。该实现可用于一部分无状态对照，不能证明有状态业务流程的有效性。

## 3. O1/S0 必须把执行回执和调度所有权落成合同

主报告已经提 compile、lease 和回执，但当前工作包对 ExecutionReceipt 定义不足，而第 325 行主要把 lease 放到未来远程 runner。建议明确两种生命周期：intent 执行所有权与 session/tunnel 资源所有权分开管理。

本地单体阶段可继续用现有原子 ClaimIntent + execution generation，不必先搭复杂分布式租约；每个 attempt 仍需唯一 generation/fencing token，重启 reconciliation 要拒绝陈旧 attempt 写入新运行状态。未来远程 executor 再在相同合同上增加 lease heartbeat。

ExecutionReceipt 最少记录 execution/attempt/intent/task、compiled plan revision、实际 tool/executor/route/auth 引用及版本、启动结束时间、终态原因、observations、artifact hashes、消耗预算与副作用状态。receipt 是运行事实，不等于 finding。租约过期、工具超时、网络断开不能自动重放有副作用动作；先检查 receipt/进程/业务后状态，无法确认则保留 unknown_outcome，进入对账。

## 4. O1 必须处理现有 blocked 的终结语义

主报告第 270 行以 blocked 原因细分等待可以做，但不能只新增 reason 文本。现有 `E:/cyberagent/artex-ymh-main/artex-ymh-main/db/exploration.go:374` 把 blocked 当 terminal；`E:/cyberagent/artex-ymh-main/artex-ymh-main/agent/coldgraph.go:83` 将其视为 settled；`E:/cyberagent/artex-ymh-main/artex-ymh-main/db/exploration.go:359` 有批量重开 blocked 的现有含义。

建议新增 awaiting 状态；或明确所有读取均用 state+reason 判定，并同步修改 frontier/HasActiveIntent/finished counts/coldgraph/overview/终态判断/重开/UI。transport_error 可保留旧 blocked 语义，unmet_prerequisite 必须保留依赖索引与定向唤醒，不能被批量重试或压缩当成已完成历史。验收增加“仅剩 awaiting 的任务不被误判成功，也不在每次心跳重复调用模型”。

## 5. O1/O2 必须写出并发原子边界与预算结算

主报告已要求原子预算，但还应写明 task/exploration namespace、inflight 唯一约束、计划 revision 校验、前置条件校验、claim 和预算 reservation 在同一事务中完成，避免分开检查导致三个 worker 都通过。

去重键中的身份应区分实际 subject/context 与 role；相同 role 的不同用户不能无条件共享越权测试结果。按 execution environment 指纹判断缓存有效性，不应仅因重建 proxy 的技术版本号变化就无限重新派同一实验。新的有效前提变化才能触发复测。

预算必须区分 reserve/consume/release：真实尝试包括重试都消费，未发送可归还预留；副作用请求超时属于已经尝试，不能全额退回后自动再试。并发名额在成功、失败、取消及崩溃恢复时都需结算。请求预算、认证失败预算与 LLM token 用量是不同计量维度。

## 6. O1 假设账本可以沿用 PG，但不能伪装成已知事实

第 271 行采用现有 payload 和索引渐进落地可接受，无需为了 Hypothesis 引入新数据库。应补：未验证的 hypothesis 使用显式 kind/subtype 和 status，与 observed fact/finding 的可用性分开；不能因存入 fact 表或加入来源边就进入 prove_goal、verified coverage 或权威事实摘要。basis/evidence 仍指向真实观察，hypothesis_id 则是待判定对象。

独立更新、查询或并发需求增长时再拆表。必须保留支持与反驳引用、next discriminating test、环境版本以及 inconclusive，而不是覆盖旧叙述后失去反例。

## 7. S0、W3/W4、I1–I4、O2 的边界与验收通过

S0 正确包含历史寻址、归档/恢复、跨任务拒绝和 capture/egress 解耦；不应缩减为只给当前 worker 注入一个 proxy 字符串。W3 按 method/template/shape/identity/test family 记账，W4 独立读取不可变证据与保留失败原因，均可接受。

I1 区分数据路由与控制路由且防环，I2 区分文件存在与能力可用，I3 区分监听观察、路由可达和服务认证，I4 引用所有权与 cleanup_pending，均覆盖了本轮内网主要风险。O2 保留现有 deferred、profiles 和冷压缩，同时把可判定约束放代码，方向正确。

补一条集成约束即可：权限范围、身份或路由变更导致快照失效时，停止当前 attempt 并保存原因；新 attempt 重新准入。不把 resolver 的兼容默认值作为显式目标路由失败时的静默 fallback。

## 8. E1、数据迁移和粗估可接受，但需保持最小闭环切片

E1 的 26 场景、3 次初筛、消融和不宣称成绩的写法可接受。precision/recall 只在有已知真值的 fixture 计算；未知真实环境报告“已验证覆盖/发现”和未知分母，不能计算不可知召回。30%/15% 是目标而非承诺，应保留当前标注。

数据模型使用逻辑实体而非一次建十张表、legacy store 明确命名、PG 事务/outbox/reconciliation、不能宣称外部副作用 exactly-once，均可接受。迁移增加 schema/version 和兼容默认值，保持单写；影子模式不得额外发送请求或做副作用探测。回滚只回滚选择逻辑，不丢弃已生成的 receipt/evidence。

32–54 人日作为注明前提的粗估可接受，不能解读为完整产品交付期。A 阶段应只要求最小隔离场景而非一口气完成全部 26 场景；B1/B2 可先选一条。环境搭建、跨 OS/浏览器兼容与复现实验成本需在 A 阶段后重新估算。O1 合同及 receipt 的最小版本应在 A 同步确定，不能等 C 阶段才发现 W2/I1 各自记了不同的 attempt。

## 最终意见

总体方案可进入实现设计。完成第 1–6 点的文字与合同修正后，编排与 CyberStrike 对照视角通过。所有工作包、数据/迁移及估计均已过目；未发现需要推翻总体路线或更换框架的证据。

## 修订后复核

已快速重读主报告修订版，前述必须修改项均落实：

1. 第 130、182、218 行已分别修正 Bolt/内置 hook 分类，定义 ExecutionReceipt/attempt/fencing，以及有状态 Web 实验的顺序、业务状态隔离和重新建立基线；副作用超时的对账及不自动重放也已保留。
2. 第 292、295、297、353 行已明确 subject/context 与语义环境指纹、task namespace 下同事务去重/前提/预算/领取、blocked 终态与压缩全链路语义、hypothesis 不能直接 prove，以及 reserved/consumed/released 结算。
3. 第 369–375 行已收窄 MVP 与估计边界，明确 A 阶段最小 receipt/准入合同、影子模式不发请求、回滚保留证据、legacy 授权回填及稳定 store_id。编排与 CyberStrike 对照视角最终通过，无剩余必须修改项；运行效果仍按 E1 验证。
