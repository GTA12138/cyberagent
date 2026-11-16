# YMH 稳定性加固实施记录

> **【已完成 · 2026-10-23 文档审核】** 第一批稳定性实施（上下文恢复 / 流量与证据来源 / 运行资源生命周期）已交付，并被 10-22、10-23 两轮改动覆盖。保留作变更溯源。
> 本文是历史记录，不是当前状态：文中写的「未做」可能已完成，文中写的「已完成」也可能已被后续改动推翻。
> 现役进度 → [能力评估与演进方案 §7.0](YMH-能力评估与演进方案.md) ｜ 唯一待办源 → [YMH-待办总表](../2026-10-23/YMH-待办总表.md)

日期：2026-10-21。项目：`E:/cyberagent/artex-ymh-main/artex-ymh-main`。

本轮已修改生产代码，完成上下文恢复、流量与证据来源、运行资源生命周期三个模块。它是前述演进方案的首批稳定性实现，未把原方案中所有 Web、路由和编排扩展都算作已完成。保留现有 Go/PostgreSQL/norma 架构，没有数据库结构迁移。

## 1. 上下文膨胀、污染与意图漂移

- `agent/context_snapshot.go` 对只读概况和目标资产执行结构化投影：单份编码结果最多 64,000 字节，限制文本、集合、节点数与深度；保持合法 JSON、UTF-8 和 int64 ID 精度。
- 被省略的内容带显式标记和按需回读提示，不能把缺省字段当作“不存在”的证据。超长资产地址、凭据等原子值整体省略，不制造可以误执行的半截值。
- 当前唯一意图和操作约束继续完整留在系统上下文。恢复输入重新锚定当前任务，提醒核实前提并刷新概况；旧历史不作为当前会话、凭据或路由依然有效的证明。
- 图谱概况和资产快照使用不可信数据边界，降低目标可控文字混入指令的风险；渲染不再删除调用方共享 map 的 coverage。
- Skill 恢复只认可 assistant 的真实调用和后续匹配且成功的 tool_result。拒绝、失败、悬挂和伪造文本不会解锁。回归覆盖真实 transcript 压缩边界后的恢复。

限制：这些预算只适用于可重新读取的快照。权威意图、约束与人工输入没有被静默裁剪，极端巨大的权威输入仍可能占满模型窗口；后续需要写入时校验与可审阅的任务拆分。

## 2. 流量、工具与证据来源一致

- Worker 在工具装配前只解析一次实际代理，将同一结果用于 Bash、WebFetch、提示词和 `RunInfo.TrafficSource`。任务代理失败时准确记录全局回退，避免工具默认读空任务库。
- 流量工具每次调用解析来源。新 ID 为 `task/<taskID>/<captureID>` 或 `global/<captureID>`；显式任务来源必须属于当前任务。浏览器仍走全局代理时可以显式查询 global。
- 旧裸 ID 固定解释为历史全局来源；旧 blob 哈希缺少 source 时也保留全局含义。不会按同名 ID 扫描其他库或猜测回退。
- `LookupRunInfo` 区分“没有上下文”与“显式清空上下文”，避免子运行重新继承父任务流量来源。
- 证据 Record 使用输入任务；Bind 在同一数据库事务内锁定 finding 后读取真实 task_id，再进行来源校验。调用者上下文不能改写 finding 的归属。
- 证据固定来源和正文哈希。raw/global 别名按完整快照元数据哈希去重，不改写旧不可变快照，不无意义增加版本；不同任务中 capture ID 相同的记录仍保持分离。
- 结束任务的流量通过只读 SQLite 打开，不创建任务目录、证书、监听器或迁移表。活动读取持有租约，CloseTask 等待证据复制后再关库。
- SQLite 文件 URI 统一编码，修复特殊路径选错索引问题。历史文件读取检查路径并返回真实缺文件错误。
- 捕获 ID 改为纳秒时间加 128 位随机 nonce，移除每秒 10,000 次回绕和重启同秒碰撞；写入使用普通 INSERT，冲突不会覆盖旧证据。
- Windows 新证据写入修复不受支持的目录 Sync 错误；正文仍先 File.Sync、关闭再 Rename，非 Windows 保留目录同步。Windows 不承诺与 POSIX 目录 fsync 相同的掉电持久性。

边界：本轮未统一浏览器 MCP 的任务专用代理，也未实现按目标动态选路。只读历史与随后重建的 writer 各有实例锁；未来若增加任务历史 blob 删除或跨实例维护，需共享资源协调。

## 3. 生命周期、取消、并发与关闭

