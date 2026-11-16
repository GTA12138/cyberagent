package webtest

import (
	"strings"
	"testing"
)

func TestReplayMutatesExactlyOneAxis(t *testing.T) {
	base := Request{
		Method:  "POST",
		URL:     "http://t/api/users/7?debug=0",
		Headers: [][2]string{{"Content-Type", "application/x-www-form-urlencoded"}, {"Cookie", "sid=abc; theme=dark"}},
		Body:    []byte("name=bob&role=user"),
	}

	got, err := Replay(base, Mutation{Kind: "path", From: "/7", To: "/8"})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "http://t/api/users/8?debug=0" {
		t.Fatalf("path 变异错: %s", got.URL)
	}
	if got.Method != base.Method || string(got.Body) != string(base.Body) {
		t.Fatal("path 变异不该动 method/body——多点变异就无法归因")
	}

	got, _ = Replay(base, Mutation{Kind: "query", Name: "debug", To: "1"})
	if got.URL != "http://t/api/users/7?debug=1" {
		t.Fatalf("query 变异错: %s", got.URL)
	}

	got, _ = Replay(base, Mutation{Kind: "body", Name: "role", From: "user", To: "admin"})
	if string(got.Body) != "name=bob&role=admin" {
		t.Fatalf("body 变异错: %s", got.Body)
	}

	got, _ = Replay(base, Mutation{Kind: "cookie", Name: "sid", To: "evil"})
	if c, _ := got.Header("Cookie"); !strings.Contains(c, "sid=evil") || !strings.Contains(c, "theme=dark") {
		t.Fatalf("cookie 变异错: %s", c)
	}

	// 原请求不能被改动（Replay 必须深拷贝）。
	if c, _ := base.Header("Cookie"); !strings.Contains(c, "sid=abc") {
		t.Fatal("Replay 改动了基线请求：后续对照会建立在被污染的基线上")
	}
}

func TestReplayRejectsAmbiguousOrWrongBaseline(t *testing.T) {
	base := Request{Method: "GET", URL: "http://t/a"}
	if _, err := Replay(base, Mutation{}); err == nil {
		t.Fatal("没有 kind 的变异必须被拒")
	}
	if _, err := Replay(base, Mutation{Kind: "path", From: "/nope", To: "/x"}); err == nil {
		t.Fatal("基线里没有的片段做变异必须报错：否则会静默产生一个「没变」的变体，对照就成了自比")
	}
	if _, err := Replay(base, Mutation{Kind: "identity", To: "admin"}); err == nil {
		t.Fatal("identity 变异应引导调用方重建请求，而不是替换单个头")
	}
}

func TestCompareTreatsServerErrorAsInconclusive(t *testing.T) {
	c := Compare(Response{Status: 500, Body: []byte("boom")}, Response{Status: 200, Body: []byte("ok")})
	if c.Verdict != VerdictInconclusive {
		t.Fatalf("5xx 必须判 inconclusive，got %s", c.Verdict)
	}
	if c.FindingState() != "inconclusive" {
		t.Fatalf("finding 状态应映射成 inconclusive，got %s", c.FindingState())
	}
}

func TestCompareStatusDifferenceIsStrongestSignal(t *testing.T) {
	c := Compare(Response{Status: 200, Body: []byte("nope")}, Response{Status: 403, Body: []byte("denied")})
	if c.Verdict != VerdictDifferent || c.FindingState() != "inconclusive" {
		t.Fatalf("状态码差异应判 different/inconclusive，got %+v", c)
	}
	if len(c.Signals) == 0 || c.Signals[0] != "status" {
		t.Fatalf("信号应指明 status，got %v", c.Signals)
	}
}

func TestCompareSameWhenBodyStable(t *testing.T) {
	body := []byte(strings.Repeat("x", 500))
	c := Compare(Response{Status: 200, Body: body}, Response{Status: 200, Body: body})
	if c.Verdict != VerdictSame || c.FindingState() != "inconclusive" {
		t.Fatalf("完全一致应判 same/inconclusive，got %+v", c)
	}
}

func TestCompareDetectsSubstantialBodyDifference(t *testing.T) {
	c := Compare(
		Response{Status: 200, Body: []byte(strings.Repeat("a", 1000))},
		Response{Status: 200, Body: []byte(strings.Repeat("b", 1000))},
	)
	if c.Verdict != VerdictDifferent {
		t.Fatalf("正文实质不同应判 different，got %+v", c)
	}
	if len(c.Signals) == 0 || c.Signals[0] != "body" {
		t.Fatalf("信号应指明 body，got %v", c.Signals)
	}
}

