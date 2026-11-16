# YMH 隧道平台适配与多路径路由实施记录

日期：2026-10-22。项目：`E:/cyberagent/artex-ymh-main/artex-ymh-main`。

本轮只做两件事，按「哪一件更卡住内网纵深」排序：

1. **隧道平台能力适配**——决定「拿到 Windows 立足点之后还有没有下一层」；
2. **多路径路由**——决定「有两个网段时能不能同时推进」。

其余工作包（协议矩阵、横向绑定、依赖准入、`execution/` `routing/` `capability/` `webtest/` `authstate/` `planning/` `evals/`）本轮未动，状态与前次记录一致：不存在或仍是空壳。本记录不表示原 12 个工作包已完成。

设计判据沿用前几轮：**能力提升，不是额外触发**。删掉本轮改动，agent 的工作流不需要改一个字；装上它，同样的动作产出更多。具体到这两项：

- 平台适配：agent 仍然只调 `tunnel_deploy(session_id, kind)`。它不需要知道「目标是 Windows」，也不需要换工具或换参数。
- 多路径路由：agent 仍然照常发请求。它不需要知道「有两网段」，也不需要声明「这个 host 走那条隧道」。选路从已发生的流量里自己学。

## 0. 与既有链路的关系（planner → worker → 自主发散）

这一节回答一个必须提前说清的问题：**逐请求选路会不会把 worker 的发散范围限死？**

答案是「不会，而且原本被限得更死」，但有一个必须守住的边界。

### 0.1 选路在传输层，不在战术层

```
planner（读态势、派意图）
   ↓  intent
worker（单意图执行器，自主决定打哪个目标、用什么手法）
   ↓  "访问 10.1.0.5:80"
任务 MITM（录制 + 选路）        ← 本轮改动在这里
   ↓  这个包走哪条 socks 隧道
隧道 T1 / T2 → 网段 A / B
```

「这个包走哪条隧道」与「打哪个目标、用什么手法、什么时候换思路」不在同一层。前者等价于操作系统的路由表项——没有人会说「内核路由表限定了 nmap 的范围」。worker 的自主性一点没少，它只是不再需要为「我有两条隧道该用哪条」这种它无法控制的事操心。

### 0.2 改造前其实也在替 agent 做决定，只是做得更糟

`pickUpstream` 原本取 `id` 最大的 alive 隧道作为**唯一**上游。也就是说系统一直在替 worker 决定「所有流量走哪条路」，只是这个决定等于「永远走最新那条」：

- 两个网段时，另一条隧道 100% 空转；
- 走错网段的请求 100% 失败，且失败是**稳定**的（每次都失败），worker 只能得出「这个目标打不了」。

改造后 worker 能打的目标集合是**变大**的（从「一个网段」变成「两个网段」）。这不是限定范围，是解除限定。

### 0.3 必须守住的边界：路由明细不能进 agent 上下文

真正的「限定范围」风险不在选路算法，而在**把路由状态喂给 agent**。如果 planner 看到「隧道 T1 的可达主机是 {10.1.0.5}」，它会倾向于不再派「打 10.1.0.6」的意图；如果 worker 看到同样的清单，它会认定那个目标打不了。那才是把发散范围钉死。

所以本轮的硬约束是：**agent 侧只能看到计数，看不到 host 清单**。

| 消费方 | 能看到什么 | 理由 |
| --- | --- | --- |
| planner / worker（`tunnel_list`） | `proven_host_count` / `tried_host_count` | 只够回答「第二条隧道是否真的在承载流量」，不构成可达目标白名单 |
| 人类界面（未来工作） | `RouteView.ProvenHosts` 完整清单 | 复盘用 |

判据是「能否据以行动」：选路不归 agent 控制，它不能手动指定「这个 host 走那条隧道」。**既然不能据此做不同的事，给出明细就只剩误读风险。** 这条与既有的「上下文预算」纪律同源。

`server.TestRouteRowFieldsExposeCountsOnly` 锁住这一点——输出字段里出现任何 host 明细即失败。

同理，`tunnel_probe` 的 `probe_unsupported` 也在防同一类错误：不让 agent 基于错误理由（「探针跑不起来」被读成「目标不出网」）去缩小自己的行动范围。

### 0.4 唯一的真实代价：新 host 首次可能失败一次

路由表对某个 host 没有证据时，会按 `tunnel_id` 升序挑一条。挑错的话这一次请求失败（`curl: (56)` / 代理 502），下一次换路即成功。

对 worker 的观感：第一次失败的形态与改造前一样，但它如果重试一次就好了。改造前是「永远失败」，改造后是「第一次失败」。

**没有做「失败后透明换路重试」**，原因是它需要先解决三个语义问题，不适合顺手塞进来：非幂等请求（POST）重试的语义、如何区分「路由不可达」与「目标拒绝」（后者不该换路）、以及要改 `go-mitmproxy` 的连接层行为。这是明确的下一步工作，不是遗漏。

## 1. 实际改动

### 1.1 目标侧平台抽象（`tunnel/platform.go`，新增）