- 隧道变更按 ID 串行，停止可取消在途部署，重复停止不取消已开始的清理。
- 健康检查获得操作门闩后重新读取状态，消除旧列表快照导致停止后重新拉起的窗口。
- 受管进程表加锁，通过进程完成通道等待 Wait，避免并发读取 ProcessState。
- 覆盖创建后取消、首次读取失败等窗口，避免永远遗留 deploying。
- 回滚使用独立且最多 30 秒的清理上下文，保留原上下文值；清理未确认时保留台账和 deploy_params.cleanup_pending，后续巡检与重启不会自行重拉。
- 权限异常、身份不明或无法确认远端结果时不报告成功清理。真实任务链仍能从台账看到未完成义务。
- Manager.Close 改为幂等关闭且不持设置锁等待任务代理，消除 Manager/taskProxy 的锁倒序；增加“证据读取需访问设置、关闭同时等待租约”的确定性死锁回归。

没有新增共享会话自动销毁策略，避免一个任务的停止误伤其他任务。旧不受管 PID 的兼容 Kill 分支尚未新增完整 OS 退出等待协议。

## 4. 子代理独立复审

| 模块 | 实施 | 独立复审及结果 |
|---|---|---|
| 上下文、恢复、Worker 来源 | context_stability | independent_review；通过 |
| 生命周期与取消 | lifecycle_stability | independent_review；发现第二取消窗口与未知清理误判，修复后通过 |
| 流量、证据、关闭与 ID | 主代理 | context_stability 和 independent_review；发现回退来源、显式清空、锁倒序及旧别名兼容边界，修复后通过 |
| 数据库集成 | lifecycle_stability | 主代理核对测试日志；各轮自建 cluster 均已停止 |

独立复核记录位于 `.build/reliability-review/INDEPENDENT-REVIEW.md`。测试发现的既有断言问题通过源码基线哈希确认后修正：审批失败仍按现有 ask 策略、JSONB 用结构化比较、企业 scope 按需读取、Windows 路径和 untrusted-data 包装；没有为通过测试而放宽生产执行策略。

## 5. 实际验证结果

测试显式隔离 ARTEX_CONFIG；无 PG 的测试清空 ARTEX_PG_DSN，防止测试自动读到运行配置。需要数据库的测试使用全新本地 PostgreSQL cluster、独立用户/库、随机 loopback 端口，未接触 YMH 的运行库。

| 验证 | 结果 | 日志或说明 |
|---|---|---|
| 全仓库 `go test -p 1 ./... -count=1`，无 PG | 通过；PG 用例跳过 | `.build/reliability-review/full-no-pg.log` |
| fresh PG：evidence 全包 | 通过，2.399s | `isolated-evidence.log` |
| fresh PG：server ScopedEvidence、FindingTraffic、FindingWorkflow | 通过，3.886s | `isolated-server-evidence.log` |
| fresh PG：db 全包 | 通过，31.957s | `isolated-agent-db.log` 的 db 结果 |
| 最终 fresh PG：agent 全包 | 通过，6.486s | `agent-final-pg.log` |
| `go vet`：agent/traffic/evidence/tunnel/server | 通过 | 检查本轮变更包 |
| tunnel 重复运行 10 次 | 通过 | 含取消、持久化失败及并发停止 fixture |
| TypeScript `tsc --noEmit --incremental false` | 通过 | 无新增类型错误 |
| Next.js 静态构建 | 通过，32 页 | `frontend-build.log` |
| Windows Go 嵌入前端构建 | 通过 | `.build/reliability/artex.exe` |
| Linux 后端交叉编译 | 通过 | `.build/reliability/artex-linux`；未作 Linux 运行验证 |

`isolated-agent-db.log` 保留首次扩大 PG 检查的 3 条旧测试契约失败及通过的 db 结果；这些断言修正后使用另一个全新 cluster 跑完整 agent 包，最终通过，以 `agent-final-pg.log` 为准。不是忽略失败或仅跑跳过数据库的测试。

未运行 race：本机 CGO/GCC 环境不满足要求。未进行真实模型和远端隧道端到端联调，因此不作“绝不会中断”的保证。

## 6. 产物与运行状态

候选程序：`.build/reliability/artex.exe`，版本 `ymh-reliability-20261021`，包含本轮前端静态产物。SHA256：`8261ebcaf7a484a25f05c9f44bfb5ee3a4c625ae845e8116f160768932e8ea26`。

本轮没有停止或重启用户服务，没有替换项目根目录正在使用的 artex.exe，没有改配置、密钥或运行任务数据。代码和候选程序已就绪；运行实例需在后续发布时更新。

后续架构工作仍依照《YMH-能力评估与演进方案》：请求与身份模型、浏览器任务隔离、共享预算和能力契约、按目标路由及资源依赖图。应继续逐模块实现、隔离验证与独立复审。
