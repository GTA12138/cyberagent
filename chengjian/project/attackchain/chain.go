// Package attackchain reconstructs an evidence-backed view of stored findings.
// It never executes a PoC, calls a model, or treats a report claim as verification.
package attackchain

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type Source struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Locator string `json:"locator"`
	Text    string `json:"text"`
	Line    int    `json:"line,omitempty"`
}

type Node struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Title     string   `json:"title"`
	Summary   string   `json:"summary"`
	Basis     string   `json:"basis"` // reported | recorded | missing
	Sources   []string `json:"sources"`
	FindingID string   `json:"finding_id,omitempty"`
	Reference string   `json:"reference,omitempty"`
	Items     []string `json:"items"`
	Action    string   `json:"action,omitempty"`
	Endpoint  string   `json:"endpoint,omitempty"`
	Ordinal   int      `json:"ordinal,omitempty"`
}

type Edge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"` // narrative | reference | recorded
	Label  string `json:"label"`
}

type Chain struct {
	FindingID    string   `json:"finding_id"`
	Title        string   `json:"title"`
	Revision     string   `json:"revision"`
	Nodes        []Node   `json:"nodes"`
	Edges        []Edge   `json:"edges"`
	Sources      []Source `json:"sources"`
	Warnings     []string `json:"warnings"`
	TrafficCount int      `json:"traffic_count"`
}

type Finding struct {
	ID                              int64
	Name, Summary, Evidence, Report string
	TrafficCount                    int
}

type Reference struct {
	Kind string
	ID   int64
}
type Related struct {
	Reference              Reference
	FindingID              int64
	Title, Summary, Detail string
}

var (
	referencePattern = regexp.MustCompile(`(?i)\b(finding|fact|intent)[ _#：:]*([0-9]+(?:\s*[/、,，]\s*[0-9]+)*)`)
	numberPattern    = regexp.MustCompile(`[0-9]+`)
	headingPattern   = regexp.MustCompile(`^#{1,6}\s+(.+)$`)
	stepPattern      = regexp.MustCompile(`^(?:\*\*)?(?:步骤\s*[一二三四五六七八九十0-9]+\s*[：:、.．-]?|[0-9]+[)）.、]\s*|[①②③④⑤⑥⑦⑧⑨⑩⑪⑫⑬⑭⑮⑯⑰⑱⑲⑳]\s*)(.+)$`)
	filePattern      = regexp.MustCompile(`(?i)(?:https?://[^\s<>"'` + "`" + `（）()，；]+|(?:\.?\.?/)?[a-zA-Z0-9_@.-]+(?:/[a-zA-Z0-9_@.-]+)*\.[a-zA-Z0-9]+(?:\?[^\s<>"'` + "`" + `）)；，]*)?)`)
	requestPattern   = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+(https?://[^\s` + "`" + `<>]+|/[^\s` + "`" + `<>]+)`)
)

func actionFor(title, body string) string {
	text := strings.ToLower(title)
	for _, rule := range []struct {
		action string
		words  []string
	}{
		{"compare", []string{"对照", "比较", "边界", "control"}},
		{"credential", []string{"密钥", "凭据", "解密", "secret", "token"}},
		{"file", []string{"脚本", "前端", ".js", "js 文件", "网页"}},
		{"write", []string{"写入", "新增", "篡改", "上传", "修改", "保存", "覆盖"}},
		{"read", []string{"读取", "下载", "获取", "枚举", "查询", "返回"}},
		{"credential", []string{"签名"}},
		{"auth", []string{"认证", "鉴权", "绕过", "登录", "会话"}},
	} {
		for _, word := range rule.words {
			if strings.Contains(text, word) {
				return rule.action
			}
		}
	}
	if requestPattern.MatchString(body) {
		return "request"
	}
	return "investigate"
}

// References explicitly preserves the exploration-node namespace used by reports.
// "finding79/80" names nodes 79 and 80, not rows 79 and 80 of findings.
func References(text string) []Reference {
	seen := map[Reference]bool{}
	out := []Reference{}
	for _, match := range referencePattern.FindAllStringSubmatch(text, -1) {
		for _, raw := range numberPattern.FindAllString(match[2], -1) {
			id, _ := strconv.ParseInt(raw, 10, 64)
			r := Reference{Kind: strings.ToLower(match[1]), ID: id}
			if id > 0 && !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	return out
}

type section struct {
	title, body string
	line        int
	end, level  int
	document    string
}

func sections(text string) []section {
	lines := strings.Split(text, "\n")
	out := []section{{title: "报告正文", line: 1, end: len(lines)}}
	active := []int{0}
	code := false
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			code = !code
		}
		match := headingPattern.FindStringSubmatch(line)
		if !code && match != nil {
			level := strings.IndexFunc(line, unicode.IsSpace)
			for len(active) > 1 && out[active[len(active)-1]].level >= level {
				out[active[len(active)-1]].end = index
				active = active[:len(active)-1]
			}
			for _, i := range active {
				out[i].body += line + "\n"
			}
			out = append(out, section{title: match[1], line: index + 2, level: level, end: len(lines)})
			active = append(active, len(out)-1)
		} else {
			for _, i := range active {
				out[i].body += line + "\n"
			}
		}
	}
	return out
}

func plain(text string) string {
	text = strings.NewReplacer("**", "", "`", "", "__", "", "\r", "").Replace(text)
	return strings.TrimSpace(text)
}

func excerpt(text string, size int) string {
	text = strings.Join(strings.Fields(plain(text)), " ")
	runes := []rune(text)
	if len(runes) > size {
		return string(runes[:size]) + "…"
	}
	return text
}

func selectSections(all []section, words ...string) []section {
	out := []section{}
	for _, s := range all {
		for _, word := range words {
			if strings.Contains(s.title, word) && strings.TrimSpace(s.body) != "" {
				contained := false
				for _, parent := range out {
					if s.line >= parent.line && s.end <= parent.end {
						contained = true
						break
					}
				}
				if contained {
					break
				}
				out = append(out, s)
				break
			}
		}
	}
	return out
}

func steps(text string) []section {
	out := []section{}
	var current *section
	code := false
	for index, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			code = !code
		}
		candidate := strings.TrimSpace(line)
		if heading := headingPattern.FindStringSubmatch(candidate); heading != nil {
			candidate = heading[1]
		}
		match := stepPattern.FindStringSubmatch(candidate)
		if !code && match != nil {
			out = append(out, section{title: strings.Trim(plain(match[1]), "*：: "), body: line + "\n", line: index + 1})
			current = &out[len(out)-1]
		} else if current != nil {
			current.body += line + "\n"
		}
	}
	return out
}

