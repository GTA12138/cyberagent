# YMH 补全出口与抓取预算修复实施记录

日期：2026-10-23。项目：`E:/cyberagent/artex-ymh-main/artex-ymh-main`。

本轮按 [`YMH-待办总表.md`](YMH-待办总表.md) 的优先级往下推，动了两件 P0 和一件 P1：

1. **T-22 补全走任务出口**——决定「补全在内网目标上有没有用」；
2. **T-01 tarpit 预算覆盖面**——决定「补全和 Bash 抓取算不算进预算」；
3. **T-04 `(type, domain)` 索引**——决定 `sites_without_endpoints` 在大任务上扫不扫全表。

**T-02（录制/路由解耦）本轮不做**，理由和可执行设计见 §3。这不是跳过，是核实后发现它比我原先写的更绕：有一个我上一轮没看到的陷阱。

---

## 0. 开工前对照（待办总表 → 本轮）

| 编号 | 待办 | 开工前核实 | 本轮结论 |
| --- | --- | --- | --- |
| T-22 | enrich 补全走全局代理 | **未做**。`server/manager.go:511` `enrich.New(m.assets, m.ProxyAddr, 4)`；形参 `proxy func() string` 不带 taskID | **已修**（§1） |
| T-01 | 修 tarpit 覆盖面 | **未做**。`guard/tarpit.go:64` 只有 `if ev.ToolName != "WebFetch" { return }` | **已修**，含刻意保留的缺口（§2） |
| T-04 | `SitesWithoutEndpoints` 加索引 | **未做**。`db/schema.sql` 无 `(type, domain)` 索引 | **已修**（§4） |
| T-02 | 录制与路由解耦（F02） | **未做**。`TaskProxyForTask` 在 `!TrafficEnabled()` 直接返回空 | **仍未做**，给出设计（§3） |
| T-03 | 跑一次真实站点 | 未做 | 未做（需要真实目标，本轮不编造结果） |

---

## 1. T-22：补全走任务出口

### 1.1 问题不是「走错代理」，是「结构上没有任务维度」

改前 `enrich.New(as, proxy func() string, workers)`。这个签名决定了它**永远只能拿一个地址**——不是调用方传错了，是类型上就没有第二个任务的余地。

调用方传的是 `m.ProxyAddr()`，而 `ProxyAddr()`（`server/manager.go:950-957`）返回的是**全局**录制代理或 `globalProxy`。任务级出口是另一套东西：`m.taskProxy.ForTask(ctx, taskID)`（`server/taskproxy.go:182-207`），每个任务一个独立 MITM 实例、独立 CA、独立流量目录 `<dir>/<taskID>`（`taskproxy.go:446`）。

后果链（上一轮已记录，这里复述一遍因为它是本轮的动机）：

1. 经隧道才可达的内网资产，补全从平台自己的出口出去 → 必然失败或拿错响应；
2. 补全流量落全局仓，任务侧 `withTraffic` 读的是 `<dir>/<taskID>`，两仓不通 → 任务内 `traffic_search` 查不到补全请求；
3. 于是 v2 §8.1 那个验证方法（「用 `traffic_search` 核对补全请求是否都落在流量仓里」）会给出**假阴性**。

### 1.2 改法：三处，一个通道

**(a) 形参改成带 taskID 的解析器**（`enrich/enrich.go:141-145`）

```go
func New(as *db.AssetStore, proxy func(taskID int64) string, workers int) *Engine
```

**(b) taskID 通过请求 context 走到 transport**（`enrich/enrich.go:242-265`）

`http.Transport.Proxy` 收到的是 `*http.Request`，所以 `req.Context()` 是现成的传递通道：

```go
type taskIDKey struct{}

func withTaskID(req *http.Request, taskID int64) *http.Request   // taskID<=0 不打标记
func taskIDOf(req *http.Request) int64                            // 0 = 无归属
```

`doHTTP` 在建完请求、发出去之前打标记（`enrich/enrich.go`，`req = withTaskID(req, j.taskID)`）。

**(c) server 侧接任务出口**（`server/manager.go`）

