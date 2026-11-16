# YMH 全量收尾实施记录（2026-10-24）

> 上一轮交付：`2026-10-23/YMH-资源租约与路由声明实施记录.md`（T-09 / T-10 / T-11 唯一键 / T-05 / T-06 声明 / T-07 账本 / T-20 / T-14 强断言）。
> 本轮目标：把待办总表 §2 里剩下的**半落地 7 项**与**完全未做 8 项**全部做完，输出报告，最终重新编译启动验证。
> 阅读顺序建议：§0（本轮判据）→ §7（明确没做的）→ §9（验证证据）。中间的 §1–§6 是实现细节，可按需跳读。

---

## 0. 本轮的统一判据

这一轮改动面很大（33 个新文件、约 6.9k 行），所以先把判据写死，否则后面每一条都会变成「我觉得应该这样」。

| 判据 | 含义 | 反例（本轮拒绝过的写法） |
| --- | --- | --- |
| **无感** | 删掉改造，agent 工作流不需要改一个字；装上它，同样的动作产出更多 | 给 agent 加一个「记得调用 `tunnel_reach`」的提示词 |
| **可据以行动** | 给 agent 的信息必须能改变下一步动作，否则是噪音 + 误读风险 | 把「拓扑边 12 条」塞进态势快照 |
| **信息同步三条件** | 有生产者 → agent 有入口可读 → 读后能改变行动。缺一条就是半落地 | 只写库不给读（本轮之前的 T-06 就是） |
| **unknown ≠ unmet ≠ met** | 缺数据必须报 unknown，不能猜「没有」 | 判据统计读失败时返回 0 个凭据 |
| **声明 ≠ 观测** | 「我以为」和「我试过」必须分列存储 | 一次局部探测把整体声明冲掉 |
| **not_executed ≠ failed** | 没执行推不出任何关于目标的结论 | 超时归 `failed`，触发 agent 重跑一条已在目标上跑起来的命令 |

---

## 1. 波次 A：把 7 项半落地接通

### 1.1 T-06 自动探测（原状态：`MarkReachVerified` 零调用方）

**问题**：`reachable_ranges` 是 agent 声明的，`reach_verified_at` 从来没人点亮。等于有一个「已验证」字段，永远为空——比没有更糟，因为读的人会以为「已验证 = 0 条」是结论。

**做法**（`server/tunnel_reach.go`，585 行）：

| 件 | 位置 | 关键约束 |
| --- | --- | --- |
| 确定性采样 | `sampleCIDR(cidr, max)` | 等距采样而非随机：同一份声明两次探测必须探同样的地址，否则「这次没通」无法归因 |
| 探测端口 | `reachProbePorts = {445, 80}` | 445 关闭时回 RST，正好给出「路通但端口没开」这个最有价值的中间态 |
| 单主机探测 | `probeOneHost` | **refused 也算正面证据，不再试下一个端口**——再试只会把「端口没开」冲淡成「都不通」 |
| 部署后自动探测 | `autoVerifyReach` | ≤6 地址 / 12s 总预算 / 并发 4；**全不可达时不写 `verified_ranges`**（「没探到」≠「确认没有」） |
| 显式工具 | `tunnel_reach`（`ReadOnly: true`） | 给 agent 一个「我现在想验一下」的入口 |
| 存储分列 | `db/schema.sql` + `db/tunnels.go` | 新增 `reach_verified_ranges`，`MarkReachVerified` 只写这一列 |

`tunnel_deploy` 加 `skip_reach_check` 参数（部署时已经知道探不通的场景不必再花 12 秒）。

**测试**：`db/tunnels_test.go` 的 `TestTunnelReachDeclarationIsNotObservation` 补三条断言——空观测不清空已有观测、带实测只写观测列、声明在观测后仍存活。

### 1.2 T-07 凭据复用打分（原状态：`HasSuccess` 零调用方）

**问题**：认证账本写得很全，但打分不读它。于是「这把密码刚才在 10.0.0.5 上打成功过」这个**最强信号**没有参与排序，agent 拿到的是「同网段 +40 分」这种弱推断。

**做法**：

- `db/auth_attempts.go` 新增 `Successes(ctx, credentialID, limit)`：`SELECT DISTINCT service, target ... WHERE outcome='success'`，批量读（一次 500 条），不做 N+1。
- `db/credential_score.go` 新增 `ReuseEvidence{ProvenServices map[int64]map[string]bool}`，打分规则加一条 **50 分**（高于「已验证」25 分与「同网段」40 分）。
- `server/credentials.go` 的 `credential_reuse_plan` 接线：用 `assetIDByHost` 把账本的 `target`（IP）落到候选资产 id 上。

**一个刻意的选择**：账本读失败只 `log.Printf` 不阻断。账本是**加分项**不是前置条件——为了一次读失败让整个复用规划不可用，是把可用性换成一致性，方向错了。

### 1.3 T-11 意图准入（原状态：`prerequisites` / `done_when` / `planning/` 全不存在）