此前对目标的每一个动作都是内联的 Unix 语法：`/tmp/.<tag>`、`mkdir -p`、`chmod +x`、`setsid env AUTH=… nohup`、`/proc/<pid>/cmdline`、`/dev/tcp`。后果是 Windows 立足点（aspx/asp 马）**能侦察、却一条隧道也建不起来**：卡在 `chmod` 上，而报错写的是「目标可能 noexec 挂载 /tmp」——那不是挂载问题，是根本没有 `chmod`。归因错，修的方向就全错。

新增 `HostShell` 作为唯一的目标侧命令生成器，覆盖：临时目录表达式、目录解析、路径拼接、建目录、投递、置执行位、后台拉起、删目录、进程核对、进程停止、出网探针、TCP 可达预检。

平台判定 `PlatformForSession` 的证据链是「会话自报观测值 > 马型推断」：新增可选接口 `session.HostPlatformProvider`（`HTTPShell` 已实现），拿不到才按 `Kind()` 推断。判据是马型而非探测结果——部署隧道本身就是「探测前的第一步」，此时没有更可靠的信号。

Linux 分支**逐字保持**原有命令，不产生任何额外目标侧往返（`TestLinuxCommandsKeepFieldProvenShapes` 锁住这一点）。

Windows 分支的具体取舍：

| 动作 | Linux（不变） | Windows |
| --- | --- | --- |
| 远端目录 | `/tmp/.<tag>` 字面量 | `%TEMP%\<tag>`，部署时先 `echo %TEMP%` 解析成字面路径再落台账 |
| 建目录 | `mkdir -p` | `if not exist … mkdir` |
| 投递 | `curl` → `wget` | `curl.exe` → `certutil -urlcache`，以「文件是否存在」为成功判据 |
| 执行位 | `chmod +x` | 无此概念，**不产生命令**，也不假装检查过 |
| 拉起 | `setsid env AUTH=… nohup ./ch … & echo $!` | `powershell Start-Process -PassThru`，AUTH 走 `$env:AUTH` |
| 进程核对 | `kill -0` + `/proc/<pid>/cmdline` + `readlink /proc/<pid>/exe` | `Get-CimInstance Win32_Process` 的 `CommandLine`（**不用 wmic**：Win11 24H2 已移除） |
| 进程停止 | `kill` → `kill -9`，每次信号前重核 exe | `Stop-Process -Force` + `Get-Process` 确认 |
| 删目录 | `rm -rf --` | `rmdir /s /q`，以「删完是否还存在」为判据 |
| 出网探针 | `bash /dev/tcp` + `timeout` | `.NET TcpClient` + **`SocketErrorCode` 枚举** |

两处细节值得单独记下来，因为都是「按直觉写就会错」的地方：

- **探针三态不能用异常文本判定。** .NET 的异常消息是本地化的，中文/日文系统上按 `"Connection refused"` 匹配会全部落到 `FAIL`，于是「路径通、只是没监听」被误判成「被过滤」——`Conclude` 会据此建议放弃反向隧道，正好反了。改用 `SocketErrorCode`（枚举，与语言无关）。
- **进程存活不能用 `tasklist` 文本判定。** 中文 Windows 的 `tasklist` 输出是「信息: 没有运行的任务匹配指定标准。」，按英文关键词判定会永远失败。改用 `Get-Process -ErrorAction SilentlyContinue`。

### 1.2 平台能力如实声明（`DeployCaps`）

新增 `DeployCaps`，复用 `session` 包已有的三态词汇（`supported/unsupported/unknown`），避免隧道子系统自造一套「行/不行」的措辞。

Windows 的 `Detach`（脱离会话后台常驻）**明确标 `unknown`**：`Start-Process -PassThru` 能拿回真实 pid，但「IIS 应用池回收后子进程是否仍活」没有在真实目标上验证过。所以部署返回里带 `platform_caps` 与 `verify_hint`，要求调用方用 `tunnel_health` 复核，不默认它会一直在。

`TestWindowsDetachCapabilityStaysUnknown` 专门锁这一条：一旦有人把它改成 `supported` 而不去验证，调用方就会停止复核——那正是「装活」的开始。

### 1.3 选型不再只看探针结论（`server/tunnel.go`）

`adapter=auto` 原先只看 `probe` 结论。现在**平台能力先于探针结论**：Windows 目标没有对应客户端二进制时，探针结论再漂亮也建不成 chisel（投递 ELF 到 Windows 只会秒退）。所以先问「这个平台有没有可用的 chisel 客户端」，再问「出网通不通」。

`tunnel_probe` 的探针命令包也按平台选。此前固定发 `bash /dev/tcp`，aspx 立足点五条全失败，却被 `Conclude` 判成「目标无出网」——方向碰巧对（都建议 suo5），理由完全错，而且掩盖了真正的问题。

### 1.4 探针不可用 ≠ 目标不出网（`tunnel/probe.go`）

新增 `TCPState = probe_error` 与 `GradeProbeUnsupported`：探针命令没回显哨兵时单独成一态，`Conclude` 返回 `probe_unsupported` 而不是 `isolated`，并给出「不要把结论当成目标不出网」的说明。

这是本轮最容易被忽略、但影响最大的一条：基于错误理由做的后续决策迟早出错。

### 1.5 客户端二进制按平台分（`tunnel/deploy.go`）

新增 `Options.ChiselPathWindows` / `ARTEX_CHISEL_WIN_PATH` / `data/tools/chisel.exe`。缺失时 `Deploy` 给出精确错误（该平台需要什么、放哪里、或者改用 suo5），**绝不用 Linux 的 ELF 顶替**——那只会产生一条不指向根因的「进程秒退」日志。

