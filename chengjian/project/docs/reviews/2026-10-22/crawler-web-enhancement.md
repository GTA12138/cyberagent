# 爬虫与 Web 攻击面能力增强方案

> ⚠️ **本文的架构主张（独立 `crawl/` 子系统 + 三个新工具）已被 `crawler-web-integration-v2.md` 取代。**
> 保留本文是因为它的**现状盘点与材料缺口**（§2、§3 的方向 B/C/E）仍然有效，那部分结论已并入 v2 并落地。
> 但「新建一个独立爬虫子系统」这个方向是错的——它把能力做成了「多一个要记得触发的东西」。
> 读架构请直接看 v2。

> 2026-10-22 · 阅读对象：`artex-ymh` 二开版（基于上游 v0.3.10，跟进至 0.3.14 精选修复）
> 输入材料：Yak Project《如何使用 AI 辅助构建最合适渗透的爬虫系统？》（2026-10-16，爬虫 1.0/2.0/3.0 范式）
> 方法：纯源码静态阅读（`E:/cyberagent/artex-ymh-main/artex-ymh-main`），未运行、未扫描任何目标。
> 与既有评审的关系：`docs/reviews/2026-10-21/web-review.md` 的 W1–W7 已覆盖"流量/身份/证据/账本"侧；**本文只谈采集侧（爬虫与材料理解）**，不重复 W1–W7 的结论，只在与采集交界处引用。

---

## 0. 结论先行

**一句话判断：本项目的瓶颈不在"知道怎么挖"，而在"没有一层确定性的执行器"。**

`skills/` 里的知识密度极高——sourcemap 还原、sitemap/robots、OpenAPI 泄露、参数逆向、WS 协议分析、历史 JS diff，`chains-recon/refs/` 和 `api-recon/SKILL.md` 全都写了。但这些知识全部以**散文 + 参考脚本**形式存在，运行时唯一的执行力是「LLM 读 grep 结果 + 手工调 `insert_assets`」。

这恰好是文章那句核心洞察的镜像：

> 此阶段限制来自"对材料语义的理解"，而不仅是"资源获取能力"。

对 ARTEX 而言要再加一句：**限制也来自"没有把理解固化成代码"。** 知识在 skill 里，能力却随每次 LLM 调用的运气波动。

所以增强路线不是"给 agent 写更多提示词"，而是把爬虫**从 skill 里的脚本提升为平台原生工具**，再把 AI 作为**受限语义层**叠在上面。三条杠杆按 ROI 排序：

| 序 | 杠杆 | 为什么 ROI 最高 |
|---|---|---|
| 1 | 爬虫平台化（Go 包 + 工具 + 预算 + 回灌） | 一次解决四个问题：可审计、可预算、可去重、可回灌资产图。现在这四件事全部靠模型自觉 |
| 2 | 补齐确定性材料面（sourcemap / runtime-config / sitemap / OpenAPI） | **零 AI 成本**，命中即得 method+path+params，是"更多接口"的最短路径 |
| 3 | 受限 AI 语义层（文章 3.0） | 只处理 1、2 抽不出的拼接/编码/未执行 chunk；投入大、收益真实但边际 |

**不建议**做的事写在 §6，其中最重要的一条：不要再往提示词里加"必须做"。`ROADMAP.md` §5 自己写了 "Prompting is not enforcement"，爬虫侧正在违反这条自家纪律。

---

## 1. 文章要点提炼（只保留可落地的部分）

### 1.1 三代模型与边界

| 代 | 观测对象 | 抽取来源 | 边界 |
|---|---|---|---|
| 1.0 | 静态 HTML | `a[href]`、`form[action]`，按深度递归 | 不执行 JS，看不到运行时请求 |
| 2.0 | 浏览器执行后的状态 | 渲染后 DOM、路由、XHR/Fetch/WS、Cookie/Storage | 看不到"未执行代码"和"非运行时材料" |
| 3.0 | **已获得的材料** | Runtime Config、Asset Manifest、Source Map、OpenAPI、混淆 JS | 输出只是**候选**，必须经确定性校验 |

2.0 的四类硬边界，就是 3.0 的切入点：

1. **非标准位置的资源描述**：`routes.json`、runtime config、asset manifest、sourcemap——浏览器下载但不解析；
2. **未执行/难执行的 JS**：未加载 chunk、未点击分支、异常分支、历史代码、混淆表达式；
3. **需专门解析的协议**：Service Worker、GraphQL、SSE、Socket.IO、泄露的 OpenAPI；
4. **依赖外部状态的业务流程**：登录、二次核身、扫码——需浏览器之外的状态参与。

