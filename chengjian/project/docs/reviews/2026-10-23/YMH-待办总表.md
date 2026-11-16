# YMH 待办总表

> 2026-10-23 文档审核产物。**本表是仓库内唯一的待办来源。**
> 其余文档（`docs/reviews/**`、`ROADMAP.md`、`INTRANET-PIVOT-DESIGN.md` 等）里的「下一步 / 待办 / 未做」段落一律视为**历史表述**；若与本表冲突，**以本表为准**。
> 现役进度（12 工作包逐项状态）→ [`2026-10-21/YMH-能力评估与演进方案.md` §7.0](../2026-10-21/YMH-能力评估与演进方案.md)。

---

## 0. 本表怎么用

### 0.1 审核方法

这份表不是把各文档的「下一步」抄一遍，而是**逐项回源码核实**：每条都给出可验证的 `file:line` 证据，并明确区分三种结论——

| 结论 | 含义 | 你该怎么做 |
| --- | --- | --- |
| **未做** | 证据显示代码里确实没有 | 可以做 |
| **部分** | 有一半（通常是有原语、缺账本/缺另一平台），照原方案重做会**推翻已有代码** | 先读「建议动作」，只补缺的那半 |
| **勿重做** | 已经在代码里，只是旧文档没写 | 不要动，见 §4 |

### 0.2 为什么要有这张表

本轮审核发现两类真实浪费风险：

1. **重复实现**：旧文档写「I3 横向原语未做」，实际 `server/lateral_exec.go`（277 行）已经实现并注册，含凭据解密、参数拼装、RoE 校验、`SetVerified` 闭环。照旧文档再做一遍，等于把已有代码推翻重写。
2. **重复提议**：有些项是被**刻意否决**的（如 I4 的 `desired_state`/`generation`），但旧文档里只留了「未做」，后人很容易再提一次。见 §5。

### 0.3 完成一项后怎么改

- 本表：把该行从 §2 移到 §4（改成「已完成」+ 完成日期 + 实施记录链接），**不要删除**。
- 方向文件 §7.0：同步更新该包状态。
- 新写实施记录：放在 `docs/reviews/<日期>/`，格式沿用 `2026-10-23/YMH-能力感知执行与资源依赖收尾实施记录.md`（做了什么 / 哪些没做 / 保留边界）。

---

## 1. 文档状态索引

`docs/reviews/` 下 17 份 + `docs/` 1 份 + 根目录 12 份，逐份判定：

### 1.1 现役（7 份）

| 文档 | 角色 | 说明 |
| --- | --- | --- |
| `2026-10-21/YMH-能力评估与演进方案.md` | **方向文件** | 12 工作包定义 + §7.0 进度表。所有设计取舍以它为准 |
| `2026-10-22/YMH-隧道平台适配与多路径路由实施记录.md` | 实施记录（现役） | Windows 平台适配 + 多路径路由；含 §6 信息系统性自查 |
| `2026-10-22/crawler-web-integration-v2.md` | 实施方案（现役） | 爬虫 3.0「无感集成」；§8 五项待办已并入本表 T-01/T-03/T-04/T-20/T-21 |
| `2026-10-23/YMH-能力感知执行与资源依赖收尾实施记录.md` | 实施记录（现役） | I2 能力感知执行 + I4 资源依赖收尾 |
| `2026-10-23/YMH-补全出口与抓取预算修复实施记录.md` | 实施记录（现役） | T-22 补全走任务出口 + T-01 tarpit 覆盖面 + T-04 索引；含 T-02 的可执行设计 |
| `2026-10-23/YMH-资源租约与路由声明实施记录.md` | 实施记录（现役） | T-09 会话租约 + T-10 终态依赖逆序 + T-11 意图唯一键 + T-05 控制通道路由 + T-06 声明字段 + T-07 认证账本 + T-20 覆盖报告 + T-14 强断言；含开工审核发现的 3 个缺陷 |
| `2026-10-24/YMH-全量收尾实施记录.md` | **实施记录（现役，最新）** | 15 项全量收尾：T-06 自动探测 / T-07 打分接线 / T-08 SSH / T-11 判据 / T-12 评测 26 / T-13 观测 / T-14 拆五块 / T-15 合同+准入 / T-16-18 / T-21 通电 / T-25 隔离 / T-03 真实站点 / 编译启动五探针。**§7 是本轮刻意没做的 10 项清单** |

### 1.2 已归档 / 已完成（13 份，已加状态横幅）