同时把「平台侧 server 二进制缺失」与「目标平台 client 二进制缺失」分开报：这是两种完全不同的故障，混成一句会让人去修错地方。

### 1.6 按目标 host 选路（`tunnel/route.go`，新增）

原逻辑：一个任务只有一个上游，`pickUpstream` 在全部 alive socks 隧道里取 `id` 最大的那条（`r.ID <= bestID` 的单调取大），其余隧道闲置。于是「有两个网段」时全部流量被塞进其中一条：进得去的那段能用，进不去的那段每次失败，而失败原因看起来是「目标不通」——真正的原因是「选错了路」。

新增 `RouteTable`：任务级、运行期、**派生物**。真相源仍是 `tunnels` 台账；路由表只缓存「可达性观测」。

选路规则（确定性，无随机）：

1. 有正面证据（该 host 经某条隧道成功过或被拒连过）→ 取最近成功的那条，**粘住**；
2. 否则 → 取该 host **尝试次数最少**的那条；并列取 hop 深度小的，再并列取 `tunnel_id` 小的。

规则 2 就是轮换：两条都没试过时第一次落在 id 小的那条，失败后它的 `tries` 变成 1，第二次自然换到另一条；成功即按规则 1 粘住。于是两个网段各自被钉到能到它的那条隧道上，**可以同时推进**，而总代价最多是「每条错路各浪费一次连接」，不需要任何额外探测动作。

三条「不做什么」比「做什么」更重要：

- **失败不作为排除依据。** 连接失败可能是端口没开、可能是抖动、可能是防火墙刚好在那一刻丢包。一旦据此永久排除，隧道恢复后也不会再被尝试（锁死），而系统不会告诉你「我把它排除了」。所以只做两件事：成功的粘住，没成功过的轮换。`TestRouteFailureNeverExcludesARoute` 锁这一条。
- **拒连算正面证据。** RST 说明包已经到了对端，是端口没开，不是路不通。算成失败会让选路在「端口没开」的主机上永远轮换。
- **认不出的错误不算证据。** TLS 握手失败、连接重置归 `unknown`，不归 `unreach`。误判成「不可达」会让选路在两条路之间抖；误判成「未知」只会继续轮换，代价更小。DNS 解析失败也在这里——socks5 的目标地址由平台侧解析，解析不到说明「这个名字不存在」，不是「这条隧道到不了这台机器」。这条是测试先发现的（`TestClassifyRouteError` 第一次跑就红）。

### 1.7 流量层加逐请求选路钩子（`traffic/traffic.go`）

`go-mitmproxy` 的 `SetUpstreamProxy(fn func(*http.Request) (*url.URL, error))` 本来就按请求回调，所以**不需要改库**，只需要把既有的单值上游换成「先问选路层、没有意见再回落」。

新增 `SetRouteHooks(selectUpstream, observe)`：

- `selectUpstream(host)` 返回该 host 的上游，返回 `""` 表示「没有意见」，回落全局上游。这样「无隧道 / 隧道全死 / 内网期阻断」三种情形完全走既有代码路径（含 `blockedUpstream` 与 `SetPassthroughEnabled(false)`），不在两处各写一遍策略。
- 钩子报错 → 请求失败。「拒绝承载」必须能生效，不能只是日志。
- 钩子返回非法 URL → 失败，**不静默回落全局出口**。那会造成「以为在隧道里、其实从平台直连」——最危险的一种降级。
- `observe(host, err)` 在 `Response` / `RequestError` / `HTTPConnectError` 三处上报结果。新增 `HTTPConnectError` 是关键：CONNECT 失败说明「这条隧道载不动我们到这个 host」，是最有信息量的失败，而此前没有任何钩子。

观测只报 `(host, err)`，不报「走的是哪条隧道」——那是选路层自己的账。`traffic` 记录字节，不决定拓扑。

### 1.8 任务代理接线（`server/taskproxy.go`）

`taskProxyInst` 增加 `routes *tunnel.RouteTable`；`buildLocked` 在 `Start()` **之前**装好选路钩子（装晚了第一批请求会全部落到回落上游——单隧道时看不出差别，双网段时就是「第一条路被塞满、第二条空转」）。

新增 `syncRoutesLocked`：一次台账读取同时喂「路由表」与「回落上游」，避免两处各读一次而在中间出现「路由表已更新、回落上游还是旧的」的窗口。

`pickUpstream` 保留原语义（取 id 最大），但注释改成「**回落**上游」：只在选路层没有意见时生效。单隧道任务的 `UpstreamProxy()` 与改造前完全一致（`TestTaskProxyLifecycle` 原断言未改）。

### 1.9 可观测（`tunnel_list`）

`tunnel_list` 每行增加 `proven_host_count` / `tried_host_count` / `target_platform`，顶层增加 `routing` 说明。

为什么是计数而不是 host 清单：见 §0.3。这是「两个网段能不能同时推进」的可见证据，同时不构成「可达目标白名单」。`routing` 文案里明确写了「不是可达范围，也不是能力边界」。

host 级明细（`RouteView.ProvenHosts`）保留在 `tunnel` 包里，供未来人类界面使用，不进 agent 上下文。

