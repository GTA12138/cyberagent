# YMH 能力感知执行与资源依赖收尾实施记录

日期：2026-10-23。项目：`E:/cyberagent/artex-ymh-main/artex-ymh-main`。

本轮接着 10-22 的「隧道平台适配 + 多路径路由」，继续补 12 工作包里**卡住内网纵深**的两项：

1. **I2 能力感知执行**——决定「拿到立足点之后，发的命令是不是这个通道能执行的」；
2. **I4 资源依赖与生命周期**——决定「删掉一个立足点时，挂在它上面的隧道会不会变成悬空资源」。

判据仍是前几轮那条：**能力提升，不是额外触发**。删掉本轮改动，agent 的工作流不需要改一个字；装上它，同样的动作少走弯路。

## 0. 开工前的核实（引用里有些判断已经过时）

收到的问题清单里，有两处我核对后确认**当前代码已经修过**，不能再按「未做」记：

| 清单里的说法 | 实际状态 | 证据 |
| --- | --- | --- |
| 「健康循环看到隧道不通会重拉——包括你刚刚手动停掉的那条」 | **已修**。`Teardown` 先把 `CleanupPending` 持久化进 `deploy_params` 再清理；`healthRecord` 遇到 `CleanupPending` 或 error 前缀「清理未完成:」直接跳过；`ListActive` 只取 `deploying/alive/error`，`stopped` 根本不进巡检候选 | `tunnel/health.go:102`、`tunnel/health.go:318`、`db/tunnels.go:144` |
| 「Windows 立足点能侦察但建不了隧道」 | **已修**（10-22 那轮）。目标侧命令全部经 `tunnel.HostShell` 按平台生成 | `tunnel/platform.go`、`tunnel/deploy.go` |

顺带还核实了一件对上一轮很重要的事：`reconCommandPack` 现在走 `session.PlatformForSession`，不再是本文件自己的字符串匹配（10-22 §6.1.2）。

所以本轮只做真正还缺的两块。

## 1. I2：能力描述参与决策（新建 `capability/`）

### 1.1 问题：能力只被显示，从未被使用

`session.Capabilities` 上一轮就实现了，`HTTPShell` 和 `reverseShell` 都实现了 `CapabilityProvider`。但全仓消费者只有两处：会话列表的元数据、前端详情。**没有任何一处是「发包之前先问能力」。**

这带来一个具体的坏形态：

- 一个 PHP eval 马，`disable_functions` 把执行函数全禁了。`session_exec` 照样发包，PHP 层返回错误，agent 看到的是「执行失败 + 一段目标侧报错」——很容易被记成「马失效了」或「命令被 WAF 拦了」，而真实原因是**这个通道的驱动根本不支持执行**。
- 一个 aspx 马（Windows）。agent 发 `ls -la`。cmd.exe 返回「'ls' 不是内部或外部命令」。agent 学到的是「目标上没有 ls」，而它真正该学到的是「我在一个 cmd 上，应该用 dir」。

这两种错误的共同点：**系统明明知道答案，却让 agent 去目标上撞出来**，而且撞出来的结论会被写进事实库。

### 1.2 做了什么

新增 `capability/` 包，两件事，都不碰目标：

**（1）`Contract`：把能力压成可直接判读的合同**

```go
type Contract struct {
    Driver, Platform string
    PlatformKnown    bool
    Exec, ReadFile, WriteFile session.CapabilityState
    ExitCode        session.CapabilityState
    SeparateStreams bool
    CanDetach       session.CapabilityState
    PersistentJobs  session.CapabilityState
    Unverified      []string   // 描述为 unknown 的项
}
func For(s session.Session) Contract
func (c Contract) Check(op Op) (allowed bool, reason string)
```

三条刻意的取舍：

- **`unknown` 一律放行，只有 `unsupported` 才拦。** 把未知当缺失，会让所有还没声明能力的驱动直接不可用——那是用一次误拦换掉整个功能。
- **`Unverified` 必须列出未知项。** 不列出来，调用方就会把沉默当保证。
- **`CanDetach` 恒为 `unknown`，且刻意不拿 `RemoteStopOnCancel` 推导。** 「取消本地调用能停掉远端进程」和「进程能在目标侧脱离会话存活」是两个问题。能杀不等于能脱离；混用会让 agent 以为后台作业是可管理的。真正的 detach 证据只能来自目标侧观察，不在这层。