| 文档 | 状态 | 归档理由 |
| --- | --- | --- |
| `2026-10-21/intranet-review.md` | 已归档 | 内网视角评审输入，结论已并入方向文件 §7 |
| `2026-10-21/web-review.md` | 已归档 | Web 视角评审输入，已并入 W1–W4 / O2 |
| `2026-10-21/cyberstrike-review.md` | 已归档 | CyberStrike 对照输入，已并入 §5 / §7 |
| `2026-10-21/intranet-final-review.md` | 已归档 | 交叉复核，七项意见已写入正文 |
| `2026-10-21/web-final-review.md` | 已归档 | 交叉复核，七项意见已写入正文 |
| `2026-10-21/orchestration-final-review.md` | 已归档 | 交叉复核，12 包意见已写入正文 |
| `2026-10-21/YMH-稳定性加固实施记录.md` | 已完成 | 第一批稳定性实施，已被后两轮覆盖 |
| `2026-10-21/YMH-内网演进与AI协作交接.md` | 已取代 | 原始规划，被 10-22 / 10-23 两份记录取代 |
| `2026-10-22/YMH-内网资源可靠性实施记录.md` | 已完成 | 第二轮实施（授权 / 能力合同 / 探活清理 / 上下文预算） |
| `2026-10-22/独立复核记录.md` | 已完成 | 子代理独立复核，无阻塞问题 |
| `2026-10-22/crawler-web-enhancement.md` (v1) | 已取代 | 架构主张（独立 `crawl/` 子系统）被 v2 自我否定，仅保留方法论 |
| `docs/漏洞流量证据.md` | 非待办 | 功能说明 |
| `2026-10-23/YMH-待办总表.md` | **本文件** | — |

### 1.3 根目录设计文档（非待办，但状态需知）

| 文档 | 状态 | 说明 |
| --- | --- | --- |
| `ROADMAP.md` | **已过时**（已加横幅） | 索引的 5 份专题文档已全部不存在；日期 2026-09-13，阶段 0–3 大部分已落地。保留作需求来源 |
| `INTRANET-PIVOT-DESIGN.md` | 大部分完成 | 期 0/1/2/3/3.5/5 完成并验收；期 4 部分；期 6 完成 |
| `CHAINS-INTEGRATION-DESIGN.md` | 部分实施 | `agent/chainskel/`（9 个 md + `chainskel.go` + `verifier.go`）；L2 蒸馏与 L3 完整路由未做 |
| `HONEYPOT-DETECTION-DESIGN.md` | 部分实施 | `honeydetect/` 存在，含 `signatures.json` |
| `DISTRIBUTED-DESIGN.md` | 未实施 | 无 `ToolTransport` / node 协议，纯设计稿 |
| `TESTING.md` / `UI_MIGRATION.md` / `README*.md` / `CHANGELOG.md` / `POSTMORTEM-RED-SUN-3.md` / `AB-REPORT-RED-SUN-3.md` | 非待办 | 说明、记录、报告类 |

---

## 2. 待办总表（执行视图，按优先级）

排序依据：**「哪一件在不通的情况下，后面的做了也白做」**。

> **2026-10-24 全量收尾已交付 15 项**，全部移入 §4.0c，实施记录见
> [`2026-10-24/YMH-全量收尾实施记录.md`](../2026-10-24/YMH-全量收尾实施记录.md)。
> 交付清单：T-03 / T-06 自动探测 / T-07 打分接线 / T-08 SSH / T-11 判据 / T-12 评测 26 /
> T-13 观测 / T-14 拆五块 / T-15 合同+准入 / T-16 / T-17 / T-18 / T-21 通电 / T-25 隔离 / 租约覆盖。
>
> **P0 清空，P1 只剩四项明确延后**（§2.2）。这不是「全做完了」的意思——
> §2.3 单独列出**本轮刻意没做的 10 项**，写清每项卡在什么前提上；
> 实施记录 §7 有逐条理由。

### 2.1 P0 — 验收补充

**无。** T-03 已在本轮跑过真实站点基线（见 §4.0c），P0 清空。

### 2.2 P1 — 剩余四项

