package server

// enrich_semantic_parse_test.go 是 T-21 要求的**评测基线**。
//
// 语义层接上模型之后，「它到底改变了什么」不能靠感觉判断：模型输出不可复现，
// 但「这段输出能变成哪些候选」完全由 parseSemanticCandidates 决定。把这一步
// 用 golden fixture 钉死，接模型前后的差异就只剩「模型给了什么文字」这一个变量。
//
// 这里的每一条都是真实会发生的模型输出形态（围栏、包一层、编造方法、跨域、
// 把 .js 当端点、把 / 当端点）。它们不是假设，是接上之后必须能拦住的。

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"chengjian.local/chengjian/enrich"
)

const semanticTestSource = "https://app.example.com/static/js/main.7f3a.js"

func mustParse(t *testing.T, raw string) ([]enrich.Candidate, []string) {
	t.Helper()
	cands, dropped := parseSemanticCandidates(raw, semanticTestSource)
	for _, c := range cands {
		if c.Method == "" || c.URL == "" {
			t.Fatalf("候选缺少 method/url: %+v", c)
		}
		if !strings.HasPrefix(c.URL, "/") &&
			!strings.HasPrefix(c.URL, "https://app.example.com/") &&
			!strings.HasPrefix(c.URL, "http://app.example.com/") {
			t.Fatalf("候选 URL 只能是路径或同源绝对地址，实际 %q", c.URL)
		}
	}
	return cands, dropped
}

func TestParseSemanticCandidatesCleanArray(t *testing.T) {
	raw := `[
	  {"method":"get","url":"/api/user/list","params":[{"location":"query","name":"page","type":"integer"}]},
	  {"method":"POST","url":"/api/login"}
	]`
	cands, dropped := mustParse(t, raw)
	if len(cands) != 2 {
		t.Fatalf("应保留 2 条候选，实际 %d（丢弃原因 %v）", len(cands), dropped)
	}
	if cands[0].Method != "GET" {
		t.Fatalf("method 应大写化，实际 %q", cands[0].Method)
	}
	if len(cands[0].Params) != 1 || cands[0].Params[0]["name"] != "page" {
		t.Fatalf("参数未按原样保留: %+v", cands[0].Params)
	}
	if cands[1].Params != nil {
		t.Fatalf("无参数的候选不应塞空参数列表: %+v", cands[1].Params)
	}
}

func TestParseSemanticCandidatesToleratesFenceAndWrapper(t *testing.T) {
	fenced := "好的，结果如下：\n```json\n[{\"method\":\"GET\",\"url\":\"/api/a\"}]\n```\n以上。"
	if cands, _ := mustParse(t, fenced); len(cands) != 1 {
		t.Fatalf("围栏输出应被解析出 1 条，实际 %d", len(cands))
	}
	wrapped := `{"endpoints":[{"method":"PUT","url":"/api/b"}]}`
	cands, _ := mustParse(t, wrapped)
	if len(cands) != 1 || cands[0].Method != "PUT" {
		t.Fatalf("包一层的输出应被解析出 1 条 PUT，实际 %+v", cands)
	}
}

func TestParseSemanticCandidatesRejectsInventedMethod(t *testing.T) {
	raw := `[
	  {"method":"FETCH","url":"/api/guess"},
	  {"url":"/api/no-method"},
	  {"method":"GET","url":"/api/ok"}
	]`
	cands, dropped := mustParse(t, raw)
	if len(cands) != 1 || cands[0].URL != "/api/ok" {
		t.Fatalf("只应保留 method 合法的那一条，实际 %+v", cands)
	}
	if len(dropped) != 2 {
		t.Fatalf("两条非法条目都应记录丢弃原因，实际 %v", dropped)
	}
	// 关键判据：看不出 method 时**不许猜 GET**。猜出来的 GET 会变成资产图里
	// 一条不存在的端点，后续所有基于它的测试都建在假前提上。
	for _, d := range dropped {
		if strings.HasPrefix(d, "bad_method") {
			return
		}
	}
	t.Fatalf("缺少 bad_method 丢弃原因: %v", dropped)
}