**（2）`CheckCommand`：发包前的方言预检**

只看命令的**首个 token**，命中高置信名单才判不匹配：

| 方言 | 必然不存在的命令头 | 明确**不**收的 |
| --- | --- | --- |
| Windows | `id` `uname` `ls` `cat` `chmod` `chown` `ps` `ifconfig` `sudo` `systemctl` `apt` `yum` `crontab` … | `dir` `type` `whoami` `netstat` `curl` `ping`（两边都有）；`bash` `sh` `wsl` `grep` `sed` `awk`（常随 Git Bash/WSL 出现，且 `bash -c "ls"` 正是要推荐的写法）；`service`（Windows 上也可能有同名可执行文件） |
| Linux | `ipconfig` `icacls` `certutil` `powershell` `tasklist` `findstr` `schtasks` `reg` `wmic` `net1` … | — |

`bash` / `wsl` / `python3` 这类**显式换解释器**的包装一律放行：那是有意为之，不是错误。

判据是**误拦比不拦更糟**：拦错一条其实能跑的命令，agent 会换着花样重试，成本比让它看到目标返回的真实报错更高。

### 1.3 接在哪（三处，都是发包之前）

| 工具 | 前置检查 | 拦下来时的返回 |
| --- | --- | --- |
| `session_exec` | `Check(OpExec)` + `CheckCommand` | `capability_missing:` 或 `dialect_mismatch（未发包）:` + 该方言下的等价命令 |
| `session_read` | `Check(OpRead)` | `capability_missing:` + 建议改走命令通道 |
| `session_write` | `Check(OpWrite)` | 同上 |
| `session_recon` | `Check(OpExec)` | `capability_missing:` + 说明侦察依赖执行（否则会空打五条，再被读成「目标没有网络信息」） |
| `tunnel_deploy` | `Check(OpExec)` | `capability_missing:` + 说明部署依赖目标侧执行（否则会走完投递才失败，在目标上留下半成品文件） |

**为什么是「拦」而不是「警告后照发」**：`dialect_mismatch` 的失败形态（「'ls' 不是内部或外部命令」）会被 agent 读成「目标没有这个命令 / 会话有问题」，而它是**零信息量**的——它既没告诉 agent 该怎么做，又留下一个错误的观察。拦下来给等价命令，信息量严格更大，且花费 0 次目标请求。

**这条改动是「无感」的**：agent 不需要知道 `capability/` 存在，它只是发现「发错的命令被拒了，并且告诉了我该怎么写」。

### 1.4 顺手补的一处：空输出不等于成功

`session_exec` 在原通道下拿不到退出码（`Execution.ExitCode = unsupported`），所以「命令返回空、没有报错」是 agent 最容易误判成成功的形态。现在只在**这个形态下**补一句：

```
note: 命令返回空输出，且本通道无法取退出码（unsupported）：空输出不等于成功，
      也可能是失败但无回显。要判定成败请显式带哨兵：linux 用 `; echo __RC=$?`，
      windows 用 `& echo __RC=!ERRORLEVEL!`（需 cmd /v:on）。
```

只在 `stdout == "" && stderr == "" && ExitCode != supported` 时出现，正常路径零字节成本。

**没有做「自动给命令加退出码哨兵」。** 理由：sh 侧追加 `; echo __RC=$?` 会被以 `&` 结尾的命令、行内注释、`exit` 打乱语义；cmd 侧 `%ERRORLEVEL%` 在整行解析期就展开，必须 `cmd /v:on` + `!ERRORLEVEL!`，而 aspx 驱动已经把命令包在 `echo START& cmd 2>&1 &echo END` 里。要在不破坏既有包装的前提下正确追加，需要改驱动层的包装结构，那是一次独立改动。**告诉 agent 怎么写，比替它改写命令安全。**

## 2. I4：会话 → 隧道的依赖收尾

### 2.1 问题：删会话留下悬空隧道