| 编号 | 事项 | 现状证据 | 建议动作 | 卡在什么前提 |
| --- | --- | --- | --- | --- |
| **T-19** | W4 ExecutionRecipe（验证者从原始证据 + 独立复测取结论） | **部分**。`agent/verifier.go`、`agent/retester.go` 存在；无 ExecutionRecipe 实体。T-23 的「提交后的重验仍通过进程内回调」与本项同源 | 按方向文件 §7 W4。**验证者必须从原始不可变证据 + 独立复测取结论**，不把发现者的总结当证据。本轮已备好前置：`observation` 的 `artifact_ref` + `artifact_hash` 让「原始不可变证据」第一次真正可引用 | 需要持久化的待验证事件（T-23），否则 Recipe 没有可靠的输入源 |
| **T-20** | `harvest_static.py` 动态交互面（Tab / 弹窗 / 分页） | **部分完成（2026-10-23）**：`--coverage` 已产出 `coverage.json`；运行时仍是单进程串行 `goto`，点不出 SPA 运行时接口 | 自动点 Tab / 弹窗 / 分页。是「原地升级」候选，**不要做成新工具** | 需要真实浏览器；且必须与 W12 一起验，单测代替只会得到假绿灯 |
| **T-23** | 验证派发可靠投递 | **部分**。合并 pending、证据版本校验、旧检查替换及人工处置保护已补 | 持久化待验证事件 + 失败重试与恢复；已运行旧会话的取消语义要明确 | 与 T-19 一起做，两者共用同一张事件表 |
| **T-24** | 页面读取/草稿合同 | **部分**。工作空间保存与关闭、任务/发现失败与重试、内网跨任务响应已修 | 系统 LLM/工具等页仍混合失败与空态；SPA 路由切换的草稿持久化、移动表格与完整无障碍验收待补 | 前端验收，需要人工走查；本轮改动全在后端，`web/` 未动 |

### 2.3 本轮刻意没做（不是遗漏）

完整清单与理由见 [`2026-10-24/YMH-全量收尾实施记录.md` §7](../2026-10-24/YMH-全量收尾实施记录.md)。摘要：

| # | 项 | 卡在什么前提 |
| --- | --- | --- |
| 1 | W12 动态交互面 | 需要真实浏览器 |
| 2 | F06 重启对账端到端 | 需要「kill -9 后核对四张台账」的 fixture |
| 3 | I2 的 winrm / ldap / rdp | 新依赖 + 适配层；SSH 已定下模式，剩下三个是重复劳动 |
| 4 | `reach_verified_ranges` 的 TTL | 需要真实环境数据才能定「多久算旧」 |
| 5 | 语义层端到端收益基线 | 与 W12 同批；模型输出不可复现，可复现的只有解析层 |
| 6 | 拓扑边跨任务视图 | **刻意不做**：会变成信息泄漏通道，跨任务共享要走授权 |
| 7 | `webtest` / `authstate` 接入 worker | 需要 worker 侧先有「差分实验」这个意图类型 |
| 8 | `intent_done` 只看终态 | 保守选择：假 done 比假 unmet 贵 |
| 9 | `tunnel_reach` 只探 445/80 | 加端口要同步加超时预算 |
| 10 | 前端未跑 | 本轮改动全在后端 |

---

## 3. 覆盖视图（12 工作包 → 待办编号）

| 包 | 状态 | 对应待办 | 备注 |
| --- | --- | --- | --- |
| S0 | **完成** | 无 | T-02 / T-22 / T-01 / T-04 全完成 |
| W1 | **完成** | 无 | T-16 `authstate/`（227 行）+ 8 例测试 |
| W2 | **完成** | 无 | T-17 `webtest.Replay` 单点变异 + `CompareWithNoise` 噪声标定 |
| W3 | **完成** | 无 | T-18 `webtest.Frontier` 五轴 + 来源强制 + found 只升不降。**原语未接 worker**，见 §2.3 #7 |
| W4 | 部分 | T-19 | ExecutionRecipe 未做；`verifier.go` / `retester.go` 已有 |
| I1 | **完成** | 无 | T-05 / T-06 自动探测 / T-02 / T-22 全完成 |
| I2 | **完成（执行通道）** | 无 | 协议矩阵 `capability/matrix.go` 五协议全登记，SSH 真实接入；winrm/ldap/rdp 刻意延后见 §2.3 #3 |
| I3 | **完成** | 无 | T-07 账本+打分 / T-08 SSH / T-13 观测与拓扑边 全完成 |
| I4 | **完成** | 无 | T-09 租约 / T-10 端到端；`desired_state`/`generation` 刻意不做，见 §5 N-01 |
| O1 | **完成** | 无 | T-11 六条全交付：唯一键（10-23）+ `prerequisites`/`done_when`/`planning/`（10-24） |
| O2 | **完成** | 无 | T-14 拆五块 / T-15 三段式合同 + `BudgetService.Admit` |
| E1 | **完成（26 项）** | 无 | T-12：23 自动 + 3 人工；W12/F06 仍是人工，见 §2.3 #1/#2 |