### 1.2 两条工程约束（比结论更重要）

- **AI 的定位是"受约束的语义分析组件"**，不是调度器。它回答三个问题：材料里有哪些技术线索 / 哪些高价值线索还没展开 / 下一步该访问什么。它**不发起请求、不扩 scope、不直接写资产**。
- **两类线索要分开处理**：`fetch("/api/v1/orders")` 这种确定性可解析的不该进模型；`fetch(apiBase + route)`、`fetch(buildURL(t,e))`、`atob(索引表)` 这种才进模型。

### 1.3 实验数据（作为方法参照，不作为效果承诺）

ColdChain 供应商 SPA，25 个 URL 闭集真值，各跑 3 次取中位数：

| 模式 | 候选 | 命中真值 | 覆盖率 | 中位耗时 |
|---|---|---|---|---|
| Katana 普通 | 3 | 3 | 12% | 11.06s |
| Katana Headless | 18 | 16 | 64% | 32.06s |
| 受限 AI 静态分析 | 48 | 21 | **84%** | 24.42s |

多出的 5 个真值主要来自**当前页面未执行的构建分支**（动态 chunk、sourcemap、Worker 及其后续接口）。注意这是**单一 SPA 靶场**的结果，不能外推成通用收益。

---

## 2. 现状盘点（带证据）

### 2.1 采集执行层

| 能力 | 现状 | 证据 | 与文章对照的差距 |
|---|---|---|---|
| 静态 endpoint 抽取 | 单文件正则 + 白名单 | `skills/api-recon/scripts/harvest_static.py:87-103` | 只有一类线索（确定性解析），**完全没有第二类（拼接/编码）** |
| 材料过滤 | `ASSET_EXT` 把 `.json`/`.map` **直接丢弃** | `harvest_static.py:88,100,109` | 与文章"边界 1：非标准位置的资源描述"**方向相反**——把最该分析的材料过滤掉了 |
| chunk 枚举 | 只认 webpack `}[x]+".js"` 与 Vite `assets/*.js` | `harvest_static.py:51-84` | 不认 `__webpack_require__.u()` 调用、不认 RSC/Next manifest、不认微前端多 runtime 的 `publicPath` 变体 |
| 下载器 | `dl()` 只接受 `.js` 结尾 | `harvest_static.py:136-138` | `.map`、`.json`、`.yaml` 永不入库 |
| sourcemap | **全仓 0 处代码处理** | `grep sourceMappingURL` 在 `skills/` 只命中 `.md` 文档 | 知识有、执行无。这是当前性价比最高的空白 |
| MPA 爬取 | 单线程 BFS，只跟 `<a>` | `skills/api-recon/scripts/spider_mpa.py:83-107` | 不读 `robots.txt` / `sitemap.xml`（零成本种子） |
| 运行时采集 | puppeteer 串行 `goto` 每个路由 | `runtime_harvest.js:201-207` | 无自动点击枚举（coverage 靠 agent 手工点）；WS 只存帧不解析 |
| 反指纹 | UA 硬编码 `"Mozilla/5.0 (spa-api-recon)"` | `harvest_static.py:23`、`spider_mpa.py:22` | 项目有 `agent/uapool.go` 的按任务稳定 UA，脚本完全没接 |
| 并发/礼貌 | 20–24 线程无间隔 | `harvest_static.py:145,163` | 对 WAF 极其显眼；与 `guard/tarpit.go` 的礼貌设计相矛盾 |
| 抓取预算 | tarpit 只统计 `WebFetch` | `guard/tarpit.go:63-66`（`if ev.ToolName != "WebFetch" { return }`） | **katana / python 脚本走 Bash，预算完全绕过**——一个 5MiB bundle 站点可无限抓 |

### 2.2 候选 → 资产的回灌层

