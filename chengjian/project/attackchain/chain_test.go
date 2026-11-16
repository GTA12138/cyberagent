package attackchain

import (
	"strings"
	"testing"
)

func TestActionUsesPrimaryBehavior(t *testing.T) {
	for title, want := range map[string]string{
		"未授权读取（签名 + 绕过头）": "read",
		"未授权获取签名密钥":       "credential",
		"计算签名":            "credential",
		"未授权写入":           "write",
		"对照检查":            "compare",
	} {
		if got := actionFor(title, ""); got != want {
			t.Errorf("%s: got %s, want %s", title, got, want)
		}
	}
}

func TestReferencesPreserveNodeNamespace(t *testing.T) {
	refs := References("依赖 finding79/80，见 fact326、fact331。finding79 与 intent 328；不是 finding_id=31")
	want := []Reference{{"finding", 79}, {"finding", 80}, {"fact", 326}, {"fact", 331}, {"intent", 328}}
	if len(refs) != len(want) {
		t.Fatalf("refs=%v", refs)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("refs=%v", refs)
		}
	}
}

func TestBuildKeepsMissingDataAndReferencesHonest(t *testing.T) {
	c := Build(Finding{ID: 31, Name: "example", Summary: "报告背景", Report: "## 复现步骤\n**步骤 1：读取配置**\nGET /config\n```python\n1. must_not_become_a_step\n```\n**步骤 2：对照检查**\n响应记录\n## 未验证/局限\n未证明执行。\n"}, []Related{{Reference: Reference{"finding", 79}, FindingID: 8, Title: "前置漏洞", Detail: `{"summary":"source"}`}}, nil, nil)
	if c.Nodes[2].Basis != "missing" {
		t.Fatal("invented inference")
	}
	if !strings.Contains(c.Nodes[5].Summary, "未证明") {
		t.Fatal("lost limitation")
	}
	steps := 0
	for _, s := range c.Sources {
		if s.Kind == "step" {
			steps++
			if strings.Contains(s.Label, "must_not") {
				t.Fatal("parsed code as step")
			}
		}
	}
	if steps != 2 {
		t.Fatalf("steps=%d", steps)
	}
	ref := c.Nodes[len(c.Nodes)-1]
	if ref.FindingID != "8" || ref.Reference != "finding79" {
		t.Fatalf("mixed ID namespaces: %+v", ref)
	}
	for _, e := range c.Edges {
		if e.Source == ref.ID && e.Kind != "reference" {
			t.Fatal("citation became verified prerequisite")
		}
	}
	if c.TrafficCount != 0 || len(c.Warnings) == 0 {
		t.Fatal("lost missing traffic warning")
	}
}

func TestBuildCitationsAndRevision(t *testing.T) {
	f := Finding{ID: 1, Report: "## 根因\n公开脚本 /static/js/app.123.js 包含配置。\n## 复现步骤\n1) 获取公开文件\n结果一\n2) 验证边界\n结果二"}
	c := Build(f, nil, nil, nil)
	found := false
	for _, s := range c.Sources {
		if s.Kind == "file" && strings.Contains(s.Text, "/static/js/app.123.js") && s.Line == 2 {
			found = true
		}
	}
	if !found {
		t.Fatal("missing file provenance")
	}
	if Build(f, nil, nil, nil).Revision != c.Revision {
		t.Fatal("non deterministic revision")
	}
	f.Report += "\n追加新证据"
	if Build(f, nil, nil, nil).Revision == c.Revision {
		t.Fatal("revision did not change")
	}
	for _, n := range c.Nodes {
		for _, id := range n.Sources {
			exists := false
			for _, s := range c.Sources {
				if s.ID == id {
					exists = true
				}
			}
			if !exists {
				t.Fatalf("dangling citation %s", id)
			}
		}
	}
}

func TestBuildFileSourcesUseActualPathExtension(t *testing.T) {
	f := Finding{ID: 2, Report: "https://example.invalid/api/data.json\nhttps://example.invalid/login.jsp\nhttps://example.invalid/api?file=app.js\nhttps://example.invalid/static/app.JS?v=1\n/index.html\n/api/data.json\nlogin.jsp\n/static/app.js.map"}
	c := Build(f, nil, nil, nil)
	var files []Source
	for _, s := range c.Sources {
		if s.Kind == "file" {
			files = append(files, s)
		}
	}
	if len(files) != 2 {
		t.Fatalf("wrong file sources: %+v", files)
	}
	if files[0].Line != 4 || files[1].Line != 5 || files[0].Locator != "finding:2/report" {
		t.Fatalf("wrong file provenance: %+v", files)
	}
}

func TestBuildCircledAndNestedStepsKeepDocumentLines(t *testing.T) {
	f := Finding{ID: 3, Report: "# 测试\n## 复现步骤\n\n**① 无租户头**\nGET /control\n**② 增加租户头**\nGET /target\n## 根因分析\n\n公开配置。"}
	c := Build(f, nil, nil, nil)
	var steps []Source
	for _, s := range c.Sources {
		if s.Kind == "step" {
			steps = append(steps, s)
		}
		if s.Label == "根因分析" && (s.Line != 10 || s.Text != "公开配置。") {
			t.Fatalf("report body line mismatch: %+v", s)
		}
	}
	if len(steps) != 2 || steps[0].Line != 4 || steps[1].Line != 6 || steps[0].Locator != "finding:3/report" {
		t.Fatalf("wrong circled step provenance: %+v", steps)
	}
	f.Report = "## 复现步骤\n### 1. 获取配置\nGET /config\n### 2. 验证\nGET /target\n## 影响与危害\n存在越权。"
	c = Build(f, nil, nil, nil)
	steps = nil
	for _, s := range c.Sources {
		if s.Kind == "step" {
			steps = append(steps, s)
		}
	}
	if len(steps) != 2 || steps[0].Line != 2 || steps[1].Line != 4 || steps[0].Text != "### 1. 获取配置\nGET /config" {
		t.Fatalf("lost nested heading steps: %+v", steps)
	}
}

func TestBuildUnstructuredEvidenceIsOneRecord(t *testing.T) {
	evidence := "\n```bash\n# 1) read a configuration\n# 2) compare the response\n```"
	c := Build(Finding{ID: 4, Evidence: evidence}, nil, nil, nil)
	events := []Node{}
	for _, n := range c.Nodes {
		if n.Kind == "event" {
			events = append(events, n)
		}
	}
	if len(events) != 1 || events[0].Title != "验证证据记录" || events[0].Basis != "reported" {
		t.Fatalf("fabricated evidence sequence: %+v", events)
	}
	for _, s := range c.Sources {
		if s.ID == events[0].Sources[0] && (s.Locator != "finding:4/evidence" || s.Line != 2 || s.Text != strings.TrimSpace(evidence)) {
			t.Fatalf("evidence provenance mismatch: %+v", s)
		}
	}
	if !strings.Contains(strings.Join(c.Warnings, "\n"), "不代表已确认执行时序") {
		t.Fatal("unstructured evidence lacks sequencing caveat")
	}
	c = Build(Finding{ID: 5, Evidence: "\n1) read\nGET /a\n2) compare\nGET /b"}, nil, nil, nil)
	for _, s := range c.Sources {
		if s.Kind == "step" && (s.Locator != "finding:5/evidence" || s.Line != 2 && s.Line != 4) {
			t.Fatalf("numbered evidence locator: %+v", s)
		}
	}
}
