// Package webtest 是 Web 侧的可复现实验原语（W1–W3 / T-16、T-17）。
//
// 此前 Web 测试的「一次验证」是：worker 发一个请求，读响应，下结论。这条路径
// 有三个结构性缺陷，都不是靠提示词能补的：
//
//  1. **没有对照。** 「返回 200 且页面里有 SQL 报错」可能来自我们注入的 payload，
//     也可能来自目标本来就坏。没有「同一请求、去掉 payload」的对照，就无法区分。
//  2. **变异不止一个。** 手工验证时经常同时改了 payload、加了头、换了身份。
//     一旦结果变化，归因不了是哪一处造成的。
//  3. **覆盖不可见。** 测过 /api/users/{id} 的 GET，不等于测过它的 DELETE，
//     也不等于用低权身份测过。把它们算成「这个接口测过了」，覆盖率就是自欺。
//
// 本包给出三个小原语，分别对应上面三条：Replay（单点变异）、Compare（对照 +
// 稳定性判定）、Frontier（分轴覆盖 + 来源）。它们都是纯函数/纯内存结构，
// 不依赖 HTTP 客户端，所以能被单测钉住语义，再被任何执行层复用。
package webtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
)

// ParamKind 是参数出现的位置。
type ParamKind string

const (
	ParamPath   ParamKind = "path"
	ParamQuery  ParamKind = "query"
	ParamBody   ParamKind = "body"
	ParamHeader ParamKind = "header"
	ParamCookie ParamKind = "cookie"
)

// Param 是一个参数槽。
type Param struct {
	Name string    `json:"name"`
	Kind ParamKind `json:"kind"`
	// Type 是粗粒度类型（string/int/json/bool），用于判断「这个 payload 合不合法」。
	Type string `json:"type,omitempty"`
	// Sample 是一个已知可用的取值（对照组用）。
	Sample string `json:"sample,omitempty"`
}

// Template 是一个请求模板：方法 + 路由模板 + 参数集合。
//
// RouteTemplate 用 `{name}` 占位（如 `/api/users/{id}`）。占位而不是具体值，
// 是为了让「同一路由的不同 id」在覆盖上合并——否则覆盖单元会被无限实例化，
// 覆盖率永远接近 0。
type Template struct {
	Method        string  `json:"method"`
	RouteTemplate string  `json:"route_template"`
	Params        []Param `json:"params,omitempty"`
	// IdentityRef 是发请求用的身份引用（authstate.Identity.Ref()）。
	IdentityRef string `json:"identity_ref,omitempty"`
	// Source 说明这个模板从哪来（traffic:<id> / openapi / sourcemap / manual）。
	// 保留来源是为了让「覆盖了但不知道从哪来的」可被发现。
	Source string `json:"source,omitempty"`
}

// Key 返回模板自身的规范键（不含身份与测试族——那两轴在 Frontier 里）。
func (t Template) Key() string {
	return strings.ToUpper(strings.TrimSpace(t.Method)) + " " + normalizeRoute(t.RouteTemplate)
}

// ParamSchema 返回参数签名的稳定表示：按 (位置, 名字) 排序后的紧凑串。
//
// 排序是必须的：同一个接口的参数集合如果顺序不同就算两个覆盖单元，
// 覆盖率会被无意义地稀释。
func (t Template) ParamSchema() string {
	ps := append([]Param(nil), t.Params...)
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Kind != ps[j].Kind {
			return ps[i].Kind < ps[j].Kind
		}
		return ps[i].Name < ps[j].Name
	})
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		parts = append(parts, string(p.Kind)+":"+p.Name)
	}
	return strings.Join(parts, ",")
}

