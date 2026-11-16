// Package enrich 是引擎侧的资产补全层（非 AI）：一个异步 worker 池，对域名做
// DNS 解析、对 Web 资产做 HTTP 探测，并把结果写回资产图。
//
// 它是「材料理解」的宿主，而不是一个独立的爬虫：任何 worker 通过 insert_assets
// 登记一个 service / subdomain，就会自动触发一次补全；补全过程把响应当材料来读，
// 从中恢复端点、页面、脚本、sourcemap 与服务基址，再回灌资产图。对 agent 而言
// 工具面、技能、提示词都不变——能力是长在既有动作上的，不是多出来一个开关。
//
// 三层接缝：
//  1. 写入接缝：insert_assets 落库后调用 ProbeSite / ResolveDomain（agent 侧）。
//  2. 材料接缝：本包的 doHTTP 做 fetch → Classify → Extract → 回灌（确定性，无 AI）。
//  3. 语义接缝：材料触发分过阈值且预算允许时，交给 SetJSAnalyzer 注入的分析器
//     （受限 AI）。未注入时整条链路照常工作，只是少这一层。
//
// 边界（与设计文档一致，勿放宽）：
//   - 只跟同源。跨域一律丢弃，作用域判定不在这里做，但这里保证不把外域带进图。
//   - 只写「method 可确定」的端点。静态材料里 method 未知的路径不猜成 GET。
//   - 派生产物不扩大任务范围（不调 AddAutoScope）：它是既有 service 的同源子路径。
//   - 每任务有硬预算（请求数/字节/深度/每主机），超限即停，不静默无限扩张。
package enrich

import (
	"context"
	"crypto/tls"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"chengjian.local/chengjian/db"

	"github.com/miekg/dns"
	"github.com/projectdiscovery/dnsx/libs/dnsx"
)

type jobKind int

const (
	jobDNS  jobKind = iota // resolve a domain
	jobHTTP                // fetch a material (service page / script / map / config)
)

type job struct {
	kind   jobKind
	id     int64  // asset id；0 = 派生抓取（不回写 service 属性）
	taskID int64  // 归属任务；0 = 无任务上下文，只做 DNS 解析不落图
	depth  int    // 抓取深度；0 = 用户/agent 直接登记的资产
	arg    string // host (DNS) or url (HTTP)
}

// Budget 是每任务的硬预算。零值表示用默认值。
type Budget struct {
	MaxDepth    int   // 从登记资产出发最多跟几跳，默认 1
	MaxRequests int   // 每任务抓取请求总数，默认 80
	MaxBytes    int64 // 每任务累计读取字节，默认 48 MiB
	PerHost     int   // 每任务每主机请求数，默认 30
}

func (b Budget) withDefaults() Budget {
	if b.MaxDepth <= 0 {
		b.MaxDepth = 1
	}
	if b.MaxRequests <= 0 {
		b.MaxRequests = 80
	}
	if b.MaxBytes <= 0 {
		b.MaxBytes = 48 << 20
	}
	if b.PerHost <= 0 {
		b.PerHost = 30
	}
	return b
}

type taskUsage struct {
	requests int
	bytes    int64
	hosts    map[string]int
	issues   int
}

// AnalyzeRequest 是交给语义层的一份证据。
type AnalyzeRequest struct {
	TaskID    int64
	SourceURL string
	Code      string   // 证据窗口（已裁剪，不是整个 bundle）
	Signals   []string // 触发来源，用于让模型知道自己在找什么
}

// JSAnalyzer 是语义层的依赖反转接口：enrich 不 import 任何 LLM 客户端，
// 由 server 侧注入。nil = 只跑确定性抽取。
type JSAnalyzer interface {
	AnalyzeCandidates(ctx context.Context, req AnalyzeRequest) ([]Candidate, error)
}