| 能力 | 现状 | 证据 |
|---|---|---|
| endpoint 资产结构 | 只有 `url + method + params[]` | `db/assets.go:52-54` |
| 写入路径 | 只有 `insert_assets` 工具 / HTTP API / session 三处 | `agent/tools_insert.go:346`、`server/assets.go:535`、`server/session.go:364` |
| 来源标记 | **无** `discovery_source` / `state` / `evidence_ref` | `Asset` 结构体无对应字段（`extra` 可用但无约定） |
| 从流量派生端点 | **无任何代码** | `UpsertEndpoint` 的三个调用点均不消费 traffic |
| 覆盖度驱动 | planner 提示词承诺 `sites_without_endpoints` | `agent/planner.go:312` 承诺；`agent/tools.go:366-613` 的 `graphOverviewData` **根本没有这个键**（`out["..."]` 全集无此项）——提示词在描述一个不存在的数据 |
| 自动补全 | `enrich/` 设计完成但**未接线** | `enrich/enrich.go:1-14` 自述："本包当前未接线——`SetEnrich` 在全仓没有任何调用点" |

### 2.3 本项目被低估的存量资产

**`traffic` 子系统已经是一台被动爬虫，只是没人读它。**

```sql
-- traffic/traffic.go:45-60
CREATE TABLE exchanges (id, ts, host, method, url_template, url, status, content_type, req_len, resp_len, path);
CREATE INDEX idx_ex_tmpl ON exchanges(host, url_template);
```

- 每个 worker 的 Bash/WebFetch 都走任务 MITM（`agent/worker.go:473-475`），所以**所有爬虫流量（含 katana）本来就落在这张表里**；
- 表里已经有 `method + url_template + status + content_type`——这正是 endpoint 资产需要的四要素；
- `content_type LIKE '%javascript%'` 的行就是 JS 材料池，**不需要额外发一个包**。

这是文章所说"已获得但未被理解的材料"的最佳现实例证。当前系统拿到了材料，却没有 consumer。

---

## 3. 六个改造方向

### 方向 A｜爬虫平台化：`crawl/` 原生子系统（P1，最高优先）

**目标**：把"爬虫"从"LLM 记得去跑某个脚本"变成"平台提供的一个带预算、带 scope、带回灌的工具"。

**新增包 `crawl/`（与 `recon/`、`enrich/` 平级）**

```
crawl/
  frontier.go    URL 前沿：去重键 = scheme+host+port+normPath+queryKeySet；按 host 令牌桶；优先级（api 前缀 > 参数化路径 > 普通页面）
  fetch.go       统一 fetcher：走任务 MITM 代理、UA 取 uapool.PickUA、每 host 限速、robots 开关、tarpit 计数
  material.go    材料分类：html/js/map/json/yaml/wasm/text；落盘 data/crawl/<task>/<crawl_id>/materials/
  extract_html.go  a/form/iframe/script src/inline-js URL
  extract_js.go    确定性第一类线索（见 §3.2）
  extract_config.go JSON/YAML 递归 URL 抽取（key 语义匹配）
  sink.go        候选 → 资产图（带 discovery_source，默认 state=candidate）
  passive.go     从 traffic 派生（见方向 C）
  budget.go      轮数/字节/请求数/时长/成本计数器（硬编码，非提示词）
```

**三个工具**（seed 进 `agent/toolcatalog.go:33-55`，绑定 `worker` / `pentest`）

| 工具 | 语义 | 关键约束 |
|---|---|---|
| `crawl_start(target, profile, budget)` | 启动一次采集，返回 `crawl_id` | profile ∈ `static` / `browser` / `both` / `passive`；budget 硬上限 |
| `crawl_status(crawl_id)` | 进度 + 材料统计 + 高价值候选 top-N | 只读 |
| `crawl_candidates(crawl_id, kind, limit)` | 候选列表（endpoint/route/js/secret/config） | 分页，默认只给 `state=candidate` |

**为什么不做成"一个自动跑完的大工具"**：worker 是单意图执行体（`agent/worker.go`），爬虫需要跨意图存活。所以 `crawl_start` 应该把状态放在 DB（新表 `crawl_runs` / `crawl_candidates`），而不是进程内存——否则 worker 一结束爬虫就断。这也是 `guard/tarpit.go` 的注释里已经意识到的同类问题（"计数器在内存里…任务重启清零"）。

**验收**：同一 target 连续跑两次，第二次候选集合应显著收敛（去重有效）；`crawl_start` 后 `traffic_search` 能看到它的请求；超预算时返回结构化 `budget_exhausted` 而非静默截断。

---

### 方向 B｜材料面补齐：四个"零 AI"高价值缺口（P0，立竿见影）

#### B1 sourcemap（最高性价比）

现状：`harvest_static.py` 的 `dl()` 直接 `if not fn.endswith(".js"): return None`（`:137`），`.map` 永不下载。

改造点：

