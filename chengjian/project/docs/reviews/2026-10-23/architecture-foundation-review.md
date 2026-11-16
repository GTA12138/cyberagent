# YMH 实例依赖与模型预算基础审查

日期：2026-10-23。独立审查者：architecture_review 子代理。

本报告接续 `independent-architecture-review.md`，只评价本轮 Runtime、模型调用预算与相关归档合同，不将待办总表或整体路线图视为完成。审查期间未修改业务源码，未读取运行配置、密钥、业务 data/logs，未访问业务数据库或外部站点。独立探针位于 `.build/architecture-foundation-review/`，通过 Go overlay 临时加入测试，仅使用本机伪模型服务和脚本创建的独立 PostgreSQL。

## 判断

本轮基础设计方向正确。实例回调从包级变量转为每个 Server 持有的 Runtime；预算从 planner 回合前检查转为 PostgreSQL 串行化的调用准入；取消后的保守结算、重启保留、账本归档也开始共享明确的状态合同。这些是机制层面的改进，能够降低多实例串扰和并发超支的风险。

仍不能将它描述为严格、完整的物理请求 token 硬上限。审查实证了 SDK 内部空响应重试会在一次外层预留下发送多次付费请求；输入估计也明确不构成供应商 tokenizer 的严格上界。持久化 reserved 的保守策略避免退款，但尚缺崩溃后的受控核销流程。工程成熟度应评价为中等，不能因有账本或测试通过就认定生产运行闭环全部完成。

## 待关闭的问题

### P1：SDK 内部重试仍可在一次预算预留下重复付费

证据：`agent/provider.go:276`、`:278` 仍把配置交给 SDK 的建连和空响应重试；`server/model_budget.go:147` 的预算封装只看见一次 `inner.Complete`。norma v0.4.3 的 `llm/openai.go:406` 在空完成时继续发送整个请求，最终仅返回最后一次 usage。把预算移入 pool 成员能够覆盖成员间故障转移，但仍位于 SDK 内部重试之外。

本机探针：第一个 HTTP 200 返回空 content、`prompt_tokens=5`、`completion_tokens=10`；第二个返回文本及同样用量。显式配置一次空响应重试，外层预留输出 10。实际结果：

```text
physical_requests=2 reservations=1 reported_output=10 charged_output=10
```

原始记录：`.build/architecture-foundation-review/sdk-probe.log`。探针通过表示复现了底层行为，并不表示预算安全断言通过。仅此一个隐藏重试即可使真实输出开销为账本记账的两倍；默认策略还允许更多次。请求结束后再记最后一次 usage 无法修复已丢失的前次开销。

最小修复是让任务归属调用禁止 SDK 内部重试，由已有、受预算保护的外层安全窗口重试；若必须保留 SDK 内部重试，则预算与用量捕获必须下沉到每次物理请求。需要保留本机“首个完成为空但有 usage”测试，断言每次发送都受准入且各次用量累加，不能只测试外层 fake provider 返回 503。

### P2：崩溃遗留预留没有受控核销入口，任务可能长期不能继续或归档

证据：`db/model_budget.go:104` 将 reserved 永久计入可用额度；`budget/budget.go:66` 容量等待有 30 秒上限；`db/model_budget.go:17`、`db/task_archives.go:572` 拒绝包含 reserved 的归档快照。当前预算 API 提供聚合状态，未见按调用列举、确认所有者已退出及保守结算遗留预留的管理流程。

复现条件是进程在 Reserve 提交后、Settle 提交前退出。重启仍保留该记录是正确的；但后续调用可能每次等待 30 秒后失败，归档也会持续被拒绝。直接自动删除或超时退款会引入真实漏记，不建议这样处理。

最小完善应提供可审计、幂等的“确认不再活跃后按原预留转 uncertain”操作，沿用 task→call 锁顺序，不释放保守计费用量；在拥有可验证的进程/调用身份之前，不应自动认定预留已过期。当前可先明确提示 pending 的原因、恢复步骤与归档受阻原因。本项是运行恢复能力缺口，不是要求放宽准入。

## 本轮已修复并复核的边界

