package server

import (
	"bytes"
	"context"
	"fmt"
	"chengjian.local/chengjian/db"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type slowActivityWriter struct {
	header                  http.Header
	body                    bytes.Buffer
	ready, entered, release chan struct{}
	readyOnce, writeOnce    sync.Once
}

func (w *slowActivityWriter) Header() http.Header { return w.header }
func (w *slowActivityWriter) WriteHeader(int)     {}
func (w *slowActivityWriter) Flush()              { w.readyOnce.Do(func() { close(w.ready) }) }
func (w *slowActivityWriter) Write(b []byte) (int, error) {
	w.writeOnce.Do(func() { close(w.entered); <-w.release })
	return w.body.Write(b)
}

type replayActivityWriter struct {
	*httptest.ResponseRecorder
	ids    []int64
	want   int
	cancel context.CancelFunc
}

func (w *replayActivityWriter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "id: ") {
			id, _ := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			w.ids = append(w.ids, id)
		}
	}
	n, err := w.ResponseRecorder.Write(b)
	if len(w.ids) >= w.want {
		w.cancel()
	}
	return n, err
}
func (w *replayActivityWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func TestActivityStreamSlowClientReconnectReplaysBurst(t *testing.T) {
	s, f, _ := trafficEvidenceServer(t)
	finding, err := s.m.pg.GetFinding(f.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	task, ok := s.m.Task(strconv.FormatInt(*finding.TaskID, 10))
	if !ok {
		t.Fatal("missing task")
	}
	_, since, err := task.Store.ActivityListContext(t.Context(), nil, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer := &slowActivityWriter{header: make(http.Header), ready: make(chan struct{}), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/activity/stream?task=%s&since=%d", task.ID, since), nil).WithContext(ctx)
	go func() { defer close(done); s.streamActivity(writer, req) }()
	select {
	case <-writer.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not subscribe")
	}
	var ids []int64
	ids = append(ids, s.engine.emitActivity(task, db.Activity{Worker: "test", Kind: "text", Summary: "first"}).ID)
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow client did not block")
	}
	for i := 0; i < 700; i++ {
		ids = append(ids, s.engine.emitActivity(task, db.Activity{Worker: "test", Kind: "text", Summary: fmt.Sprintf("burst %d", i)}).ID)
	}
	close(writer.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("overflow left stream connected")
	}
	if !strings.Contains(writer.body.String(), fmt.Sprintf("id: %d\n", ids[0])) {
		t.Fatal("first delivered cursor missing")
	}
	replayCtx, stop := context.WithCancel(t.Context())
	defer stop()
	replay := &replayActivityWriter{ResponseRecorder: httptest.NewRecorder(), want: 700, cancel: stop}
	// Header takes precedence over a stale/incorrect query. More than one batch
	// must be replayed and every event after the last delivered cursor must arrive.
	reconnect := httptest.NewRequest("GET", "/api/activity/stream?task="+task.ID+"&since=999999999", nil).WithContext(replayCtx)
	reconnect.Header.Set("Last-Event-ID", strconv.FormatInt(ids[0], 10))
	replayDone := make(chan struct{})
	go func() { defer close(replayDone); s.streamActivity(replay, reconnect) }()
	select {
	case <-replayDone:
	case <-time.After(10 * time.Second):
		stop()
		<-replayDone
		t.Fatal("replay did not catch up")
	}
	if len(replay.ids) != 700 {
		t.Fatalf("replay count=%d", len(replay.ids))
	}
	for i, id := range replay.ids {
		if id != ids[i+1] {
			t.Fatalf("replay gap at %d: %d != %d", i, id, ids[i+1])
		}
	}
}