1. 在 `harvest_static.py` 增加 `extract_sourcemap_refs(js_text)`：匹配 `//# sourceMappingURL=`、`/*# sourceMappingURL= */`、`X-SourceMap` 响应头；
2. `dl()` 放宽为白名单 `{.js,.mjs,.map,.json,.yaml,.yml}`，`.map` 单文件设上限（建议 64MB，流式写盘）；
3. 新增 `unpack_sourcemap.py`：解析 `sources[]` / `sourcesContent[]`，还原文件树到 `sourcemap/`；对还原后的**未压缩源码**再跑一遍 endpoint/route 抽取——命中率远高于 bundle；
4. 顺带产出高价值情报：`sources` 里的 `/Users/<name>/`、`node_modules/` 依赖树（→ 版本 → CVE 匹配）、`webpack://` 原始路径、注释里的 TODO/测试账号。

产物：`sourcemap/`、`sm_apis.txt`、`sm_routes.txt`、`sm_leaks.json`。

**为什么这条排第一**：一次命中 = 整套前端源码 + 完整路由表 + 依赖树。这是"拿到更多 web 信息"的单点最大收益，且完全不需要 AI。

#### B2 runtime config / manifest / 全局状态

`ASSET_EXT` 现在把 `.json` 过滤掉（`:88,100`），等于主动屏蔽了文章 5.2 的整个 MVP 场景。

改造：新增 `extract_config.py`

- 从 bundle 字符串里定位：`runtime-config.json`、`asset-manifest.json`、`routes.json`、`manifest.json`、`__NEXT_DATA__`、`__INITIAL_STATE__`、`__NUXT__`、`__APOLLO_STATE__`；
- 下载并解析 JSON/YAML，**递归**遍历，对 key 做语义匹配（`base|api|endpoint|host|url|gateway|ws|socket|graphql|service`），value 形如 URL/路径即收；
- 同时把"看起来像配置"的内联 JSON（HTML 里的 `<script type="application/json">`）一并处理。

**注意**：这一步**先用确定性 key 匹配就能覆盖文章 5.2 实验的 70%**。文章用了 LLM FunctionCall 才恢复 10 个 URL——但那个 JSON 的字段名（`api_base`、`route_catalog`、`service_worker`）本身已经高度自解释。把确定性部分先吃掉，AI 只处理 key 名无意义的场景（如 `a1`、`b2`），成本和失败面都小得多。

#### B3 robots / sitemap / well-known

`spider_mpa.py` 现在是裸 BFS。加 20 行：先取 `/robots.txt` 的 `Sitemap:` 指令，再递归 `/sitemap.xml`（含 index），把 URL 作为**深度 0 种子**入队；顺带 `/.well-known/security.txt`。零成本、零风险、几乎必中。

#### B4 OpenAPI / GraphQL schema

这是**质量最高**的端点来源——直接给出 method + path + 参数 + 类型，不需要任何推断。

- 路径字典（10 条以内足够）：`/openapi.json`、`/swagger.json`、`/v2/api-docs`、`/v3/api-docs`、`/swagger/v1/swagger.json`、`/api-docs`、`/graphql`、`/api/graphql`；
- 命中 OpenAPI → 解析 `paths` → 直接 `UpsertEndpoint`（带 `discovery_source=openapi`），参数进 `params[]`（`db/assets.go` 的 `ParamItem` 已有 `Location/Name/Type`，模型现成）；
- 命中 GraphQL → 试 introspection（`__schema`）→ 产出 operation/字段清单；失败则从前端 JS 抽 gql 模板字符串。

注意：探测本身要过 RoE（`guard.checkRoE`），且应作为 `crawl` 的一个 profile 而非独立扫描器，避免又出现一条不受预算约束的路径。

#### B5 构建残留与第三方库版本

- 备份残留：`app.js.bak`、`.old`、`.swp`、`/static/js/` 目录索引；
- 第三方库指纹：从 JS 文件头注释（`/*! jQuery v1.12.4 */`）、`package.json`（若泄露）、sourcemap 的 `node_modules` 路径提取库名+版本 → 直接对接 CVE 库。

这条是"从信息直接变漏洞"的少数捷径之一，且工程极轻（正则 + 一张表）。

---

### 方向 C｜流量驱动被动发现（P1，本项目独有优势）

**核心思路**：平台已经录下了所有流量，让它变成爬虫的免费传感器。

改造点：

1. `traffic` 包新增只读查询：