`DELETE /api/sessions/{id}` 只做三件事：关驱动、移出注册表、删 DB 行。**完全不看 `tunnels.via_session_id`。**

后果不是「少清理一个文件」，而是造出一个谁也管不到的资源：

- 平台侧 chisel server 还在监听端口；
- 目标侧 chisel client 还在按 `deploy_params` 重连；
- 但重拉需要会话，会话没了 → 健康巡检的 `maybeRedeploy` 永远走不到它；
- 台账里它还是 `alive`，`intranetSummary` 会继续报「有 1 条 alive 隧道」，planner 据此以为内网通道还在。

这是一个**典型的「信息与事实分叉」**：台账说 alive，实际已经无法操作，而且没有任何机制会纠正它。

### 2.2 做了什么

- `db.TunnelStore.ListActiveByViaSession(ctx, sessionID)`：列出经该会话部署、且状态在 `deploying|alive|error` 的隧道（`stopped` 不算依赖，否则会对一条已经收干净的隧道重复清理）。
- `DELETE /api/sessions/{id}` 改为**按依赖逆序**：先逐条 `tunnels.Teardown`，再关会话、删记录。

失败语义刻意如此：

| 情况 | 行为 |
| --- | --- |
| 依赖隧道全部收掉 | 继续删会话，响应里带 `cascaded_tunnels: [id...]` |
| 任一隧道收尾失败 | **返回 409，会话保留**，响应里列出失败的隧道与原因，提示先 `tunnel_teardown` 再删 |

「收不掉就不删会话」是这一轮唯一一处「拒绝用户操作」的地方，理由是：远端可能还有活着的进程，把记录留下让人重试，比留下一个查不到的悬空隧道诚实。这与既有的「远端不可达时保留待清理记录，不承诺无法观测的远端已消失」是同一条纪律。

级联收掉的隧道要显式回报（`cascaded_tunnels`）：否则这些停止在内网拓扑上就是「无解释的消失」。

### 2.3 没有做的

- **任务停止/归档时的依赖逆序释放**：已由 `tunnel.Manager.Cleanup(ctx, taskID)` 覆盖（按 task 收全部未终结隧道），本轮未改。
- **共享会话（handoff）的引用计数**：一个会话被多个任务引用时，删它应该只减引用而不是直接删。当前 `session_access_grants` 只有授权没有引用计数，本轮未动——它属于 I4 的「独立 grant/引用/租约」子项，需要先决定「最后一个引用消失时是否自动关闭远端」，那是产品决策，不是顺手能定的。
- **`desired_state` / `generation` 列**：核实后确认现有 `CleanupPending` + `state` 已能表达「停止请求 / cleanup_pending / 清理已确认」三态，再加一列是重复真相源。**不加**。

## 3. 验证

源码级（本机 Go 1.27.1）：

- `go build ./...` 通过。
- `go vet ./...` 通过。
- `go test ./... -count=1`：26 个有测试的包全部通过（新增 `capability` 包）。
- 新增测试 10 个：`capability/capability_test.go` 9 个（纯逻辑，本机实跑）；`db/tunnels_test.go` 追加 1 段依赖枚举断言（需 `ARTEX_PG_DSN`，本机无 PG 时按既有约定 SKIP）。

关键断言：

| 断言 | 位置 | 守的是什么 |
| --- | --- | --- |
| 合同推导不碰目标 | `TestForReadsDriverFactsWithoutTouchingTarget` | 桩的 Exec/ReadFile/Kind 全 panic，能跑通即证明只读元数据 |
| `CanDetach` 恒 unknown | 同上 | 不用「能杀远端进程」冒充「能后台常驻」 |
| 未知项必须列出 | `TestForListsUnverifiedFields` | 沉默不等于保证 |
| 已声明项不进 Unverified | `TestForDeclaredFieldsAreNotUnverified` | 否则「未知」标记失去意义 |
| 只有 unsupported 才拦 | `TestCheckOnlyBlocksExplicitUnsupported` | 把未知当缺失会让大多数驱动不可用 |
| Windows 下 Unix 命令被拦 | `TestCheckCommandWindows` | 核心场景 |
| 两边都有的命令不拦 | 同上 | 误拦比不拦更糟 |
| 显式包装放行 | `TestCheckCommandAllowsExplicitWrapper` | `bash -c` 是有意换方言，不是错误 |
| 平台未知一律放行 | `TestCheckCommandUnknownPlatformNeverBlocks` | 不知道就不猜 |
| 首 token 解析 | `TestFirstToken` | env 赋值/`nohup`/`time`/路径前缀都能穿透 |
| 依赖枚举只看未终结 | `db/tunnels_test.go` | 已 stopped 的不算依赖，不重复清理 |