// normalizeRoute 归一化路由：去掉查询串与尾部斜杠，占位符统一成 {p}。
func normalizeRoute(r string) string {
	r = strings.TrimSpace(r)
	if i := strings.IndexByte(r, '?'); i >= 0 {
		r = r[:i]
	}
	r = strings.TrimSuffix(r, "/")
	if r == "" {
		r = "/"
	}
	// 把 {id} / {userId} / :id 统一成 {p}，避免「同一路由不同占位名」算两处。
	var b strings.Builder
	depth := 0
	for _, ch := range r {
		switch ch {
		case '{':
			depth++
			b.WriteRune('{')
		case '}':
			if depth > 0 {
				depth--
			}
			b.WriteRune('}')
		default:
			if depth > 0 {
				continue // 占位名丢掉
			}
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// Request 是一次具体请求（可变异的单位）。
type Request struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers [][2]string `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

// Clone 深拷贝。
func (r Request) Clone() Request {
	out := r
	out.Headers = append([][2]string(nil), r.Headers...)
	out.Body = append([]byte(nil), r.Body...)
	return out
}

// Header 读一个头（大小写不敏感）。
func (r Request) Header(name string) (string, bool) {
	for _, kv := range r.Headers {
		if strings.EqualFold(kv[0], name) {
			return kv[1], true
		}
	}
	return "", false
}

// Mutation 是一次单点变异：只改一个东西。
type Mutation struct {
	// Kind 是变异类别：path / query / body / header / cookie / method / identity。
	Kind string `json:"kind"`
	// Name 是参数名（identity 类变异可为空）。
	Name string `json:"name,omitempty"`
	// From / To 是变异前后的值。From 只用于校验（防止在错误的基线上做变异）。
	From    string `json:"from,omitempty"`
	FromSet bool   `json:"-"`
	To      string `json:"to,omitempty"`
	// Replace 为 true 时把整个 body 换成 To（而不是替换某个参数值）。
	Replace bool `json:"replace,omitempty"`
}

func (m *Mutation) UnmarshalJSON(raw []byte) error {
	type plain Mutation
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	_, value.FromSet = fields["from"]
	*m = Mutation(value)
	return nil
}

func (m Mutation) hasFrom() bool { return m.FromSet || m.From != "" }

// Replay 在 base 上施加**一个**变异，返回变体请求。
//
// 单点变异是本包最重要的约束：一次改多处，结果变化就无法归因。所以这里
// 拒绝「Replace=true 且还带 Name」这种含糊用法，也拒绝空 Kind。
func Replay(base Request, m Mutation) (Request, error) {
	kind := strings.ToLower(strings.TrimSpace(m.Kind))
	if kind == "" {
		return Request{}, fmt.Errorf("变异必须指定 kind（path/query/body/header/cookie/method/identity）")
	}
	out := base.Clone()
	if m.Replace && (kind != "body" || m.Name != "") {
		return Request{}, fmt.Errorf("replace 仅用于不带 name 的整体 body 变异")
	}
	switch kind {
	case "path":
		u, err := url.Parse(out.URL)
		if err != nil || m.From == "" || !strings.Contains(u.EscapedPath(), m.From) {
			return Request{}, fmt.Errorf("基线 URL %q 里没有 %q：不要在错误的基线上做变异", out.URL, m.From)
		}
		rawPath := strings.Replace(u.EscapedPath(), m.From, m.To, 1)
		path, err := url.PathUnescape(rawPath)
		if err != nil || rawPath == u.EscapedPath() || strings.ContainsAny(rawPath, "?#") {
			return Request{}, fmt.Errorf("path 变异无变化或包含非法路径编码/查询分隔符")
		}
		u.Path, u.RawPath = path, rawPath
		out.URL = u.String()
		return out, nil

	case "query":
		if strings.TrimSpace(m.Name) == "" {
			return Request{}, fmt.Errorf("query 变异必须给参数名")
		}
		u, err := url.Parse(out.URL)
		if err != nil {
			return Request{}, err
		}
		u.RawQuery, err = mutatePairs(u.RawQuery, "&", m, true)
		if err != nil {
			return Request{}, err
		}
		out.URL = u.String()
		return out, nil

	case "body":
		if m.Replace {
			if (m.hasFrom() && string(out.Body) != m.From) || string(out.Body) == m.To {
				return Request{}, fmt.Errorf("body 基线不匹配或变异未改变正文")
			}
			out.Body = []byte(m.To)
			return out, nil
		}
		if strings.TrimSpace(m.Name) == "" {
			return Request{}, fmt.Errorf("body 变异必须给参数名（或用 replace=true 整体替换）")
		}
		body, err := mutatePairs(string(out.Body), "&", m, false)
		if err != nil {
			return Request{}, err
		}
		out.Body = []byte(body)
		return out, nil

	case "header":
		if strings.TrimSpace(m.Name) == "" {
			return Request{}, fmt.Errorf("header 变异必须给头名")
		}
		count := 0
		for _, kv := range out.Headers {
			if strings.EqualFold(kv[0], m.Name) {
				count++
			}
		}
		cur, exists := out.Header(m.Name)
		if count > 1 || (m.hasFrom() && (!exists || cur != m.From)) || (exists && cur == m.To) {
			return Request{}, fmt.Errorf("header 基线不匹配、重复或变异未改变值")
		}
		out.Headers = setHeader(out.Headers, m.Name, m.To)
		return out, nil

	case "cookie":
		if strings.TrimSpace(m.Name) == "" {
			return Request{}, fmt.Errorf("cookie 变异必须给 Cookie 名")
		}
		cur, _ := out.Header("Cookie")
		count := 0
		for _, kv := range out.Headers {
			if strings.EqualFold(kv[0], "Cookie") {
				count++
			}
		}
		if count > 1 {
			return Request{}, fmt.Errorf("多个 Cookie 头的基线不明确")
		}
		cookie, err := mutatePairs(cur, ";", m, true)
		if err != nil {
			return Request{}, err
		}
		out.Headers = setHeader(out.Headers, "Cookie", cookie)
		return out, nil

	case "method":
		if strings.TrimSpace(m.To) == "" {
			return Request{}, fmt.Errorf("method 变异必须给目标方法")
		}
		out.Method = strings.ToUpper(strings.TrimSpace(m.To))
		if (m.hasFrom() && !strings.EqualFold(base.Method, m.From)) || out.Method == strings.ToUpper(base.Method) {
			return Request{}, fmt.Errorf("method 基线不匹配或变异未改变方法")
		}
		return out, nil

	case "identity":
		// 身份变异改的是 Authorization / Cookie 两组头，属于「同一类变异」，
		// 不算多点变异。具体值由调用方从 authstate 取好放进 To/Headers。
		return Request{}, fmt.Errorf("identity 变异请用 authstate 的 HeaderPairs 重建请求，不要在这里替换单个头")
	}
	return Request{}, fmt.Errorf("未知变异类别 %q", m.Kind)
}

// mutatePairs changes one raw wire value without re-encoding unrelated fields.
// From is the original wire value; a nonempty From must match exactly.
// A missing body field is always an error; query/cookie additions require no From.
func mutatePairs(raw, separator string, m Mutation, allowAdd bool) (string, error) {
	parts := strings.Split(raw, separator)
	index := -1
	for i, part := range parts {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		decoded, err := url.QueryUnescape(name)
		if separator == ";" {
			decoded, err = name, nil
		}
		if err != nil || decoded != m.Name {
			continue
		}
		if index >= 0 {
			return "", fmt.Errorf("参数 %q 重复，不能确定单点变异位置", m.Name)
		}
		if (m.hasFrom() || !allowAdd) && value != m.From {
			return "", fmt.Errorf("参数 %q 的原值不匹配", m.Name)
		}
		if value == m.To {
			return "", fmt.Errorf("参数 %q 变异没有改变值", m.Name)
		}
		index = i
	}
	if index < 0 {
		if !allowAdd || m.hasFrom() {
			return "", fmt.Errorf("基线缺少参数 %q", m.Name)
		}
		name := m.Name
		if separator == "&" {
			name = url.QueryEscape(name)
		}
		if raw == "" {
			return name + "=" + m.To, nil
		}
		return raw + separator + name + "=" + m.To, nil
	}
	name, _, _ := strings.Cut(parts[index], "=")
	parts[index] = name + "=" + m.To
	return strings.Join(parts, separator), nil
}

func setQuery(rawURL, name, value string) string {
	base, query := rawURL, ""
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		base, query = rawURL[:i], rawURL[i+1:]
	}
	pairs := []string{}
	found := false
	if query != "" {
		for _, p := range strings.Split(query, "&") {
			if p == "" {
				continue
			}
			k := p
			if i := strings.IndexByte(p, '='); i >= 0 {
				k = p[:i]
			}
			if k == name {
				pairs = append(pairs, name+"="+value)
				found = true
				continue
			}
			pairs = append(pairs, p)
		}
	}
	if !found {
		pairs = append(pairs, name+"="+value)
	}
	return base + "?" + strings.Join(pairs, "&")
}

func setHeader(hs [][2]string, name, value string) [][2]string {
	out := make([][2]string, 0, len(hs)+1)
	replaced := false
	for _, kv := range hs {
		if strings.EqualFold(kv[0], name) {
			out = append(out, [2]string{kv[0], value})
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, [2]string{name, value})
	}
	return out
}

func setCookieValue(cookieHeader, name, value string) string {
	parts := []string{}
	found := false
	for _, p := range strings.Split(cookieHeader, ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k := p
		if i := strings.IndexByte(p, '='); i >= 0 {
			k = p[:i]
		}
		if k == name {
			parts = append(parts, name+"="+value)
			found = true
			continue
		}
		parts = append(parts, p)
	}
	if !found {
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

// Response 是一次响应的最小视图。
type Response struct {
	Truncated bool        `json:"truncated,omitempty"`
	Status    int         `json:"status"`
	Headers   http.Header `json:"headers,omitempty"`
	Body      []byte      `json:"body,omitempty"`
}

// Verdict 是对照实验的结论。
type Verdict string

const (
	// VerdictDifferent 变体与对照有实质差异——差异可归因于那一次变异。
	VerdictDifferent Verdict = "different"
	// VerdictSame 无实质差异：这次变异没有改变目标行为（阴性）。
	VerdictSame Verdict = "same"
	// VerdictInconclusive 无法判定（对照不稳定、双方 5xx、响应被截断等）。
	VerdictInconclusive Verdict = "inconclusive"
)

// Comparison 是一次对照实验的完整结论。
type Comparison struct {
	Verdict Verdict  `json:"verdict"`
	Reason  string   `json:"reason"`
	Signals []string `json:"signals,omitempty"`
}

// FindingState preserves compatibility without claiming vulnerability verification.
// A response difference (or its absence) alone cannot prove or disprove a finding.
func (c Comparison) FindingState() string {
	return "inconclusive"
}

// noiseFloor 是「视为噪声」的默认正文差异比例（1%）。
//
// 取 1% 而不是 5%：布尔型注入的差异常常只占页面的千分之几（「用户不存在」与
// 「密码错误」只差几个字）。阈值定得太宽，最有价值的差异会被当成噪声吞掉；
// 定得太窄，CSRF token 这类每次都变的字段会让所有对照都判「不同」。
// 真正的解法不是调阈值，而是用 CompareWithNoise 先测出这个站点的实际抖动。
const noiseFloor = 0.01

// byteDiffRatio 返回两段正文的差异比例（0 = 完全相同，1 = 完全不同）。
//
// 逐字节比较 + 长度差计入分子。刻意不做编辑距离/相似度算法：O(n) 的字节比较
// 对 100KB 级页面已经够用，而编辑距离会把「一个字符插入导致后续全部错位」
// 算成巨大差异，反而不适合判断「页面结构变没变」。
func byteDiffRatio(a, b []byte) float64 {
	la, lb := len(a), len(b)
	maxLen := la
	if lb > maxLen {
		maxLen = lb
	}
	if maxLen == 0 {
		return 0
	}
	n := la
	if lb < n {
		n = lb
	}
	diff := maxLen - n // 长度差
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			diff++
		}
	}
	return float64(diff) / float64(maxLen)
}

// Stable 判断同一请求的两次响应是否稳定（对照组本身可信）。
//
// 不稳定的对照组意味着「目标每次返回都不一样」，此时任何差异都无法归因到
// 变异——所以调用方应先判 Stable，再判 Compare。
func Stable(a, b Response) bool {
	return !a.Truncated && !b.Truncated && a.Status == b.Status && reflect.DeepEqual(comparisonHeaders(a.Headers), comparisonHeaders(b.Headers)) && byteDiffRatio(a.Body, b.Body) <= noiseFloor
}

// Compare 对照 control 与 variant（单次对照，使用默认噪声下限）。
//
// 判定顺序（每一层都为了避开一类假阳性）：
//  1. 任一方 5xx → inconclusive。服务端自己出错了，差异说明不了任何事。
//  2. 状态码不同 → different（最强信号）。
//  3. 正文差异超过默认噪声下限 → different。
//  4. 其余 → same。
//
// **局限（必须知道）**：默认 1% 是猜的。若目标页面本身抖动很大（每次渲染都变），
// 单次对照会判成 different（假阳性）；若差异很小但真实（布尔盲注），可能被判成
// same（假阴性）。要避免这两头，用 CompareWithNoise：先对同一请求跑两次测出
// 这个站点自己的抖动，再拿变体去比。
func Compare(control, variant Response) Comparison {
	return CompareWithNoise(control, control, variant)
}

// CompareWithNoise 用「同一请求的两次对照」标定噪声，再判定变体。
//
// controlA 与 controlB 是**完全相同的请求**跑两次。它们的差异就是这台目标的
// 抖动下限。变体与对照的差异必须显著超过它，才算「变异真的改变了行为」。
//
// 这是差分实验的正确形式：没有噪声标定的对照，判据只是猜。
func CompareWithNoise(controlA, controlB, variant Response) Comparison {
	if controlA.Truncated || controlB.Truncated || variant.Truncated {
		return Comparison{Verdict: VerdictInconclusive, Reason: "响应正文被截断：证据不完整，不能判定差异"}
	}
	if controlA.Status >= 500 || controlB.Status >= 500 || variant.Status >= 500 {
		return Comparison{Verdict: VerdictInconclusive,
			Reason: "任一次响应 5xx：服务端自身出错，差异无法归因到变异"}
	}
	if controlA.Status != controlB.Status {
		return Comparison{Verdict: VerdictInconclusive,
			Reason: fmt.Sprintf("对照组自身不稳定（同一请求两次返回 %d / %d）：先排除目标抖动再下结论",
				controlA.Status, controlB.Status)}
	}
	if !reflect.DeepEqual(comparisonHeaders(controlA.Headers), comparisonHeaders(controlB.Headers)) {
		return Comparison{Verdict: VerdictInconclusive, Reason: "对照组响应头不稳定，无法归因变体差异", Signals: []string{"headers"}}
	}
	noise := byteDiffRatio(controlA.Body, controlB.Body)
	if noise > 0.05 {
		return Comparison{Verdict: VerdictInconclusive,
			Reason: fmt.Sprintf("对照组正文抖动 %.1f%%（超过 5%%）：这个目标不适合做单次差分，需要抓稳定特征再比",
				noise*100)}
	}
	// 阈值 = max(噪声的 2 倍, 默认下限)：既尊重目标自身抖动，也不放过小差异。
	threshold := noise * 2
	if threshold < noiseFloor {
		threshold = noiseFloor
	}
	if controlA.Status != variant.Status {
		return Comparison{Verdict: VerdictDifferent,
			Reason:  fmt.Sprintf("状态码不同（对照 %d → 变体 %d）", controlA.Status, variant.Status),
			Signals: []string{"status"}}
	}
	if !reflect.DeepEqual(comparisonHeaders(controlA.Headers), comparisonHeaders(variant.Headers)) {
		return Comparison{Verdict: VerdictDifferent, Reason: "响应头不同", Signals: []string{"headers"}}
	}
	delta := byteDiffRatio(controlA.Body, variant.Body)
	if delta > threshold {
		return Comparison{Verdict: VerdictDifferent,
			Reason: fmt.Sprintf("正文差异 %.2f%% 超过噪声阈值 %.2f%%（对照组自身抖动 %.2f%%）",
				delta*100, threshold*100, noise*100),
			Signals: []string{"body"}}
	}
	return Comparison{Verdict: VerdictSame,
		Reason: fmt.Sprintf("正文差异 %.2f%% 未超过噪声阈值 %.2f%%：这次变异没有改变目标行为",
			delta*100, threshold*100)}
}

// Digest 返回响应的稳定摘要（用于留痕与去重）。
func Digest(r Response) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%t\n", r.Status, r.Truncated)
	headers := comparisonHeaders(r.Headers)
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(h, "%q:%q\n", key, headers[key])
	}
	h.Write(r.Body)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Exclude only known transport/time noise. Keep Location, Set-Cookie and auth
// headers: changing identity or redirect destinations is experimental evidence.
func comparisonHeaders(headers http.Header) map[string][]string {
	out := map[string][]string{}
	for key, values := range headers {
		key = strings.ToLower(key)
		switch key {
		case "date", "age", "connection", "keep-alive", "transfer-encoding":
			continue
		}
		out[key] = append(out[key], values...)
	}
	return out
}