```go
// traffic/passive.go（新增）
type EndpointCandidate struct {
    Host, Method, URLTemplate, ContentType string
    Status int
    Hits   int
    FirstSeen, LastSeen int64
}
func (t *Traffic) DistinctEndpoints(since int64, hosts []string, limit int) ([]EndpointCandidate, error)
func (t *Traffic) JSAssets(since int64) ([]string, error) // content_type LIKE '%javascript%'
```

2. `crawl/passive.go` 消费：`DistinctEndpoints` → `sink.go` 入图（`discovery_source=traffic`，`state=reachable`——因为**真的发出去过**，这是比静态候选更强的证据等级）；
3. `JSAssets` → 材料池 → 走方向 B 的抽取器（**不额外发包**，零成本拿到 JS 分析面）；
4. 响应内子 URL 抽取：`preload.js` 已有 `extractUrlsFromResponse`（`skills/api-recon/scripts/preload.js`），Go 侧没有对应实现，需要补。

**为什么这条特别值**：它天然带 `method + status + auth 上下文 + 时间`，是文章里"候选 → 在线请求验证"那一步的**已完成态**。别人要发一次请求才知道接口存在，你们查一次 SQLite 就知道。

**约束**（沿用 `web-review.md` 第 6 条）：跨任务隔离。`DistinctEndpoints` 必须按 task 的 store 查，不能读全局。

---

### 方向 D｜受限 AI 语义层（P2，按文章落地但更保守）

#### D1 触发器（确定性，先跑起来）

照搬文章 5.5 的三组锚点，用 Go RE2 实现（文章正则无反向引用，RE2 可直接用）：

- **A 组 网络请求锚点**：`\bfetch\s*\(`、`\.\s*open\s*\(`、`\b(?:axios|ky|got)\s*(?:\.\s*(?:get|post|...))?\s*\(`、`\bnew\s+(?:XMLHttpRequest|Request|WebSocket|EventSource|Worker)\b`、`sendBeacon`、`serviceWorker\.register`、`importScripts`；
- **B 组 配置/路由锚点**：`baseURL|baseUrl|apiBase|apiHost|endpoint|graphqlEndpoint|wsUrl|socketUrl`、`createBrowserRouter|createHashRouter|useRoutes|createRouter`；
- **C 组 构建/编码锚点**：`__webpack_require__|__vite__mapDeps|webpackChunk`、`sourceMappingURL=`、`asset-manifest\.json|manifest\.json|routes\.json`、`atob|decodeURIComponent|unescape|String\.fromCharCode|charCodeAt|TextDecoder`、`eval|Function\s*\(`、`(?:\\x[0-9A-Fa-f]{2}){8,}`、`[A-Za-z0-9+/]{100,}={0,2}`。

**触发判据**（关键，决定成本）：不能"命中就送模型"。建议打分：

| 信号 | 分 |
|---|---|
| 命中 A 组，且实参是字面量（`fetch("/api/x")`） | 0（确定性抽取器已处理） |
| 命中 A 组，且实参含 `+` / 变量 / 函数调用 | +3 |
| 命中 C 组编码函数 | +2 |
| 命中 C 组 chunk 运行时 | +1 |
| 文件是入口 runtime / manifest | +2 |
| 文件体积 > 1MB 且命中密度 < 1/100KB | −2 |

阈值（建议 4 分）应可配，默认值在本地靶场上标定，不要拍脑袋。

#### D2 窗口裁剪（不要固定字节切片）

文章明确批评了固定切片（跨切片丢失数据依赖）。实现要点：

- 以命中位置为中心，**向语句边界扩展**（向前找最近的 `;` `{` `}` 或换行，向后同理），而不是 `text[i-2048:i+2048]`；
- 若命中实参引用标识符，**沿定义点再取一个窗口**（一跳，不递归），拼成多段证据；
- 每份材料的窗口数、单窗字节数、总字节数三个上限都要硬编码。

#### D3 求值：先用窄 DSL，不要一上来引 JS 引擎

文章用了"无网络无文件权限的 JS 沙箱"。在 Go 里这需要 `goja` 之类的新依赖，而 `go.mod` 现在连浏览器驱动都没有（无 `go-rod`/`chromedp`），引入完整 JS 引擎的维护成本不低。

**建议分两步**：