## 2. 迁移与兼容

- **无 schema 变更**，无新表。路由表是内存派生物，不落库。
- `Plan.Platform` 是新增 JSON 字段，`omitempty`。既有台账行反序列化后为空 → `PlatformOf("")` → Linux → 仍走原路径。`TestPlanRemotePathsPerPlatform` 断言这一点。
- `tunnel.Options` 新增 `ChiselPathWindows`，零值表示「Windows 目标只能走 suo5」，不是启动错误。
- `session` 新增可选接口 `HostPlatformProvider`，与 `CapabilityProvider` 同款可选设计，不实现它的驱动行为不变。
- `traffic.SetRouteHooks` 是新增方法，不调用它时 `resolveUpstreamFor` 直接返回全局上游，与改造前逐字节等价。
- `probe.Probe` 保留（语义固定为 Linux 命令包），新增 `ProbeOn` 接受平台；既有调用与测试不变。

配置项新增：

```text
ARTEX_CHISEL_WIN_PATH   Windows 目标用的 chisel.exe（默认 data/tools/chisel.exe）
```

## 3. 验证

源码级验证（本机，Go 1.27.1）：

- `go build ./...` 通过。
- `go vet ./traffic/ ./tunnel/ ./server/ ./session/` 通过。
- `go test ./... -count=1`：25 个有测试的包全部通过，另 1 个包无测试文件（`session/ldpreloadgen`）。
- 新增测试 28 个：`tunnel/platform_test.go` 12 个、`tunnel/route_test.go` 12 个、`traffic/route_test.go` 3 个、`server/taskproxy_test.go` 新增 1 个。`tunnel` 包测试总数 84 个，全绿。
- §6 自查追加后（`session/platform_test.go` 6 个、`server` 新增 5 个用例、`db` 新增 1 个需 PG 的用例），再次 `go build ./...` / `go vet` / `go test ./... -count=1` 全通过。`db` 的 PG 用例在未设 `ARTEX_PG_DSN` 时按既有约定 SKIP（本机无 PG，未实跑）。

关键断言：

| 断言 | 位置 | 守的是什么 |
| --- | --- | --- |
| Linux 命令逐字不变 | `TestLinuxCommandsKeepFieldProvenShapes` | 已在真实目标跑通的路径不被「顺手重构」改坏 |
| Windows 命令无 Unix 原语 | `TestWindowsCommandsContainNoUnixPrimitives` | `chmod`/`setsid`/`/proc`/`/dev/tcp`/`mkdir -p`/`rm -rf`/`pwd` 一个都不能出现 |
| 拉起用 `-PassThru` + `$env:AUTH` | `TestWindowsSpawnUsesPassThruAndEnvAuth` | pid 可核对；口令不进命令行 |
| 探针用 `SocketErrorCode` | `TestWindowsProbeUsesSocketErrorCode` | 不依赖本地化异常文本 |
| 进程操作不解析 `tasklist` | `TestWindowsProcessOpsAreLocaleFree` | 中文 Windows 上不假失败 |
| Windows `Detach` 保持 unknown | `TestWindowsDetachCapabilityStaysUnknown` | 未验证的能力不许装成已验证 |
| 探针不可用 ≠ 不出网 | `TestConcludeProbeUnsupportedNotIsolated` | 不用错误理由支撑正确结论 |
| 单隧道恒选它 | `TestRouteSingleTunnelAlwaysSelected` | 「无感」：单网段任务零行为变化 |
| 双网段各自钉住 | `TestRouteTwoSegmentsGetPinnedIndependently` | 核心场景：两条隧道同时在用 |
| 失败不排除路由 | `TestRouteFailureNeverExcludesARoute` | 不允许静默永久排除 |
| 拒连算可达 | `TestRouteRefusedCountsAsReachable` | 「端口没开」不被当成「路不通」 |
| 未知错误不作证据 | `TestRouteUnknownErrorIsNotEvidence` | 一次协议错误不挤走好隧道 |
| DNS 失败不是路由证据 | `TestClassifyRouteError` | 不为一个不存在的名字反复轮换 |
| 空表无意见 | `TestRouteNoOpinionWhenEmpty` | 阻断/回落策略只在一处 |
| 钩子优先级与强制失败 | `TestResolveUpstreamForPrecedence` | 非法上游不静默回落直连 |
| 实例装钩子 + 路由表同步 | `TestTaskProxyInstallsRouteHooks` | 接线真的接上了，不是只写了接口 |
| 路由明细不进 agent 上下文 | `TestRouteRowFieldsExposeCountsOnly` | 不把「可达清单」变成「能力边界」 |
| 平台未知不得降级成 linux | `TestDescribePlatformUnknownIsEmpty` | 「不知道」不许被悄悄当成「按 Unix 办」 |
| 驱动事实优先于马型推断 | `TestPlatformForSessionPrefersDriverFact` | 自报与推断冲突时以自报为准 |
| 方言探测可区分 sh/cmd | `TestDetectShellDialect` / `TestDialectProbeShape` | 反转 shell 猜错方言时能现场纠正 |
| Windows 立足点要改派工方式 | `TestIntranetSummaryKeyConclusions`（Windows 两条） | 有则说、无则不说，不白花 prompt |
| 授权投影与执行同谓词 | `TestGrantsForTaskMatchesEnforcement` | 列表说能做的，执行侧必须真的能做 |
| 侦察走「马型 → 方言 → 命令包」 | `TestReconCommandPackSelectsWindowsForASPX` | 侦察与隧道不许各判一套平台 |

