// Package planning 把「派这条意图之前要有什么」与「什么算做完」变成可判定的
// 结构（O1 / T-11 的 prerequisites、done_when 部分）。
//
// 为什么需要它：此前「先确认出网再建隧道」「拿到 hash 再横向」这类先后关系只
// 存在于提示词的自然语言里。提示词是软约束——模型可以合理地忽略它，而系统
// 无法告诉你它被忽略了。于是 planner 会在没有立足点时派横向意图，worker 会
// 在只拿到半个结论时声称做完，两者都不留痕。
//
// 本包只做判定，不做 IO：输入是一份 State 快照，输出是逐条的 Check。这样它
// 可以在 planner 派活前、worker 收工前、复盘脚本里复用同一套判据——三处若各写
// 一遍，迟早出现「planner 认为满足、worker 认为不满足」的分裂。
//
// 三条判据纪律：
//
//  1. **unknown 不等于 unmet，也不等于 met。** 拿不到判据所需的数据时，如实报
//     unknown，不猜。把 unknown 当 unmet 会拦住本来能做的事；当 met 会把「没
//     检查」伪装成「检查过」。
//
//  2. **未满足不硬拦。** 判定结果用于「排序与提示」，不是准入闸门：真实渗透
//     里「先打了再补前置」是常态，硬拦会让 planner 卡死在一个它无法直接满足
//     的条件上（比如前置是另一条意图的产出）。
//
//  3. **判据必须能被观测。** 只收「系统里有对应记录」的判据（fact/asset/
//     credential/session/tunnel/intent_done）。「目标已经想清楚了」这种判据
//     无法判定，收进来只会制造假绿灯。
package planning

import (
	"fmt"
	"sort"
	"strings"
)

// RequirementKind 是前置条件的类别。
type RequirementKind string

const (
	// ReqFact 要求已存在含指定关键词的事实（fact:<关键词>，关键词可空 = 任意事实）。
	ReqFact RequirementKind = "fact"
	// ReqAsset 要求已发现指定类型的资产（asset:<type>，如 asset:domain）。
	ReqAsset RequirementKind = "asset"
	// ReqCredential 要求已登记指定服务的可用凭据（credential:<service>，服务可空）。
	ReqCredential RequirementKind = "credential"
	// ReqSession 要求存在存活的立足点会话（session）。
	ReqSession RequirementKind = "session"
	// ReqTunnel 要求存在存活的隧道（tunnel 或 tunnel:socks）。
	ReqTunnel RequirementKind = "tunnel"
	// ReqIntentDone 要求某条 summary 匹配的意图已完成（intent_done:<关键词>）。
	ReqIntentDone RequirementKind = "intent_done"
)

// Status 是单条判据的结论。
type Status string

const (
	// StatusMet 判据已满足，且证据可指认。
	StatusMet Status = "met"
	// StatusUnmet 判据未满足（有数据，判定为否）。
	StatusUnmet Status = "unmet"
	// StatusUnknown 缺少判定所需的数据，无法给出结论。
	StatusUnknown Status = "unknown"
	// StatusInvalid 判据本身写错了（拼写/取值非法），需要调用方修正。
	StatusInvalid Status = "invalid"
)

// Requirement 是一条解析后的前置条件。
type Requirement struct {
	Raw  string          `json:"raw"`
	Kind RequirementKind `json:"kind"`
	Arg  string          `json:"arg,omitempty"`
}

// Parse 解析一条前置条件。
//
// 语法刻意做成 `kind:arg` 而不是自然语言：自然语言判据无法判定（见包注释第 3 条）。
// 空串与非空但只有空白视为非法——静默跳过会让「写了判据但没生效」无法察觉。
func Parse(raw string) (Requirement, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Requirement{Raw: raw}, fmt.Errorf("前置条件不能为空")
	}
	kind, arg := s, ""
	if i := strings.IndexByte(s, ':'); i >= 0 {
		kind, arg = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	}
	k := RequirementKind(strings.ToLower(kind))
	switch k {
	case ReqFact, ReqAsset, ReqCredential, ReqSession, ReqTunnel, ReqIntentDone:
		// session/tunnel 允许带参数（tunnel:socks），也允许不带。
		return Requirement{Raw: s, Kind: k, Arg: arg}, nil
	}
	return Requirement{Raw: s, Kind: k, Arg: arg},
		fmt.Errorf("未知的前置条件类型 %q（可用：fact / asset / credential / session / tunnel / intent_done）", kind)
}