**问题**：意图的 `payload` 是自由文本。如果「开工前应具备」和「收工前自查」只写进提示词，那就变成「提示词里写了但没人检查」——agent 读到一段散文，系统没有任何判定。

**做法**：新建 `planning/` 包（486 行，**零 IO 纯函数**）。

判据语法 `kind:arg`：

| kind | 含义 | 数据来源 |
| --- | --- | --- |
| `fact:<关键词>` | 探索图里存在匹配的 fact | `exploration_nodes` |
| `asset:<类型>` | 资产图里有该类型资产 | `assets` |
| `credential:<服务>` | 有该服务的凭据 | `credentials` |
| `session` | 有存活会话 | `session_leases` |
| `tunnel[:socks]` | 有 alive 隧道 | `tunnels` |
| `intent_done:<关键词>` | 该方向的意图已完成 | `exploration_nodes` |

三条硬约束：

1. **`Counts` 用指针表示「未知」**：`Credentials *[]CredentialView`、`Sessions *int`、`Tunnels *int`。`nil ≠ 0`。这是全包最重要的一个类型选择——如果读失败时返回 0，「0 个凭据」和「不知道有几个凭据」在 `Evaluate` 里长得一样，agent 就会去修一个没坏的东西。
2. **非法判据保留可见**：`ParseAll` 把解析失败的项留成 `Kind=""`，不静默丢弃。丢掉等于让一条写错的判据永远显示「通过」。
3. **`command` 类完成判据要求 `ManualNote` 非空**：显式承认「这条要人判」，不产生假绿灯。

落库：`exploration_nodes` 加 `prerequisites` / `done_when` 两列（JSONB），**同时**写进 payload 一份——worker 读的是 payload，不在里面它就看不到要求。

消费侧：

- `add_intent` 返回 `unmet_prerequisites`（**只报告不阻断**：阻断会把「缺前置」变成「不能开工」，而很多意图的正确做法是「带着风险试一次」）。
- `worker.renderIntentTask` 把判据渲染成人话：`开工前应具备（缺了不必等，但要预期撞墙）` / `**收工前自查**（这些是这条意图的完成判据，缺一条就说明还没做完）`。
- `server/planning_counts.go` 提供 `planningCountsForTask` 闭包，**任何一类读失败整份返回零值 Counts**（部分数据会混出半真状态，比全 unknown 更危险）。

**测试**：`planning/planning_test.go` 12 例，钉住「unknown ≠ unmet」「确认 0 才算 unmet」「非法判据保留可见」「空判据无 gap」。

### 1.4 T-14 提示词拆五块

**问题**：`workerSystem` 是一段长字符串，稳定职责和易变态势混在一起。后果有两个：整段失去缓存价值；模型眼里「你是一个渗透执行器」和「当前有 3 个会话」权重相同。

**做法**（`agent/prompt_blocks.go`，93 行）：

| 块 | 内容 | 位置 |
| --- | --- | --- |
| `BlockStableRole` | 稳定职责 | system |
| `BlockCriteria` | 本次场景判据 | system |
| `BlockStopContract` | 输出停止合同 | system |
| `BlockCurrentTask` | 当前 goal + intent | user |
| `BlockState` | 结构化态势 | user |

**state 刻意放 user 不放 system**：易变内容混进 system 会让整段失去缓存价值，且让「稳定职责」与「当前态势」在模型眼里权重相同。

`titleOf(id)` 对未知 ID 返回 ID 本身（不是空串），这样拼错块名会立刻在提示词里暴露。

### 1.5 T-10 端到端（原状态：只有单测，没有真库端到端）

**问题**：`releaseTaskResources` 依赖真实隧道才能测，而真实隧道要远端。于是这条最危险的路径（任务终态按依赖逆序释放）只有零散单测。

**做法**：`server.Server` 加 `tunnelTeardownOverride func(ctx, id) error` 注入点。两个理由写在字段注释里：① 集成测试要在无真实远端时验证收尾语义；② 将来换隧道后端不该改收尾路径。

`server/resource_release_e2e_test.go`：三条隧道（alive / alive / stopped）+ 真实会话 + 两条租约；注入 override 记录顺序并让第二条失败。断言四件事：id 倒序、stopped 跳过、成功进 `Tunnels` 失败进 `Failures`、租约最后放且**只放本任务的**。

> **这里我一开始写错了期望**：原断言是「应释放 2 条租约（含别的任务持有的那条）」。跑出来只有 1 条才想明白——别的任务的租约是**它的**存活声明，`ReleaseTask` 放掉它等于替别人宣告死亡。改成断言 `rel.Leases == 1` + `OtherHolders == [otherTask]`，再显式释放另一条验证归零。

### 1.6 租约覆盖（`tunnel_deploy` 不记租约）

`tunnel_deploy` 成功后补 `s.noteSessionLease(ctx, rec.ViaSessionID, "tunnel")`。之前只有 `session_recon` / `session_exec` 记租约，于是「隧道挂在会话上、会话却不知道自己被依赖」——删会话时看不到这条依赖。