### 3.1 I2 的执行通道已定，剩下三个协议是重复劳动

10-23 的判断是「I2 剩下的四项都卡在需要真实客户端」。本轮 T-08 把 SSH 接完，**协议适配层的形状就定下来了**：`capability/matrix.go` 声明能力与退出码档位 → `lateral_ssh.go` 提供 `dialer / auth / run / failureSignature` 四件 → `lateral_exec` 按 method 分派 → 四出口统一入账。

所以 winrm / ldap / rdp **不再是「不知道怎么做」，而是「知道怎么做，暂时不做」**：每个都是同一模式的重复劳动（新依赖 + 适配层 + 测试面），且当前没有真实目标能验证。矩阵里已为三者写清 `Missing`，agent 不会反复试错后把结果记成目标失败。

### 3.2 附：web 侧爬虫能力清单（回答「现在有什么」）

**结论：没有新增「爬虫工具」，而是三条既有路径各自变强。** 判断依据是「agent 的工作流有没有变」——没变，所以按无感判据算能力提升，不算新功能。

| 层 | 能力 | 位置 | 状态 |
| --- | --- | --- | --- |
| **S1 触发** | 登记 `root_domain`/`subdomain` 自动触发 DNS 解析；登记 `service` 自动触发一次 HTTP 抓取。agent 工具签名、技能、提示词全部不变 | `agent/tools_insert.go:376-388`；接口 `agent/tools.go:141-144` | **在跑** |
| **S2 材料理解** | 把响应当材料读，而不是只取 `<title>`。材料类型：HTML / JS / JSON / sourcemap / OpenAPI（`enrich/material.go:27-34`） | `enrich/material.go`（735 行） | **在跑**（确定性，无 AI） |
| | 抽取器五种：`html-form`（表单 method/action/字段）、`js-fetch`、`js-xhr`、`js-axios`、`openapi`（`material.go:304 / 398 / 416 / 424 / 688`） | 同上 | **在跑** |
| | 路由声明 `path:/to:/redirect:/href:` → 只扩「待抓页面」集合，不产生端点（`material.go:361, 446-450`） | 同上 | **在跑** |
| | 只写「method 可确定」的端点；静态材料里 method 未知的路径**不猜成 GET**（`enrich.go:15-19`） | `enrich/enrich.go` | 刻意约束 |
| **S2 回灌** | 端点写回资产图，并在 `task_asset_links.source` 打 `enrich:<抽取器>`，能区分「读出来的」还是「AI 猜的」 | `enrich/enrich.go:441-455` | **在跑** |
| **S2 派生抓取** | 只在预算深度内跟一跳（默认 `MaxDepth=1`） | `enrich/enrich.go:418-421` | **在跑** |
| **S2 预算** | 每任务 80 请求 / 48 MiB / 每主机 30 / 深度 1；零值取默认 | `enrich/enrich.go:58-79` | **在跑** |
| **S2 出口** | 经录制代理发请求，UA 按任务稳定分配（`e.ua(j.taskID)`） | `enrich/enrich.go:203-224, 357-362` | **已修**（T-22）：改走任务 MITM，内网可达且流量落任务仓 |
| **S3 语义层** | `JSAnalyzer` 接口 + `Trigger` 打分 + `EvidenceWindows` 裁剪 + 语义预算 | `enrich/enrich.go:97-99, 191-197, 460-494`；接线在 `server/enrich_semantic.go`（320 行）+ `server/enrich_semantic_parse.go`（239 行） | **已通电**（T-21，2026-10-24）：成本走 `budget.Service.Admit`，额度不足**只降级不暂停任务**；解析层 8 例测试钉住「编造 method / 跨域 / 静态资源一律丢」 |
| **抓取预算** | tarpit 按「每任务每 host」计抓取次数，超预算写一次 hint | `guard/tarpit.go`（`NoteFetch` 单一入口） | **已修**（T-01）：WebFetch + Bash 抓取二进制 + enrich 三条来源合并计数 |
| **skill 侧** | `harvest_static.py` 原地升级到 468 行：sourcemap 还原源码再抽一遍、robots/sitemap 当种子、runtime-config/asset-manifest/routes.json 递归抽基址、产出 `api_methods.txt` / `api_static.txt` / `configs.txt` / `sourcemaps.txt` / `sm_apis.txt`，读 `ARTEX_UA` 保持指纹一致。**命令契约向后兼容** | `skills/api-recon/scripts/harvest_static.py` | **在跑** |
| **态势信号** | `graph_overview` 补上 `sites_without_endpoints`（有 service 但同域无 endpoint 的站点）；`intranet_snapshot` 补上拓扑投影（可达但未打的、声明但未验的） | `agent/tools.go:615` → `db/task_web_surface.go:21`；`server/intranet_snapshot.go` | **在跑**。T-04 索引已补；T-03 真实站点基线已跑（2026-10-24，增量偏小，见 §4.0c） |