未做的验证（见 §4）：真实 Windows 目标、真实双网段环境、`race` 检测、前端构建。

## 4. 保留边界

### 4.1 Windows 常驻性未验证

`Start-Process -PassThru` 在多数机器上可用，但以下三条都没有在真实目标上验证过：

- IIS 应用池回收后子进程是否仍活；
- `-WindowStyle Hidden` 在 Server Core / 无桌面会话下的行为；
- PowerShell 被 AppLocker / ConstrainedLanguage 限制时的失败形态（当前只保证错误信息指出这一可能，并建议改走 suo5）。

因此 `DeployCaps.Detach` 保持 `unknown`，部署返回带 `verify_hint`。

### 4.2 Windows 依赖两个外部条件

- chisel 隧道需要用户自备 `chisel.exe`（未内置、未做供应链校验之外的来源管理；`NewManager` 只做 sha256 记录）。
- 探针与进程操作依赖 PowerShell 2.0+。缺失时：探针 → `probe_error`（不再误判为不出网）；拉起 → 明确报错并建议 suo5。

### 4.3 路由只到 host 级，没有网段级

`RouteTable` 的证据是「某个 host 经某条隧道成过」，**没有** CIDR 级可达性。原因：资产表不记 `session_id`，无法把 `session_recon` 发现的网段挂到具体立足点（进而挂到具体隧道）上。要做到网段级，需要先补「资产 → 发现它的会话」这条来源链，那是另一项工作。

实践影响：两个网段时，每个新 host 仍要各浪费一次连接才能钉住（而不是「知道这个 /24 走 T2」就直接钉）。功能可用，代价是每个 host 一次。

### 4.4 路由证据不持久

`RouteTable` 是内存对象，进程重启后证据清零，回到「按 id 轮换」。这是刻意的：持久化可达性会形成第二个真相源，而隧道重新部署会拿到新 `tunnel_id`，旧证据对新隧道无意义。代价是重启后要重新学一遍（每个 host 一次连接）。

### 4.5 没有人工干预接口

不能手动「把 10.1.0.0/16 钉到隧道 3」。当前只能靠流量自己学。需要时可以用 `tunnel_deploy` 的 `via_tunnel_id` 改变拓扑，但没有「路由策略」这种显式声明面。

反过来说，这也是刻意的：一旦允许 agent 声明路由策略，它就必须知道拓扑，而「不知道拓扑也能打」正是本轮要保住的性质。

### 4.5b 新 host 首次可能失败一次，未做透明换路重试

见 §0.4。要做需要先解决三个语义问题（POST 重试、不可达 vs 拒绝的区分、改库的连接层行为），本轮不做。

如果实践里这个代价被证明过高（例如 worker 因此频繁写出「目标不可达」的错误 fact），优先做的不是「重试」，而是**让 worker 更容易重试**——那是既有提示词层的调整，不需要动传输层。

### 4.6 DNS 策略与协议路由未做

`Route` 实体在 I1 设计里包含 `dns_policy` 与按协议（tcp/udp/icmp）分流。本轮只做 TCP over socks5。UDP 走 socks5 需要 `UDP ASSOCIATE`，`go-mitmproxy` 的 `helper.GetProxyConn` 不支持，未动。

### 4.7 横向绑定与依赖准入未做

本轮不涉及。`route` 只是「怎么到」，不是「到了之后做什么」。

### 4.8 平台侧（ARTEX 自身）的 OS 依赖

`tunnel/health.go` 的 `serverCmdlineMatches` 依赖 `/proc`，已有 `runtime.GOOS` 分流：非 Linux 保守返回 `false`（只靠本进程台账句柄，不靠命令行核对）。这是「平台自己跑在哪」的问题，不是「目标是什么系统」的问题，本轮未改。

### 4.9 已知小边界

- `suo5` 在 Windows 上用 `cd` 定位 webshell 目录；`cd` 对 UNC 路径只打印不切换，此时 `PayloadPath` 可能不是预期位置。未处理（会由后续 `probePayloadURL` 的 404 检查暴露）。
- `%TEMP%` 解析只做一次字符串替换，不做大小写归一；`echo %TEMP%` 的回显本身就是展开后的字面路径，实践中不构成问题。
- 观测归因用「某 host 最近一次被分给哪条隧道」。并发请求同一 host 时通常得到同一答案（选路对同一份证据是确定的），探索期若错配只影响一个样本。
- 未跑 `-race`；未做前端构建验证（本轮无前端改动）。

### 4.10 反转 shell 的平台仍是推断值

`reverse` 会话的驱动拿到的是 PTY，结构上不区分 OS，`DescribePlatform` 返回 `""`，平台由马型推断为 linux。§6.1.2 的哨兵回显只能在「五条命令全灭」时把它纠正过来——也就是说，**Windows 反转 shell 的第一次侦察仍然要浪费一轮**。要根治得让 penelope 侧在握手时上报 OS，属于另一个子系统的工作。

