package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDashboardNetworkRate(t *testing.T) {
	for _, tc := range []struct {
		name              string
		previous, current uint64
		seconds, want     float64
	}{
		{"normal", 100, 350, 2.5, 100},
		{"counter reset", 350, 100, 2.5, 0},
		{"zero elapsed", 100, 350, 0, 0},
		{"clock reversal", 100, 350, -1, 0},
		{"idle", 100, 100, 2.5, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := networkRate(tc.previous, tc.current, tc.seconds); got != tc.want {
				t.Fatalf("rate=%v want %v", got, tc.want)
			}
		})
	}
}

type blockedDashboardWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
}

func (w *blockedDashboardWriter) Header() http.Header { return w.header }
func (w *blockedDashboardWriter) WriteHeader(int)     {}
func (w *blockedDashboardWriter) Write(body []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(body), nil
}

func TestDashboardSlowClientDoesNotBlockOtherClients(t *testing.T) {
	s := &Server{}
	s.dashboardTelemetry.data.SampledAt = time.Now()
	w := &blockedDashboardWriter{make(http.Header), make(chan struct{}), make(chan struct{})}
	t.Cleanup(func() { close(w.release) })
	go s.dashboardSystem(w, httptest.NewRequest(http.MethodGet, "/api/dashboard/telemetry", nil))
	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("first response did not start")
	}
	done := make(chan struct{})
	go func() {
		s.dashboardSystem(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/dashboard/telemetry", nil))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slow response blocked the sampler cache")
	}
}