三个已知不足全部处理完毕：**出口走错（T-22）已修**、**预算外通道（T-01）已修**、**真实站点（T-03）已跑基线**（2026-10-24，`zhihu.com/signin`：`sites_without_endpoints` 1→0、endpoint 0→1、方法 GET）。真实站点的增量偏小是**结构性的**——公开静态面能读到的就这么多，接口主要靠 JS 运行时才出现，这正是 W12 仍是人工的原因。另有一处文档与实现不一致：`Candidate.Source` 注释列了 `js-route`，但 `reJSRoute` 只往 `m.Pages` 塞，从不产生 Candidate（`material.go:41` vs `:446-450`）——注释该删掉这一项。

---

## 4. 已完成，勿重做

### 4.0a 2026-10-23 工程审查与本机回归

见 [YMH-工程审查与补全实施记录](YMH-工程审查与补全实施记录.md)。

| 编号 | 能力 | 代码位置 | 说明 |
| --- | --- | --- | --- |
| **T-02** | 录制与任务路由解耦 | `traffic/traffic.go`、`server/taskproxy.go`、`server/manager.go`、`agent/worker.go` | 录制开关以写锁阻断正文/Blob/索引持久化，影响存量及新建实例；关录制仍保留任务代理、CA 与阻断状态；提示词依据最终工具集，不依据 CA。 |
| 工程修复 | 补全冷却隔离及退出 drain | `enrich/enrich.go`、`enrich/lifecycle_test.go` | 冷却键加入 taskID；退出取消在途 HTTP 并等待 worker；Manager 在代理和 PG 前关闭补全。 |
| 工程交付 | 可复现质量入口 | `scripts/test.ps1`、`.github/workflows/check.yml`、`web/package.json`、`evals/` | 独立 PostgreSQL、逐用例 JSON 记录及跳过统计；普通分支/PR 增加质量门。E1 保持部分状态。 |
| 工程修复 | 证据版本、发现投影与关键页面 | `db/finding_checks.go`、`db/finding_retests.go`、`db/findings.go`、`web/src/lib/workspace-draft.ts` 及四个页面 | 旧证据结论不能确认新材料；元数据与节点事务一致；草稿保存不误确认，关闭有保护，跨任务及资产范围请求隔离，失败有持续提示。可靠派发及全站统一仍是 T-23/T-24。 |

### 4.0 2026-10-23 第二批（本轮代码改动）

见 [`YMH-补全出口与抓取预算修复实施记录.md`](YMH-补全出口与抓取预算修复实施记录.md)。

| 编号 | 能力 | 代码位置 | 说明 |
| --- | --- | --- | --- |
| **T-22** | 补全走任务出口 | `enrich/enrich.go`（`New` 形参 `func(taskID int64) string`、`withTaskID`/`taskIDOf`）、`server/manager.go`（`enrichProxyForTask`/`taskByNumericID`） | taskID 经请求 context 到 transport 的 Proxy func；任务级拿不到时回落全局并留日志。**DNS 仍走平台 resolvers**，等 T-06 |
| **T-01** | tarpit 预算覆盖补全与 Bash 抓取 | `guard/tarpit.go`（`NoteFetch`、`fetchBinaries`、`scriptRunners`、`fetchTargets`）、`enrich/enrich.go`（`SetFetchObserver`） | 三条来源（WebFetch / Bash 抓取二进制 / enrich）合并到同一份预算。**已知残留**：shell 循环只计 1 次；`python`/`node` 裸 IP 不计（刻意，防误杀） |
| **T-04** | `(type, domain)` 部分索引 | `db/schema.sql`（`idx_av2_type_domain`） | 首次执行会锁表扫一遍 assets，大库升级要选窗口 |

### 4.0c 2026-10-24 全量收尾（本轮代码改动，15 项）

见 [`2026-10-24/YMH-全量收尾实施记录.md`](../2026-10-24/YMH-全量收尾实施记录.md)。