func TestStableAllowsSmallVolatileNoise(t *testing.T) {
	// 真实站点的正文常带时间戳/token，差几个字节是正常的。判「不稳定」会让
	// 所有结论变成 inconclusive，那等于没有对照。
	body := []byte(strings.Repeat("-", 400))
	withToken := []byte("t=1" + strings.Repeat("-", 397))
	if !Stable(Response{Status: 200, Body: body}, Response{Status: 200, Body: withToken}) {
		t.Fatal("1% 以内的抖动应判稳定")
	}
	c := Response{Status: 200, Body: []byte(strings.Repeat("-", 400))}
	if Stable(Response{Status: 200, Body: []byte(strings.Repeat("x", 400))}, c) {
		t.Fatal("内容完全不同必须判不稳定（只比长度会漏掉这种情况）")
	}
	if Stable(Response{Status: 200}, Response{Status: 201}) {
		t.Fatal("状态码不同一律不稳定")
	}
}

func TestCompareWithNoiseCalibratesAgainstControlJitter(t *testing.T) {
	// 对照组自身抖动 3%：变体差异 4% 不该被当成信号（那是抖动范围）。
	stable := []byte(strings.Repeat("a", 100))
	jitter := []byte(strings.Repeat("a", 97) + "bbb") // 3% 差异
	if Stable(Response{Status: 200, Body: stable}, Response{Status: 200, Body: jitter}) {
		t.Fatal("3% 抖动不该判稳定（默认阈值 1%）")
	}
	c := CompareWithNoise(
		Response{Status: 200, Body: stable},
		Response{Status: 200, Body: jitter},
		Response{Status: 200, Body: []byte(strings.Repeat("a", 96) + "cccc")},
	)
	if c.Verdict != VerdictSame {
		t.Fatalf("差异落在噪声范围内应判 same，got %+v", c)
	}

	// 变体差异远超抖动：必须判 different。
	c = CompareWithNoise(
		Response{Status: 200, Body: stable},
		Response{Status: 200, Body: jitter},
		Response{Status: 200, Body: []byte(strings.Repeat("z", 100))},
	)
	if c.Verdict != VerdictDifferent {
		t.Fatalf("差异远超噪声应判 different，got %+v", c)
	}
}

func TestCompareWithNoiseRejectsUnstableControl(t *testing.T) {
	// 对照组自身抖动 40%：这个目标不适合单次差分，必须判 inconclusive 而不是硬判。
	c := CompareWithNoise(
		Response{Status: 200, Body: []byte(strings.Repeat("a", 100))},
		Response{Status: 200, Body: []byte(strings.Repeat("b", 100))},
		Response{Status: 200, Body: []byte(strings.Repeat("c", 100))},
	)
	if c.Verdict != VerdictInconclusive {
		t.Fatalf("对照组抖动过大应判 inconclusive，got %+v", c)
	}
	// 对照组两次状态码不同同样不可用。
	c = CompareWithNoise(Response{Status: 200}, Response{Status: 302}, Response{Status: 200})
	if c.Verdict != VerdictInconclusive {
		t.Fatalf("对照组状态码不一致应判 inconclusive，got %+v", c)
	}
}

func TestTemplateKeyCollapsesPlaceholderNames(t *testing.T) {
	a := Template{Method: "get", RouteTemplate: "/api/users/{id}/"}
	b := Template{Method: "GET", RouteTemplate: "/api/users/{userId}"}
	if a.Key() != b.Key() {
		t.Fatalf("同一路由不同占位名应合并，got %q vs %q", a.Key(), b.Key())
	}
	if !strings.Contains(a.Key(), "GET ") {
		t.Fatalf("方法应大写，got %q", a.Key())
	}
}

func TestParamSchemaIsOrderInsensitive(t *testing.T) {
	a := Template{Params: []Param{{Name: "id", Kind: ParamPath}, {Name: "q", Kind: ParamQuery}}}
	b := Template{Params: []Param{{Name: "q", Kind: ParamQuery}, {Name: "id", Kind: ParamPath}}}
	if a.ParamSchema() != b.ParamSchema() {
		t.Fatalf("参数顺序不同不该算两个覆盖单元: %q vs %q", a.ParamSchema(), b.ParamSchema())
	}
}