// Engine owns the resolver, the proxy-routed HTTP client, and the worker pool.
type Engine struct {
	as     *db.AssetStore
	resolv *dnsx.DNSX
	client *http.Client

	jobs    chan job
	cool    sync.Map // dedup/cooldown: task-scoped asset or URL -> time.Time
	seen    sync.Map // "taskID|url" -> struct{}：跨深度去重，防环
	once    sync.Once
	closed  chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup

	ua       func(taskID int64) string
	budget   Budget
	mu       sync.Mutex
	usage    map[int64]*taskUsage
	analyzer JSAnalyzer
	// onFetch reports "this task just sent one fetch to host" to the tarpit fuse
	// (see guard.Guard.NoteFetch). nil = 不上报，tarpit 只统计 agent 侧的 WebFetch。
	// 这是本包唯一一条「向外汇报自身行为」的通道：补全会自动发请求，如果不汇报，
	// tarpit 预算对补全就是空的，补全成了预算外通道。
	onFetch func(taskID int64, host string, n int)
	onIssue func(taskID int64, sourceURL, reason string)
	issues  sync.Map // bounded by the task's request budget; deduplicate diagnostics
}

const (
	cooldown    = 5 * time.Minute
	httpTimeout = 12 * time.Second
	queueSize   = 1024

	// 材料体积上限。JS/map 给到 8 MiB 是因为 5 MiB 的 bundle 在真实 SPA 里很常见，
	// 1 MiB 会把最有价值的材料截断在半条语句上。
	maxJSMaterial   = 8 << 20
	maxTextMaterial = 2 << 20
	maxMapMaterial  = 32 << 20

	// 单份材料最多派生的后续抓取数。防止一个导航页把队列打满。
	maxFollowScripts = 12
	maxFollowPages   = 8
	maxFollowMaps    = 4
	maxFollowService = 4

	// 语义层触发的默认阈值（证据强度）。
	triggerThreshold = 4
)

// New builds the engine. proxy(taskID) returns the egress proxy address to route
// this task's HTTP probes through; "" = direct. It is evaluated per request (so the
// runtime routing changes take effect live) and it is **per task** on
// purpose: a task's egress is its own MITM instance, which is what binds the probe
// to that task's tunnel route and to that task's traffic store. Returning a single
// global address here (the pre-2026-10-23 behaviour) sent every task's enrichment
// out of the platform's own egress: intranet assets became unreachable and the
// probe traffic landed in the global store, invisible to the task's traffic_search.
//
// Returns a usable engine even if the resolver fails to init (DNS becomes a no-op).
func New(as *db.AssetStore, proxy func(taskID int64) string, workers int) *Engine {
	if workers <= 0 {
		workers = 4
	}
	resolv, err := dnsx.New(dnsx.Options{
		BaseResolvers: dnsx.DefaultResolvers,
		MaxRetries:    3,
		QuestionTypes: []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeCNAME},
		Timeout:       4 * time.Second,
	})
	if err != nil {
		log.Printf("[enrich] dnsx 初始化失败，DNS 解析停用：%v", err)
		resolv = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		as:     as,
		resolv: resolv,
		client: buildClient(proxy),
		jobs:   make(chan job, queueSize),
		closed: make(chan struct{}),
		usage:  map[int64]*taskUsage{},
		ctx:    ctx, cancel: cancel,
	}
	e.workers.Add(workers)
	for i := 0; i < workers; i++ {
		go e.worker()
	}
	return e
}

// SetBudget 覆盖默认预算（server 启动时按配置注入）。
func (e *Engine) SetBudget(b Budget) {
	if e == nil {
		return
	}
	e.budget = b.withDefaults()
}

// SetUserAgent 注入按任务稳定分配的 UA（server 侧接 agent.PickUA）。
// 不注入时用内置浏览器 UA：绝不用 "chengjian-enrich/1.0" 这类自报家门的串——
// 那等于给目标一个免费的指纹。
func (e *Engine) SetUserAgent(fn func(taskID int64) string) {
	if e == nil {
		return
	}
	e.ua = fn
}