同理，`Capabilities().Runtime.OS` 目前**恒为 `unknown`**（没有任何驱动上报）。它在 `session_list` 里继续出现，是为了保留「观测」与「驱动元数据」的分野，而不是因为有人会去填它。若后续决定不再保留这个分野，正确做法是删掉 `Runtime.OS` 字段，而不是拿 `platform` 去填它。

## 5. 交付状态

本轮只改源码与文档，**不重新构建候选程序、不替换项目根目录 `artex.exe`、不重启用户服务**。源码可构建、全包测试通过即可交付评审；需要运行实例时再单独构建。

变更文件：

| 文件 | 性质 | 内容 |
| --- | --- | --- |
| `tunnel/platform.go` | 新增 | `Platform` / `HostShell` / `DeployCaps` / 全部目标侧命令生成；`PlatformForSession` 在 §6.1.2 改为委托 `session.PlatformForSession` |
| `tunnel/platform_test.go` | 新增 | 12 个用例 |
| `tunnel/route.go` | 新增 | `RouteTable` / `RouteOutcome` / 选路与归因 |
| `tunnel/route_test.go` | 新增 | 12 个用例 |
| `traffic/route_test.go` | 新增 | 3 个用例 |
| `tunnel/plan.go` | 修改 | `Plan.Platform`、`PlanOpts.Platform`、按平台生成远端路径、`RollbackSteps` 分平台描述 |
| `tunnel/deploy.go` | 修改 | `ChiselPathWindows`、`chiselBinaryFor`、`resolveRemotePaths`、投递/拉起/杀进程/清理改走 `HostShell`、portfwd 预检分平台 |
| `tunnel/probe.go` | 修改 | `TCPProbeError`、`ProbeReport.Platform`、`ProbeRan`、`ProbeOn`、`GradeProbeUnsupported` |
| `tunnel/suo5.go` | 修改 | `Suo5PlanOpts.Platform`、目录定位改用 `WorkDirCmd` |
| `session/capabilities.go` | 修改 | 新增可选接口 `HostPlatformProvider` |
| `session/capabilities_http.go` | 修改 | `HTTPShell.HostPlatform` 实现 |
| `traffic/traffic.go` | 修改 | `routeHooks`、`SetRouteHooks`、`resolveUpstreamFor`、`observeRoute`、`sink.HTTPConnectError` |
| `server/taskproxy.go` | 修改 | `routes` 字段、`syncRoutesLocked`、`buildLocked` 装钩子、`RoutesForTask` |
| `server/tunnel.go` | 修改 | `ChiselPathWindows` 装配、探针/选型/计划按平台、`platform_caps` 与 `verify_hint`、`tunnel_list` 路由视图 |
| `server/taskproxy_test.go` | 修改 | 新增 `TestTaskProxyInstallsRouteHooks` |
| `session/platform.go` | 新增（§6.1.1） | `DescribePlatform` / `PlatformForKind` / `PlatformForSession` / `DetectShellDialect` |
| `session/platform_test.go` | 新增（§6.1.1） | 6 个用例 |
| `server/session_capabilities.go` | 修改（§6.1.1） | `sessionPlatform(id)` |
| `server/intranet_snapshot.go` | 修改（§6.1.1/§6.1.3） | `sessions[].platform`、Windows 结论句、`granted_actions` |
| `server/session.go` | 修改（§6.1.1/§6.1.3） | `session_list` 增 `platform` 与 `granted_actions`，描述区分 platform 与 runtime.os |
| `server/session_recon.go` | 修改（§6.1.2） | 按方言选包、全灭时哨兵测方言换包、返回 `platform`/`platform_source`/`command_pack` |
| `server/session_recon_pack_test.go` | 修改（§6.1.2） | 断言「马型 → 方言 → 命令包」整条链 + 未知方言不判 Windows |
| `db/session_access.go` | 修改（§6.1.3） | `GrantsForTask`（与 `CanAccess` 同谓词的授权投影） |
| `db/session_access_test.go` | 修改（§6.1.3） | `TestGrantsForTaskMatchesEnforcement` |
| `server/intranet_snapshot_test.go` | 修改（§6.1.1） | Windows 结论句正反两条 |

未修改 `go.mod` / `go.sum`；未引入新依赖（`go-mitmproxy` 的 `SetUpstreamProxy` 本来就按请求回调，不需要改库）。

后续如果要继续按「哪一件更卡住纵深」排序，我的判断是：

1. **补「资产 → 发现它的会话」来源链**，把路由从 host 级提到网段级（§4.3）——这是当前多路径路由的主要效率损失；
2. **协议矩阵**（UDP / 非 HTTP 承载），现在只有 TCP over socks5；
3. **`intent` 的 `prerequisites` / `done_when`**，让「修链路」这类意图能被结构化判定完成，而不是靠模型自觉。

## 6. 信息系统性自查（跨全部新增能力）

这一节回答一个比「本轮改动有没有 bug」更重要的问题：**系统里有没有「信息已经产生、但没有同步到做决定的那一层」的地方？** 也就是用户点出的那种情况——意图识别 → 执行 → 信息回流的链条上，某一环的数据存在、却到不了决策者手里。

### 6.0 判据

一条信息只有同时满足三条才算「同步到位」：