---

## 2. 波次 B1：T-08 Linux/SSH 横向 + I2 协议矩阵

### 2.1 T-08 SSH（原状态：`lateralMethods` 只有 impacket 五个）

**做法**（`server/lateral_ssh.go`，394 行）：

| 件 | 关键决定 |
| --- | --- |
| `sshAuth` | 只收 `CredPassword` / `CredSSHKey`；hash 类报错并**指向 wmiexec**（hash 对 SSH 无用，报「不支持」比报「认证失败」诚实） |
| 带口令私钥 | 用 `ssh.PassphraseMissingError` 明确报「不支持交互式解密」，不假装能解 |
| `sshDialer` | 空 proxyURL = 直连；非空经 `parseSocksURL` + `proxy.SOCKS5`，断言 `proxy.ContextDialer`（否则拿不到带 ctx 的 dial，超时失效） |
| `HostKeyCallback` | `ssh.InsecureIgnoreHostKey()`，理由写在代码里：渗透场景目标是任意主机，没有可信 known_hosts；把它做成配置项会让「默认安全」变成「默认不可用」 |
| 超时 | 发 `ssh.SIGKILL`，远端进程被**真实终止**（这是 SSH 相对 impacket 的核心优势之一） |
| `sshFailureSignature` | 只认认证类；命令失败由**退出码**表达，不塞进签名 |

**选路复用**：`server/taskproxy.go` 新增 `UpstreamForTaskHost(taskID, host)`，复用 `RouteTable.Select`。这样非 HTTP 的受管动作（SSH、将来的 SMB）共享同一套选路，不会出现「HTTP 走隧道、SSH 直连」的双轨。

**四出口入账**：经 `s.m.taskProxy.UpstreamForTaskHost` 选路后，四种结局都写认证账本。**未执行** → `StatusNotExecuted` + `ClassifyTransportError`；**执行了** → `StatusSucceeded`/`StatusFailed` + `ReasonNone`。

`lateral_exec` 的尝试预算检查**移到 SSH 分支之前**——否则 SSH 成了绕过「30 分钟同签名失败 5 次」的后门。

### 2.2 I2 协议矩阵（原状态：全仓零命中）

**做法**（`capability/matrix.go`，168 行）：五协议全登记，未实现的写清缺什么。

| 协议 | Implemented | 退出码 | 说明 |
| --- | --- | --- | --- |
| ssh | ✅ | `real` | cancel = SIGKILL，远端进程真实终止 |
| smb | ✅（method=wmiexec） | `inferred` | cancel 只能杀本地 impacket 进程，**远端已落地的服务/进程可能残留** |
| winrm | ❌ | — | `Missing` 写明缺客户端依赖 |
| ldap | ❌ | — | 特意说明「**不是执行通道**，塞进 `lateral_exec` 会误导 agent」 |
| rdp | ❌ | — | 「刻意不实现」 |

`ExitStatusOf(method)` 对未知 method 返回 `ExitNone` 而不是 `ExitReal`——默认成 real 会让一个没有退出码的通道凭空长出「命令执行成功」的结论。

**测试**：`capability/matrix_test.go` 6 例、`server/lateral_ssh_test.go` 6 例。

---

## 3. 波次 B2：T-15 ToolCapability 合同 + BudgetService

### 3.1 三段式工具结果合同（`capability/contract.go`，153 行）

`execution_status`（succeeded/failed/not_executed/blocked/partial）× `failure_reason`（none/auth/network/capability/precondition/budget/timeout/environment/unknown）× `assessment`。

`Validate()` 强制三条不变量：

1. `succeeded` 必须 `none`（成功却带失败原因 = 两套结论）
2. `not_executed` / `blocked` 必须给**明确归因**，禁 `none` / `unknown`（「没执行，不知道为什么」等于没信息，但会看起来像有信息）
3. 未知枚举拒绝

`ClassifyTransportError` **先认证后网络**：`permission denied` 归 auth。归错的代价不对称——归成网络，agent 会去修一个没坏的网络；归成认证，agent 会去换凭据。

`lateral_exec` 两条路径（impacket / SSH）都接。超时出口用 `StatusPartial` + `ReasonTimeout`：命令可能已经在目标上跑起来了，归 `failed` 会触发重跑，风险更大。

### 3.2 BudgetService 准入（`budget.Service.Admit`）

`Reserve` 抽出 `reserveWithWait`（唯一等待实现，避免两处各写一份而一份没超时）。

`Admit(ctx, taskID, tokens)` 是**非 completion 类开销**（语义分析、内联判定）的准入入口：复用同一 store，不引入第二套配额；`maxTokens=0`；拒绝负估算与 `taskID<=0`。

---

## 4. 波次 B3：T-16 / T-17 / T-18

### 4.1 `authstate/`（227 行）— W1 身份真值