// Build organizes source excerpts. Narrative edges are presentation order only;
// report citations remain citations, rather than silently becoming proven prerequisites.
func Build(f Finding, related []Related, extra []Source, warnings []string) Chain {
	c := Chain{FindingID: strconv.FormatInt(f.ID, 10), Title: f.Name, Nodes: []Node{}, Edges: []Edge{}, Sources: []Source{}, Warnings: append([]string{}, warnings...), TrafficCount: f.TrafficCount}
	if c.Title == "" {
		c.Title = "漏洞 #" + c.FindingID
	}
	addSource := func(kind, label, text string, line int, document string) string {
		id := fmt.Sprintf("source-%d", len(c.Sources)+1)
		trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
		if line > 0 {
			line += strings.Count(text[:len(text)-len(trimmed)], "\n")
		}
		c.Sources = append(c.Sources, Source{ID: id, Kind: kind, Label: label, Locator: "finding:" + c.FindingID + "/" + document, Text: strings.TrimSpace(text), Line: line})
		return id
	}
	all := sections(f.Report)
	summaryID := ""
	if f.Summary != "" {
		summaryID = addSource("summary", "漏洞摘要", f.Summary, 0, "summary")
	}
	evidenceID := ""
	if f.Evidence != "" {
		evidenceID = addSource("evidence", "原始证据 / PoC", f.Evidence, 0, "evidence")
	}
	makeStage := func(id, kind, title string, selected []section, fallback string) Node {
		n := Node{ID: id, Kind: kind, Title: title, Basis: "reported", Sources: []string{}, Items: []string{}}
		for _, s := range selected {
			n.Sources = append(n.Sources, addSource("report", s.title, s.body, s.line, "report"))
			n.Items = append(n.Items, excerpt(s.body, 150))
		}
		if len(n.Sources) == 0 && fallback != "" {
			n.Summary = excerpt(fallback, 160)
			if summaryID != "" {
				n.Sources = append(n.Sources, summaryID)
			}
		} else if len(n.Items) > 0 {
			n.Summary = n.Items[0]
		}
		if len(n.Sources) == 0 {
			n.Basis = "missing"
			n.Summary = "现有记录未明确记载，请补充证据。"
		}
		return n
	}
	prereq := makeStage("prerequisites", "prerequisite", "前置条件", selectSections(all, "前置", "前提", "利用条件", "必要条件"), "")
	if prereq.Basis == "missing" && f.Summary != "" {
		// Preserve the exact summary as context; do not invent an unauthenticated prerequisite.
		prereq.Basis = "reported"
		prereq.Summary = excerpt(f.Summary, 160)
		prereq.Sources = []string{summaryID}
		prereq.Title = "前置条件与背景"
	}
	source := makeStage("origins", "source", "入口与信息来源", selectSections(all, "来源", "发现过程", "根因", "成因", "受影响范围"), "")
	fileSeen := map[string]bool{}
	for _, value := range filePattern.FindAllString(f.Report+"\n"+f.Evidence, -1) {
		value = strings.TrimRight(value, ".;，。；:")
		parsed, err := url.Parse(value)
		if err != nil {
			continue
		}
		ext := strings.ToLower(path.Ext(parsed.Path))
		if ext != ".js" && ext != ".mjs" && ext != ".html" && ext != ".htm" {
			continue
		}
		if fileSeen[value] {
			continue
		}
		fileSeen[value] = true
		source.Items = append(source.Items, value)
		// Cite the containing document, including the exact occurrence context.
		doc := f.Report
		document := "report"
		label := "报告中的网页 / JS 线索"
		if !strings.Contains(doc, value) {
			doc = f.Evidence
			document = "evidence"
			label = "证据中的网页 / JS 线索"
		}
		pos := strings.Index(doc, value)
		start := strings.LastIndex(doc[:pos], "\n") + 1
		end := strings.Index(doc[pos:], "\n")
		if end < 0 {
			end = len(doc) - pos
		}
		source.Sources = append(source.Sources, addSource("file", label, doc[start:pos+end], strings.Count(doc[:start], "\n")+1, document))
		if len(fileSeen) >= 20 {
			c.Warnings = append(c.Warnings, "网页 / JS 线索最多展示 20 项，其余请查看原始报告。")
			break
		}
	}
	if len(fileSeen) > 0 {
		source.Basis = "reported"
		source.Summary = fmt.Sprintf("%d 个网页 / JS 文件线索，点击查看引用上下文。", len(fileSeen))
	}
	inference := makeStage("inference", "inference", "分析与推断", selectSections(all, "分析", "原因", "原理", "判断", "根因", "成因"), "")
	verification := makeStage("verification", "verification", "验证与信息获取", selectSections(all, "复现步骤", "验证过程", "利用过程", "复现过程"), "")
	var extracted []section
	for _, s := range selectSections(all, "复现", "验证过程", "利用过程") {
		if strings.Contains(s.title, "PoC") {
			continue
		}
		parts := steps(s.body)
		for _, part := range parts {
			part.line += s.line - 1
			part.document = "report"
			extracted = append(extracted, part)
		}
		if len(extracted) > 0 {
			break
		}
	}
	if len(extracted) == 0 {
		extracted = steps(f.Evidence)
		for i := range extracted {
			extracted[i].document = "evidence"
		}
	}
	if len(extracted) == 0 && strings.TrimSpace(f.Evidence) != "" {
		// An unstructured document is a single record, never a fabricated execution sequence.
		extracted = []section{{title: "验证证据记录", body: f.Evidence, line: 1, document: "evidence"}}
		c.Warnings = append(c.Warnings, "原始证据没有可提取的独立步骤，以单条证据记录展示；不代表已确认执行时序。")
	}
	if len(extracted) > 0 {
		verification.Sources = []string{}
		verification.Items = []string{}
	}
	for i, s := range extracted {
		if i >= 16 {
			c.Warnings = append(c.Warnings, "验证步骤超过 16 个，剩余步骤请查阅原始报告。")
			break
		}
		id := addSource("step", fmt.Sprintf("记录 %d · %s", i+1, excerpt(s.title, 50)), s.body, s.line, s.document)
		verification.Items = append(verification.Items, fmt.Sprintf("%02d · %s", i+1, excerpt(s.title, 90)))
		verification.Sources = append(verification.Sources, id)
	}
	if len(verification.Sources) == 0 && evidenceID != "" {
		verification.Basis = "reported"
		verification.Summary = excerpt(f.Evidence, 160)
		verification.Sources = []string{evidenceID}
	}
	if len(extracted) > 0 {
		verification.Basis = "reported"
		verification.Summary = fmt.Sprintf("%d 条行为记录 · 展开查看请求、结果与对照证据", min(len(extracted), 16))
	}
	impact := makeStage("impact", "impact", "结果与影响", selectSections(all, "影响与危害", "危害", "安全影响", "风险评估", "影响分析", "影响范围"), f.Summary)
	limits := makeStage("limitations", "limitation", "边界与未验证项", selectSections(all, "局限", "未验证", "未证实", "限制", "边界"), "")
	c.Nodes = append(c.Nodes, prereq, source, inference, verification, impact, limits)
	for i := 1; i < 6; i++ {
		c.Edges = append(c.Edges, Edge{ID: fmt.Sprintf("narrative-%d", i), Source: c.Nodes[i-1].ID, Target: c.Nodes[i].ID, Kind: "narrative", Label: "阅读顺序"})
	}
	previous := "origins"
	for i, step := range extracted {
		if i >= 16 {
			break
		}
		sid := verification.Sources[i]
		n := Node{ID: fmt.Sprintf("step-%d", i+1), Kind: "event", Title: excerpt(step.title, 70), Summary: excerpt(step.body, 140), Basis: "reported", Sources: []string{sid}, Items: []string{}, Action: actionFor(step.title, step.body), Ordinal: i + 1}
		if match := requestPattern.FindStringSubmatch(step.body); match != nil {
			n.Endpoint = match[1] + " " + strings.TrimRight(match[2], ".,，；。")
		}
		c.Nodes = append(c.Nodes, n)
		c.Edges = append(c.Edges, Edge{ID: "step-edge-" + n.ID, Source: previous, Target: n.ID, Kind: "narrative", Label: "文档顺序"})
		previous = n.ID
	}
	if len(extracted) > 0 {
		c.Edges = append(c.Edges, Edge{ID: "step-outcome", Source: previous, Target: "impact", Kind: "narrative", Label: "报告结果"})
	}
	for _, r := range related {
		id := fmt.Sprintf("ref-%s-%d", r.Reference.Kind, r.Reference.ID)
		sid := addSource("node", fmt.Sprintf("%s%d · 探索记录", r.Reference.Kind, r.Reference.ID), r.Detail, 0, fmt.Sprintf("reference/%s%d", r.Reference.Kind, r.Reference.ID))
		n := Node{ID: id, Kind: "fact", Title: r.Title, Summary: excerpt(r.Summary, 140), Basis: "recorded", Sources: []string{sid}, Reference: fmt.Sprintf("%s%d", r.Reference.Kind, r.Reference.ID), Items: []string{}}
		if r.Reference.Kind == "finding" {
			n.Kind = "dependency"
			n.FindingID = strconv.FormatInt(r.FindingID, 10)
			if r.FindingID == 0 {
				n.FindingID = ""
			}
		}
		if r.Reference.Kind == "intent" {
			n.Kind = "intent"
		}
		c.Nodes = append(c.Nodes, n)
		c.Edges = append(c.Edges, Edge{ID: "edge-" + id, Source: id, Target: "prerequisites", Kind: "reference", Label: "报告引用 · 因果待确认"})
	}
	for _, s := range extra {
		c.Sources = append(c.Sources, s)
	}
	if f.TrafficCount == 0 {
		c.Warnings = append(c.Warnings, "未绑定 HTTP 流量快照；步骤依据现有报告和探索记录还原。")
	}
	if len(fileSeen) == 0 {
		c.Warnings = append(c.Warnings, "当前漏洞文档未定位具体网页 / JS 文件；可从关联漏洞继续追溯。")
	}
	if inference.Basis == "missing" {
		c.Warnings = append(c.Warnings, "现有报告没有独立的分析章节，推断过程保留为空。")
	}
	// Include every response field in the revision so changes to mapped references invalidate it.
	fingerprint := fmt.Sprintf("%#v", c)
	c.Revision = fmt.Sprintf("%x", sha256.Sum256([]byte(fingerprint)))[:16]
	return c
}

// SortRelated keeps reconstruction deterministic across PostgreSQL query plans.
func SortRelated(related []Related) {
	sort.Slice(related, func(i, j int) bool {
		if related[i].Reference.Kind == related[j].Reference.Kind {
			return related[i].Reference.ID < related[j].Reference.ID
		}
		return related[i].Reference.Kind < related[j].Reference.Kind
	})
}