| 编号 | 能力 | 代码位置 | 说明 |
| --- | --- | --- | --- |
| **T-06** | 隧道可达性自动探测 | `server/tunnel_reach.go`（585 行）、`db/schema.sql`（`reach_verified_ranges`）、`server/tunnel.go` | 部署后确定性采样 ≤6 地址 / 12s 预算自动探；`tunnel_reach` 工具供显式调用。**refused 也算正面证据**（RST 说明包到了对端）；全不可达时不写 `verified_ranges`。声明与观测**分列** |
| **T-07** | 认证账本接入复用打分 | `db/auth_attempts.go`（`Successes`）、`db/credential_score.go`（`ReuseEvidence` + 50 分规则）、`server/credentials.go` | 「已在此主机成功」成为最强信号（50 > 已验证 25 / 同网段 40）。账本读失败只降级不阻断——加分项不该有一票否决权 |
| **T-11** | 意图准入与完成判据 | `planning/`（486 行，零 IO）、`db/schema.sql`（`prerequisites`/`done_when`）、`agent/tools.go`、`agent/worker.go`、`server/planning_counts.go` | 判据语法 `kind:arg` 六种。**`Counts` 用指针：`nil`=未知 ≠ `0`=确认没有**。非法判据保留可见不静默丢弃。`command` 类要求 `ManualNote`，不产假绿灯。**只报告不阻断** |
| **T-14** | 提示词拆五块 | `agent/prompt_blocks.go`（93 行）、`agent/worker.go` | 稳定职责 / 当前任务 / 结构化状态 / 本次判据 / 输出停止合同。**state 放 user 不放 system**（易变内容混进 system 会让整段失去缓存价值） |
| **T-10** | 终态释放端到端 | `server/resource_release_e2e_test.go`、`server/server.go`（`tunnelTeardownOverride` 注入点） | 真库 + 替身执行器。断言 id 倒序 / stopped 跳过 / 成败分流 / 租约最后放且**只放本任务的** |
| 租约覆盖 | `tunnel_deploy` 记租约 | `server/tunnel.go` | 补 `noteSessionLease(..., "tunnel")`。之前隧道挂在会话上、会话却不知道自己被依赖 |
| **T-08** | Linux/SSH 横向 | `server/lateral_ssh.go`（394 行）、`server/lateral_exec.go`、`server/taskproxy.go`（`UpstreamForTaskHost`） | `x/crypto/ssh` 真实接入；超时发 SIGKILL 远端真终止。hash 类凭据报错并**指向 wmiexec**。尝试预算检查移到 SSH 分支之前（否则成绕过预算的后门）。经任务选路，与 HTTP 共用一套 |
| **I2 矩阵** | 协议能力矩阵 | `capability/matrix.go`（168 行） | 五协议全登记；未实现的写清 `Missing`（LDAP 特意说明「不是执行通道，塞进 `lateral_exec` 会误导 agent」）。退出码三档 `real/inferred/none`，未知 method 返回 `ExitNone` |
| **T-15a** | 三段式工具结果合同 | `capability/contract.go`（153 行） | `execution_status` × `failure_reason` × `assessment`。三条不变量由 `Validate` 强制；**`not_executed` 禁 `none`/`unknown`**。`ClassifyTransportError` **先认证后网络**。超时归 `partial` 不归 `failed` |
| **T-15b** | 预算准入入口 | `budget/budget.go`（`Admit`）、`budget/admission_test.go` | 非 completion 类开销（语义分析、内联判定）复用同一 store，不引入第二套配额 |
| **T-16** | 身份真值 | `authstate/authstate.go`（227 行） | 替换即版本递增；`Stale` 给原因；Cookie 名排序固定（顺序变化会污染服务端指纹） |
| **T-17** | 差分实验 | `webtest/webtest.go`（448 行） | `Replay` 单点变异，基线缺参数就报错。`CompareWithNoise` **先测对照自身抖动**，>5% 判 inconclusive。`byteDiffRatio` O(n) 逐字节（不用编辑距离——一字符插入会让后续全部错位） |
| **T-18** | 带来源的覆盖前沿 | `webtest/frontier.go`（216 行） | 五轴覆盖单元；`Source` 强制；**found 只升不降**（重测未复现追加 Evidence）；skipped 必须有 Reason |
| **T-21** | S3 语义层通电 | `server/enrich_semantic.go`（320 行）、`server/enrich_semantic_parse.go`（239 行）、`server/server.go` | 前置三件齐了才接：成本走 `Admit`、失败有明确归因、解析层可复现。**编造 method / 跨域 / 静态资源一律丢**。额度不足只降级不暂停任务 |
| **T-13** | Observation 结构化 + 拓扑边 | `observation/`（411 + 157 行）、`db/observations.go`（217 行）、`server/observation_wire.go`（408 行）、`recon/recon.go` | `artifact_ref` + sha256 + 原文落盘；四条不能省的列（observer_session / route / confidence / hash）。拓扑边**分种类**（listener ≠ reach）；`valid_until` 让新鲜可判定，**过期不删除**。顺带修掉「同端口不同绑定被合成一条」的建模缺口 |
| **T-25** | 实例依赖接线 | `agent/deps_isolation_test.go`（176 行）、`server/instance_isolation_test.go`（140 行） | 回调从包级搬到每运行时只读依赖；两道守卫测试（AST 扫包级可变 + 判据统计绑定各自任务） |
| **T-12** | 评测 12 → 26 | `evals/scenarios.json` v3、`evals/run.ps1` | 23 自动 + 3 人工（W11/W12/F06）。纪律：**会跳过的测试不进 automated**（等于把「没跑」记成「通过」） |
| **T-03** | 真实站点基线 | `server/real_site_test.go`（151 行） | `zhihu.com/signin`：`sites_without_endpoints` 1→0、endpoint 0→1、GET。**增量偏小是结构性的**，接口靠 JS 运行时才出现 |
| 工程 | 编译启动验证 | `scripts/verify-start.ps1`（199 行）、`scripts/pgtest.ps1`（119 行） | 五探针：health / 匿名 401 / 登录全链路 / psql 查表结构 / 启动日志标记。`EXIT=0` |

