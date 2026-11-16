// 本文件是 enrich 的「材料理解层」：把一次 HTTP 响应当材料而不是当页面，
// 从里面恢复端点、页面、脚本、sourcemap 与服务基址。
//
// 设计约束（决定了它为什么长这样）：
//   - 纯函数。不碰 DB、不发网络、不看时钟。全部输入是 (base, kind, body)，
//     全部输出是结构体。这样才能被真实响应样例直接单测，也才能在 enrich 的
//     worker 池里被任意并发调用。
//   - 只产出「能确定 method 的端点」。静态材料里 method 未知的路径不猜成 GET
//     （见 docs/reviews/2026-10-21/web-review.md W4），而是留在 Trigger 里交给
//     后续语义层；宁缺勿造，避免把幻觉写进资产图。
//   - 一切 URL 先做同源归一。跨域一律丢弃——作用域判定是调用方的事，这里只保证
//     不把外域材料带进图。
package enrich

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// Kind 是材料类型。它决定用哪个抽取器，而不是决定「要不要分析」。
type Kind string

const (
	KindHTML      Kind = "html"
	KindJS        Kind = "js"
	KindJSON      Kind = "json"
	KindSourceMap Kind = "sourcemap"
	KindOpenAPI   Kind = "openapi"
	KindOther     Kind = "other"
)

// Candidate 是一条 method 已确定的端点候选。
type Candidate struct {
	Method string           // 大写 HTTP 方法
	URL    string           // 绝对 URL（同源）
	Params []map[string]any // {location,name,type} —— 与 db.ParamItem 对齐
	Source string           // html-form | js-fetch | js-xhr | js-axios | js-route | openapi
}

// Material 是一次材料抽取的完整结果。
type Material struct {
	Kind Kind

	// Endpoints 是 method 已确定、可以直接写进资产图的端点。
	Endpoints []Candidate

	// Pages 是同源、可继续取一层的页面（<a href>、iframe）。
	Pages []string
	// Scripts 是同源、值得下载分析的 JS。
	Scripts []string
	// SourceMaps 是 JS 里声明的同源 sourcemap。
	SourceMaps []string
	// Services 是从配置/OpenAPI servers 里恢复出的同源基址（可能带新路径前缀）。
	Services []string

	// Trigger > 0 表示这份材料里有确定性规则吃不下的东西（拼接、编码、未执行
	// 分支）。数值是证据强度，调用方用它和预算一起决定要不要进语义层。
	Trigger int
	// Signals 记录触发来源，便于日志与调参；不进模型上下文。
	Signals []string
}

func (m *Material) addSignal(name string) {
	for _, s := range m.Signals {
		if s == name {
			return
		}
	}
	m.Signals = append(m.Signals, name)
}

// ---------------------------------------------------------------------------
// 分类
// ---------------------------------------------------------------------------

// Classify 判断材料类型。顺序刻意如此：扩展名/CT 先于内容嗅探，避免把
// 一个 .js 的 JSONP 响应误判成 JSON。
func Classify(rawURL, contentType string, body []byte) Kind {
	u, _ := url.Parse(rawURL)
	path := ""
	if u != nil {
		path = strings.ToLower(u.Path)
	}
	ct := strings.ToLower(contentType)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)

	if strings.HasSuffix(path, ".map") {
		return KindSourceMap
	}
	if strings.Contains(ct, "javascript") || strings.Contains(ct, "ecmascript") ||
		strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".mjs") || strings.HasSuffix(path, ".cjs") {
		return KindJS
	}
	if strings.Contains(ct, "json") || strings.HasSuffix(path, ".json") {
		if looksOpenAPI(body) {
			return KindOpenAPI
		}
		return KindJSON
	}
	if strings.Contains(ct, "html") || strings.HasSuffix(path, ".htm") || strings.HasSuffix(path, ".html") {
		return KindHTML
	}
	// 无 CT / octet-stream 时的兜底嗅探：只看前 512 字节。
	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	h := strings.ToLower(strings.TrimSpace(string(head)))
	switch {
	case strings.HasPrefix(h, "<!doctype html"), strings.HasPrefix(h, "<html"), strings.HasPrefix(h, "<head"):
		return KindHTML
	case strings.HasPrefix(h, "{"), strings.HasPrefix(h, "["):
		if looksOpenAPI(body) {
			return KindOpenAPI
		}
		return KindJSON
	case strings.Contains(h, "function") || strings.Contains(h, "=>"):
		return KindJS
	}
	return KindOther
}