1. **窄 DSL（P2 先做，覆盖 80% 场景）**：识别并静态求值这几类模式——
   - `atob("...")` → base64 解码；
   - `String.fromCharCode(97,98,...)` / `charCodeAt` 循环；
   - 数组索引表：`const T=["a","b"]; T[3]+T[1]`；
   - 简单字符串拼接与模板字面量；
   - `decodeURIComponent` / `unescape`。
   这些都是纯函数，用一个 200 行的求值器就能覆盖，无需执行任意 JS，**天然无副作用**。

2. **goja 沙箱（P2 可选）**：只有当窄 DSL 在实测中漏报显著时再引入。届时约束：无 `require`、无 `fetch`、无 `fs`、执行超时 2s、内存上限、只允许返回字符串。

#### D4 回灌（AI 的边界）

- 模型只输出候选 URL 字符串列表，**不带 method、不带 body**（避免模型幻觉出"看起来合理"的请求）；
- 候选必须过 `config.CheckShouldBeHandledURL` 同级校验（域名/路径/scope）；
- 候选入 `crawl_candidates`（`state=candidate`），**不直接写 assets**；
- 是否真的请求，由 `frontier` 的预算和 RoE 决定，不由模型决定。

这条与文章"AI 扩大候选发现面，没有绕过作用域或任意发起请求的权限"完全一致，也与本项目 `ROADMAP.md` §5 的"三条纪律"一致。

---

### 方向 E｜候选 → 验证账本（P1，与 W4/W5 对齐）

**不要另起一套**——`web-review.md` 的 W4（来源分轴）和 W5（测试账本）已经定义了语义，这里只补"采集侧要往那个模型里灌什么"。

最小改动：

1. `db.Asset` 增加（或用 `extra` 并写死约定）：
   - `discovery_source`：`static` / `sourcemap` / `config` / `openapi` / `traffic` / `browser` / `ai`；
   - `state`：`candidate` / `reachable` / `observed` / `tested`；
   - `evidence_ref`：指向 traffic exchange 或材料文件；
   - `first_seen`。
2. 静态候选 method 未知时**保持空**，不要自动补 GET（`web-review.md` W4 的明确要求）；
3. 新增只读工具 `list_endpoints(state, host, limit)`，让 planner 能看到"这个站点有 300 个未测端点"——这是把爬虫产出转成**派工信号**的关键；
4. **修 `sites_without_endpoints`**：`agent/planner.go:312` 承诺了这个键，`graphOverviewData` 里不存在。实现约 30 行（`type='service'` 且无子 endpoint 的站点计数）。这是当前提示词与代码的**事实性不一致**，会让 planner 基于不存在的信号做决策。

---

### 方向 F｜反指纹与预算统一（P0/P1）

| 问题 | 现状 | 改法 |
|---|---|---|
| UA 指纹 | 脚本硬编码 `spa-api-recon` | 读 `ARTEX_UA`（worker 已注入，见 `agent/uapool.go` 注释）；无则用 `PickUA(pool, taskID)` |
| 并发指纹 | 20–24 线程无间隔 | 每 host 令牌桶，默认 2 并发 + 200ms 间隔，可配 |
| 预算绕过 | tarpit 只看 `WebFetch`（`guard/tarpit.go:65`） | ① `crawl` 工具自带上限并回报；② `guard` 对 Bash 里的抓取类二进制（katana/httpx/ffuf/gobuster/dirsearch）按命令行里的 host 计数；③ 两者共用同一 hint 通道 |
| TLS | `NODE_TLS_REJECT_UNAUTHORIZED=0` 全局 | 限定到目标域，或走 MITM CA（本来就有 `traffic/ca.go`） |
| 礼貌 | 无 robots 尊重 | `crawl` 提供 `--respect-robots`，默认开；渗透场景可显式关并留审计 |

**注意**：`guard/tarpit.go` 的预算默认 40 次/主机（`:24`）。这个值对 SPA（几十上百个 chunk）明显偏低——一旦把 Bash 纳入统计，正常爬取会立刻熔断。所以改法②必须同时把预算改成**按 profile 区分**（静态材料抓取 vs 业务请求），否则会误伤。

---

## 4. 修改清单

### P0（1 周内，纯确定性，可独立交付）