- `Identity{Principal, Version, Headers, Cookies, Roles, Source, UpdatedAt}`；`Ref()` = `principal@version`。
- `Put` 替换时**版本递增**：否则基于旧会话得出的结论看起来仍然成立。
- `Stale(ref)`：空引用、身份不存在、版本落后都算过期，**并给出原因**。
- `HeaderPairs()` 按 Cookie 名排序：顺序变化会影响服务端指纹/缓存，让同一身份在两次请求里长得不一样。

### 4.2 `webtest/`（448 + 216 行）— W2 差分 + W3 覆盖

`webtest.go`：

- `Template` 用 `{name}` 占位；`Key()` 归一化占位名（`/api/user/{id}` 与 `/api/user/{uid}` 是同一单元），`ParamSchema()` 排序后拼串（顺序不同不算两个单元）。
- `Replay(base, Mutation)` **单点变异**；`From` 不在基线里就报错（防「在错误基线上变异后把结果当差异」）。
- `CompareWithNoise(controlA, controlB, variant)`：**先跑两次同一请求测抖动**，抖动 >5% 判 `inconclusive`；阈值 = `max(噪声×2, 1%)`。
- `byteDiffRatio` 用 O(n) 逐字节 + 长度差计入分子。**不用编辑距离**：会把「一字符插入导致后续错位」算成巨大差异。
- `FindingState()` 映射到既有 `verified/false_positive/inconclusive`，**不新建第二套 finding 状态**。

`frontier.go`：

- 覆盖单元**五轴**：method × route template × param schema × identity × test family。
- `Mark` 强制：`Source` 非空、`skipped` 必须有 `Reason`、状态合法。
- **`found` 只升不降**：重测未复现时追加到 `Evidence`，不抹掉发现。
- `Coverage()` 里 `skipped` 不计入 tested，但 `Uncovered` 里算已处理（两种口径都有用，所以分开）。

**测试**：`webtest_test.go` 16 例、`authstate_test.go` 8 例。

---

## 5. 波次 B4：T-21 通电 / T-13 观测 / T-25 隔离

### 5.1 T-21 S3 语义层通电（原状态：`SetJSAnalyzer` 全仓零调用方）

**问题**：接口、`Trigger` 打分、`EvidenceWindows` 裁剪、语义预算全都在，就是没人接。一个不通电的语义层比没有更糟——它让「S3 已实现」这句话看起来成立。

**前置三件**（上一轮记录的约束：三件齐了再接）：

| 件 | 落点 |
| --- | --- |
| 成本归属 | `budget.Service.Admit`（按任务准入，额度不足**只降级不暂停任务**） |
| 失败语义 | 模型不可用 / 解析失败 → 返回空候选 + 明确 reason，**不当作「没有候选」** |
| 评测基线 | `server/enrich_semantic_parse_test.go` 8 例：模型输出不可复现，但「这段文字能变成哪些候选」可复现 |

**做法**：