// SetJSAnalyzer 注入语义层。nil 表示关闭（默认）。
func (e *Engine) SetJSAnalyzer(a JSAnalyzer) {
	if e == nil {
		return
	}
	e.analyzer = a
}

// SetFetchObserver 注入「本任务刚向 host 发了 n 次抓取」的上报通道（server 侧接
// guard.Guard.NoteFetch，让 tarpit 预算把补全算进去）。nil = 不上报。
func (e *Engine) SetFetchObserver(fn func(taskID int64, host string, n int)) {
	if e == nil {
		return
	}
	e.onFetch = fn
}

// SetIssueObserver exposes incomplete background work to the owning task.
// Configure before submitting jobs, just like the other observer setters.
func (e *Engine) SetIssueObserver(fn func(int64, string, string)) { e.onIssue = fn }

func (e *Engine) issue(j job, reason string) {
	e.mu.Lock()
	if e.usage == nil {
		e.usage = map[int64]*taskUsage{}
	}
	u := e.usageFor(j.taskID)
	if u.issues >= 64 {
		e.mu.Unlock()
		return
	}
	key := itoa(j.taskID) + "|" + j.arg + "|" + reason
	if _, loaded := e.issues.LoadOrStore(key, struct{}{}); loaded {
		e.mu.Unlock()
		return
	}
	u.issues++
	if u.issues == 64 {
		reason += "；此任务完整性提示已达 64 条，后续同类问题不再逐条记录，请显式核对资源覆盖范围"
	}
	e.mu.Unlock()
	log.Printf("[enrich] task=%d source=%s incomplete=%s", j.taskID, j.arg, reason)
	if e.onIssue != nil {
		e.onIssue(j.taskID, j.arg, reason)
	}
}

// noteFetch 上报一次抓取。零值引擎/未注入/无任务上下文时静默跳过。
func (e *Engine) noteFetch(taskID int64, host string, n int) {
	if e == nil || e.onFetch == nil || taskID <= 0 || host == "" || n <= 0 {
		return
	}
	e.onFetch(taskID, host, n)
}