| # | 动作 | 文件 | 验收 |
|---|---|---|---|
| 1 | `ASSET_EXT` 不再丢弃 `.json/.map/.yaml`，改为分类器 | `skills/api-recon/scripts/harvest_static.py:88,100,109` | `runtime-config.json` 能被收进 `paths_other.txt` 而非消失 |
| 2 | 解析 `sourceMappingURL` 并下载 `.map` | `harvest_static.py` 新增 `extract_sourcemap_refs` + 放宽 `dl()` | 靶场站点能下到 map 并还原出 `sourcesContent` |
| 3 | 新增 `unpack_sourcemap.py` | `skills/api-recon/scripts/` | 还原文件树；对还原源码重跑抽取，`sm_apis.txt` 非空 |
| 4 | 新增 `extract_config.py` | 同上 | 从 runtime-config 确定性恢复 ≥ 文章 MVP 的 URL 数 |
| 5 | UA 改读 `ARTEX_UA`，并发改可配 | `harvest_static.py:23,145,163`、`spider_mpa.py:22` | 抓包看 UA 与任务 UA 一致；并发参数生效 |
| 6 | `robots.txt` / `sitemap.xml` 种子 | `spider_mpa.py` | 靶场 sitemap 里的 URL 全部进 `links.txt` |
| 7 | OpenAPI/GraphQL 路径字典 + 解析 | 新增 `probe_docs.py` | 命中 swagger 时产出带 method 的端点列表 |
| 8 | 修 `sites_without_endpoints` | `agent/tools.go:366-613` | planner 提示里的键在 `graph_overview` 中真实存在 |

### P1（2–3 周，平台化）

| # | 动作 | 文件 | 验收 |
|---|---|---|---|
| 9 | 新增 `crawl/` 包（frontier/fetch/material/extract_*/sink/budget） | 新目录 | 单元测试：去重键、令牌桶、预算耗尽 |
| 10 | `crawl_start` / `crawl_status` / `crawl_candidates` 三工具 | `agent/tools_insert.go` 或新 `tools_crawl.go` + `agent/toolcatalog.go:33-55` | 工具可见于 worker；seed 进 tools 表 |
| 11 | 新表 `crawl_runs` / `crawl_candidates` | `db/schema.sql` + 迁移 | 重启后爬虫状态不丢 |
| 12 | traffic 被动端点查询 | `traffic/passive.go`（新） | 一次 `crawl_start(profile=passive)` 能产出端点候选 |
| 13 | 流量里的 JS 进材料池 | `crawl/passive.go` | `content_type=javascript` 的响应被分析，且**没有新增请求** |
| 14 | endpoint 资产加 `discovery_source/state/evidence_ref` | `db/assets.go:52-54`、`UpsertEndpoint` | 静态候选 `method` 为空且 `state=candidate` |
| 15 | `list_endpoints(state, host)` 工具 | `agent/tools_insert.go` | planner 能看到未测端点计数 |
| 16 | `enrich` 接线（DNS/HTTP 补全） | `enrich/enrich.go` + `SetEnrich` 调用点 | 新资产自动解析/探测，且受 RoE 门控 |
| 17 | tarpit 覆盖 Bash 抓取类命令 + 按 profile 分预算 | `guard/tarpit.go:63-66,24` | katana 抓取计数生效；正常 SPA 爬取不误熔断 |

### P2（4–6 周，AI 语义层）

| # | 动作 | 文件 | 验收 |
|---|---|---|---|
| 18 | 三组锚点正则 + 触发打分 | `crawl/ai_trigger.go`（新） | `fetch("/api/x")` 不触发；`fetch(base+decode(t[i]))` 触发 |
| 19 | 语句边界窗口裁剪（非固定切片） | `crawl/ai_window.go` | 窗口内含被引用变量的定义点 |
| 20 | 窄 DSL 求值器（atob/索引表/fromCharCode/拼接） | `crawl/js_eval.go` | 文章 5.3 的 `__ccRead__` 样例能被还原出明文 URL |
| 21 | 预算计数器（轮/窗/字节/时长/成本） | `crawl/budget.go` | 超限返回结构化错误，不静默截断 |
| 22 | 候选回灌 + scope 校验 | `crawl/sink.go` | AI 候选不越域、不自动补 method |
| 23 | （可选）goja 沙箱 | `crawl/js_sandbox.go` | 无 require/fetch/fs；2s 超时 |
| 24 | 评测夹具（25 URL 闭集法） | `testdata/` 或本地靶场 | 有基线数字，改动可回归 |

---

## 5. 预期收益与度量方法

**不要承诺百分比**。文章 84% 是单一靶场（ColdChain SPA）的结果，且真值集只有 25 条。正确做法是**先建自己的基线**：

