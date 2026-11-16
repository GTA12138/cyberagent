package enrich

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type completeMaterialTransport struct{ body string }

func (tr completeMaterialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, ContentLength: int64(len(tr.body)), Body: io.NopCloser(strings.NewReader(tr.body)), Request: req}, nil
}

func TestOversizedMaterialIsNotAnalyzedAsComplete(t *testing.T) {
	body := `<script src="/must-not-follow.js"></script>` + strings.Repeat(" ", maxTextMaterial)
	e := &Engine{ctx: context.Background(), client: &http.Client{Transport: completeMaterialTransport{body}}, jobs: make(chan job, 4), usage: map[int64]*taskUsage{}}
	var reasons []string
	e.SetIssueObserver(func(id int64, u, reason string) {
		if id != 7 {
			t.Error("wrong task")
		}
		reasons = append(reasons, reason)
	})
	e.doHTTP(job{taskID: 7, arg: "http://fixture.test/"})
	if len(e.jobs) != 0 || len(reasons) != 1 || !strings.Contains(reasons[0], "body_limit") {
		t.Fatalf("partial material treated as complete: jobs=%d reasons=%v", len(e.jobs), reasons)
	}
}

func TestIncompleteEnrichmentIsObservableAndBounded(t *testing.T) {
	e := &Engine{ctx: context.Background(), client: &http.Client{Transport: &materialTransport{}}, jobs: make(chan job, 4), usage: map[int64]*taskUsage{}}
	var reasons []string
	e.SetIssueObserver(func(_ int64, _ string, r string) { reasons = append(reasons, r) })
	e.doHTTP(job{taskID: 1, arg: "http://fixture.test/"})
	if len(reasons) != 1 || !strings.Contains(reasons[0], "body_read_failed") {
		t.Fatalf("read failure hidden: %v", reasons)
	}
	j := job{taskID: 2, arg: "http://fixture.test/"}
	for i := 0; i < 100; i++ {
		e.issue(j, strings.Repeat("x", i+1))
	}
	if len(reasons) != 65 {
		t.Fatalf("unbounded diagnostics: %d", len(reasons))
	}
}