```go
m.enrich = enrich.New(m.assets, m.enrichProxyForTask, 4)

func (m *Manager) enrichProxyForTask(taskID int64) string {
	if taskID > 0 {
		if addr, _ := m.TaskProxyForTask(taskID); addr != "" {
			return addr
		}
		log.Printf("[enrich] 任务 %d 无任务级出口，回落全局出口（...）", taskID)
	}
	return m.ProxyAddr()
}
```

### 1.3 为什么用 context 传 taskID，而不是每个任务一个 client

备选方案是 `sync.Map[taskID]*http.Client`。否掉的理由很具体：`buildClient` 设了 `DisableKeepAlives: true`，但仍然会**给每个任务持有一个 `http.Transport`**（以及它的 idle 连接表、TLS session cache）直到进程结束。任务数上百时这是纯泄漏，而 context 方案共用一份 transport，成本为零。

代价是：`Proxy` 回调里要做一次 context 取值。这是 map lookup 级别，可以忽略。

### 1.4 前后对照

| 维度 | 改前 | 改后 |
| --- | --- | --- |
| enrich 出口 | `m.ProxyAddr()`（全局录制代理 / globalProxy） | `m.enrichProxyForTask(taskID)` → 任务 MITM 实例 |
| 传 taskID 的通道 | **不存在**（`proxy func() string`） | 请求 context（`withTaskID` / `taskIDOf`） |
| 内网资产补全 | 从平台出口出去，必然失败 | 走该任务的隧道 |
| 补全流量落仓 | 全局流量仓 | 任务流量仓 `<dir>/<taskID>` |
| 任务内 `traffic_search` | 查不到补全请求 | 能查到 |
| v2 §8.1 的验证方法 | **假阴性**（查不到 ≠ 没发） | 有效 |
| 任务级解析失败时 | 静默用全局 | 回落全局 **+ 留日志**（与 worker 的回落语义一致） |

### 1.5 保留的边界

- **不改变回落策略**。`worker.proxyForTaskSource`（`agent/worker.go:252-259`）在任务级拿不到时就回落全局，补全跟着同一套规则，不另立一套。
- **DNS 不走任务路由**。`doDNS` 用 `dnsx` 的默认 resolvers，仍然从平台解析。内网域名的解析要等 I1 的 `dns_policy`（待办 T-06），本轮不动。
- **录制关闭时任务出口仍为空**。这是 T-02 的耦合点，见 §3。

---

## 2. T-01：tarpit 预算把补全和 Bash 抓取算进来

### 2.1 问题

`guard/tarpit.go:64` 原文只有一行门禁：`if ev.ToolName != "WebFetch" { return }`。

v2 落地补全之后，这个门禁的意义变了：**补全会自己发请求**（登记 service 抓一次 + `follow` 派生跟一跳，见 `enrich/enrich.go` 的 `doHTTP` / `follow`）。而且它恰好是「一个导航页派生十几个脚本」这种最容易撞上 tarpit 的形态。补全不计入预算 = 补全是预算外通道。

v2 自己也写了这一点，并标注「本次改动引入的新风险，优先级最高」。

### 2.2 改法：统一到一个计数入口

原来计数逻辑内联在 `checkTarpit` 里，只有 WebFetch 能走到。现在抽出公开方法：

```go
// guard/tarpit.go
func (g *Guard) NoteFetch(source, host string, n int)
```

`source` 只用于审计行（`WebFetch` / `Bash` / `enrich`），不参与预算判断。三条来源全部汇到它：

| 来源 | 入口 | host 怎么来 |
| --- | --- | --- |
| agent 的 `WebFetch` | `checkTarpit` 解析 `url` 字段 | `url.Hostname()` |
| agent 的 `Bash`/`shell_*` | `checkTarpit(ev, cmd)` → `fetchTargets(cmd)` | 见 §2.4 |
| 引擎侧补全 | `enrich.SetFetchObserver` → `Guard.NoteFetch("enrich", …)` | `base.Hostname()` |

server 侧接线（`server/manager.go`）：