| 合同 | 当前实现与独立判断 |
| --- | --- |
| 宿主隔离 | `agent/runtime.go:14` 将工具、提示、收尾和证据策略置于实例值；各 runner 在入口 `WithRuntime`。`agent/assembly.go:88` 保留工具来源 Runtime，叠加到调用者新 context，取消与任务归因可继续保留。未再发现本轮八类回调通过包级变量覆盖其他 Server。闭包内部对象仍需并发安全，SetRuntime 仍应限于发布前。 |
| 调用准入唯一真值 | `db/model_budget.go:41` 在 task 行锁下读限额、汇总 spent/reserved 和创建调用记录；Settle 先锁相同 task，再锁 call，幂等地把 reserved 转 settled/uncertain。没有第二份可写内存余额，跨 goroutine、跨进程序列化方向正确。 |
| 任务归属与旁路 | task router、普通任务对话、finding check/retest 后续对话、side question 均有任务归属注入。最新 `conversationRunContext` 还把 finding 对话纳入任务运行操作计数；finding 追问的归属不依赖仍为 pending。`server/model_budget.go:61` 从 llmrec 或 RunInfo 动态归属，裁判模型可沿 `providerForProfile` 得到同样准入。 |
| pool 封装边界 | `server/llmpool.go` 的成员为动态 dispatch 包装，pool managed marker 避免再次整体预留。`llmpool/pool.go:90`、`:139` 把宿主预算/生命周期错误直接返回，不当作模型健康失败或继续故障转移。这个边界修复的是 pool 多成员调用，不能覆盖上述 SDK 隐藏请求。 |
| 取消与缺失 usage | `server/model_budget.go:137` 使用新的 5 秒结算 context，不受模型调用取消影响；提前结束 stream 也通过 defer 结算。缺失/不完整用量至少记原预留，持久化失败保留 reserved，方向是保守拒绝。提前停止 stream 时结算失败不能再交付给消费者，建议至少记录日志供诊断。 |
| 缓存用量 | 审查发现原实现对 InputTokens 再加缓存子集，导致重复扣费。最新 `budget/budget.go:100` 只计 norma 已归一的总 InputTokens；`db/model_budget.go:59` 历史导入也只汇总 input_tokens。该修复与 norma 的 OpenAI/Anthropic 合同一致。 |
| 归档准入 | `db/model_budget.go:45` 在相同 task 行锁下拒绝 archive_queued/archiving 等非 archive_failed 状态新增预留。独立隔离 PG 覆盖 queued、archiving 两个场景，均返回 ErrTaskArchiveState。 |
| 归档快照与压缩删除 | Snapshot 拒绝 unresolved reserved；`db/task_archives_restore.go:53` 再检查无 pending，并在 task 锁下比较当前账本与包内快照，变化即拒绝热数据删除。独立测试验证 pending 快照拒绝、结算后快照成功、快照后账本变化导致完成归档失败且热账本保留。此检查补上了只 drain 本 Server 任务不足以防住其他后台调用的问题。 |

归档相关新增预算操作采用 task→call 的锁序；settlement 与预留、归档队列/完成共用 task 锁。只读快照采用 repeatable read，完成归档前再比较账本，避免把一致快照误认为以后不会发生改变。该复核未发现新预算写路径的反向 call→task 锁序。

## 仍需诚实表述的能力界限

- 输入预留由完整中性请求的 JSON 字节数加固定开销估计，见 `budget/budget.go:64`。它对常见文本 tokenizer 偏保守，但没有供应商 tokenizer 或供应商新增内容的严格上界，也可能提前拒绝实际仍负担得起的调用。应把“共享预留准入”与“精确输入 token 限额”区分。
- `Usage` 没有字段存在性标记。当前任一维度为零时按不确定用量处理，避免漏记；真实的零输出也可能被保守多记。这是当前数据合同的取舍，应在诊断中可见。
- 同一进程中的全部宿主状态并未因此全部实例化。本轮消除了指定八类 agent 回调；不应据此推导任意全局缓存、签名引擎和第三方 SDK 都已经支持任意多宿主隔离。
- 旧报告指出的持久化 finding 派发 outbox、共享会话活性租约、完整效果评测等仍是单独未完成事项。预算账本不等于这些领域的事务和生命周期已经闭合。

## 验证记录与成熟度

独立 DB 验证记录：`.build/verification/20261023-232118-1c8c4c/`，`go vet ./db` 通过，测试 4 通过、0 跳过。测试文件通过 overlay 注入，业务源码未改。覆盖归档 queued/archiving 准入、pending 快照和陈旧账本快照保护。

独立 SDK 本机伪端点诊断：`.build/architecture-foundation-review/sdk-probe.log`，1 个探针完成，复现两次 HTTP 请求仅返回一次 usage。它只针对依赖库行为，使用本机 HTTP 服务。

独立 server 验证记录：`.build/verification/20261023-233732-40706a/`，`go vet ./server` 通过，测试 4 通过、0 跳过。覆盖 `reviewCompletion` 裁判路径在 llmrec/RunInfo 两种归因下的预算拒绝，已耗尽时两者都没有调用伪模型；pool 的首成员 503、第二成员成功，严格形成两条调用账本，输出支出 13、pending=0，未产生第三条外层重复预留。首次执行曾遇到并行编辑中的缺失 import，修正后重跑通过；这个中间编译状态不列作最终设计缺陷。主代理全量回归及后续重跑结果应以最终实施记录为准。

架构方向评价为较强：双图、任务路由、证据版本、实例依赖和持久化准入具有连贯的领域思想。工程成熟度仍为中等：正常路径已有坚实防护，跨依赖库重试、崩溃恢复和用户可操作诊断仍需补足。扩展成本比旧全局接线降低，但 server 仍同时承担 HTTP、运行调度、provider 组合和领域接线；后续宜保持 Store/Service/Provider 边界，避免在更多调用点增加互相绕过的临时包装。