- `server/enrich_semantic.go`（320 行）：`enrichSemantic` 实现 `enrich.JSAnalyzer`。取 provider 走 `s.providerForProfile`，成本走 `Admit`，超时/取消单独归因。
- `server/enrich_semantic_parse.go`（239 行）：解析模型输出。**编造 method / 跨域 / 静态资源一律丢**——编造端点比漏掉端点贵得多（agent 会去打一个不存在的端点，然后把 404 读成「端点关了」）。
- `server/enrich_semantic_parse_test.go`（211 行）8 例，含「容忍 ```json 围栏与 wrapper」「确定性：同样输入同样输出顺序」。
- `server.New` 里 `m.enrich.SetJSAnalyzer(...)`，启动日志打出 `[enrich] S3 语义层已通电：按任务预算准入，额度不足只降级不暂停任务`。

**为什么「只降级不暂停任务」**：语义层是**增强**不是**依赖**。额度不足时退回确定性抽取器，产出少一些但流程不断——这才符合无感判据。

### 5.2 T-13 Observation 结构化（原状态：`artifact_ref` 全仓零命中）

**问题**：侦察输出（`session_recon`）直接进资产图，原文丢掉。后果是「这个端口是谁看到的、从哪条路看到的、输出有没有被截断」三个问题都答不了。

**做法**：

`observation/observation.go`（411 行）+ `observation/artifact.go`（157 行）+ `db/observations.go`（217 行）+ 两张表。

`observations` 表四条不能省的列：

| 列 | 为什么不能省 |
| --- | --- |
| `observer_session` | 同一条结论从不同立足点看可能不同（NAT/ACL/分区），必须能回答「谁看到的」 |
| `route` | 经过哪个出口；路由变了，旧结论的适用范围就变了 |
| `confidence` | `complete/truncated/partial`，与证据等级分开：**被截断的输出仍是 observed，但支撑不了「没有别的端口」这种否定结论** |
| `artifact_ref` + `artifact_hash` | 原文落盘 + 可校验；不存原文的观测等于不可复核 |

`topology_edges` 表：

- **分种类建模**，不是一张主机-主机图。`listener`（本机事实：这台机器 445 开着）与 `reach`（探测事实：从我的立足点能连到 445）是两件事，**只有后者能支撑「能打」**。
- `valid_until` 让「新鲜」可判定。**过期边不删除**——删除会让「环境变了」和「从来没有过」在快照里长得一样。
- 幂等键 `(task_id, kind, from_node, to_node)`；证据等级升级规则在 Go 侧（`observation.Promote` + 行锁），不在 SQL 里再写一份。

**顺带修掉的一个建模缺口**（`recon/recon.go`）：旧 `ParseConnTable` 按 `proto|port` 去重，同一个 3306 同时绑在 `0.0.0.0` 与 `127.0.0.1` 时会被合成一条，**留下哪条取决于输出顺序**。于是「这个端口能不能远程打」变成了一个由排序决定的结果。改为保留 `Bind` 并归一化各工具写法（`*` / `[::]` / `0.0.0.0` → `0.0.0.0`）。

> 这是 I3 验收里最贵的一条误判：把 `127.0.0.1:3306` 当成 `10.0.0.5:3306`，planner 会派一条注定失败的横向意图，而失败会被读成「服务关了」。

**消费侧**（信息同步三条件的第二条）：

- `server/observation_wire.go`（408 行）：把 `session_recon` 输出、`tunnel_reach` 探测结果写成观测 + 拓扑边。
- `server/intranet_snapshot.go`：把拓扑投影加进 planner 每轮读到的态势快照——**只放能改变下一步行动的东西**（可达但未打的、声明但未验的），不放「12 条边」这种统计。

### 5.3 T-25 实例依赖接线

**问题**：agent 的工具/提示词依赖「捕获宿主」的包级可变回调。单实例看不出问题，多实例时一个实例的配置会漏进另一个。

**做法**：把回调从包级搬到每运行时的只读依赖对象，并加两道守卫测试：

- `agent/deps_isolation_test.go`（176 行）：`TestNoPackageLevelMutableCallbacks`（AST 扫包级 `var ... = func` 形态）+ `TestSetMethodsOnlyTouchTheirReceiver`。
- `server/instance_isolation_test.go`（140 行）：`TestPlanningCountsAreBoundToTheirOwnTask`（两个任务各读各的）+ `TestPlanningCountsDegradeToAllUnknown`（读失败整份 unknown）。

> 写这个测试时踩了一次坑：一开始只装 `pg`，判据统计四个子系统缺三个，于是「两个任务都是全 unknown」也算通过——测试在证明「因为没装所以都空」。补齐 credStore / sessStore / tunnels 之后，它才真的在验证「装配齐全时各读各的」。

---

## 6. 波次 C：评测扩到 26 / 真实站点 / 编译启动

### 6.1 T-12 评测 12 → 26

`evals/scenarios.json` v3，26 条：

| 组 | 编号 | 数量 | 内容 |
| --- | --- | --- | --- |
| Web | W01–W12 | 12 | 差分实验 / 身份版本 / 覆盖五轴 / 模板归一 / 语义层候选过滤 / 增量 / 跨任务隔离 / 判据 unknown / 实例隔离 / 静态收割 / 动态交互面 |
| 内网路由 | I01–I08 | 8 | 双段独立选路 / 单路径切换 / 无环与 hop 上限 / 回环不当远程 / 协议矩阵 / 退出码三档 / refused 也是路通 / 声明与观测分列 |
| 故障恢复 | F01–F06 | 6 | fail-closed 授权 / stop 与健康重拉竞态 / 远端不可达不虚记 clean / timeout 后状态核对 / 依赖逆序释放 / 重启对账 |

**23 自动 + 3 人工**（W11 静态收割覆盖报告、W12 动态交互面、F06 重启对账）。

写进 `notes` 的三条纪律：

1. 本文件只登记**能跑起来的证据**。没有测试支撑的场景不写 automated——那会让「有 eval」变成自我安慰。
2. 会跳过的测试（如依赖环境变量的真实站点用例）**不进 automated**：会跳过写成 automated 等于把「没跑」记成「通过」。
3. `partial` / `manual` 必须写清缺什么。

`evals/run.ps1` 同步修：manual 项字段从 `reason` 改 `gap` 后仍兼容两种写法；结尾统计从「不是完整 26 项」改为「自动通过 N / 人工缺口 M（共 26 项登记）」。

### 6.2 T-03 真实站点

`server/real_site_test.go`（151 行）：`ARTEX_REAL_SITE` 环境变量驱动，未设置则 `t.Skip`。

跑了一轮 `https://www.zhihu.com/signin`：

| 指标 | 结果 |
| --- | --- |
| `sites_without_endpoints` | 1 → 0 |
| endpoint 增量 | 0 → 1 |
| 方法 | GET（可确定） |

**诚实结论：增量偏小。** 公开静态面能读到的就这么多——真实站点的接口主要靠 JS 运行时才出现，这正是 W12（动态交互面）仍是人工的原因。该用例按环境变量跳过，所以**不进 automated 清单**。

