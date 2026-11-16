# 爬虫 3.0 的「无感集成」改造方案（v2）

> 2026-10-22 · 对象：`artex-ymh` 当前源码（已含 2026-10-22 内网资源可靠性改动）
> 前情：`docs/reviews/2026-10-22/crawler-web-enhancement.md`（v1）提出的是一个**独立的 `crawl/` 子系统 + 三个新工具**。
> 本文是对 v1 的**自我否定与重做**：那个设计方向错了——它把能力做成了「多一个要记得去触发的东西」。
> 本文给出的是**长在既有接缝上**的版本，并附带已落地的代码。

---

## 0. 我理解的「无感」是什么

先把两种做法的差别说死，后面的设计才有判据。

**反例（v1 的做法，不对）**

```
新增 crawl_start / crawl_status / crawl_candidates 三个工具
  → 进 toolcatalog seed → 进 worker 工具列表
  → planner 要知道什么时候调它 → 提示词里加一条「必须做采集」
  → skill 里加一节「什么时候用 crawl」
```

这不是能力提升，这是**给 agent 增加了一个新的动作要求**。代价是：

- 工具面变宽，worker 的注意力被稀释（本项目刻意收敛工具面，见 `worker.go` 注释）；
- 提示词里又多一条「必须」，而 `ROADMAP.md` §5 第一条就是 "Prompting is not enforcement"；
- 忘了调 = 能力为零。而「模型会不会记得」恰恰是最不可靠的一环；
- 出现第二套台账（crawl_runs / candidates），与「不另建互相矛盾的图」的设计原则冲突。

**正例（本次做法）**

```
能力挂在「已经在发生的事」上：
  agent 登记一个 service（本来就会做）
    → 引擎侧顺手抓一次（本来就有 enrich 的 HTTP 探测）
      → 把响应当材料读（本来只取 title）
        → 抽出的端点回灌资产图（本来就有 UpsertEndpoint）
          → planner 在 graph_overview 里自然看到「这个站点还没端点」
            → 自然派工
```

对 agent 来说：**工具列表没变、技能没变、提示词没变**。它照常干活，资产图里自己长出更多东西。

判据只有一条：

> **删掉这套改造，agent 的工作流不需要改一个字；装上它，同样的动作产出更多。**
> 如果一项改造需要 agent「知道」它的存在，那它就是额外触发，不是能力提升。

---

## 1. 重新分析：项目现状（这次真的去看了）

### 1.1 上一次分析里说错的地方

上一版报告我写「`enrich` 未接线（`SetEnrich` 无调用点）」。这次重查，**这句话是错的**：

```go
// agent/worker.go:494
tsx.SetEnrich(enr)   // async DNS/HTTP auto-completion for assets this worker writes
```

依赖注入已经完成：`server/manager.go:511` 构造 engine → `Worker.Execute(..., enr, ...)` → `ToolSet.enrich`。

但**真正的缺口比「未接线」更精确**：

```bash
$ grep -rn "ProbeSite(\|ResolveDomain(" --include=*.go . | grep -v _test
agent/tools.go:138:	ResolveDomain(id int64, host string)   # 接口声明
agent/tools.go:139:	ProbeSite(id int64, url string)        # 接口声明
enrich/enrich.go:123:func (e *Engine) ResolveDomain(...)  # 实现
enrich/enrich.go:126:func (e *Engine) ProbeSite(...)       # 实现
```

**全仓没有任何调用点。** 管道接好了，水龙头没人开。`enrich/enrich.go:8` 的注释还写着「本包当前未接线」——注释已过期，会误导后续维护。

这个精确诊断很重要：它意味着**不需要新建子系统，只需要在两个已经存在的调用点各加一行**。

### 1.2 这次引入的代码带来了什么

新增的是内网资源可靠性那一批（`session/capabilities.go`、`server/resource_access.go`、`tunnel/health.go`、`db/session_access.go`、`db/session_page.go` 等）与三份评审文档。对采集侧的影响是**确认了设计哲学**：