// ParseAll 解析一组前置条件，把写错的单条作为 invalid 保留在结果里。
//
// 不因一条写错就丢弃整组：planner 需要看到「你写的这条不成立」，而不是整批
// 静默失败。
func ParseAll(raws []string) []Requirement {
	out := make([]Requirement, 0, len(raws))
	for _, raw := range raws {
		if strings.TrimSpace(raw) == "" {
			continue // 空项直接忽略：数组里多一个逗号不该产生一条 invalid
		}
		r, err := Parse(raw)
		if err != nil {
			r.Kind = ""
			r.Arg = err.Error() // 用 Arg 带出原因，Check 里原样呈现
		}
		out = append(out, r)
	}
	return out
}

// FactView 是一条事实的判定视图。
type FactView struct {
	ID      int64  `json:"id"`
	Summary string `json:"summary"`
}

// CredentialView 是一条凭据的判定视图（不含秘密）。
type CredentialView struct {
	ID       int64  `json:"id"`
	Service  string `json:"service,omitempty"`
	Verified bool   `json:"verified"`
}

// Counts 是「有多少」类判据的输入。
//
// 用指针表示「未知」：nil = 调用方没有这份数据，判定为 unknown；
// 非 nil 的 0 = 确认没有，判定为 unmet。这个区别是刻意的——把「没查」当成
// 「没有」会让前置条件在数据缺失时全部判 unmet，从而把提示变成噪音。
type Counts struct {
	AssetsByType map[string]int
	Credentials  *[]CredentialView
	Sessions     *int
	Tunnels      *int
	TunnelKinds  map[string]int
}

// State 是一次判定的输入快照。
type State struct {
	Facts     []FactView
	DoneItems []string // 已完成意图的 summary（用于 intent_done）
	Counts    Counts
}

// Check 是一条判据的判定结果。
type Check struct {
	Requirement string          `json:"requirement"`
	Kind        RequirementKind `json:"kind,omitempty"`
	Arg         string          `json:"arg,omitempty"`
	Status      Status          `json:"status"`
	Detail      string          `json:"detail,omitempty"`
}

// Met 报告该条是否明确满足。
func (c Check) Met() bool { return c.Status == StatusMet }

// Blocking 报告该条是否「有数据、且判定为否」——即真正需要处理的缺口。
// unknown / invalid 不算 blocking：前者缺数据，后者是写法问题。
func (c Check) Blocking() bool { return c.Status == StatusUnmet }

// Evaluate 逐条判定前置条件。
func Evaluate(reqs []Requirement, st State) []Check {
	out := make([]Check, 0, len(reqs))
	for _, r := range reqs {
		if r.Kind == "" {
			out = append(out, Check{
				Requirement: r.Raw, Status: StatusInvalid,
				Detail: "判据写法无法解析：" + r.Arg,
			})
			continue
		}
		out = append(out, evaluateOne(r, st))
	}
	return out
}