### 6.3 编译启动验证

新增 `scripts/verify-start.ps1`（199 行）：一个 PowerShell 会话拥有完整生命周期（initdb → start → app → 五个探针 → stop）。

> **为什么必须一个会话**：`pg_ctl` 启动的 postgres 是调用会话的子进程，会话被回收它跟着走。跨会话分步做，症状是一个看起来完全像应用 bug 的 `connection refused`。

五个探针：

| 探针 | 验证什么 | 结果 |
| --- | --- | --- |
| 1 `/api/health` | HTTP 服务 + 路由表 | `ok=True service=artex version=dev` |
| 2 匿名 `/api/tasks` | **fail-closed 鉴权** | 401 ✅（返回 200 说明部署大门敞开） |
| 3 init 密码 → token → `/api/tasks` | handler → bcrypt → settings 表 → JWT → 中间件 全链路 | 0 task(s) |
| 4 psql 查表结构 | 启动迁移真的执行了 | `observations` / `topology_edges` / `prerequisites` / `done_when` / `reach_verified_ranges` 全在 |
| 5 启动日志标记 | 构造期接线的子系统真的起来了 | S3 语义层 / planner 只读工具 / 隧道子系统 三个标记全中 |

> **探针 4 为什么必须有**：静默 no-op 的迁移会产出一个「启动正常、第一次写入才炸」的服务。对后台 agent 这是最坏的失败形态——它看起来像目标的问题。

踩过的两个坑，都写进注释了：

1. `pg_ctl ... start | Out-Null` **永远不返回**。pg_ctl 把 stdout 句柄交给它 spawn 的 postgres daemon，PowerShell 等所有写者关闭管道，而 daemon 整个生命周期都持有它。症状是打印 `start postgres` 后永久挂起——读起来完全像「应用没起来」。
2. 探针 5 的锚点必须**纯 ASCII**。PowerShell 5.1 用 ANSI 代码页解码无 BOM 脚本，脚本里的中文会变成乱码，于是健康启动也会报「标记缺失」。锚点改成 `[enrich] S3` / `[tools] planner` / `20000-21000`——每个都由唯一调用点打印且不含中文。

**最终结果**：`EXIT=0`，五个探针全过。

### 6.4 评测套件跑通（并修掉工具链的 3 个缺陷）

`evals/run.ps1` 第一次运行时是**坏的**。三个缺陷串在一起，症状是「评测跑不起来」，但根因全在脚本而不在代码：

| # | 缺陷 | 症状 | 修法 |
| --- | --- | --- | --- |
| 1 | `Get-Content -Raw` 缺 `-Encoding UTF8` | `ConvertFrom-Json` 报「传入的数组无效」，指向一行看起来完全正常的 JSON | PS 5.1 按 ANSI 解码无 BOM 的 UTF-8 文件，中文字节被截断成非法序列，引号配对破坏。`scenarios.json` 与 `summary.json` 两处读取都补 `-Encoding UTF8` |
| 2 | `scripts/test.ps1` 用 `Start-Process` 起 PG | `ArgumentException: 字典中的关键字 http_proxy 所添加的关键字 HTTP_PROXY` | Windows 上代理键大小写不敏感，同时导出 `http_proxy` 与 `HTTP_PROXY` 会让 `Start-Process` 抛异常。**只删大写那份不够**（哪份存活是任意的），四组键的大小写变体都要摘掉，结束时恢复 |
| 3 | `scripts/test.ps1` 用 `.ExitCode` 判断 pg_ctl 成败 | PG 实际起来了/停了，脚本却报 `startup failed` / `shutdown failed` | `Start-Process -PassThru` 的 `.ExitCode` 在 PS 5.1 上不可靠。改为直接调用 + `$LASTEXITCODE`（与 `pgtest.ps1` 一致） |

> **第 3 条牵出一个更值得记的问题**：原来的 `finally` 里 `throw 'Isolated PostgreSQL shutdown failed.'`。**`finally` 里的 throw 会替换掉把控制流送到这里的那条异常**，于是「收尾失败」把「真正的失败」盖掉了——第一次跑只看到 shutdown 报错，实际是启动阶段就挂了。改成记录到 `summary.shutdown_error` + `Write-Warning`，不抛：收尾失败要报告，但绝不能篡改首错。

还有一个必须记的坑：**`pg_ctl start` 不能有任何重定向**。`| Out-Null` 和 `*> file` 都会挂死——pg_ctl 把 stdout 句柄交给了它 spawn 的 postgres daemon，shell 等所有写者关闭，而 daemon 整个生命周期都持有它。`pgtest.ps1` / `verify-start.ps1` / `test.ps1` 三处现在都是裸调用。

**最终结果**：

```
evals/run.ps1  -> EXIT=0
"status": "passed", "go_tests_passed": 63, "go_tests_skipped": 0
Automated scenarios passed: 23; manual gaps: 3 (of 26 registered).
Manual gaps are not failures of this run: they need a real target, a real browser or a real restart.
```