func looksOpenAPI(body []byte) bool {
	if len(body) > 64*1024 {
		body = body[:64*1024]
	}
	return bytesContainsAny(body, `"openapi"`, `"swagger"`, `'openapi'`)
}

func bytesContainsAny(b []byte, subs ...string) bool {
	s := string(b)
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 抽取入口
// ---------------------------------------------------------------------------

// Extract 按材料类型分派到对应抽取器。base 是响应所属 URL（用于同源归一）。
// 任何解析失败都返回空 Material 而不是错误——材料理解是尽力而为，
// 一次抽取失败不应该影响补全主流程。
func Extract(base *url.URL, kind Kind, body []byte) Material {
	m := Material{Kind: kind}
	if base == nil {
		return m
	}
	switch kind {
	case KindHTML:
		extractHTML(base, body, &m)
	case KindJS:
		extractJS(base, string(body), &m, true)
	case KindJSON:
		extractJSON(base, body, &m)
	case KindOpenAPI:
		extractOpenAPI(base, body, &m)
	case KindSourceMap:
		extractSourceMap(base, body, &m)
	}
	return m
}

// ---------------------------------------------------------------------------
// 同源归一
// ---------------------------------------------------------------------------

// sameOrigin 判断 ref 解析后是否与 base 同源（scheme + host + 有效端口）。
// 端口按 scheme 补默认值后比较，避免 https://a 与 https://a:443 被当成两个源。
func sameOrigin(base *url.URL, ref *url.URL) bool {
	if base == nil || ref == nil {
		return false
	}
	if !strings.EqualFold(ref.Scheme, base.Scheme) {
		return false
	}
	if !strings.EqualFold(ref.Hostname(), base.Hostname()) {
		return false
	}
	return effectivePort(ref) == effectivePort(base)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		return "443"
	case "http", "ws":
		return "80"
	}
	return ""
}

// resolveSameOrigin 把 ref 解析成绝对 URL；非同源、非 http(s)、空值一律返回 ""。
func resolveSameOrigin(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "javascript:") || strings.HasPrefix(ref, "mailto:") ||
		strings.HasPrefix(ref, "tel:") || strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "#") {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	abs := base.ResolveReference(u)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return ""
	}
	if !sameOrigin(base, abs) {
		return ""
	}
	abs.Fragment = ""
	return abs.String()
}

var assetExt = regexp.MustCompile(`(?i)\.(css|png|jpe?g|gif|svg|ico|woff2?|ttf|eot|mp4|webm|webp|pdf|zip|gz|tar|exe|dmg)(\?|$)`)

func looksLikeAsset(u string) bool { return assetExt.MatchString(u) }

// ---------------------------------------------------------------------------
// HTML
// ---------------------------------------------------------------------------

var (
	reFormAction = regexp.MustCompile(`(?i)action\s*=\s*["']([^"']*)["']`)
	reMethodAttr = regexp.MustCompile(`(?i)method\s*=\s*["']([^"']*)["']`)
	reInputName  = regexp.MustCompile(`(?i)name\s*=\s*["']([^"']+)["']`)
	reMetaRefresh = regexp.MustCompile(`(?i)url\s*=\s*([^"'>\s]+)`)
)