- 「先授权再读」「能力不授予权限」「台账不声明并发不可变」——这套克制同样适用于爬虫：**候选不等于可达，可达不等于已测**；
- 「不另建互相矛盾的图」——所以我不能新建 `crawl_candidates` 表；
- 「捕获状态是执行回执的一部分」——所以补全产生的资产必须能追溯到来源。

### 1.3 仍然成立的三处事实性缺口（本次已修）

| # | 缺口 | 证据 | 影响 |
|---|---|---|---|
| 1 | `sites_without_endpoints` 提示词承诺、代码不存在 | `agent/planner.go:312` 承诺；`graphOverviewData`（`tools.go`）无此键 | planner 长期基于空信号决策 |
| 2 | `ProbeSite`/`ResolveDomain` 无调用点 | 见上 | 补全能力等于不存在 |
| 3 | `enrich` 抓取 UA 自报 `artex-enrich/1.0` | `enrich.go:237`（改前） | 与 `uapool.go` 的指纹一致性策略直接冲突 |

### 1.4 仍然存在的结构性空白（本次未动，列出来是为了不假装做完）

- `harvest_static.py` 的运行时采集仍是单进程串行 `goto`，coverage 靠 agent 手工点；
- `guard/tarpit.go:65` 只统计 `WebFetch`，Bash 起的 katana/python 绕过预算；
- 语义层（LLM）本次只留了接口与预算，未接真实模型——理由见 §5。

---

## 2. 三个接缝

整个改造只做三件事，每件都挂在「已经发生的事」上。

```
┌─────────────────────────────────────────────────────────────────┐
│ S1 写入接缝（agent 侧，1 行）                                     │
│   insert_assets 落库 service / subdomain                          │
│     → t.enrich.ProbeSite(id, taskID, url)                         │
│     → t.enrich.ResolveDomain(id, taskID, host)                    │
│   对 agent 透明：它只是照常登记资产                                │
└──────────────────────────┬──────────────────────────────────────┘
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│ S2 材料接缝（引擎侧，确定性，无 AI）                               │
│   doHTTP: fetch → Classify → Extract → sink → follow(一跳)        │
│   材料类型：HTML / JS / JSON / sourcemap / OpenAPI                │
│   产出：method 已确定的端点、同源页面、脚本、map、服务基址          │
│   回灌：UpsertEndpoint + task_asset_links.source=enrich:<抽取器>   │
└──────────────────────────┬──────────────────────────────────────┘
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│ S3 语义接缝（受限 AI，默认关闭）                                   │
│   Trigger >= 阈值 且 语义预算未用尽 → JSAnalyzer.AnalyzeCandidates │
│   输入：EvidenceWindows 裁剪后的证据（非整个 bundle）              │
│   输出：候选 URL；仍要过同源 + method 已知 + 预算三关              │
└─────────────────────────────────────────────────────────────────┘
```

三层的共同点：**没有一层对外暴露新的调用面**。S1 是既有工具的副作用，S2 是既有引擎的升级，S3 是既有引擎里的可选一段。

---

## 3. 已落地的改动

### 3.1 新增 `enrich/material.go`（735 行）— 材料理解层

纯函数，不碰 DB、不发网络、不看时钟。输入 `(base, kind, body)`，输出结构体。

```go
func Classify(rawURL, contentType string, body []byte) Kind
func Extract(base *url.URL, kind Kind, body []byte) Material

type Material struct {
    Kind       Kind
    Endpoints  []Candidate   // method 已确定，可直接入图
    Pages      []string      // 同源页面，可跟一跳
    Scripts    []string      // 同源 JS
    SourceMaps []string      // 同源 .map
    Services   []string      // 配置/OpenAPI 恢复出的基址
    Trigger    int           // 确定性规则吃不下的证据强度
    Signals    []string      // dynamic-call / decode / chunk-runtime / ...
}
```

覆盖：