三条人工缺口：W11（静态收割覆盖报告）、W12（动态交互面）、F06（重启对账）。它们被打印成 `GAP` 而不是失败——**这是刻意的**：把「没跑」记成「通过」比没有评测更糟。

---

## 7. 明确没做的（诚实清单）

这一节比上面六节更重要。以下都是**知道该做、这轮没做**，不是遗漏。

| # | 项 | 为什么没做 | 什么时候该做 |
| --- | --- | --- | --- |
| 1 | **W12 动态交互面**（Tab / 弹窗 / 分页后的请求面） | 需要真实浏览器。单进程串行 `goto` 点不出 SPA 的运行时接口 | T-20 的剩余部分，做成 `harvest_static.py` 原地升级而不是新工具 |
| 2 | **F06 重启对账** | 只有零散单测（孤儿标 error、残留 running 意图重置），没有端到端 fixture | 需要一个「kill -9 后核对四张台账」的 fixture |
| 3 | **I2 的 winrm / ldap / rdp** | 每个都要新依赖 + 新适配层 + 新测试面。矩阵里已写清 `Missing`，agent 不会反复试错 | SSH 已把「协议适配层长什么样」定下来，剩下三个是同一模式的重复劳动 |
| 4 | **`reach_verified_ranges` 的 TTL** | 探测结果会陈旧，但「多久算旧」需要一个真实环境的数据才能定 | 现在只记 `reach_verified_at`，读的人自己判断 |
| 5 | **语义层的评测基线只有解析层** | 模型输出不可复现，可复现的只有「文字 → 候选」。端到端收益（补全了多少真端点）要真实站点 + 多轮数据 | 与 W12 同批 |
| 6 | **`observation` 的拓扑边没有跨任务视图** | 刻意：跨任务拓扑会变成「信息泄漏」的通道。任务间共享要走授权，不是走拓扑 | 等有明确的多任务协同需求 |
| 7 | **`webtest` / `authstate` 尚未接入 worker** | 这两个包是**原语**，接入需要 worker 侧有「差分实验」这个意图类型。先有原语再有用法，比反过来省事 | W2/W3 的接线批次 |
| 8 | **`planning` 的 `intent_done` 判据只能看终态** | 「做了一半」的意图算 unmet，不算 done。这是保守选择：假 done 比假 unmet 贵 | 等出现「部分完成也要放行」的真实场景 |
| 9 | **`tunnel_reach` 只探 445/80** | 加端口就要加超时预算。当前 12s 总预算只够两个端口 | 按需扩，且要同步扩预算 |
| 10 | **前端未跑** | 本轮改动全在后端，`web/` 未动 | `scripts/test.ps1` 的完整质量门会跑 |

---

## 8. 关键设计取舍（写下来防止下一轮推翻）

| 取舍 | 决定 | 理由 |
| --- | --- | --- |
| 声明 vs 观测 | **两列分开** | 合在一列会让一次局部探测静默抹掉未探到的声明 |
| `Counts` 用指针 | **`nil` = 未知，`0` = 确认没有** | 混在一起会让 agent 去修一个没坏的东西 |
| 前置条件不阻断 | **只报告 `unmet_prerequisites`** | 阻断会把「缺前置」变成「不能开工」，而正确做法常是「带着风险试一次」 |
| 语义层额度不足 | **降级不暂停** | 语义层是增强不是依赖 |
| 超时归 `partial` | **不是 `failed`** | 命令可能已经在目标上跑起来了，归 failed 会触发重跑 |
| 账本读失败不阻断打分 | **只降级为无账本信号** | 加分项不该有一票否决权 |
| 过期拓扑边不删除 | **只让 `valid_until` 过期** | 删除会让「环境变了」和「从来没有过」长得一样 |
| `found` 只升不降 | **重测未复现追加 Evidence** | 抹掉发现等于让一次抖动删掉一条真实结论 |
| `HostKeyCallback` 忽略 | **写死在代码里** | 做成配置项会让「默认安全」变成「默认不可用」 |
| 未知 method 退出码 | **`ExitNone`** | 默认 `real` 会让没退出码的通道凭空长出「执行成功」 |

---

## 9. 验证证据

### 9.1 编译与静态检查

```
go build ./...   → EXIT=0
go vet ./...     → EXIT=0
```

### 9.2 全量测试（隔离 PostgreSQL）

```
scripts/pgtest.ps1 -Packages "./..."   -> EXIT=0，32 包全 ok
```

单命令内「起库（新库名）→ test → 停库 → 删库」，`-p 1` 串行，避免包间争库。耗时最长的两包：`db` 157.5s、`server` 158.7s。

### 9.2b 评测套件

```
evals/run.ps1   -> EXIT=0
"status": "passed", "go_tests_passed": 63, "go_tests_skipped": 0
Automated scenarios passed: 23; manual gaps: 3 (of 26 registered).
```

评测清单里 23 个 automated 场景引用的**每一个测试名**都单独核对过存在（脚本级交叉校验），避免改名后场景静默失效。