func extractHTML(base *url.URL, body []byte, m *Material) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return
	}
	var inlineJS strings.Builder

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "a", "area":
				if href := attr(n, "href"); href != "" {
					if abs := resolveSameOrigin(base, href); abs != "" && !looksLikeAsset(abs) {
						m.Pages = append(m.Pages, abs)
					}
				}
			case "iframe", "frame":
				if src := attr(n, "src"); src != "" {
					if abs := resolveSameOrigin(base, src); abs != "" && !looksLikeAsset(abs) {
						m.Pages = append(m.Pages, abs)
					}
				}
			case "script":
				if src := attr(n, "src"); src != "" {
					if abs := resolveSameOrigin(base, src); abs != "" {
						m.Scripts = append(m.Scripts, abs)
					}
				} else if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
					inlineJS.WriteString(n.FirstChild.Data)
					inlineJS.WriteString("\n")
				}
			case "link":
				// modulepreload / preload as=script 也是脚本线索
				if rel := strings.ToLower(attr(n, "rel")); strings.Contains(rel, "modulepreload") ||
					(strings.Contains(rel, "preload") && strings.Contains(strings.ToLower(attr(n, "as")), "script")) {
					if href := attr(n, "href"); href != "" {
						if abs := resolveSameOrigin(base, href); abs != "" {
							m.Scripts = append(m.Scripts, abs)
						}
					}
				}
			case "form":
				formHTML := renderNode(n)
				action := ""
				if a := reFormAction.FindStringSubmatch(formHTML); a != nil {
					action = a[1]
				}
				method := "GET"
				if mm := reMethodAttr.FindStringSubmatch(formHTML); mm != nil {
					if v := strings.ToUpper(strings.TrimSpace(mm[1])); v != "" {
						method = v
					}
				}
				if action == "" {
					action = base.String()
				}
				abs := resolveSameOrigin(base, action)
				if abs == "" {
					break
				}
				c := Candidate{Method: method, URL: abs, Source: "html-form"}
				for _, nm := range reInputName.FindAllStringSubmatch(formHTML, -1) {
					c.Params = append(c.Params, map[string]any{
						"location": "form", "name": nm[1], "type": "string",
					})
				}
				m.Endpoints = append(m.Endpoints, c)
			case "meta":
				if strings.EqualFold(attr(n, "http-equiv"), "refresh") {
					if mm := reMetaRefresh.FindStringSubmatch(attr(n, "content")); mm != nil {
						if abs := resolveSameOrigin(base, mm[1]); abs != "" && !looksLikeAsset(abs) {
							m.Pages = append(m.Pages, abs)
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if js := inlineJS.String(); strings.TrimSpace(js) != "" {
		extractJS(base, js, m, false)
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return strings.TrimSpace(a.Val)
		}
	}
	return ""
}

// renderNode 把子树重新序列化成 HTML 文本，供正则处理表单字段。
// 用正则而不是再遍历一次，是因为表单字段的语义（name/method/action）本来就是
// 属性文本，序列化后一次匹配比维护两套遍历简单。
func renderNode(n *html.Node) string {
	var b strings.Builder
	_ = html.Render(&b, n)
	return b.String()
}

// ---------------------------------------------------------------------------
// JS
// ---------------------------------------------------------------------------

var (
	// 只认「字面量路径」：/ 开头、由路径字符组成。
	reJSLiteral = regexp.MustCompile("[\"'`](/[A-Za-z0-9_\\-./{}:$~]*)['\"`]")
	// 拼接头部："/api/v1/" + x —— 只能拿到前缀，method 未知，因此不进 Endpoints，
	// 只作为触发信号。
	reJSConcatHead = regexp.MustCompile("[\"'](/[A-Za-z0-9_\\-./]+/)[\"']\\s*\\+")
	// 路由声明（前端路由，不是后端接口）。
	reJSRoute = regexp.MustCompile("(?:path|to|redirect|href)\\s*:\\s*[\"'`](/[^\"'`]*)['\"`]")
	// 配置键：基址类字段。产出 Services，不产出 Endpoints。
	reJSConfig = regexp.MustCompile("(?i)(?:baseURL|baseUrl|apiBase|apiHost|apiRoot|endpoint|graphqlEndpoint|wsUrl|socketUrl|uploadUrl)\\s*[:=]\\s*[\"'`]([^\"'`]{2,300})['\"`]")
	// sourcemap 声明。
	reSourceMap = regexp.MustCompile(`(?://|/\*)[#@]\s*sourceMappingURL=([^\s*]+)`)

	// —— method 可确定的调用点 ——
	reFetchLiteral = regexp.MustCompile("\\bfetch\\s*\\(\\s*[\"'`]([^\"'`]+)['\"`]")
	reFetchMethod  = regexp.MustCompile("\\bfetch\\s*\\(\\s*[\"'`]([^\"'`]+)['\"`]\\s*,\\s*\\{[^{}]{0,400}?method\\s*:\\s*[\"'`]([A-Za-z]+)['\"`]")
	reXHROpen      = regexp.MustCompile("\\.open\\s*\\(\\s*[\"'`]([A-Za-z]+)['\"`]\\s*,\\s*[\"'`]([^\"'`]+)['\"`]")
	reAxiosVerb    = regexp.MustCompile("\\b(?:axios|ky|got|superagent)\\s*\\.\\s*(get|post|put|patch|delete|head|options)\\s*\\(\\s*[\"'`]([^\"'`]+)['\"`]")
	reClientVerb   = regexp.MustCompile("\\b(?:request|http|api|client|service|apiClient|httpClient)\\s*\\.\\s*(get|post|put|patch|delete)\\s*\\(\\s*[\"'`]([^\"'`]+)['\"`]")

	// —— 触发信号（确定性规则吃不下的迹象）——
	reSigDynamicCall = regexp.MustCompile("\\b(?:fetch|axios\\s*\\.\\s*(?:get|post|put|patch|delete|request)|request|http)\\s*\\(\\s*[A-Za-z_$]")
	reSigDecode      = regexp.MustCompile(`\b(?:atob|decodeURIComponent|unescape|String\.fromCharCode|charCodeAt|TextDecoder)\s*\(`)
	reSigChunk       = regexp.MustCompile(`\b(?:__webpack_require__|__vite__mapDeps|webpackChunk[A-Za-z0-9_$]*)\b`)
	reSigEval        = regexp.MustCompile(`\b(?:eval|Function)\s*\(`)
	reSigHex         = regexp.MustCompile(`(?:\\x[0-9A-Fa-f]{2}){8,}`)
	reSigB64         = regexp.MustCompile(`[A-Za-z0-9+/]{100,}={0,2}`)
)

// extractJS 从 JS 文本里抽端点与线索。inline=true 表示这段 JS 来自 <script> 内联，
// 此时它的相对路径仍以页面 URL 为基准（调用方传的 base 就是页面）。
func extractJS(base *url.URL, js string, m *Material, countTrigger bool) {
	// 先剔注释，避免把注释里的示例路径当成真实调用。不追求完美——
	// 字符串里的 // 会被误伤，但误伤的代价只是少一条候选，不会造出假端点。
	code := stripJSComments(js)

	// 1) fetch / xhr / axios：method 可确定，直接进 Endpoints。
	// 先收显式 method 的调用点，再补 fetch 的无 init 默认 GET——否则
	// fetch("/x", {method:"POST"}) 会同时产出 GET 和 POST 两条，其中 GET 是错的。
	explicit := map[string]struct{}{}
	for _, sub := range reFetchMethod.FindAllStringSubmatch(code, -1) {
		if abs := resolveSameOrigin(base, sub[1]); abs != "" {
			explicit[abs] = struct{}{}
			m.Endpoints = append(m.Endpoints, Candidate{
				Method: strings.ToUpper(sub[2]), URL: abs, Source: "js-fetch",
			})
		}
	}
	for _, sub := range reFetchLiteral.FindAllStringSubmatch(code, -1) {
		abs := resolveSameOrigin(base, sub[1])
		if abs == "" {
			continue
		}
		if _, ok := explicit[abs]; ok {
			continue // 该 URL 的 method 已由显式 init 确定
		}
		// fetch(url) 无 init 时按 Fetch 规范就是 GET，这是确定性推断而非猜测。
		m.Endpoints = append(m.Endpoints, Candidate{Method: "GET", URL: abs, Source: "js-fetch"})
	}
	for _, sub := range reXHROpen.FindAllStringSubmatch(code, -1) {
		if abs := resolveSameOrigin(base, sub[2]); abs != "" {
			m.Endpoints = append(m.Endpoints, Candidate{
				Method: strings.ToUpper(sub[1]), URL: abs, Source: "js-xhr",
			})
		}
	}
	for _, re := range []*regexp.Regexp{reAxiosVerb, reClientVerb} {
		for _, sub := range re.FindAllStringSubmatch(code, -1) {
			if abs := resolveSameOrigin(base, sub[2]); abs != "" {
				m.Endpoints = append(m.Endpoints, Candidate{
					Method: strings.ToUpper(sub[1]), URL: abs, Source: "js-axios",
				})
			}
		}
	}

	// 2) 配置键 → Services（基址，可能是新前缀）。
	for _, sub := range reJSConfig.FindAllStringSubmatch(code, -1) {
		if abs := resolveSameOrigin(base, sub[1]); abs != "" {
			m.Services = append(m.Services, strings.TrimRight(abs, "/"))
		}
	}

	// 3) sourcemap 声明 → SourceMaps。必须在剥注释之前从原文取：`//# sourceMappingURL=`
	//    本身就是行注释，先剥就再也找不到了。
	for _, sub := range reSourceMap.FindAllStringSubmatch(js, -1) {
		if abs := resolveSameOrigin(base, sub[1]); abs != "" {
			m.SourceMaps = append(m.SourceMaps, abs)
		}
	}

	// 4) 路由声明：不是接口，但能扩展后续要访问的页面集合。
	for _, sub := range reJSRoute.FindAllStringSubmatch(code, -1) {
		if abs := resolveSameOrigin(base, sub[1]); abs != "" && !looksLikeAsset(abs) {
			m.Pages = append(m.Pages, abs)
		}
	}

	if !countTrigger {
		return
	}

	// 5) 触发打分。只表示「这份材料还有确定性规则吃不下的部分」，
	//    不代表存在漏洞，也不代表必须调用模型——是否进语义层由预算决定。
	if n := len(reSigDynamicCall.FindAllString(code, -1)); n > 0 {
		m.Trigger += 3 * minInt(n, 3)
		m.addSignal("dynamic-call")
	}
	if reSigDecode.MatchString(code) {
		m.Trigger += 2
		m.addSignal("decode")
	}
	if reSigChunk.MatchString(code) {
		m.Trigger++
		m.addSignal("chunk-runtime")
	}
	if reSigEval.MatchString(code) {
		m.Trigger++
		m.addSignal("eval")
	}
	if reSigHex.MatchString(code) || reSigB64.MatchString(code) {
		m.Trigger++
		m.addSignal("encoded-blob")
	}
	if reJSConcatHead.MatchString(code) {
		m.Trigger++
		m.addSignal("concat")
	}
}

// stripJSComments 去掉行注释与块注释，保留字符串字面量里的内容。
// 简化状态机：只处理 ' " ` 三种引号与转义，不做正则字面量识别——
// 把正则字面量当代码看最多多出几条候选，而漏掉真实调用代价更大。
func stripJSComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			b.WriteByte(c)
			if c == '\\' && i+1 < len(s) {
				b.WriteByte(s[i+1])
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch {
		case c == '\'' || c == '"' || c == '`':
			quote = c
			b.WriteByte(c)
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i++ // 跳过结尾的 /
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// JSON / 配置
// ---------------------------------------------------------------------------

// configKeys 是「这个键的值很可能是一个基址」的语义集合。
// 用键名而不是位置，是因为 runtime-config / asset-manifest 这类文件的字段名
// 本身就是自解释的——这也是文章 5.2 那个 MVP 能成立的原因。
var configKeys = []string{
	"api_base", "apibase", "baseurl", "base_url", "api_url", "apiurl", "api_host",
	"endpoint", "graphql", "graphqlendpoint", "wsurl", "ws_url", "socketurl",
	"socket_url", "gateway", "backend", "server", "host", "origin", "cdn", "assets_base",
	"publicpath", "service_worker", "serviceworker", "manifest", "route_catalog",
	"service_catalog", "integration_spec",
}

func extractJSON(base *url.URL, body []byte, m *Material) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return
	}
	walkConfig(base, "", v, m, 0)
}

func walkConfig(base *url.URL, key string, v any, m *Material, depth int) {
	if depth > 8 {
		return
	}
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			walkConfig(base, k, child, m, depth+1)
		}
	case []any:
		for _, child := range x {
			walkConfig(base, key, child, m, depth+1)
		}
	case string:
		if !keyLooksLikeBase(key) {
			return
		}
		if abs := resolveSameOrigin(base, x); abs != "" {
			m.Services = append(m.Services, strings.TrimRight(abs, "/"))
		}
	}
}