```go
m.enrich.SetFetchObserver(func(taskID int64, host string, n int) {
	if t := m.taskByNumericID(taskID); t != nil && t.Guard != nil {
		t.Guard.NoteFetch("enrich", host, n)
	}
})
```

`taskByNumericID` 是新加的小helper：`m.tasks` 是按字符串 key 存的，补全侧只有 `int64`，不想为此走公共 API 做字符串往返。

**计数时机刻意放在发请求之前**（`enrich.go`，`e.noteFetch(j.taskID, base.Hostname(), 1)` 在 `e.client.Do(req)` 之前）。理由：tarpit 迷宫对每次抓取都回 200，按响应计永远数不到；按「即将发出」计，连目标不可达的循环也能被计数——那时 hint 说的「抓取次数超预算」仍然为真。

### 2.3 前后对照

| 计数来源 | 改前 | 改后 |
| --- | --- | --- |
| `WebFetch` | 计 | 计 |
| Bash 里的 `curl` / `wget` / `katana` / `httpx` / `ffuf` / `gobuster` / `feroxbuster` / `dirsearch` / `dirb` / `wfuzz` / `nikto` / `whatweb` / `nuclei` / `wpscan` / `gospider` / `hakrawler` | 不计 | 计（`ExtractHosts` 抽 host） |
| Bash 里的 `python` / `python3` / `node` **带显式 scheme URL** | 不计 | 计 |
| Bash 里的 `python` / `node` **只有裸 IP** | 不计 | **不计**（刻意，见 §2.4） |
| Bash 里的 `nmap` / `ssh` / `nc` / `psql` | 不计 | 不计（不产生页面抓取） |
| enrich 补全（S2 抓取 + 派生跟一跳） | **不计** | 计 |
| 计数入口 | 内联在 `checkTarpit` | `Guard.NoteFetch` 单一入口 |
| 审计行来源 | 只有 tool 名 | `WebFetch` / `Bash` / `enrich` 可区分 |
| 并发 | 只有 PreToolUse（单线程） | 加锁，agent 侧与 enrich worker 并发 |
| 预算语义 | 每 host 计数 | 不变（同一份预算、同一个「每 host 只熔断一次」） |

### 2.4 刻意保留的缺口，以及为什么

**`python3 exploit.py --target 10.0.0.5` 不计。** 只有命令里出现显式 `http(s)://` 才计（`scriptRunners` 分支）。

理由：从命令行无法判断一个解释器是在**抓取**还是在**利用**。把 `python` 无条件算进来，等于任何打点脚本都会被记成「对目标抓了 40 次」，熔断 hint 会说「疑似 tarpit 迷宫」——而 planner 读到 hint 会**停掉一个本来正确的方向**。误杀的代价（丢一条真漏洞线）远大于漏计的代价（少一个 hint）。

同理还有两处已知保守跳过，都写进了测试用例，防止后人当成 bug 修：

- `httpx -u c.example`（裸主机名）：`-u` 在 `valueFlags` 里（`curl -u` 是凭据），同名字段无法两全，按「宁缺勿滥」跳过；
- `ExtractHosts` 抽不出 host 的任何命令：不计。

**残留风险（诚实记录）**：`for i in $(seq 100); do curl …; done` 这种 shell 循环只按 1 次计。要真正封住得在网络层做，不在 guard 这一层。本轮不假装解决。

---

## 3. T-02：本轮不做，但设计已经可执行

### 3.1 为什么这一轮不做

T-02 的目标是「关掉抓包 ≠ 关掉出口」。核实后发现它不是「删一个 if」，而是**四个地方要同时改，漏一个就出现新的不一致**。而且改错的后果是「录制关了却还在写盘」或「提示词说有流量工具、实际没注册」——都属于比现状更难查的错。

### 3.2 具体四处改动

**(1) `traffic.Traffic` 加录制开关**