func TestFrontierRequiresSourceAndReason(t *testing.T) {
	f := NewFrontier()
	u := CoverageUnit{Method: "GET", RouteTemplate: "/api/x", Family: "injection"}
	if err := f.Mark(Record{Unit: u, State: UnitTested}); err == nil {
		t.Fatal("缺来源必须被拒：没有来源的覆盖无法复核")
	}
	if err := f.Mark(Record{Unit: u, State: UnitSkipped, Source: "traffic:1"}); err == nil {
		t.Fatal("跳过必须给原因")
	}
	if err := f.Mark(Record{Unit: u, State: UnitTested, Source: "traffic:1", Evidence: "200/相同"}); err != nil {
		t.Fatal(err)
	}
}

func TestFrontierCoverageSeparatesAxes(t *testing.T) {
	f := NewFrontier()
	base := Template{Method: "GET", RouteTemplate: "/api/users/{id}", Params: []Param{{Name: "id", Kind: ParamPath}}}
	// 同一路由、不同测试族 + 不同身份 = 四个独立覆盖单元。
	units := []CoverageUnit{
		FromTemplate(base, "admin@1", "injection"),
		FromTemplate(base, "admin@1", "authz"),
		FromTemplate(base, "low@1", "authz"),
		FromTemplate(Template{Method: "DELETE", RouteTemplate: "/api/users/{id}"}, "admin@1", "authz"),
	}
	if len(map[string]bool{units[0].Key(): true, units[1].Key(): true, units[2].Key(): true, units[3].Key(): true}) != 4 {
		t.Fatal("不同族/身份/方法必须是不同覆盖单元，否则覆盖率是自欺")
	}
	for _, u := range units[:2] {
		if err := f.Mark(Record{Unit: u, State: UnitTested, Source: "traffic:1"}); err != nil {
			t.Fatal(err)
		}
	}
	un := f.Uncovered(units)
	if len(un) != 2 {
		t.Fatalf("应剩 2 个未覆盖，got %d: %v", len(un), un)
	}
	tested, total := f.Coverage()
	if tested != 2 || total != 2 {
		t.Fatalf("覆盖率错: %d/%d", tested, total)
	}
}

func TestFrontierSkipCountsAsHandled(t *testing.T) {
	f := NewFrontier()
	u := CoverageUnit{Method: "GET", RouteTemplate: "/x", Family: "ssrf"}
	if err := f.Mark(Record{Unit: u, State: UnitSkipped, Source: "manual", Reason: "目标无出网，SSRF 无意义"}); err != nil {
		t.Fatal(err)
	}
	if got := f.Uncovered([]CoverageUnit{u}); len(got) != 0 {
		t.Fatal("skip 是决策不是遗漏：若算未覆盖，前沿会永远积压")
	}
	tested, total := f.Coverage()
	if tested != 0 || total != 1 {
		t.Fatalf("skip 不该计入已测: %d/%d", tested, total)
	}
}

func TestFrontierFoundIsSticky(t *testing.T) {
	f := NewFrontier()
	u := CoverageUnit{Method: "GET", RouteTemplate: "/x", Family: "injection"}
	_ = f.Mark(Record{Unit: u, State: UnitFound, Source: "traffic:1", Evidence: "finding#9"})
	_ = f.Mark(Record{Unit: u, State: UnitTested, Source: "traffic:2", Evidence: "未复现"})
	r, _ := f.Get(u)
	if r.State != UnitFound {
		t.Fatal("重测未复现不该抹掉「发现过」——那会让已确认漏洞从覆盖里消失")
	}
	if !strings.Contains(r.Evidence, "重测") {
		t.Fatalf("重测结果应追加进证据，got %q", r.Evidence)
	}
}

func TestFrontierFamilyStatsExposeImbalance(t *testing.T) {
	f := NewFrontier()
	_ = f.Mark(Record{Unit: CoverageUnit{Method: "GET", RouteTemplate: "/a", Family: "injection"}, State: UnitTested, Source: "traffic:1"})
	_ = f.Mark(Record{Unit: CoverageUnit{Method: "GET", RouteTemplate: "/b", Family: "injection"}, State: UnitTested, Source: "traffic:1"})
	_ = f.Mark(Record{Unit: CoverageUnit{Method: "GET", RouteTemplate: "/c", Family: "authz"}, State: UnitUntested, Source: "traffic:1"})
	stats := f.FamilyStats()
	if stats["injection"] != [2]int{2, 2} || stats["authz"] != [2]int{0, 1} {
		t.Fatalf("按族统计错: %+v", stats)
	}
	if !strings.Contains(f.Summary(), "authz 0/1") {
		t.Fatalf("摘要必须暴露族间不平衡，got %q", f.Summary())
	}
}