func evaluateOne(r Requirement, st State) Check {
	c := Check{Requirement: r.Raw, Kind: r.Kind, Arg: r.Arg}
	switch r.Kind {
	case ReqFact:
		hits := matchFacts(st.Facts, r.Arg)
		if len(hits) == 0 {
			if r.Arg == "" && len(st.Facts) == 0 {
				c.Status, c.Detail = StatusUnmet, "还没有任何事实"
				return c
			}
			c.Status = StatusUnmet
			c.Detail = fmt.Sprintf("没有匹配 %q 的事实", r.Arg)
			return c
		}
		c.Status = StatusMet
		c.Detail = fmt.Sprintf("命中事实 %s", idsOf(hits))
		return c

	case ReqAsset:
		if st.Counts.AssetsByType == nil {
			c.Status, c.Detail = StatusUnknown, "调用方未提供资产统计，无法判定"
			return c
		}
		arg := strings.ToLower(strings.TrimSpace(r.Arg))
		if arg == "" {
			total := 0
			for _, n := range st.Counts.AssetsByType {
				total += n
			}
			if total == 0 {
				c.Status, c.Detail = StatusUnmet, "还没有任何资产"
				return c
			}
			c.Status, c.Detail = StatusMet, fmt.Sprintf("已有 %d 个资产", total)
			return c
		}
		n := st.Counts.AssetsByType[arg]
		if n == 0 {
			c.Status, c.Detail = StatusUnmet, fmt.Sprintf("没有 %s 类型资产", arg)
			return c
		}
		c.Status, c.Detail = StatusMet, fmt.Sprintf("已有 %d 个 %s 类型资产", n, arg)
		return c

	case ReqCredential:
		if st.Counts.Credentials == nil {
			c.Status, c.Detail = StatusUnknown, "调用方未提供凭据清单，无法判定"
			return c
		}
		arg := strings.ToLower(strings.TrimSpace(r.Arg))
		matched := []string{}
		for _, cv := range *st.Counts.Credentials {
			if arg == "" || strings.Contains(strings.ToLower(cv.Service), arg) {
				matched = append(matched, fmt.Sprintf("#%d", cv.ID))
			}
		}
		if len(matched) == 0 {
			c.Status, c.Detail = StatusUnmet, fmt.Sprintf("没有匹配 %q 的凭据", r.Arg)
			return c
		}
		c.Status, c.Detail = StatusMet, "已有凭据 "+strings.Join(matched, ",")
		return c

	case ReqSession:
		if st.Counts.Sessions == nil {
			c.Status, c.Detail = StatusUnknown, "调用方未提供会话统计，无法判定"
			return c
		}
		if *st.Counts.Sessions == 0 {
			c.Status, c.Detail = StatusUnmet, "没有存活的立足点会话"
			return c
		}
		c.Status, c.Detail = StatusMet, fmt.Sprintf("有 %d 个存活会话", *st.Counts.Sessions)
		return c

	case ReqTunnel:
		if st.Counts.Tunnels == nil {
			c.Status, c.Detail = StatusUnknown, "调用方未提供隧道统计，无法判定"
			return c
		}
		arg := strings.ToLower(strings.TrimSpace(r.Arg))
		if arg != "" && st.Counts.TunnelKinds != nil {
			if n := st.Counts.TunnelKinds[arg]; n > 0 {
				c.Status, c.Detail = StatusMet, fmt.Sprintf("有 %d 条 %s 隧道", n, arg)
				return c
			}
			c.Status, c.Detail = StatusUnmet, fmt.Sprintf("没有 %s 类型的存活隧道", arg)
			return c
		}
		if *st.Counts.Tunnels == 0 {
			c.Status, c.Detail = StatusUnmet, "没有存活的隧道"
			return c
		}
		c.Status, c.Detail = StatusMet, fmt.Sprintf("有 %d 条存活隧道", *st.Counts.Tunnels)
		return c

	case ReqIntentDone:
		arg := strings.ToLower(strings.TrimSpace(r.Arg))
		for _, d := range st.DoneItems {
			if arg == "" || strings.Contains(strings.ToLower(d), arg) {
				c.Status, c.Detail = StatusMet, "已完成意图："+truncate(d, 60)
				return c
			}
		}
		c.Status, c.Detail = StatusUnmet, fmt.Sprintf("没有已完成且匹配 %q 的意图", r.Arg)
		return c
	}
	c.Status, c.Detail = StatusInvalid, "未知判据类型"
	return c
}

// matchFacts 返回摘要包含关键词的事实（关键词空 = 全部）。
func matchFacts(facts []FactView, kw string) []FactView {
	kw = strings.ToLower(strings.TrimSpace(kw))
	out := []FactView{}
	for _, f := range facts {
		if kw == "" || strings.Contains(strings.ToLower(f.Summary), kw) {
			out = append(out, f)
		}
	}
	return out
}