// buildClient returns an HTTP client that dials via the egress proxy resolved for
// **this request's task** and skips TLS verification (the proxy re-signs with its
// MITM CA; targets are often self-signed — this is a pentest probe).
//
// The task id reaches the Proxy func through the request context (see withTaskID):
// http.Transport.Proxy receives the *http.Request, so this keeps one shared client
// and one shared transport while still selecting per task. A per-task client cache
// would work too, but it would hold a Transport (and its connections) per task for
// the lifetime of the process.
func buildClient(proxy func(taskID int64) string) *http.Client {
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: httpTimeout,
	}
	if proxy != nil {
		tr.Proxy = func(req *http.Request) (*url.URL, error) {
			p := proxy(taskIDOf(req))
			if p == "" {
				return nil, nil // direct
			}
			return url.Parse(p)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   httpTimeout,
		// cap redirects; keep them within scope by re-checking at probe time
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// taskIDKey is the request-context key carrying the owning task id. Distinct
// unexported type so no other package can collide with it.
type taskIDKey struct{}

// withTaskID stamps the request with its task so the transport's Proxy func can
// pick that task's egress. taskID<=0 leaves the request untouched (→ direct).
func withTaskID(req *http.Request, taskID int64) *http.Request {
	if req == nil || taskID <= 0 {
		return req
	}
	return req.WithContext(context.WithValue(req.Context(), taskIDKey{}, taskID))
}

// taskIDOf reads the task stamped by withTaskID. 0 = unknown → the resolver decides
// (server returns the global egress for task 0, matching the legacy behaviour).
func taskIDOf(req *http.Request) int64 {
	if req == nil {
		return 0
	}
	if v, ok := req.Context().Value(taskIDKey{}).(int64); ok {
		return v
	}
	return 0
}

// ResolveDomain enqueues a DNS resolution for a domain asset (ungated). host is the
// FQDN. No-op for an empty engine.
func (e *Engine) ResolveDomain(id int64, taskID int64, host string) {
	e.enqueue(job{kind: jobDNS, id: id, taskID: taskID, arg: host})
}

// ProbeSite enqueues an HTTP probe for a web-asset (site). rawURL is the site URL.
// id is the service asset id (write-back target); taskID scopes budget and assets.
func (e *Engine) ProbeSite(id int64, taskID int64, rawURL string) {
	e.enqueue(job{kind: jobHTTP, id: id, taskID: taskID, arg: rawURL})
}

func (e *Engine) enqueue(j job) {
	if e == nil || id0(j.id) && j.kind == jobDNS || j.arg == "" {
		return
	}
	select {
	case <-e.closed:
		return
	default:
	}
	select {
	case e.jobs <- j:
	default: // queue full → drop (best-effort enrichment)
		e.issue(j, "queue_full: 补全队列已满，该资源尚未获取，可显式重新获取")
	}
}

func id0(id int64) bool { return id <= 0 }

// Close cancels in-flight HTTP work and waits for workers before returning.
func (e *Engine) Close() {
	if e == nil {
		return
	}
	e.once.Do(func() {
		close(e.closed)
		if e.cancel != nil {
			e.cancel()
		}
	})
	e.workers.Wait()
}

func (e *Engine) worker() {
	defer e.workers.Done()
	for {
		select {
		case <-e.closed:
			return
		case j := <-e.jobs:
			if e.ctx.Err() != nil {
				return
			}
			if e.onCooldown(j) {
				continue
			}
			switch j.kind {
			case jobDNS:
				e.doDNS(j.id, j.arg)
			case jobHTTP:
				e.doHTTP(j)
			}
		}
	}
}

// onCooldown returns true (skip) if this (kind,id) ran within the cooldown window.
func (e *Engine) onCooldown(j job) bool {
	key := itoa(j.taskID) + ":" + string(rune(j.kind)) + ":" + itoa(j.id)
	if j.id <= 0 {
		// 派生抓取没有资产 id，用 URL 做冷却键，避免同一脚本被反复拉取。
		key = itoa(j.taskID) + ":" + string(rune(j.kind)) + ":u:" + j.arg
	}
	if v, ok := e.cool.Load(key); ok {
		if t, ok := v.(time.Time); ok && time.Since(t) < cooldown {
			return true
		}
	}
	e.cool.Store(key, time.Now())
	return false
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// ---- DNS ----

func (e *Engine) doDNS(id int64, host string) {
	if e.resolv == nil {
		return
	}
	data, err := e.resolv.QueryMultiple(host)
	if err != nil || data == nil {
		return
	}
	ips := uniq(append(append([]string{}, data.A...), data.AAAA...))
	// Upsert resolved IPs into the asset store.
	for _, ip := range ips {
		_, _ = e.as.UpsertIP(db.UpsertIPReq{
			IP:           ip,
			BoundDomains: []string{host},
		})
	}
	// Record A records as subdomains if host looks like a subdomain.
	for _, a := range data.A {
		_, _ = e.as.UpsertSubdomain(db.UpsertSubdomainReq{
			Domain:      host,
			RecordType:  "A",
			RecordValue: []string{a},
		})
	}
	for _, aaaa := range data.AAAA {
		_, _ = e.as.UpsertSubdomain(db.UpsertSubdomainReq{
			Domain:      host,
			RecordType:  "AAAA",
			RecordValue: []string{aaaa},
		})
	}
	for _, cname := range data.CNAME {
		_, _ = e.as.UpsertSubdomain(db.UpsertSubdomainReq{
			Domain:      host,
			RecordType:  "CNAME",
			RecordValue: []string{cname},
		})
	}
}

// ---- HTTP：抓取一份材料 ----

var reTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

const defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

func (e *Engine) doHTTP(j job) {
	base, err := url.Parse(j.arg)
	if err != nil || base.Hostname() == "" {
		return
	}
	if !e.takeBudget(j.taskID, base.Hostname()) {
		e.issue(j, "budget_exhausted: 自动补全预算已耗尽，该资源尚未获取")
		return
	}

	req, err := http.NewRequestWithContext(e.ctx, "GET", j.arg, nil)
	if err != nil {
		return
	}
	// 打上任务标记：transport 的 Proxy func 靠它选该任务的出口（不是全局出口）。
	req = withTaskID(req, j.taskID)
	ua := defaultUA
	if e.ua != nil {
		if v := e.ua(j.taskID); v != "" {
			ua = v
		}
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")

	// tarpit 预算：把「补全自己发的请求」算进去。位置刻意放在发请求之前——
	// tarpit 迷宫会对每次抓取都回 200，按响应计会永远数不到；按「即将发出」计，
	// 连目标不可达的循环也能被计数（那时 hint 说的「抓取次数超预算」仍然为真）。
	e.noteFetch(j.taskID, base.Hostname(), 1)

	resp, err := e.client.Do(req)
	if err != nil {
		if e.ctx.Err() == nil {
			e.issue(j, "request_failed: 资源请求失败，请检查任务出口后显式重取")
		}
		return
	}
	defer resp.Body.Close()

	// 先按 URL + Content-Type 定型，再按类型决定读多少——避免用 1 MiB 的帽子
	// 把 5 MiB 的 bundle 截断在半条语句上。
	kind := Classify(j.arg, resp.Header.Get("Content-Type"), nil)
	limit := int64(maxTextMaterial)
	switch kind {
	case KindJS:
		limit = maxJSMaterial
	case KindSourceMap:
		limit = maxMapMaterial
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	e.chargeBytes(j.taskID, int64(len(body)))
	if err != nil || e.ctx.Err() != nil {
		if e.ctx.Err() == nil {
			e.issue(j, "body_read_failed: 响应中断，不将半份材料作为完整证据")
		}
		return // 不把中断或传输失败的半份材料当成完整证据。
	}
	if int64(len(body)) > limit {
		e.issue(j, fmt.Sprintf("body_limit: 材料超过 %d 字节，自动分析已停止；请用任务内 HTTP 工具保存原文或用 Range 分段读取并核验 206/Content-Range/ETag", limit))
		return
	}

	// 内容嗅探补判（CT 缺失或被伪装时）。
	if kind == KindOther {
		kind = Classify(j.arg, resp.Header.Get("Content-Type"), body)
	}

	// 1) 既有行为：把站点指纹写回 service 资产（只有真实 service 资产才写）。
	if j.id > 0 && kind == KindHTML {
		statusCode := resp.StatusCode
		bodyLen := resp.ContentLength
		if bodyLen < 0 {
			bodyLen = int64(len(body))
		}
		_, _ = e.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
			URL:           j.arg,
			StatusCode:    &statusCode,
			ContentLength: &bodyLen,
			PageTitle:     extractTitle(body),
			TaskID:        j.taskID,
		})
	}

	// 2) 新材料：把响应当材料读，恢复端点与服务基址。
	if kind == KindOther {
		return
	}
	mat := Extract(base, kind, body)
	if len(mat.Endpoints) > 0 || len(mat.Services) > 0 {
		e.sink(j.taskID, mat)
	}

	// 3) 语义接缝：确定性规则吃不下的部分才交给分析器，且受预算约束。
	if e.analyzer != nil && mat.Trigger >= triggerThreshold {
		e.analyze(j, mat, string(body))
	}

	// 4) 派生抓取：只在预算深度内跟一跳。
	if j.depth >= e.budget.withDefaults().MaxDepth {
		return
	}
	e.follow(j, mat)
}

// sink 把材料里 method 已确定的端点写进资产图。
// 不调用 AddAutoScope：这些是既有 service 的同源子路径，范围已由父资产确定，
// 派生结果再扩范围会破坏「side-effect 派生不扩 scope」的既有约定。
func (e *Engine) sink(taskID int64, mat Material) {
	if e.as == nil {
		return
	}
	seen := map[string]struct{}{}
	for _, c := range mat.Endpoints {
		if c.Method == "" || c.URL == "" {
			continue
		}
		key := c.Method + " " + c.URL
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		id, err := e.as.UpsertEndpoint(db.UpsertEndpointReq{
			URL:    c.URL,
			Method: c.Method,
			Params: c.Params,
			TaskID: taskID,
		})
		if err != nil {
			continue
		}
		// 来源可追溯：task_asset_links.source 是自由文本，用 enrich:<抽取器> 区分
		// 静态/配置/OpenAPI 三条来路，供后续判断「这条端点是猜的还是读出来的」。
		if taskID > 0 {
			_ = e.as.SetTaskAssetSource(taskID, id, "enrich:"+c.Source,
				"资产补全自动抽取（"+c.Source+"）", nil)
		}
	}
}

// analyze 调语义层，并把候选并入资产图。候选同样只接受同源、method 已知的。
func (e *Engine) analyze(j job, mat Material, code string) {
	if e.ctx.Err() != nil {
		return
	}
	if !e.takeSemantic(j.taskID) {
		return
	}
	windows := EvidenceWindows(code, 24*1024)
	if windows == "" {
		return
	}
	ctx, cancel := context.WithTimeout(e.ctx, 60*time.Second)
	defer cancel()
	cands, err := e.analyzer.AnalyzeCandidates(ctx, AnalyzeRequest{
		TaskID:    j.taskID,
		SourceURL: j.arg,
		Code:      windows,
		Signals:   mat.Signals,
	})
	if err != nil || ctx.Err() != nil {
		if ctx.Err() != nil {
			if e.ctx.Err() == nil {
				e.issue(j, "analysis_timeout: 语义分析未完成，确定性分析结果仍保留")
			}
			return
		}
		log.Printf("[enrich] 语义层失败 url=%s: %v", j.arg, err)
		e.issue(j, "analysis_failed: 语义分析失败，确定性分析结果仍保留")
		return
	}
	base, _ := url.Parse(j.arg)
	out := Material{Kind: mat.Kind}
	for _, c := range cands {
		abs := resolveSameOrigin(base, c.URL)
		if abs == "" || c.Method == "" {
			continue // 语义层没有扩域权，也没有造 method 权
		}
		out.Endpoints = append(out.Endpoints, Candidate{
			Method: strings.ToUpper(c.Method), URL: abs, Params: c.Params, Source: "ai",
		})
	}
	if len(out.Endpoints) > 0 {
		e.sink(j.taskID, out)
	}
}

// follow 把材料里值得继续取的 URL 入队。数量逐类封顶，避免一个导航页打满队列。
func (e *Engine) follow(j job, mat Material) {
	push := func(list []string, capN int) {
		if len(list) > capN {
			e.issue(j, "follow_limit: 派生资源超过自动跟进数量上限；请从原文读取其余资源并按需显式获取")
			list = list[:capN]
		}
		for _, u := range list {
			if !e.markSeen(j.taskID, u) {
				continue
			}
			e.enqueue(job{kind: jobHTTP, taskID: j.taskID, depth: j.depth + 1, arg: u})
		}
	}
	push(dedupStrings(mat.Scripts), maxFollowScripts)
	push(dedupStrings(mat.SourceMaps), maxFollowMaps)
	push(dedupStrings(mat.Pages), maxFollowPages)
	push(dedupStrings(mat.Services), maxFollowService)
}

// markSeen 返回 true 表示这是本任务第一次见到该 URL。
func (e *Engine) markSeen(taskID int64, u string) bool {
	key := itoa(taskID) + "|" + u
	if _, loaded := e.seen.LoadOrStore(key, struct{}{}); loaded {
		return false
	}
	return true
}

// ---- 预算 ----

func (e *Engine) usageFor(taskID int64) *taskUsage {
	u := e.usage[taskID]
	if u == nil {
		u = &taskUsage{hosts: map[string]int{}}
		e.usage[taskID] = u
	}
	return u
}

// takeBudget 在发请求前扣一次预算。taskID<=0（无任务上下文）不设限——
// 此时不落资产图，只做一次性探测，风险由调用方控制。
func (e *Engine) takeBudget(taskID int64, host string) bool {
	if taskID <= 0 {
		return true
	}
	b := e.budget.withDefaults()
	e.mu.Lock()
	defer e.mu.Unlock()
	u := e.usageFor(taskID)
	if u.requests >= b.MaxRequests || u.bytes >= b.MaxBytes || u.hosts[host] >= b.PerHost {
		return false
	}
	u.requests++
	u.hosts[host]++
	return true
}

func (e *Engine) chargeBytes(taskID int64, n int64) {
	if taskID <= 0 || n <= 0 {
		return
	}
	e.mu.Lock()
	e.usageFor(taskID).bytes += n
	e.mu.Unlock()
}

// takeSemantic 是语义层的独立预算：比抓取更贵，所以单独计数。
// 默认每任务最多 6 次，避免一个混淆站点烧穿整任务的 token 预算。
func (e *Engine) takeSemantic(taskID int64) bool {
	if taskID <= 0 {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	u := e.usageFor(taskID)
	if u.hosts["__semantic__"] >= 6 {
		return false
	}
	u.hosts["__semantic__"]++
	return true
}

// ---- 语义层的证据裁剪 ----

var reWindowAnchor = regexp.MustCompile(`\b(?:fetch|\.open|axios|XMLHttpRequest|WebSocket|importScripts|atob|fromCharCode|charCodeAt|__webpack_require__|sourceMappingURL)\b`)

// EvidenceWindows 围绕命中位置裁剪证据窗口，而不是切固定字节。
// 固定切片会跨窗口丢掉变量定义（文章 5.3 的教训）；这里以锚点为中心向两侧
// 扩展到语句边界，再按 maxBytes 封顶。返回多段用换行分隔，段间标注省略。
func EvidenceWindows(code string, maxBytes int) string {
	if len(code) <= maxBytes {
		return code
	}
	locs := reWindowAnchor.FindAllStringIndex(code, -1)
	if len(locs) == 0 {
		return code[:maxBytes]
	}
	const span = 1500 // 单窗口基准半宽
	var b strings.Builder
	last := -1
	for _, loc := range locs {
		if b.Len() >= maxBytes {
			break
		}
		start := loc[0] - span
		if start < 0 {
			start = 0
		}
		end := loc[1] + span
		if end > len(code) {
			end = len(code)
		}
		start = alignBack(code, start)
		end = alignForward(code, end)
		if start <= last {
			continue // 与上一窗口重叠
		}
		if last >= 0 {
			b.WriteString("\n/* … */\n")
		}
		if end > len(code) {
			end = len(code)
		}
		b.WriteString(code[start:end])
		last = end
	}
	out := b.String()
	if len(out) > maxBytes {
		out = out[:maxBytes]
	}
	return out
}

// alignBack 向前找最近的语句边界（; { } 或换行），避免从标识符中间切断。
func alignBack(code string, i int) int {
	for ; i > 0; i-- {
		switch code[i-1] {
		case ';', '{', '}', '\n':
			return i
		}
	}
	return 0
}

// alignForward 向后找最近的语句边界。
func alignForward(code string, i int) int {
	for ; i < len(code); i++ {
		switch code[i] {
		case ';', '{', '}', '\n':
			return i + 1
		}
	}
	return len(code)
}

// ---- helpers ----

func extractTitle(body []byte) string {
	m := reTitle.FindSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(string(m[1])))
}

func dedupStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func uniq(in []string) []string {
	seen := map[string]struct{}{}
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