func TestParseSemanticCandidatesRejectsCrossOriginAndAssets(t *testing.T) {
	raw := `[
	  {"method":"GET","url":"https://evil.example.net/api/steal"},
	  {"method":"GET","url":"https://app.example.com/api/same-origin-ok"},
	  {"method":"GET","url":"/static/js/chunk.1.js"},
	  {"method":"GET","url":"/"},
	  {"method":"GET","url":"api/relative"}
	]`
	cands, dropped := mustParse(t, raw)
	if len(cands) != 1 || cands[0].URL != "https://app.example.com/api/same-origin-ok" {
		t.Fatalf("只应保留同源绝对地址那一条，实际 %+v", cands)
	}
	// 同源绝对地址在 enrich 侧会被 ResolveReference 归一成路径，这里保留原样，
	// 因为跨源判定必须发生在「解析成绝对地址之后」——提前归一等于放弃这道检查。
	if len(dropped) != 4 {
		t.Fatalf("四条应被丢弃，实际丢弃 %d 条: %v", len(dropped), dropped)
	}
}

func TestParseSemanticCandidatesDedupesAndCaps(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 40; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"method":"GET","url":"/api/x"}`)
	}
	b.WriteString("]")
	cands, dropped := mustParse(t, b.String())
	if len(cands) != 1 {
		t.Fatalf("重复条目应去重成 1 条，实际 %d", len(cands))
	}
	if len(dropped) != 39 {
		t.Fatalf("39 条重复应全部记录，实际 %d", len(dropped))
	}

	var big strings.Builder
	big.WriteString("[")
	for i := 0; i < 40; i++ {
		if i > 0 {
			big.WriteString(",")
		}
		big.WriteString(`{"method":"GET","url":"/api/p`)
		big.WriteString(string(rune('a' + i%26)))
		big.WriteString(strconv.Itoa(i))
		big.WriteString(`"}`)
	}
	big.WriteString("]")
	cands, dropped = mustParse(t, big.String())
	if len(cands) > semanticMaxCandidates {
		t.Fatalf("候选数应被截到 %d，实际 %d", semanticMaxCandidates, len(cands))
	}
	found := false
	for _, d := range dropped {
		if d == "over_cap" {
			found = true
		}
	}
	if !found {
		t.Fatalf("截断必须留下 over_cap 痕迹，否则「模型吐了 200 条」这件事会完全隐形: %v", dropped)
	}
}

func TestParseSemanticCandidatesGarbage(t *testing.T) {
	for _, raw := range []string{"", "   ", "抱歉，我无法完成这个请求。", "{not json at all"} {
		cands, dropped := parseSemanticCandidates(raw, semanticTestSource)
		if len(cands) != 0 {
			t.Fatalf("垃圾输入不应产出候选: %q → %+v", raw, cands)
		}
		if len(dropped) == 0 {
			t.Fatalf("垃圾输入必须给出丢弃原因，否则失败不可诊断: %q", raw)
		}
	}
}

func TestParseSemanticCandidatesNormalizesParams(t *testing.T) {
	raw := `[{"method":"POST","url":"/api/save","params":[
	  {"location":"BODY","name":"title","type":"STRING"},
	  {"location":"weird","name":"id"},
	  {"location":"query","name":"  "},
	  {"name":"flag"}
	]}]`
	cands, _ := mustParse(t, raw)
	if len(cands) != 1 {
		t.Fatalf("应有 1 条候选，实际 %d", len(cands))
	}
	params := cands[0].Params
	if len(params) != 3 {
		t.Fatalf("空名参数应被丢掉，剩 3 条，实际 %d: %+v", len(params), params)
	}
	if params[0]["location"] != "body" || params[0]["type"] != "string" {
		t.Fatalf("location/type 应小写化，实际 %+v", params[0])
	}
	if params[1]["location"] != "query" {
		t.Fatalf("未知 location 应回落 query，实际 %+v", params[1])
	}
	if params[2]["type"] != "string" {
		t.Fatalf("缺 type 应补 string，实际 %+v", params[2])
	}
}

// TestParseSemanticCandidatesIsDeterministic 钉住「同一份模型输出两次解析结果相同」。
// 评测基线的前提是这一步无随机性；一旦引入 map 迭代顺序之类的隐含随机，
// 所有基于基线的对比都会失真。
func TestParseSemanticCandidatesIsDeterministic(t *testing.T) {
	raw := `[{"method":"GET","url":"/api/a","params":[{"location":"query","name":"z"},{"location":"body","name":"a"}]},
	         {"method":"POST","url":"/api/b"}]`
	first, _ := parseSemanticCandidates(raw, semanticTestSource)
	for i := 0; i < 20; i++ {
		next, _ := parseSemanticCandidates(raw, semanticTestSource)
		a, _ := json.Marshal(first)
		b, _ := json.Marshal(next)
		if string(a) != string(b) {
			t.Fatalf("解析不确定：第 %d 次结果不同\n%s\n%s", i, a, b)
		}
	}
}
