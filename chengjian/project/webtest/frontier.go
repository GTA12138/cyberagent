// frontier.go 带来源的 Web 覆盖前沿（W3 / T-18）。
//
// 覆盖单元不能是一个「接口」——那是最常见的自欺。同一个路由至少要在五个轴上
// 分别记录：
//
//	method × route template × param schema × identity × test family
//
// 少任何一轴都会把「没测」读成「测过」：测过 GET 不等于测过 DELETE；用 admin
// 测过不等于用低权账号测过；测过注入不等于测过越权。
//
// 另一条纪律是**来源**：每个已覆盖单元必须能说出它从哪来（流量 / OpenAPI /
// sourcemap / 人工）。没有来源的覆盖无法复核，也无法在「为什么漏了」时追溯。
// 所以 Mark 强制要 evidence，Uncovered 会把「有候选但没来源」的单列出来。
package webtest

import (
	"fmt"
	"sort"
	"strings"
)

// 覆盖单元的状态。刻意不做成互斥的单一状态机：一个单元可以「测过但没发现」，
// 也可以「发现了」——后者是前者的子集，不是替代。
const (
	// UnitUntested 还没测。
	UnitUntested = "untested"
	// UnitTested 测过，没有发现。
	UnitTested = "tested"
	// UnitFound 测过并且发现了问题。
	UnitFound = "found"
	// UnitSkipped 主动跳过（附原因）。跳过必须留原因，否则等同于漏测。
	UnitSkipped = "skipped"
)

// CoverageUnit 是一个覆盖单元。
type CoverageUnit struct {
	Method        string `json:"method"`
	RouteTemplate string `json:"route_template"`
	// ParamSchema 来自 Template.ParamSchema()。允许为空（无参数接口）。
	ParamSchema string `json:"param_schema,omitempty"`
	// IdentityRef 是身份引用（含版本），空表示「未认证视角」。
	IdentityRef string `json:"identity_ref,omitempty"`
	// Family 是测试族：injection / authz / ssrf / xss / upload / business ...
	Family string `json:"family"`
}

// Key 是覆盖单元的规范键。
func (u CoverageUnit) Key() string {
	return strings.Join([]string{
		strings.ToUpper(strings.TrimSpace(u.Method)),
		normalizeRoute(u.RouteTemplate),
		u.ParamSchema,
		u.IdentityRef,
		strings.ToLower(strings.TrimSpace(u.Family)),
	}, "|")
}

// FromTemplate 由模板 + 身份 + 测试族构造覆盖单元。
func FromTemplate(t Template, identityRef, family string) CoverageUnit {
	return CoverageUnit{
		Method:        t.Method,
		RouteTemplate: t.RouteTemplate,
		ParamSchema:   t.ParamSchema(),
		IdentityRef:   identityRef,
		Family:        family,
	}
}

// Record 是一个覆盖单元的记录（状态 + 来源）。
type Record struct {
	Unit CoverageUnit `json:"unit"`
	// State 取 UnitUntested/Tested/Found/Skipped。
	State string `json:"state"`
	// Source 是这个单元从哪来（traffic:<id> / openapi / sourcemap / manual）。
	Source string `json:"source"`
	// Evidence 是支撑状态的证据摘要（响应摘要、finding id 等）。
	Evidence string `json:"evidence,omitempty"`
	// Reason 在 skipped 时必填。
	Reason string `json:"reason,omitempty"`
}

// Frontier 是覆盖前沿的并发不安全实现（由调用方串行化；它本身只是账本）。
type Frontier struct {
	recs map[string]Record
}

// NewFrontier 构造空前沿。
func NewFrontier() *Frontier {
	return &Frontier{recs: map[string]Record{}}
}