func keyLooksLikeBase(key string) bool {
	k := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	if k == "" {
		return false
	}
	for _, want := range configKeys {
		if strings.Contains(k, want) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// SourceMap
// ---------------------------------------------------------------------------

func extractSourceMap(base *url.URL, body []byte, m *Material) {
	var sm struct {
		Sources       []string `json:"sources"`
		SourceRoot    string   `json:"sourceRoot"`
		SourcesContent []string `json:"sourcesContent"`
	}
	if err := json.Unmarshal(body, &sm); err != nil {
		return
	}
	// sourcesContent 才是真正有价值的：还原后的未压缩源码。
	// 这里不展开成文件（那是脚本层的活），只把内容当 JS 再抽一遍。
	for _, src := range sm.SourcesContent {
		if strings.TrimSpace(src) == "" {
			continue
		}
		extractJS(base, src, m, true)
	}
	// sources 里的路径只用于判断「是否有 node_modules / 开发者路径」这类线索，
	// 不产出端点——它们不是可请求的资源。
	for _, s := range sm.Sources {
		if strings.Contains(s, "node_modules/") {
			m.addSignal("dep-tree")
			break
		}
	}
}

// ---------------------------------------------------------------------------
// OpenAPI / Swagger
// ---------------------------------------------------------------------------

var httpVerbs = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true, "delete": true,
	"head": true, "options": true, "trace": true,
}

func extractOpenAPI(base *url.URL, body []byte, m *Material) {
	var doc struct {
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
		BasePath string                    `json:"basePath"`
		Host     string                    `json:"host"`
		Schemes  []string                  `json:"schemes"`
		Paths    map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return
	}
	for _, s := range doc.Servers {
		if abs := resolveSameOrigin(base, s.URL); abs != "" {
			m.Services = append(m.Services, strings.TrimRight(abs, "/"))
		}
	}
	if doc.Host != "" {
		scheme := "https"
		if len(doc.Schemes) > 0 {
			scheme = doc.Schemes[0]
		}
		if abs := resolveSameOrigin(base, scheme+"://"+doc.Host+doc.BasePath); abs != "" {
			m.Services = append(m.Services, strings.TrimRight(abs, "/"))
		}
	}
	prefix := strings.TrimRight(doc.BasePath, "/")
	// 端点要相对 OpenAPI 自己声明的 base 拼，而不是相对 .json 文件的位置：
	// /openapi.json 配 servers[0].url=/api 时，真实端点是 /api/users；
	// 若拿文件 URL 当基准解析 "/users" 会得到 /users（少一层前缀，请求必 404）。
	// 因此这里做路径拼接而不是 ResolveReference——OpenAPI 的 path 恒以 / 开头，
	// 引用解析会把它当绝对路径而丢掉 server 的路径前缀。
	apiScheme, apiHost, apiPath := base.Scheme, base.Host, ""
	for _, s := range doc.Servers {
		if s.URL == "" {
			continue
		}
		if abs, err := url.Parse(s.URL); err == nil {
			if resolved := base.ResolveReference(abs); sameOrigin(base, resolved) {
				apiScheme, apiHost, apiPath = resolved.Scheme, resolved.Host, resolved.Path
			}
		}
		break
	}
	if apiPath == "" {
		apiPath = prefix
	} else {
		apiPath = strings.TrimRight(apiPath, "/") + prefix
	}
	for p, ops := range doc.Paths {
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		target := apiScheme + "://" + apiHost + apiPath + p
		if u, err := url.Parse(target); err != nil || !sameOrigin(base, u) {
			continue
		}
		for verb, raw := range ops {
			if !httpVerbs[strings.ToLower(verb)] {
				continue
			}
			c := Candidate{Method: strings.ToUpper(verb), URL: target, Source: "openapi"}
			c.Params = openAPIParams(raw)
			m.Endpoints = append(m.Endpoints, c)
		}
	}
}

// openAPIParams 从 operation 对象里取 parameters（含 path/query/header 三级）。
// body 参数在 v2/v3 里形状不同，这里只取两版都稳定的 parameters 数组。
func openAPIParams(raw any) []map[string]any {
	op, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	list, ok := op["parameters"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		p, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := p["name"].(string)
		loc, _ := p["in"].(string)
		if name == "" {
			continue
		}
		typ := "string"
		if schema, ok := p["schema"].(map[string]any); ok {
			if t, ok := schema["type"].(string); ok && t != "" {
				typ = t
			}
		} else if t, ok := p["type"].(string); ok && t != "" {
			typ = t
		}
		out = append(out, map[string]any{"location": loc, "name": name, "type": typ})
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