1. 在本地靶场（`Vulinbox` / 红日 3 / 自建 SPA）上人工核对源码与注册路由，建立**闭集真值**（每个场景 20–30 条）；
2. 三个对照组各跑 3 次取中位数：`katana` 普通 / `katana -headless` / 现有 `harvest_static.py`；
3. 加 P0 后跑第四次，加 P1 后跑第五次，加 P2 后跑第六次；
4. 记录：候选数、命中真值、覆盖率、耗时、请求数、LLM token 成本。

**预期形态**（基于缺口推断，非实测）：

- P0（sourcemap + config + sitemap + openapi）：覆盖率提升主要来自"原本被 `ASSET_EXT` 过滤掉的材料"，成本为零；
- P1（平台化 + 流量被动）：候选数大幅上升，但**真正价值是 method/status 的确定性**——静态候选没有 method，没法直接测；
- P2（AI 层）：只在"未执行 chunk + 编码拼接"场景有增量，与文章实验的增量来源一致。

---

## 6. 不建议做的事

1. **不要让 AI 决定"爬哪里"**。文章的结论很明确：AI 是分析组件，调度权在确定性爬虫手里。让 Agent 自主控制爬虫，会同时失去预算、scope、可复现性——而这三样正是本项目 `ROADMAP.md` §5 的立身之本。
2. **不要把所有 JS 都喂模型**。5MiB bundle 里 95% 是数据定义和构建运行时。先确定性筛选，只送"普通解析器处理不了的局部"。
3. **不要用固定字节切片**。文章已经证明会跨切片丢依赖。
4. **不要为了爬虫再引入一套浏览器**。已有 puppeteer（`api-recon/scripts`）+ `playwright-cli` skill + 可选的 MCP。再加一套只会让"哪条路径受预算约束"更难说清。
5. **不要继续往提示词里加"必须做 X"**。`ROADMAP.md` §5 第一条就是 "Prompting is not enforcement"。爬虫侧现在有 4 条"禁止/必须"（`SKILL.md:33-46`），本质是把工程约束写成了自然语言——模型换个上下文就可能不遵守。
6. **不要迁移到 Caido / Strix 全家桶**。`web-review.md` 已经明确"不因此迁移整套 Caido"。现有 `traffic` + `evidence` 已经够用，缺的是 consumer 不是 storage。
7. **不要把 `ASSET_EXT` 里的静态资源过滤当成"降噪"**。它现在过滤的是 `.json/.map`——这两类恰恰是文章里价值最高的材料。降噪应该是**分类**，不是**丢弃**。

---

## 7. 30 天落地路径（单人）

```
第 1 周  P0-1..P0-5   材料面（sourcemap + config + UA/并发）   ← 当天可见效果
第 2 周  P0-6..P0-8   sitemap/openapi + 修 sites_without_endpoints
第 3 周  P1-9..P1-13  crawl 包 + 三工具 + 流量被动
第 4 周  P1-14..P1-17 资产来源分轴 + list_endpoints + enrich 接线 + tarpit
──────────────────────────────────────────────
第 5-6 周 P2-18..P2-22 锚点/窗口/DSL/预算/回灌
第 7 周  P2-24        评测夹具与基线数字
```

**第一个可验证的里程碑**：第 1 周结束时，对一个有 sourcemap 的靶场站点，`api_static.txt` 的行数应显著上升，且新增的行来自 `sourcemap/` 而非 bundle——这是"材料理解"生效的最直接证据。

---

## 附：与文章的对应关系

| 文章概念 | 本项目落点 |
|---|---|
| 爬虫 1.0 | `spider_mpa.py`（MPA）+ 外部 `katana` |
| 爬虫 2.0 | `runtime_harvest.js`（puppeteer）+ `preload.js`（CDP 注入） |
| 爬虫 3.0 | 方向 D（`crawl/ai_*.go`） |
| 边界 1：非标准位置资源 | 方向 B1/B2（当前被 `ASSET_EXT` 主动过滤） |
| 边界 2：未执行 JS | 方向 D2/D3（窗口 + 窄 DSL） |
| 边界 3：需专门解析的协议 | 方向 B4（GraphQL/OpenAPI）；WS/SSE 已有基础（`runtime_harvest.js:105-119`） |
| 边界 4：外部状态业务流程 | 不在本文范围，见 `web-review.md` W2（AuthContext） |
| 按需触发 / 预算受限 | 方向 D1/D4 + 方向 F |
| 结果可验证 | 方向 E（候选态 vs 可达态分离） |