## 4. 保留边界

### 4.1 方言预检是启发式，不是解析器

只看首个 token。`for f in *; do cat $f; done` 这种首 token 是 `for` 的复合命令不拦（正确）；但 `echo $(ls)` 里的 `ls` 也不会被发现（漏拦）。**漏拦可接受，误拦不可接受**——这是刻意的单向设计。

### 4.2 能力门禁今天不会真的触发

`HTTPShell` 和 `reverseShell` 都声明 `Exec = supported`，所以 `Check(OpExec)` 目前永远放行。**这不是死代码，是合同先立、实现后补**：`HTTPShell.Capabilities()` 明确写了「不读可变探测状态」，所以「这个 PHP 马现在还有没有执行函数」不能拿来降级 `DriverSupport`——那会把驱动能力和环境状态混成一个值。

真正会触发门禁的时机是：将来有驱动（例如 thin runner、受限会话）明确声明 `unsupported`。**在那之前，本轮的实际行为收益全部来自方言预检。**

### 4.3 `SeparateStreams` 目前恒为 false

`shellDriverCapabilities` 声明 `OutputStreams = merged`，两个驱动都一样。所以这个字段现在是常量。保留它是因为 `session.OutputStreamsSeparate` 已经存在，将来接 runner 时不需要改合同。

### 4.4 会话删除的级联只在人工通道生效

agent 没有「删会话」工具（只有 register/exec/read/write/recon/tunnel_*），所以悬空隧道只可能由人工界面产生。**没有给 agent 加删除工具**——删立足点是不可逆操作，不该由模型自主决定。

### 4.5 未验证的部分

- 没有真实 aspx 目标可测，`dialect_mismatch` 的端到端行为只在单测层面验证。
- 会话删除的级联没有端到端测试（需要真实隧道 + 远端），只验证了枚举查询与失败分支的语义。
- 未跑 `-race`。

### 4.6 平台判定的已知误判：Windows 上的 PHP/JSP 马

`HTTPShell` 的平台来自**载荷语言**（aspx ⇒ windows，php/jsp ⇒ linux）。这是驱动事实，不是观测。所以一个跑在 IIS + PHP 上的目标会被判成 linux，方言预检于是放行 `id`/`ls` 这类命令，而它们在 cmd 下不存在。

**这一层不修**，因为修它需要目标侧观测（`uname -s` / `cmd /c ver`），那是 `session_probe` 的职责而不是判定的职责。现状是：判错时方言预检不拦，agent 仍会拿到目标返回的真实报错——**退化成改造前的行为，不会更糟**。`session_recon` 的全灭方言探测（10-22 §6.1.2）是这条链上唯一能自愈的地方。

同理，反转 shell 的驱动结构上不知道平台（PTY 不区分 OS），`DescribePlatform` 返回 `""`，方言预检对它完全不生效。

## 5. 与 12 工作包的对应关系

| 包 | 本轮覆盖 | 状态 |
| --- | --- | --- |
| S0 | — | 前轮已完成（授权/移交子集） |
| W1–W4 | — | 未动 |
| I1 | — | 前轮完成「按目标选路」；录制/路由解耦、会话控制路由、网段声明仍未做（见 §6） |
| **I2** | `capability/` 合同 + 5 处前置检查 + 方言预检 + 空输出语义 | **本轮新增，最小闭环** |
| I3 | — | 未动 |
| **I4** | 会话→隧道依赖收尾 | **本轮新增，子集**（引用计数、租约未做） |
| O1 / O2 | — | 未动 |
| E1 | — | 未动 |