`traffic/traffic.go:901` 的 `record(f *mproxy.Flow)` 是**唯一**的落盘入口（写 blob + 写 index 都在它里面，由 `wmu` 串行化）。所以加一个 `recordOff atomic.Bool` + `SetRecordEnabled(bool)`，在 `record()` 开头早退即可。改动面小，但必须确认没有第二条写路径——`spill()` 只在 `record()` 内被调用，安全。

**(2) `TaskProxyForTask` 去掉录制前置条件**

```go
// 现在
if m.taskProxy == nil || !m.TrafficEnabled() { return "", "" }
// 改成
if m.taskProxy == nil { return "", "" }
```

拿到实例后按 `m.TrafficEnabled()` 设 `inst.tr.SetRecordEnabled(...)`。

**(3) 提示词信号必须和 CA 解耦（这是陷阱）**

`agent/worker.go:349` 的 `workerTrafficBlock(recording bool)` 是**对的**——它按 recording 而不是按 CA 决定要不要注入 traffic 工具说明。但它的调用方传的是 `caCert != ""`：

```go
recording := workerSystem("127.0.0.1:8080", "/ca.pem", "/data", "/data", false)
```

（见 `agent/prompt_test.go:45-74` 的注释：「[B] trafficTool present only when RECORDING (caCert set)」）

一旦 T-02 让「录制关闭但仍有 CA」，这条推导就**反了**：prompt 会注入 traffic 工具说明，而 `HostTools()`（`server/manager.go:931-936`）在 `!TrafficEnabled()` 时返回 nil——模型被告知去用 `traffic_search`，但工具不存在。

所以 `workerSystem` / `plannerSystem` / `mainagent` / `chat` 这一串的签名要拆成 `(proxyAddr, caCert string, recording bool, ...)`，CA 管「要不要信任证书」，recording 管「有没有流量工具」。**这是 T-02 里最容易漏的一处，也是它必须先设计再动手的原因。**

**(4) `ProxyAddr()` / `ProxyCACert()` 的语义分开**

现在 `ProxyCACert()` 的空值同时表示「录制关闭」和「不需要自定义 CA」。T-02 之后这两件事必须分开表达，否则 `server/manager.go:742-743` 那两行 `proxy := m.ProxyAddr(); cert := m.ProxyCACert()` 又会把两者绑回去。

### 3.3 验收方式（必须真实流量，不能用单测代替）

1. 录制关闭 + 有任务隧道 → `curl` 一个只有隧道内可达的内网地址，应成功；
2. 同上，查任务流量仓 → 应为空（**没录制**）；
3. 同上，查 `HostTools()` → 应为 nil，且 prompt 里**不出现** `traffic_search`；
4. 录制打开 → 同一请求应出现在任务流量仓，prompt 出现 `traffic_search`。

第 3 条是单测能覆盖的（`workerSystem` 是纯函数），第 1/2 条必须有真实隧道环境。

---

## 4. T-04：`(type, domain)` 索引

`db/task_web_surface.go:28-39` 的查询有两处要靠这个索引：

```sql
FROM assets s
WHERE s.type = 'service' AND s.url <> '' AND s.domain <> ''
  AND $1 = ANY(s.task_ids)
  AND NOT EXISTS (
        SELECT 1 FROM assets e
        WHERE e.type = 'endpoint' AND $1 = ANY(e.task_ids) AND e.domain = s.domain)
```

外层过滤和 `NOT EXISTS` 子查询都需要 `(type, domain)`。现有索引里 `idx_av2_type_seen(type, last_seen)` 的第二列用不上，`idx_av2_domain(domain)` 单独走会退化成大量回表。

新增（`db/schema.sql`）：

```sql
CREATE INDEX IF NOT EXISTS idx_av2_type_domain
    ON assets(type, domain) WHERE domain IS NOT NULL AND domain <> '';
```

部分谓词与查询条件完全一致（`domain <> ''`），避免把大量 `domain` 为空的 IP 行算进索引。

**注意**：`CREATE INDEX IF NOT EXISTS` 在已有库上是幂等迁移，但**首次执行会锁表扫一遍 assets**。大库升级要考虑窗口。

---

## 5. 验证