### 9.3 编译启动验证

```
scripts/verify-start.ps1   → EXIT=0
[verify] health: ok=True service=artex version=dev
[verify] auth: anonymous /api/tasks -> 401 (fail-closed)
[verify] auth: password initialised, /api/tasks -> 0 task(s)
[verify] schema: observations=observations topology_edges=topology_edges prereq_col=1 donewhen_col=1 reachver_col=1
[verify] subsystem up: S3 semantic layer (enrich.SetJSAnalyzer)
[verify] subsystem up: planner read-only intranet tools
[verify] subsystem up: tunnel subsystem (port pool 20000-21000)
```

### 9.4 新增测试函数

| 文件 | 用例数 | 钉住什么 |
| --- | --- | --- |
| `webtest/webtest_test.go` | 16 | 单点变异 / 噪声标定 / 五轴覆盖 / found 只升不降 |
| `planning/planning_test.go` | 12 | unknown ≠ unmet / 非法判据保留 / command 需人工注记 |
| `observation/observation_test.go` | 11 | 监听可及范围 / 观测与拓扑边 / 证据等级升级 |
| `server/enrich_semantic_parse_test.go` | 8 | 编造 method / 跨域 / 静态资源一律丢 / 确定性 |
| `authstate/authstate_test.go` | 8 | 版本递增 / Stale 判定 / Cookie 排序 |
| `capability/matrix_test.go` | 6 | 五协议全登记 / 未实现写清缺什么 / 退出码三档 |
| `server/lateral_ssh_test.go` | 6 | 凭据类型 / hash 拒绝 / 目标地址 / 失败签名 |
| `capability/contract_test.go` | 5 | 三条不变量 / 先认证后网络 |
| `budget/admission_test.go` | 5 | 非 completion 准入 / 负估算拒绝 |
| `agent/prompt_blocks_test.go` | 4 | 块顺序 / 未知 ID 暴露 |
| `recon/recon_test.go` | +2 | 同端口不同绑定 / 回环不当远程 |
| `db/tunnels_test.go` | +3 断言 | 声明与观测分列 |
| `server/instance_isolation_test.go` | 2 | 判据统计绑定各自任务 / 降级为全 unknown |
| `agent/deps_isolation_test.go` | 2 | 无包级可变回调 / setter 只动 receiver |
| `server/resource_release_e2e_test.go` | 1 | 依赖逆序 + 只放本任务租约 |
| `server/real_site_test.go` | 1 | 真实站点增量（环境变量驱动） |

### 9.5 新增/改动文件

| 类型 | 文件 |
| --- | --- |
| **新包** | `planning/`、`authstate/`、`webtest/`、`observation/` |
| **新文件（server）** | `tunnel_reach.go`、`lateral_ssh.go`、`planning_counts.go`、`enrich_semantic.go`、`enrich_semantic_parse.go`、`observation_wire.go` |
| **新文件（其他）** | `capability/matrix.go`、`capability/contract.go`、`agent/prompt_blocks.go`、`db/observations.go`、`db/credential_score.go`（扩） |
| **新脚本** | `scripts/pgtest.ps1`、`scripts/verify-start.ps1` |
| **修脚本** | `scripts/test.ps1`（代理双键 / `.ExitCode` 不可靠 / `finally` 篡改首错）、`evals/run.ps1`（UTF-8 解码） |
| **改 schema** | `db/schema.sql`：`observations`、`topology_edges` 两表；`reach_verified_ranges`、`prerequisites`、`done_when` 三列 |
| **改接线** | `server/server.go`、`server/manager.go`、`server/task_llm.go`、`server/tunnel.go`、`server/credentials.go`、`server/lateral_exec.go`、`server/session_recon.go`、`server/intranet_snapshot.go`、`server/taskproxy.go`、`server/resource_release.go`、`agent/tools.go`、`agent/planner.go`、`agent/worker.go`、`budget/budget.go`、`enrich/`（未改，仅接 `SetJSAnalyzer`） |
| **改评测** | `evals/scenarios.json`（12 → 26）、`evals/run.ps1` |

---

## 10. 一句话总结

待办总表 §2 的 15 项（半落地 7 + 完全未做 8）已全部落地。验证四层全绿：`go build` / `go vet`（EXIT=0）、隔离库全量测试（32 包 ok）、评测套件（23/26 自动场景，63 个测试）、编译启动五探针（EXIT=0）。

**仍然没做的 10 项在 §7 逐条列明**——其中 3 项（W12 / F06 / 语义层端到端收益）卡在「需要真实浏览器或真实重启」，2 项（winrm/ldap/rdp）是刻意的重复劳动延后，其余是保守选择或等真实场景。

另外这轮顺带修好了三个**工具链**缺陷（§6.4）：它们不会让代码出错，但会让「验证」这个动作本身不可信——一个永远报 `startup failed` 的脚本，和一个永远报 `passed` 的脚本，危害是一样的。