| 材料 | 抽取内容 | 关键决策 |
|---|---|---|
| HTML | `<a>` `<area>` `<iframe>` `<script src>` `<link modulepreload>` `<form>` + 内联 JS | 用 `golang.org/x/net/html` 真解析，不靠正则 |
| JS | `fetch` / `xhr.open` / `axios|ky|got.get|post|...` / `client.get|...` | 只产出 method 能确定的；`fetch(url,{method})` 不会再同时冒出 GET |
| JS 配置键 | `baseURL/apiBase/graphqlEndpoint/wsUrl/...` | 产出 Services，不产出 Endpoints |
| sourcemap | `sourceMappingURL` + `sourcesContent` | 从**原文**取（先剥注释就再也找不到）；还原源码再抽一遍 |
| JSON 配置 | 递归 key 语义匹配 | 键名自解释，这是文章 5.2 MVP 能成立的原因 |
| OpenAPI | `paths` × verb、`parameters`、`servers[].url` | 路径拼接而非 ResolveReference（否则 `/openapi.json` + `servers=/api` 会丢前缀） |

两个容易写错、已经踩过的点，在代码里留了注释：

1. `//# sourceMappingURL=` 本身就是行注释，**必须在 `stripJSComments` 之前**从原文提取；
2. `fetch("/x", {method:"POST"})` 会同时命中「带 init」和「字面量」两个正则，不处理就会**造出一条不存在的 GET**。

### 3.2 重写 `enrich/enrich.go`（688 行）— 抓取从「取 title」变成「读材料」

| 改前 | 改后 |
|---|---|
| `GET` → `LimitReader(1MiB)` → 只取 `<title>` | 先按 URL+CT 定型，再按类型决定读多少（JS 8MiB / map 32MiB / 文本 2MiB） |
| 单一 job | job 带 `taskID` / `depth`，支持一跳派生抓取 |
| 无预算 | 每任务 `MaxRequests/MaxBytes/PerHost/MaxDepth`，超限即停 |
| `artex-enrich/1.0` | `SetUserAgent`，server 侧接 `agent.PickUA`，与 worker 同一套 UA |
| 无去重 | 冷却（同资产 5 分钟）+ 跨深度 URL 去重（防环） |
| — | `SetJSAnalyzer` 语义层注入点 + 独立语义预算（每任务 6 次） |
| — | `EvidenceWindows`：以锚点为中心向语句边界扩，**不是固定字节切片** |

`sink()` 的两个刻意的克制：

- **不调 `AddAutoScope`**。派生端点是既有 service 的同源子路径，范围已由父资产确定；再扩范围会破坏「side-effect 派生不扩 scope」的既有约定（`tools_insert.go` 的原注释）。
- **不猜 method**。静态材料里 method 未知的路径直接丢弃，留给 S3。宁可少写一条，不可写错一条——一条 method 错的端点会污染后续所有测试。

来源可追溯但不新建字段：`task_asset_links.source` 本来就是自由文本，用 `enrich:js-fetch` / `enrich:config` / `enrich:openapi` / `enrich:ai` 区分来路。

### 3.3 `agent/tools_insert.go:373` — S1 接缝（11 行）

```go
// 引擎侧补全（对 agent 无感）：登记完资产就顺手解析/抓一次。
if t.enrich != nil && taskID > 0 {
    switch typ {
    case "root_domain", "subdomain":
        t.enrich.ResolveDomain(id, taskID, item.Domain)
    case "service":
        if item.URL != "" {
            t.enrich.ProbeSite(id, taskID, item.URL)
        }
    }
}
```

放在 `anchorOwner(id)` 之后、`SetTaskAssetSource` 之前：资产已落库、归属已确定，此时触发不会读到半成品。没有 `taskID` 时不触发——无法记账的请求不做。

### 3.4 `agent/tools.go:141` — `EnrichTrigger` 接口加 taskID

```go
type EnrichTrigger interface {
    ResolveDomain(id int64, taskID int64, host string)
    ProbeSite(id int64, taskID int64, url string)
}
```

为什么必须带 taskID：补全是**每任务**的资源消耗（有请求/字节预算），派生产物也要挂回该任务的 `task_asset_links`。全仓只有 `enrich.Engine` 一个实现，改动面为零。

### 3.5 `agent/tools.go:609` + `db/task_web_surface.go`（64 行）— 补上不存在的信号

```go
out["sites_without_endpoints"] = map[string]any{"total": N, "sample": [...], "note": "..."}
```

