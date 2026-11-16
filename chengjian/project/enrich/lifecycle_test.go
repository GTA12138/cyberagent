package enrich

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type cancelAnalyzer struct {
	started chan struct{}
	release chan struct{}
}

func (a *cancelAnalyzer) AnalyzeCandidates(ctx context.Context, _ AnalyzeRequest) ([]Candidate, error) {
	close(a.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-a.release:
		return nil, nil
	}
}

func TestCloseCancelsInFlightAnalysisAndWaits(t *testing.T) {
	e := New(nil, nil, 1)
	a := &cancelAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
	e.SetJSAnalyzer(a)
	defer e.Close()
	defer close(a.release) // 故障时也释放 worker，避免测试本身挂起。
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		e.analyze(job{taskID: 1, arg: "http://127.0.0.1/fixture.js"}, Material{}, "fetch('/fixture')")
	}()
	select {
	case <-a.started:
	case <-time.After(3 * time.Second):
		t.Fatal("analysis did not start")
	}
	done := make(chan struct{})
	go func() { e.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel and drain semantic work")
	}
}

type failedMaterialBody struct{ io.Reader }

func (b failedMaterialBody) Read(p []byte) (int, error) {
	n, _ := b.Reader.Read(p)
	return n, errors.New("fixture body truncated")
}

func (failedMaterialBody) Close() error { return nil }

type materialTransport struct{ requests atomic.Int32 }

func (tr *materialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.requests.Add(1)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}},
		Body: failedMaterialBody{strings.NewReader(`<script src="/should-not-follow.js"></script>`)}, Request: req}, nil
}

func TestFailedHTTPBodyDoesNotDispatchDerivedWork(t *testing.T) {
	tr := &materialTransport{}
	e := &Engine{ctx: context.Background(), client: &http.Client{Transport: tr},
		jobs: make(chan job, 4), usage: map[int64]*taskUsage{}}
	e.doHTTP(job{taskID: 1, arg: "http://127.0.0.1/fixture"})
	if len(e.jobs) != 0 || tr.requests.Load() != 1 {
		t.Fatal("failed response dispatched derived work")
	}
}

func TestCooldownIsTaskScoped(t *testing.T) {
	e := &Engine{}
	a := job{kind: jobHTTP, id: 12, taskID: 1, arg: "http://fixture.test/"}
	b := a
	b.taskID = 2
	if e.onCooldown(a) || e.onCooldown(b) || !e.onCooldown(a) {
		t.Fatal("shared asset cooldown leaked between tasks")
	}
	a.id, b.id = 0, 0
	if e.onCooldown(a) || e.onCooldown(b) || !e.onCooldown(a) {
		t.Fatal("derived material cooldown leaked between tasks")
	}
}

func TestCloseCancelsInFlightHTTPAndWaits(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer site.Close()
	e := New(nil, nil, 1)
	defer e.Close()
	e.ProbeSite(1, 1, site.URL)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP job did not start")
	}
	done := make(chan struct{})
	go func() { e.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel and drain HTTP work")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("request context was not canceled")
	}
}