1. **有生产者**：确实被计算/探测/落库；
2. **有消费入口**：在 agent 能读到的位置（`graph_overview` 快照 / 某个工具返回值）；
3. **可据以行动**：读到之后能改变它下一步做的事。

只满足 1、2 而不满足 3 的信息不是资产，是噪音 + 误读风险（§0.3 已按这条判据砍掉了路由 host 明细）。只满足 1 而缺 2 的信息则是**结构性失明**——系统知道，决策层不知道，于是决策层按「不知道」行动，并把这个假设写进事实库。

自查覆盖本轮及此前几轮新增的全部能力，逐条结论如下。

### 6.1 发现的真问题（已修）

#### 6.1.1 目标平台判定出来了，但只有一次可见（P0）

| 项 | 内容 |
| --- | --- |
| 生产者 | `session.ProbeInfo.Platform`（`register_session` 探测）、`HostPlatformProvider`、`tunnel.Plan.Platform` |
| 原消费入口 | `register_session` 返回值（一次性）、`tunnel_deploy`/`tunnel_list` 的 `target_platform`（仅隧道）、WebUI `/api/sessions` |
| 缺失 | `intranetSnapshot` 的 `sessions` 投影、`session_list` 工具——即 planner 每轮态势与 worker 台账 |
| 后果 | worker 登记 aspx 会话时看到一次 `platform=windows`；一轮之后该信息从上下文消失，且无法再查回。planner 于是对着 Windows 立足点派「跑 `id` / `uname -a` / `ls -la`」的意图，worker 执行全失败，再把失败记成「会话可能已失效 / 命令被拦」——一个纯粹由信息不同步制造出来的**假事实**，且它会进事实库影响后续所有判断 |

这一条尤其要紧：它直接抵消了本轮 §1.1 平台适配的价值。Windows 隧道建得起来，但 agent 不知道自己站在 Windows 上，就不会去建。

修法（不新增工具、不新增触发）：

- `session/platform.go`（新增）：`DescribePlatform`（驱动事实，未知返回 `""`）、`PlatformForKind`（马型推断）、`PlatformForSession`（二者合一，全系统唯一入口）。未知取值**不猜**——把「不知道」悄悄降级成「按 Unix 办」正是这条链上一连串误判的起点。
- `server/session_capabilities.go`：`sessionPlatform(id)`，读注册表，不解密任何密文。
- `server/intranet_snapshot.go`：`snapshotSessions` 每行补 `platform`；`intranetSummary` 在存在 Windows 立足点时追加一句「命令按 cmd/PowerShell 写，不要用 sh 语法与 /tmp」。只在真有 Windows 立足点时出现，纯 Linux 任务零成本。
- `server/session.go`：`session_list` 每行补 `platform`，并在工具描述里写清 **`platform` 是驱动自报的方言，`capabilities.runtime.os` 是观测字段且当前恒为 `unknown`，不代表平台未知**。

`session.TestDescribePlatformUnknownIsEmpty`、`TestPlatformForSessionPrefersDriverFact`、`server.TestIntranetSummaryKeyConclusions`（含「Windows 立足点要说明方言」与「纯 Linux 立足点不提方言」两条）锁住行为。

#### 6.1.2 侦察命令包自己判平台，与隧道子系统可能不一致（P1）

`server/session_recon.go` 原有一份独立的 `reconCommandPack(kind)`，用字符串匹配 `Kind()`。而隧道侧走 `session.PlatformForSession`。两套判据意味着**同一个立足点可以「隧道按 Windows 部署、侦察按 Linux 发包」**。已改为按 `session.PlatformForSession` 的方言选包，删掉重复实现。

同时补一个真实缺口：反转 shell（`reverse`）的驱动结构上不知道平台（PTY 不区分 OS），只能按马型兜底判成 linux。若目标其实是 Windows，五条侦察命令全失败、报告为空，planner 读到「网卡 0 / 路由 0 / 邻居 0」会当成「这个立足点没有任何网络信息」，进而放弃整条横向分支。

修法：**全灭且方言本来就是猜的**时候，用一条哨兵回显现场测方言（`echo __A$((40+2))__ 2>&1`，sh 会算成 42、cmd 原样输出，与 `http_shell.go` 的 `echoProbeCommand` 同一原理），判出来不同就换包重跑。正常路径零额外开销——只在「已经全失败」时才多花 1 条命令。返回值同时补 `platform` / `platform_source`（`driver`|`inferred`）/ `command_pack`，全灭时给顶层 `note` 明确「空报告 ≠ 目标没有网络」。

`server.TestReconCommandPackSelectsWindowsForASPX` 现在断言的是「马型 → 方言 → 命令包」整条链，`TestReconCommandPackDefaultsToLinuxForUnknownPlatform` 锁住「未知方言不得被当成 Windows」。

#### 6.1.3 授权来的会话只显示存在、不显示能做什么（P1）

| 项 | 内容 |
| --- | --- |
| 生产者 | `session_access_grants`（任务交接时写入） |
| 原消费入口 | 只有执行侧（`CanAccess`）与人工界面（`/api/sessions/{id}/grants`） |
| 缺失 | `intranetSnapshot.sessions`、`session_list` |
| 后果 | 任务 B 从任务 A 交接拿到一条只有 `read` 授权的会话。planner 看到「有 1 个存活立足点」→ 派「用它执行命令查进程」的意图 → worker 到执行那一刻才被拒（`任务 X 未获会话 Y 的 exec 操作授权`）→ 意图空转。信息一直在库里，只是没到派意图的那一层 |