### 4.0b 2026-10-23 第三批（资源租约 / 路由声明 / 认证账本）

见 [`YMH-资源租约与路由声明实施记录.md`](YMH-资源租约与路由声明实施记录.md)。

| 编号 | 能力 | 代码位置 | 说明 |
| --- | --- | --- | --- |
| **T-09** | 会话租约（存活 ≠ 准入） | `db/session_leases.go`、`server/session.go`、`server/session_recon.go`、`server/intranet.go` | `session_leases` 与 `session_access_grants` 刻意分开；删会话有其它持有者时 409，需 `force=1`。租约 best-effort，写失败留日志 |
| **T-10** | 任务终态依赖逆序释放 | `server/resource_release.go`、`server/server_mgmt.go`、`server/task_archives.go` | 先隧道（id 倒序）后租约；独立 90s ctx，不挂请求 ctx。失败只报告不回滚（PG 已提交） |
| **T-11** | 意图 inflight 唯一键 | `db/schema.sql`（`uq_expnodes_intent_live`）、`db/exploration.go`（`ErrIntentAlreadyLive`、`liveIntentID`） | 唯一冲突返回带已有 id 的可读错误，不静默复用；终态后同一方向可重新派 |
| **T-05** | 会话控制通道路由 | `session/http_shell.go`、`session/http_shell_enc.go`、`server/session_control_route.go`、`server/session.go` | 只接受 alive socks；防环（隧道由本会话部署则拒绝）；路由模式拒绝 `lang=auto`；回显剥凭据 |
| **T-06** | 可达范围 / DNS 策略声明 | `db/schema.sql`、`db/tunnels.go`、`server/tunnel.go` | 声明不点亮 `reach_verified_at`；`tunnel_list` 分字段输出。**自动探测未做** |
| **T-07** | 认证结果账本 | `db/auth_attempts.go`、`server/lateral_exec.go` | 四出口入账（含 `error` 与 `failure` 的区分）；30 分钟同签名失败 5 次不再发包。**`HasSuccess` 未接入复用打分** |
| **T-20** | 静态收割覆盖报告 | `skills/api-recon/scripts/harvest_static.py --coverage` | 产出 `coverage.json`；**动态交互面未覆盖**（需浏览器，不假装） |
| **T-14** | 强断言修正 | `agent/chainskel/web.md`、`agent/chainskel/meta_rules.go` | SSTI/JWT 改对照式；「工具阳性可信」→「需独立对照」。**提示词拆五块未做** |

### 4.1 更早的（旧文档写成「未做」但代码里早已存在）

以下各项在旧文档里被写成「未做」，但**代码里已经存在**。照旧文档重做会推翻现有实现。