查询语义：任务范围内 `type='service'` 且同域无任何 `endpoint` 的站点数。

- 用 `domain` 对齐而不是 URL 前缀：`/api/v1/x` 与 `/api/v2/x` 前缀匹配会漏；
- **不受 `coverageDisabled` 影响**：它描述的是资产结构（这个站点还没被展开），不是测试覆盖度（这个资产还没被验证）。覆盖度开关关掉只是不算百分比，不代表「没端点」这件事消失。

这是把 S2 的产出**变成派工信号**的那一环：补全越多，这个数字越准；planner 看到它，自然派「去把某个站点展开」的意图——不需要任何人教它「现在该爬虫了」。

### 3.6 `skills/api-recon/scripts/harvest_static.py` — 原地升级（468 行）

**命令不变**：`harvest_static.py <BASE_URL> <OUTDIR>`。agent 的调用方式、skill 里的流程、`wc -l` 校验点全部不变。

变的是它内部：

| 能力 | 改前 | 改后 |
|---|---|---|
| `.json` / `.map` | 被 `ASSET_EXT` **直接过滤掉** | 分类处理，进 `materials/` |
| sourcemap | 完全无 | 解析 `sourceMappingURL` → 下载 → `sourcesContent` 还原源码 → 再抽一遍 |
| 配置文件 | 无 | 递归抽基址 → `configs.txt` |
| robots/sitemap | 无 | 作为种子（零成本，通常白捡一批页面） |
| UA | `"Mozilla/5.0 (spa-api-recon)"` | 读 `ARTEX_UA`（worker 的 BashEnv 已注入） |
| 并发 | 20–24 无间隔 | 默认 8，`ARTEX_HARVEST_WORKERS` 可调 |
| method 抽取 | 无 | `api_methods.txt`（`METHOD /path`，可直接入图） |
| 注释噪声 | 会把注释里的示例路径当接口 | `strip_comments` 先剥，但保留字符串内容 |

新增产出（旧产出一个不少）：`api_methods.txt`、`configs.txt`、`sourcemaps.txt`、`sm_apis.txt`、`materials/`。

这就是「能力提升」的字面意义：**同一句话，干得更多**。

---

## 4. 为什么这不是「另一套台账」

| 担心 | 本次做法 |
|---|---|
| 新建候选表 → 第二套真值 | 没有新表。候选直接进 `assets`（`type=endpoint`），来源写 `task_asset_links.source` |
| 候选污染「已发现」 | 只写 method 已确定的；method 未知的不写（不是写个 unknown 占位） |
| 派生资产偷偷扩范围 | 明确不调 `AddAutoScope`；范围仍由父 service 决定 |
| 绕过 scope | `sameOrigin` 在抽取层就丢跨域；`resolveSameOrigin` 是唯一出口 |
| 绕过预算 | 每任务请求/字节/主机/深度四重上限，语义层另有 6 次独立预算 |
| 无限爬 | 深度默认 1；单材料派生上限（12 脚本 / 8 页面 / 4 map / 4 基址）；跨深度 URL 去重 |

---

## 5. 语义层为什么只留接口、不接模型

S3 的接口、预算、证据裁剪都写好了（`JSAnalyzer` / `takeSemantic` / `EvidenceWindows`），但默认 `nil`。

理由不是「没时间」，是**不想重复这个项目已经犯过的错**：`enrich` 的注释写着「已实现」，实际两年没人调——一个「看起来接好了」的半成品比没有更危险。

接模型需要三件东西，缺一不可：

1. **成本归属**：语义调用要记进哪个任务的 token 账？现在 `llm_usage` 是按 run 记的，补全的调用不在任何 run 里；
2. **失败语义**：模型超时/幻觉时，补全应该静默跳过还是记 fact？现在没有答案；
3. **评测基线**：没有闭集真值，接上去也无法判断「多了 5 条候选」是收益还是噪声。

这三件都属于「要有数据才能定」的事，所以先把确定性层跑出数据（§7），再决定 S3 的参数。这也符合文章本身的立场：AI 只处理确定性规则吃不下的部分——**先把确定性规则做满，才知道剩下的是什么**。

---

## 6. 与既有评审的关系