// Mark 记录一个覆盖单元。
//
// 强制三条：
//   - Source 不能为空——没有来源的覆盖无法复核；
//   - skipped 必须给 Reason——不说原因的跳过就是漏测；
//   - 状态必须是已知值。
//
// 状态只升不降：found 不会被后来的 tested 覆盖（重测没复现是另一件事，应记
// evidence 而不是把「发现过」抹掉）。
func (f *Frontier) Mark(r Record) error {
	key := r.Unit.Key()
	if key == "||||" {
		return fmt.Errorf("覆盖单元为空")
	}
	switch r.State {
	case UnitTested, UnitFound, UnitSkipped, UnitUntested:
	default:
		return fmt.Errorf("未知覆盖状态 %q", r.State)
	}
	if strings.TrimSpace(r.Source) == "" {
		return fmt.Errorf("覆盖单元 %s 缺来源：没有来源的覆盖无法复核", key)
	}
	if r.State == UnitSkipped && strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("跳过 %s 必须给原因：不说原因的跳过等同于漏测", key)
	}
	if prev, ok := f.recs[key]; ok && prev.State == UnitFound && r.State != UnitFound {
		// 保留 found，但把新证据追加进去——重测结果本身就是信息。
		prev.Evidence = strings.TrimSpace(prev.Evidence + " | 重测: " + r.Evidence)
		f.recs[key] = prev
		return nil
	}
	f.recs[key] = r
	return nil
}

// Get 读一个单元的记录。
func (f *Frontier) Get(u CoverageUnit) (Record, bool) {
	r, ok := f.recs[u.Key()]
	return r, ok
}

// Records 返回全部记录（按 key 升序）。
func (f *Frontier) Records() []Record {
	out := make([]Record, 0, len(f.recs))
	for _, r := range f.recs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Unit.Key() < out[j].Unit.Key() })
	return out
}

// Coverage 返回 (已测, 总数) —— skipped 计入已处理但不计入已测。
func (f *Frontier) Coverage() (tested, total int) {
	for _, r := range f.recs {
		total++
		if r.State == UnitTested || r.State == UnitFound {
			tested++
		}
	}
	return tested, total
}

// Uncovered 返回 candidates 里还没被处理过的单元。
//
// 「处理过」包括 tested/found/skipped：skip 是决策，不是遗漏。这一点很重要——
// 如果把 skipped 也算未覆盖，前沿会永远有积压，planner 会反复重派已经决定
// 不做的方向。
func (f *Frontier) Uncovered(candidates []CoverageUnit) []CoverageUnit {
	out := []CoverageUnit{}
	for _, u := range candidates {
		r, ok := f.recs[u.Key()]
		if !ok || r.State == UnitUntested {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// Sources 返回覆盖来源的分布（用于「这批覆盖有多少来自真实流量」这类问题）。
func (f *Frontier) Sources() map[string]int {
	out := map[string]int{}
	for _, r := range f.recs {
		out[r.Source]++
	}
	return out
}

// FamilyStats 按测试族统计 (tested, total)。
//
// 这是回答「我们到底测了什么」的唯一可用视图：总覆盖率 60% 可能意味着注入
// 测了 90%、越权只测了 10%——只看总数会把后者藏起来。
func (f *Frontier) FamilyStats() map[string][2]int {
	out := map[string][2]int{}
	for _, r := range f.recs {
		cur := out[r.Unit.Family]
		cur[1]++
		if r.State == UnitTested || r.State == UnitFound {
			cur[0]++
		}
		out[r.Unit.Family] = cur
	}
	return out
}

// Summary 是给 agent 读的一句话覆盖结论。
func (f *Frontier) Summary() string {
	tested, total := f.Coverage()
	if total == 0 {
		return "覆盖前沿为空：还没有登记任何覆盖单元"
	}
	fam := f.FamilyStats()
	fams := make([]string, 0, len(fam))
	for k := range fam {
		fams = append(fams, k)
	}
	sort.Strings(fams)
	parts := make([]string, 0, len(fams))
	for _, k := range fams {
		s := fam[k]
		parts = append(parts, fmt.Sprintf("%s %d/%d", k, s[0], s[1]))
	}
	return fmt.Sprintf("覆盖 %d/%d；按测试族：%s。注意：未列出的测试族是完全没测，不是「测了没发现」。",
		tested, total, strings.Join(parts, "、"))
}