| 项 | 结果 |
| --- | --- |
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go test ./... -count=1` | **26 个包全绿**（`session/ldpreloadgen` 无测试文件，按既有状态） |
| `guard` 新增用例 | 9 个（Bash 抓取计数 / 非抓取不计 / 解释器需 scheme / enrich 来源 / 跨来源共享预算 / 未装配 no-op / 并发不丢计数 / `fetchTargets` 8 个子用例） |
| `enrich` 新增用例 | 5 个（taskID 往返 / 按任务选代理 / 空地址直连 / nil resolver / 观察者上报） |
| `server` 新增用例 | 3 个（任务级回落 / 与录制的耦合现状 / `taskByNumericID`） |

`server` 的 `TestEnrichProxyForTaskCoupledToRecording` 是**故意钉住现状**的：它断言「录制关闭时回落全局」。T-02 落地后这个测试会失败，那时应该把它改成「录制关闭仍返回任务出口」——这是设计意图，不是遗漏。

---

## 6. 已知残留（本轮不修，别当成做完）

| 残留 | 现状 | 归属 |
| --- | --- | --- |
| 录制关闭 = 出口关闭 | `TaskProxyForTask` 仍以 `TrafficEnabled()` 为前置 | T-02（§3 有设计） |
| enrich 的 DNS 不走任务路由 | `doDNS` 用 dnsx 默认 resolvers | T-06（`dns_policy`） |
| shell 循环内的抓取只计 1 次 | `fetchTargets` 按命令计 | 网络层问题，不在 guard |
| `python`/`node` 裸 IP 不计 | 刻意的保守取舍（§2.4） | 设计选择，非缺陷 |
| 补全没在真实内网目标上验证过 | 无运行记录 | T-03 |
| `S3` 语义层仍未通电 | `SetJSAnalyzer` 零调用方 | T-21 |

---

## 7. 与待办总表的对应

| 编号 | 状态变化 |
| --- | --- |
| T-22 | 未做 → **已完成**（本轮），移至总表 §4 |
| T-01 | 未做 → **已完成（含已知残留）**，移至总表 §4，残留单列 |
| T-04 | 未做 → **已完成**，移至总表 §4 |
| T-02 | 未做 → **未做（设计已就绪）**，留在 P0，补 §3 的四处改动 |

---

## 8. 变更清单

| 文件 | 性质 | 内容 |
| --- | --- | --- |
| `enrich/enrich.go` | 修改 | `New` 形参改 `func(taskID int64) string`；新增 `taskIDKey`/`withTaskID`/`taskIDOf`；`doHTTP` 打任务标记 + 发请求前上报抓取；新增 `onFetch` 字段与 `SetFetchObserver`/`noteFetch` |
| `enrich/proxy_test.go` | 新增 | 5 个用例 |
| `guard/tarpit.go` | 修改 | 抽出 `NoteFetch(source, host, n)`；`checkTarpit` 接 `cmd` 并支持 Bash 来源；新增 `fetchBinaries`/`scriptRunners`/`fetchTargets` |
| `guard/guard.go` | 修改 | `checkTarpit(ev, cmd)` 传命令 |
| `guard/tarpit_test.go` | 新增 | 9 个用例 |
| `server/manager.go` | 修改 | `enrich.New` 接 `m.enrichProxyForTask`；`SetFetchObserver` 接任务 Guard；新增 `enrichProxyForTask`/`taskByNumericID` |
| `server/enrich_proxy_test.go` | 新增 | 3 个用例 |
| `db/schema.sql` | 修改 | 新增 `idx_av2_type_domain` 部分索引 |

---

## 9. 交付状态

- 代码：已改，`build` / `vet` / `test` 全绿。
- 未验证：**没有在真实内网目标上跑过**。T-22 的正确性目前只有单测级证据（proxy 选择按任务正确），端到端「内网资产补全能成功」需要 T-03 的实测。
- 下一步：按总表 P0 剩项 —— T-02（设计已在 §3）或 T-03（真实站点验证）。若目标是「尽快确认 web 补全真的有用」，先做 T-03；若目标是「不录制时也能走隧道」，先做 T-02。