| 评审条目 | 本次如何对齐 |
|---|---|
| `web-review.md` W4（来源分轴） | `task_asset_links.source = enrich:<抽取器>`；method 未知不写，不猜 GET |
| `web-review.md` W5（测试账本） | 未做。本次只补「发现面」，测试账本属另一条线，不混做 |
| `web-review.md` W7（捕获退化可见） | 补全抓取走同一个 MITM，退化状态由既有 traffic 通道负责，未新增 |
| `web-final-review.md` 第 6 条（跨任务隔离） | `doHTTP` 按 `taskID` 计预算、按 `taskID` 写 `task_ids`；不读全局流量 |
| `web-final-review.md` 第 7 条（逐跳检查 scope） | `follow()` 每条派生 URL 都过 `markSeen` + 抽取层的 `sameOrigin` |
| `ROADMAP.md` §5（三条纪律） | 预算在代码里（`takeBudget`），不在提示词里 |

---

## 7. 验证

已执行（本地，Go 1.27.1）：

```
go build ./...                                              → 0
go test ./enrich/ ./agent/ ./db/ ./server/ -count=1          → 4/4 ok
```

`enrich` 新增 11 个用例，覆盖文章里的关键场景：

- `TestJSObfuscatedTriggersSemanticLayer` — 文章 5.3 的 `__ccRead__` 索引表 + `atob` 样例：**不产出猜测端点**，且 `Trigger >= 阈值`、signals 含 `decode` + `dynamic-call`；
- `TestJSPlainFileDoesNotTrigger` — 全字面量的普通文件 `Trigger == 0`（成本闸门第一道）；
- `TestJSDeterministicEndpoints` — 六种调用点全部命中，且**断言 `fetch(url,{method:'post'})` 不产生多余 GET**；
- `TestOpenAPIPaths` — `/openapi.json` + `servers=/api` → `/api/users`（前缀不丢）；
- `TestJSONConfigRecovery` — `api_base` / 嵌套 `baseUrl` 恢复，`version:"1.2.3"` 不被当 URL；
- `TestSourceMapSourcesContent` — 从 `sourcesContent` 里抽出 `/api/from-map`；
- `TestEvidenceWindowsBoundedAndAligned` — 超限脚本里窗口 ≤ 8KiB 且锚点行不丢；
- `TestJSCrossOriginDropped` / `TestHTMLFormAndLinks` / `TestStripJSCommentsKeepsStrings` / `TestClassify`。

Python 侧对 `harvest_static.py` 的纯函数做了等价冒烟（method 抽取、配置恢复、sourcemap 解包、注释剥离）。

**未验证**：真实目标端到端（没有靶场跑），语义层（未接），`PostgreSQL` 场景下的 `SitesWithoutEndpoints`（本次跑的是无 DB 的包级测试）。这三项应在下一步补。

---

## 8. 下一步（按「先出数据、再谈调参」排序）

1. **跑一次真实站点**，看 `graph_overview` 的 `sites_without_endpoints` 与 endpoint 增量是否如预期；用 `traffic_search` 核对补全请求是否都落在流量仓里。
2. **修 tarpit 的覆盖面**（`guard/tarpit.go:65`）：补全现在会发请求，而 tarpit 只数 `WebFetch`——补全成了预算外通道。要么让 enrich 自己回报计数，要么把 Bash 里的抓取类二进制纳入统计。这是本次改动**引入的新风险**，优先级最高。
3. **给 `SitesWithoutEndpoints` 加索引**（`assets(type, domain)` 部分索引），大范围任务下这个查询会扫全表。
4. **S3 的前置三件**（成本归属 / 失败语义 / 评测基线）齐了再接模型。
5. `harvest_static.py` 的 coverage 模式（自动点 Tab/弹窗）仍靠 agent 手工——这是下一个「原地升级」候选，不是新工具。

---

## 9. 一句话总结

> 上一版我在造一个爬虫；这一版我在**把爬虫拆成三件长在既有流程上的小事**：
> 登记资产时顺手抓一次、抓回来当材料读、读出来的端点回灌资产图。
> agent 的工作流一个字没改，资产图里的东西变多了。
