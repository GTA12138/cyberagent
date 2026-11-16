package server

// enrich_semantic_parse.go 是语义层的**纯函数**出口：模型回了什么文字，
// 最终能变成哪些候选端点，完全由这里的规则决定。
//
// 把它从 enrich_semantic.go 拆出来的理由是可评测：T-21 要求「接模型之前先有
// 评测基线」。模型调用不可复现（同一份材料两次可能给不同结果），但
// 「这段文字能不能变成候选」是可复现的——golden fixture 钉住这一步，
// 接上模型之后的行为变化才有对照物。

import (
	"encoding/json"
	"net/url"
	"strings"

	"chengjian.local/chengjian/enrich"
)

// semanticMethods 是允许出现在候选里的方法。白名单而不是黑名单：
// 未知方法名（"FETCH"、"request"）说明模型在编，直接丢。
var semanticMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true,
	"DELETE": true, "HEAD": true, "OPTIONS": true,
}

// semanticParamLocations 与 db.ParamItem.location 的取值域对齐。
// 不一致的 location 会让参数在 UI 上落进「未知位置」，等于没记。
var semanticParamLocations = map[string]bool{
	"query": true, "body": true, "header": true, "cookie": true, "path": true,
}

type semanticCandidate struct {
	Method string           `json:"method"`
	URL    string           `json:"url"`
	Params []map[string]any `json:"params"`
	Why    string           `json:"why"`
}

// parseSemanticCandidates 把模型输出解析成可入库的候选。
// 返回保留下来的候选与被丢弃条目的原因（丢弃原因用于日志与评测断言，
// 不能静默吞掉——「模型总在编跨域地址」这件事只有统计丢弃原因才看得出来）。
func parseSemanticCandidates(raw, sourceURL string) ([]enrich.Candidate, []string) {
	body := extractJSONPayload(raw)
	if body == "" {
		return nil, []string{"no_json_payload"}
	}
	var items []semanticCandidate
	if err := json.Unmarshal([]byte(body), &items); err != nil {
		// 兼容 {"endpoints":[...]} 这种包了一层的写法。模型经常自作主张加一层，
		// 为此丢掉整份产出不划算——但这层兼容只认这一个键，不做无限猜测。
		var wrap struct {
			Endpoints []semanticCandidate `json:"endpoints"`
			Items     []semanticCandidate `json:"items"`
			Results   []semanticCandidate `json:"results"`
		}
		if werr := json.Unmarshal([]byte(body), &wrap); werr != nil {
			return nil, []string{"json_unmarshal: " + firstLineOf(err.Error())}
		}
		switch {
		case len(wrap.Endpoints) > 0:
			items = wrap.Endpoints
		case len(wrap.Items) > 0:
			items = wrap.Items
		case len(wrap.Results) > 0:
			items = wrap.Results
		default:
			return nil, []string{"empty_payload"}
		}
	}

	base, baseErr := url.Parse(sourceURL)
	seen := map[string]bool{}
	out := make([]enrich.Candidate, 0, len(items))
	dropped := []string{}
	for _, it := range items {
		if len(out) >= semanticMaxCandidates {
			dropped = append(dropped, "over_cap")
			break
		}
		method := strings.ToUpper(strings.TrimSpace(it.Method))
		if !semanticMethods[method] {
			dropped = append(dropped, "bad_method:"+truncateForSnapshot(it.Method, 16))
			continue
		}
		path := normalizeSemanticPath(strings.TrimSpace(it.URL))
		if path == "" {
			dropped = append(dropped, "empty_url")
			continue
		}
		// 跨源一律丢：语义层没有扩域权。enrich 侧还会再判一次同源，
		// 这里先丢是为了让「丢弃原因」能反映模型的真实行为。
		if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
			if baseErr != nil || !sameOriginLoose(base, path) {
				dropped = append(dropped, "cross_origin:"+truncateForSnapshot(path, 48))
				continue
			}
		} else if !strings.HasPrefix(path, "/") {
			dropped = append(dropped, "not_absolute_path:"+truncateForSnapshot(path, 48))
			continue
		}
		if looksLikeStaticAsset(path) {
			dropped = append(dropped, "static_asset:"+truncateForSnapshot(path, 48))
			continue
		}
		key := method + " " + path
		if seen[key] {
			dropped = append(dropped, "duplicate:"+truncateForSnapshot(key, 64))
			continue
		}
		seen[key] = true
		out = append(out, enrich.Candidate{
			Method: method,
			URL:    path,
			Params: normalizeSemanticParams(it.Params),
		})
	}
	return out, dropped
}

// normalizeSemanticParams 只保留 location 合法、name 非空的参数。
// 参数名编造的风险低于端点编造（它不会造出一条不存在的路由），但 location
// 错了会让参数在界面上无处安放，所以宁缺勿滥。
func normalizeSemanticParams(in []map[string]any) []map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(in))
	for _, p := range in {
		name, _ := p["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		loc, _ := p["location"].(string)
		loc = strings.ToLower(strings.TrimSpace(loc))
		if !semanticParamLocations[loc] {
			loc = "query"
		}
		typ, _ := p["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		if typ == "" {
			typ = "string"
		}
		out = append(out, map[string]any{"location": loc, "name": name, "type": typ})
		if len(out) >= 24 {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeSemanticPath 去掉包裹引号与行内空白，保留查询串（参数信息在里面）。
func normalizeSemanticPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "\"'`")
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		// "/" 是页面导航，不是端点。单独放行它没有意义：它一定会被
		// 确定性抽取先拿到，语义层再报一次只是重复。
		return ""
	}
	if strings.HasPrefix(p, "javascript:") || strings.HasPrefix(p, "data:") || strings.HasPrefix(p, "#") {
		return ""
	}
	return p
}

// sameOriginLoose 只比较 scheme+host，忽略路径与端口写法差异之外的细节。
// 语义层给的绝对 URL 只要主机对得上就算同源，后续 enrich 的 sameOrigin 会再判一次。
func sameOriginLoose(base *url.URL, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, base.Host) && strings.EqualFold(u.Scheme, base.Scheme)
}

// looksLikeStaticAsset 与 enrich 的 assetExt 判据一致（这里不 import 未导出的
// 正则，宁可各写一份也不导出内部实现——导出的代价是语义层和抽取层的边界会糊）。
func looksLikeStaticAsset(p string) bool {
	lower := strings.ToLower(p)
	if i := strings.IndexAny(lower, "?#"); i >= 0 {
		lower = lower[:i]
	}
	for _, ext := range []string{
		".css", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico",
		".woff", ".woff2", ".ttf", ".eot", ".mp4", ".webm", ".webp",
		".pdf", ".zip", ".gz", ".tar", ".exe", ".dmg", ".map", ".js",
	} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// extractJSONPayload 从可能带代码围栏/前后缀说明的文字里取出 JSON 主体。
// 取第一个 '[' 到最后一个 ']'（或 '{' 到 '}'），不尝试修复损坏的 JSON——
// 半修好的 JSON 比丢弃更危险，它会产出结构正确但内容错位的候选。
func extractJSONPayload(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// 去掉 ```json ... ``` 围栏
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	start := strings.IndexAny(s, "[{")
	if start < 0 {
		return ""
	}
	open := s[start]
	closing := byte(']')
	if open == '{' {
		closing = '}'
	}
	end := strings.LastIndexByte(s, closing)
	if end <= start {
		return ""
	}
	return strings.TrimSpace(s[start : end+1])
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