修法：`db.GrantsForTask(taskID)`（一次查询覆盖整页，谓词与 `CanAccess` 完全一致，投影不可能比执行侧更宽松），在 `snapshotSessions` 与 `session_list` 里对**非本任务创建**的会话附加 `granted_actions`。本任务自己创建的会话不加这个字段——它的权限是隐含的，加一行只是浪费 prompt。

`db.TestGrantsForTaskMatchesEnforcement` 锁住「投影与执行侧同谓词」：有 read 投影时 exec 必须仍被拒，过期/撤销后投影必须同步消失。（该用例需要 `ARTEX_PG_DSN`，本机无 PG 时按既有约定 SKIP。）

### 6.2 检查过、确认已闭环的

| 能力 | 生产者 | 消费入口 | 结论 |
| --- | --- | --- | --- |
| `sites_without_endpoints` | `db.SitesWithoutEndpoints` → `graphOverviewData` | planner 提示词 `planner.go:312` 早已承诺该键 | 前一轮补齐了产出，键与提示词一致；worker 侧也可见 |
| `probe` 的 `probe_ran` / `probe_unsupported` | `tunnel/probe.go` | `tunnel_probe` 返回值 + 工具描述 + `adapter=auto` 分支 | 三层都在：顶层 `probe_ran=false`、描述里写明「不要据此判断出网能力」、auto 选型把非 TCP 结论一律导向 suo5 |
| `tunnel.DeployCaps` | `tunnel/platform.go` | `tunnel_deploy` 的 `platform_caps` + `verify_hint` | 部署当时可见且可行动（提示去跑 `tunnel_health` 复核）；不持久进快照，见 §6.3 |
| `session.Capabilities` | `DescribeCapabilities` | `session_list` 每行 `capabilities`（含 2048 字节上限与整体省略） | 可达；`execution.exit_code=unsupported` 等字段能让 agent 知道「别依赖退出码」 |
| `RouteTable` | `tunnel/route.go` | `tunnel_list` 只给计数 | 刻意只给计数，见 §0.3 |
| `enrich` 的端点回灌 | `enrich.sink` → `UpsertEndpoint` + `task_asset_links.source=enrich:<src>` | `coverage.by_type.endpoint`、`sites_without_endpoints` 计数下降 | 间接但可达：补全产出会让「无端点站点」计数下降、`endpoint` 类型总数上升。这是刻意的「无感」——`insert_assets` 签名与返回值不变，见 §6.3 的衰减风险 |
| `session_health` 探活 | `ApplyProbe` | `sessions.status` → 快照 `sessions[].status` | 闭环 |
| `registry_cleanup` | `CloseAndRemove` | 删除路径 | 闭环（生命周期，不面向 agent） |

### 6.3 检查过、**不改**的（明确取舍，不是遗漏）

1. **`platform_caps` 不进 `tunnel_list`。** 它回答的是「这条隧道建成那一刻，目标侧哪些层是 supported/unknown」。agent 真正需要的运行期信号是 `tunnel_health` 的结果，而不是一份静态能力声明。把静态声明也塞进台账，只会让 agent 在「caps 说 unknown」和「health 说 alive」之间做无谓的权衡。

2. **`sites_without_endpoints` 会在没有人工派工的情况下自己降到 0。** 因为 `insert_assets` 登记 service 就会触发 `enrich` 抓取并回灌端点。这是设计意图（能力长在既有动作上），但有个副作用：对需要登录/纯前端渲染的站点，`enrich` 抓不到端点，计数却可能因其他站点被补全而下降，planner 会误以为「采集已经做完」。**本轮不修**——修它需要一个「补全是否真的覆盖了该站点」的判定，那是采集侧的独立工作包。记录在此，避免被当成「信号可靠」。

3. **`enrich` 触发时不回告 agent。** `insert_assets` 的返回值里没有「已排队补全」这一项。这是 §「无感」的直接要求：一旦返回值里出现「补全已排队」，agent 就会开始围绕它做规划（等结果、重试、抱怨没结果），工作流就被改变了。代价是 agent 只能通过覆盖率与无端点站点数间接感知，接受。

4. **`db.SessionStore.ListAccessible` 是生产死代码**（仅被自身测试引用，已被 `ListAccessiblePage` 取代）。保留是因为那条测试同时锁住了「授权谓词先于解密」这一语义。**建议后续删除并把该断言迁进 `ListAccessiblePage` 的用例**，避免有人误以为它是现役授权路径。

### 6.4 本轮自查的净结论

四类真问题里，三条已修（平台信息不同步、侦察方言双判据、授权动作集缺失），一条记录不改（`ListAccessible` 死代码）。修法的共同点是**都不新增工具、不新增触发、不改变 agent 的工作流**——只是把已经算出来的东西送到该看它的地方。

反过来说，这次自查也确认了一件事：**「信息同步」这类缺陷不会自己暴露**。它不报错、不 panic、测试全绿，只是让 agent 基于一个空字段做决定，然后把决定写进事实库。所以它必须在每次新增「生产者」时成对检查「谁消费、消费方能否据以行动」，而不是等复盘时从错误结论倒推。