| 编号 | 能力 | 代码位置 | 说明 |
| --- | --- | --- | --- |
| **D-01** | **横向执行受管原语** | `server/lateral_exec.go`（277 行），注册于 `server/credentials.go:35` | 含凭据解密、明文/hash 参数拼装（`lateralAuthArgs`）、RoE 范围校验、0600 临时文件传凭据、失败签名识别（`lateralFailureSignature`）、成功即 `SetVerified(true)`。**I3 缺的是 Linux/SSH（T-08）与认证结果账本（T-07），不是这个工具** |
| **D-02** | Windows 隧道部署四件套 | `tunnel/platform.go` | probe / plan / deploy / health 按平台分叉；`HostShell` 提供 `BinName`/`TempDirExpr`/`SpawnCmd`/`KillInspectCmd`/`ProbeCmd` |
| **D-03** | 目标平台唯一判据 | `session/platform.go` `PlatformForSession` | 驱动自报 > 机型推断；`platform` 已进 `session_list` 与每轮态势快照（`server/intranet_snapshot.go`） |
| **D-04** | 能力合同 + 方言预检 | `capability/` 包（`For` / `Check` / `CheckCommand`） | 5 处发包前门禁：`session_exec` / `session_read` / `session_write` / `session_recon` / tunnel deploy |
| **D-05** | 删会话按依赖逆序收隧道 | `server/intranet.go`（`ListActiveByViaSession` + 409） | 收不掉返回 409 并保留会话；响应带 `cascaded_tunnels` |
| **D-06** | 授权来的会话显示能做什么 | `db.GrantsForTask` + `server/intranet_snapshot.go` | 非本任务创建的行附 `granted_actions`；查询失败时整列省略（不用假信息换缺失信息） |
| **D-07** | `sites_without_endpoints` 已接线 | `agent/tools.go:615` → `db/task_web_surface.go:21` | 索引（T-04）已完成，本机隔离验证已通过；真实站点验证（T-03）仍未完成 |
| **D-08** | 原生部署链 | `cmd/artex/doctor.go`、`packaging/tools-manifest.json` | chisel / suo5 / gogo 已钉 sha256 |
| **D-09** | chainskel 骨架路由 | `agent/chainskel/`（9 个 md + `chainskel.go` + `verifier.go`） | `chain_tags` 已实现并用于 worker system 注入 |
| **D-10** | 抓包 / 证据的任务归属 | `server/finding_traffic.go`、`traffic/`、`evidence/` | 10-22 轮完成 |
| **D-11** | 上下文分页预算 | 10-22 轮 | 见 `2026-10-22/YMH-内网资源可靠性实施记录.md` |

---

## 5. 刻意不做，勿再提议

以下各项是**评估后主动否决**的，不是遗漏。写在这里是为了让下一次评估不再重新提议一遍。

| 编号 | 被否决的提议 | 否决理由 | 记录位置 |
| --- | --- | --- | --- |
| **N-01** | I4 的 `desired_state` / `generation` 版本字段 | 与已有的 `CleanupPending` 形成**两个真相源**。用户停止后自动重部署的问题，用「先持久化清理意图再清理」已经解决，再加一层版本比较只会让两个状态互相打架 | `2026-10-23/…` §2.3 |
| **N-02** | `enrich` 把抽取结果回告给 agent（写 hint / 加提示） | 违反无感判据。它会变成「多一个要记得去读的东西」，而正确形态是「同样的登记动作产出更多资产」 | v2 §0 / §4 |
| **N-03** | `platform_caps` 写进资源台账 | 平台能力是**会话属性**，不是资源属性。进台账会产生两份平台真相，且台账在会话消失后仍然存在，会变成陈旧断言 | `2026-10-22/YMH-隧道平台适配与多路径路由实施记录.md` §6 |
| **N-04** | 自动给 agent 的命令加退出码哨兵（如包一层 `echo $?`） | 会**改用户命令语义**（影响重定向、管道、`set -e`、交互命令）。当前做法是「空输出 + 无退出码」时给诚实提示 + 给哨兵写法让 agent 自己选 | `2026-10-23/…` §1 |
| **N-05** | 独立 `crawl/` 子系统 + 三个新爬虫工具 | v1 方案，已被 v2 自我否定：把能力做成「多一个要记得触发的东西」 | `2026-10-22/crawler-web-enhancement.md` 顶部横幅 |
| **N-06** | 把 `db.SessionStore.ListAccessible` 接上 | 生产死代码，接入等于多一条未被验证的授权路径 | 轮 2 自查记录 |

---

## 6. 维护规则

1. **新增待办只能加在本表**。写进实施记录正文的「下一步」不具约束力。
2. **每项必须有 `file:line` 证据**。没有证据的待办不算待办，算猜测。
3. **「部分」必须写清缺哪半**。只写「部分完成」等于没写。
4. **完成即迁移到 §4，不删除**。删除会让人下次重新发现同一个东西。
5. **否决即写进 §5**。否决理由比待办本身更值钱。
6. **本表与方向文件 §7.0 双向一致**：包级状态改 §7.0，事项级状态改本表。