## 6. 下一件该做什么（按「哪一件更卡住纵深」排）

1. **I1 的录制/路由解耦**（清单里的 F02）。现状：`TaskProxyForTask` 在 `!TrafficEnabled()` 时直接返回空，worker 于是回落全局出口——**关掉抓包就等于关掉隧道选路**。这在 OPSEC 要求不录制时是致命的：你既不能走隧道，也不能假装自己在内网。
   难点不在开关，而在三个耦合的不变量：任务代理实例本身就是一个 `traffic.Traffic`（录制是它的内建行为）、`ProxyCACert()` 为空被当作「未录制」信号、以及关闭录制后 MITM 仍在中间人（CA 仍然必须被信任）。正确做法是给 `traffic.Open` 加录制开关并让 CA/信号与「是否有代理」解耦，**需要真实流量环境验证**，所以本轮只记录不实现。

2. **I3 的认证结果账本 + Linux/SSH 横向**。
   **更正（2026-10-23 文档审核）**：本条原文写「agent 只能自己拼 `nxc` 命令行」，这是**错的**。`server/lateral_exec.go`（277 行）已实现并注册（`server/credentials.go:35`）：过 RoE scope、按凭据类型拼 impacket 参数、0600 临时文件传凭据、判定成功、成功即 `SetVerified(true)`。真实缺口只剩两块：
   - **Linux/SSH 完全没覆盖**——`lateralMethods` 只有 psexec/wmiexec/smbexec/atexec/dcomexec（`:41-47`），票据/SSH key/token 直接报「暂只支持 password / hash_nt / hash_lm」（`:235-237`），`go.mod` 无 `golang.org/x/crypto/ssh`；
   - **认证结果没有独立实体**——`principal + credential_ref + service + target + route + auth_method + time + outcome` 这条记录目前只体现为一个 `verified` 布尔，`artifact_ref` 全仓零命中，失败缓存与账户锁定预算也不存在。

   所以最小可用形态不是再写一个 `lateral_attempt`，而是：给 `lateral_exec` 补 `ssh` method（优先复用「ssh 会话登记 + `session_exec`」这条路，符合无感判据），并加认证结果表。
   详见 [`YMH-待办总表.md`](YMH-待办总表.md) T-07 / T-08。

3. **O1 的意图准入**。`prerequisites` 全仓零命中，planner 只能把依赖关系记在内存 todo 里，任务一重启就丢。注意设计文档自己的警告：一旦用 `blocked` 表达等待，就必须同步改 coldgraph settled 判定、`HasActiveIntent`、完成判定、重开与压缩规则——**不能只加字段**。这是下一轮最需要小心的一件。

## 7. 变更清单

| 文件 | 性质 | 内容 |
| --- | --- | --- |
| `capability/capability.go` | 新增 | `Contract` / `For` / `Check` / `CheckCommand` / 方言名单 / 首 token 解析 |
| `capability/capability_test.go` | 新增 | 9 个用例 |
| `session/capabilities.go` | 修改 | 补 `OutputStreamsSeparate` 常量（合同需要三态，原来只有 unknown/merged） |
| `server/session.go` | 修改 | `session_exec` 能力门禁 + 方言预检 + 空输出语义；`session_read`/`session_write` 能力门禁 |
| `server/session_recon.go` | 修改 | 侦察前 `Check(OpExec)`，避免空打五条 |
| `server/tunnel.go` | 修改 | 部署前 `Check(OpExec)`，避免走完投递才失败 |
| `server/intranet.go` | 修改 | 删会话前按依赖逆序收隧道；收不掉返回 409 并保留会话；响应带 `cascaded_tunnels` |
| `db/tunnels.go` | 修改 | `ListActiveByViaSession` |
| `db/tunnels_test.go` | 修改 | 依赖枚举断言（含「stopped 不算依赖」） |

未修改 `go.mod` / `go.sum`；未引入新依赖。

## 8. 交付状态

本轮只改源码与文档，**不重新构建候选程序、不替换项目根目录 `artex.exe`、不重启用户服务**。源码可构建、全包测试通过即可交付评审。