func idsOf(facts []FactView) string {
	ids := make([]string, 0, len(facts))
	for _, f := range facts {
		ids = append(ids, fmt.Sprintf("#%d", f.ID))
	}
	if len(ids) > 6 {
		ids = append(ids[:6], "…")
	}
	return strings.Join(ids, ",")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Summary 是判定结果的聚合视图，给 planner 直接读。
type Summary struct {
	Total    int      `json:"total"`
	Met      int      `json:"met"`
	Unmet    int      `json:"unmet"`
	Unknown  int      `json:"unknown"`
	Invalid  int      `json:"invalid"`
	Blocking []string `json:"blocking,omitempty"` // 未满足的判据原文（按输入顺序）
	Notes    []string `json:"notes,omitempty"`    // unknown/invalid 的可读说明
}

// Summarize 汇总判定结果。
func Summarize(checks []Check) Summary {
	s := Summary{Total: len(checks)}
	for _, c := range checks {
		switch c.Status {
		case StatusMet:
			s.Met++
		case StatusUnmet:
			s.Unmet++
			s.Blocking = append(s.Blocking, c.Requirement)
		case StatusUnknown:
			s.Unknown++
			s.Notes = append(s.Notes, fmt.Sprintf("%s：%s", c.Requirement, c.Detail))
		case StatusInvalid:
			s.Invalid++
			s.Notes = append(s.Notes, fmt.Sprintf("%s：%s", c.Requirement, c.Detail))
		}
	}
	sort.Strings(s.Blocking) // 稳定输出，便于测试与比对
	return s
}

// ---- done_when ----

// DoneKind 是完成判据的类别。
type DoneKind string

const (
	// DoneFact 要求已写下含关键词的事实（fact:<关键词>）。
	DoneFact DoneKind = "fact"
	// DoneFinding 要求已登记发现（finding，可带严重度筛选 finding:high）。
	DoneFinding DoneKind = "finding"
	// DoneEvidence 要求至少有 N 条证据（evidence:<n>，默认 1）。
	DoneEvidence DoneKind = "evidence"
	// DoneCommand 表示「用一条命令的输出即可判定」，系统无法自动判定，
	// 必须由 worker 在结论里给出那条输出。它不产生假绿灯，只是显式承认
	// 「这条要人来判」。
	DoneCommand DoneKind = "command"
)

// DoneCriterion 是一条解析后的完成判据。
type DoneCriterion struct {
	Raw  string   `json:"raw"`
	Kind DoneKind `json:"kind"`
	Arg  string   `json:"arg,omitempty"`
}

// ParseDone 解析一条完成判据。
func ParseDone(raw string) (DoneCriterion, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return DoneCriterion{Raw: raw}, fmt.Errorf("完成判据不能为空")
	}
	kind, arg := s, ""
	if i := strings.IndexByte(s, ':'); i >= 0 {
		kind, arg = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	}
	k := DoneKind(strings.ToLower(kind))
	switch k {
	case DoneFact, DoneFinding, DoneEvidence, DoneCommand:
		return DoneCriterion{Raw: s, Kind: k, Arg: arg}, nil
	}
	return DoneCriterion{Raw: s, Kind: k, Arg: arg},
		fmt.Errorf("未知的完成判据类型 %q（可用：fact / finding / evidence / command）", kind)
}

// ParseDoneAll 解析一组完成判据。
func ParseDoneAll(raws []string) []DoneCriterion {
	out := make([]DoneCriterion, 0, len(raws))
	for _, raw := range raws {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		c, err := ParseDone(raw)
		if err != nil {
			c.Kind = ""
			c.Arg = err.Error()
		}
		out = append(out, c)
	}
	return out
}

// DoneState 是判定「做完没有」的输入。
type DoneState struct {
	Facts      []FactView
	Findings   []FactView // 复用同结构：ID + Summary（severity 在 Summary 前缀里）
	EvidenceN  int
	ManualNote string // worker 对 command 类判据的自述
}

// DoneGap 是一条未满足的完成判据。
type DoneGap struct {
	Criterion string   `json:"criterion"`
	Kind      DoneKind `json:"kind,omitempty"`
	Reason    string   `json:"reason"`
}

// EvaluateDone 判定完成判据，返回未满足项。
//
// 空判据列表返回 nil（没有判据 = 没有额外要求，不是「未完成」）。
func EvaluateDone(criteria []DoneCriterion, st DoneState) []DoneGap {
	gaps := []DoneGap{}
	for _, c := range criteria {
		if c.Kind == "" {
			gaps = append(gaps, DoneGap{Criterion: c.Raw, Reason: "判据写法无法解析：" + c.Arg})
			continue
		}
		switch c.Kind {
		case DoneFact:
			if len(matchFacts(st.Facts, c.Arg)) == 0 {
				gaps = append(gaps, DoneGap{Criterion: c.Raw, Kind: c.Kind,
					Reason: fmt.Sprintf("还没有写入匹配 %q 的事实", c.Arg)})
			}
		case DoneFinding:
			if len(matchFacts(st.Findings, c.Arg)) == 0 {
				gaps = append(gaps, DoneGap{Criterion: c.Raw, Kind: c.Kind,
					Reason: fmt.Sprintf("还没有登记匹配 %q 的发现", c.Arg)})
			}
		case DoneEvidence:
			want := 1
			if c.Arg != "" {
				if n, err := atoiSafe(c.Arg); err == nil && n > 0 {
					want = n
				}
			}
			if st.EvidenceN < want {
				gaps = append(gaps, DoneGap{Criterion: c.Raw, Kind: c.Kind,
					Reason: fmt.Sprintf("证据条数 %d < 要求 %d", st.EvidenceN, want)})
			}
		case DoneCommand:
			if strings.TrimSpace(st.ManualNote) == "" {
				gaps = append(gaps, DoneGap{Criterion: c.Raw, Kind: c.Kind,
					Reason: "该判据要求给出可判定的命令输出，但结论里没有"})
			}
		}
	}
	return gaps
}

func atoiSafe(s string) (int, error) {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("非数字 %q", s)
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return 0, fmt.Errorf("非正数 %q", s)
	}
	return n, nil
}
