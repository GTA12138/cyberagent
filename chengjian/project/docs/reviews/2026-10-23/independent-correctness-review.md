# 独立正确性复核（2026-10-23）

复核结论：审查期间确认两个可重复的生命周期问题和一个测试环境隔离问题，已反馈主代理修复。两个运行时问题均使用相同、未改动的独立复现测试完成“修复前失败、修复后通过”验证。当前复核范围内没有尚未修复的具体阻断问题；这不是对整个系统或真实站点效果的完整认证。

复核由实现工作之外的子代理执行。对照 `.build/project-audit-20261023/baseline` 阅读 `traffic/traffic.go`、`server/manager.go`、`server/taskproxy.go`、`agent/worker.go`、`enrich/enrich.go`、API 测试上下文变更、`scripts/test.ps1`、`evals/` 和 CI 配置。未读取业务 `config.json`、`jwt.key`、`data/` 或 `logs/`，未连接 PostgreSQL、模型服务或外部目标，未启动真实任务。所有执行测试都将 `ARTEX_CONFIG` 指向不存在的路径并清空 `ARTEX_PG_DSN`。本次独立复核只修改此报告；复现通过 Go overlay 在系统临时目录注入测试，不修改业务源码。

| 已确认问题 | 影响和证据 | 修复与复测 |
| --- | --- | --- |
| P2：关闭补全引擎不能取消语义分析 | `analyze` 使用 `context.Background()` 创建 60 秒上下文；新增 `Close` 的 `workers.Wait()` 因此会继续等待分析器。独立测试从本机 HTTP 服务取得触发语义分析的 JS，模拟分析器等待 `ctx.Done()`，调用 `Close` 后 300ms 仍不返回，手动释放分析器才返回。修复前 `TestReviewCloseCancelsSemanticWork` 失败。 | `enrich/enrich.go:560` 改为从引擎 `e.ctx` 派生超时；取消或传输失败的正文在 `enrich/enrich.go:468` 后停止处理。相同独立测试修复后通过（0.00s）；新增的正式语义取消、HTTP 取消、失败正文回归亦通过。 |
| P2：关闭中的任务代理可能在全局关闭录制后继续持久化 | `CloseTask` 从 `proxies` 移出实例后，仍可能等待证据读取租约，此时监听保持可用。录制开关只遍历 `proxies`，漏掉该实例。独立测试持住 `withTraffic` 租约，启动 `CloseTask`，等实例进入 `closing`，再关闭录制并经该代理访问本机 HTTP fixture；SQLite 实际增加 1 条交换。修复前 `TestReviewCaptureOffDuringTaskClose` 失败。 | `server/taskproxy.go:413` 在持管理器锁、移出实例前先禁用其录制。相同测试修复后通过（0.14s），关闭录制后的交换不再落库，租约仍能正常释放。 |
| P2：本地回归入口继承真实模型测试开关 | `TestLiveContextReview` 无 build tag，只要继承的 `ARTEX_REVIEW_LIVE_CONFIG` 非空就会读取外部配置并调用真实模型。原测试脚本只覆盖普通配置与 PG DSN，不能保证所声明的“无 LLM”范围。此项由代码路径静态确认，没有发起外呼。 | `scripts/test.ps1:27` 保存该变量，测试期间清空，finally 恢复。已核对三处保存、清空、恢复逻辑。独立复核没有重新启动隔离 PG 或执行脚本全流程。 |

独立执行的验证包括：录制关闭后不写交换与大正文 blob、开关后恢复录制、任务间冷却隔离、在途 HTTP 和语义分析取消、失败正文不再派发派生抓取、任务代理生命周期、关闭录制保留出口与 CA、证据读取租约、流量源归属、代理地址脱敏，以及 worker 流量上下文。所选 `enrich`、`traffic`、`server`、`agent` 测试均通过。额外的两个 overlay 测试分别实际经过本机 HTTP 服务和本机任务代理；不是只比较字段或提示词。

录制屏障位于 `record` 的写锁内、在任何正文或索引写入之前。路由观察在录制调用之前执行，未受 `recordOff` 影响。`SetTrafficEnabled` 用独立外锁维持持久设置与应用顺序；管理器释放 `m.mu` 后才等待任务代理锁或录制写锁；`record` 不反向取得这些管理器锁。关闭中代理的遗漏修复后，本次未再发现该变更引入的具体锁序环。

测试门的范围应继续按证据表述：当前 eval 清单有 5 个自动化场景和 1 个明确缺口，不能称为完整的 26 场景验收；路由模型单元测试也不能替代真实多网段验证。清单所引用的测试名称目前均存在。建议后续让 eval 入口逐项检查清单用例确实产生 `pass` 事件，防止将来测试改名、过滤错误或意外跳过时仍输出已实现场景通过。此为门禁改进建议，并非当前已复现的业务失败。

本次没有独立重跑完整数据库回归、前端构建或 CI Linux 流程，也没有执行 race detector。完整回归数量、前端路由构建和数据库相关结果应以主代理的隔离执行记录为准。HTTP/语义取消验证不等于 DNS 库查询可即时取消，真实模型质量、自主执行成功率和外部站点验收亦不在此结论内。
