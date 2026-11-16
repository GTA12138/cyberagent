package llmrec

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestCaptureBudgetPreservesWireAndMarksMissingBytes(t *testing.T) {
	_, c := NewCapture(context.Background())
	c.SetRequest(strings.Repeat("x", maxCaptureRequest+1))
	if !strings.Contains(c.RawRequest(), "truncated") || len(c.RawRequest()) > maxCaptureRequest+100 {
		t.Fatal("unbounded or silent request capture")
	}
	want := int64(maxCaptureBytes + 1024)
	for i := 0; i < 2; i++ {
		body := c.TeeResponse(200, io.NopCloser(io.LimitReader(captureRepeater{}, want)))
		n, err := io.Copy(io.Discard, body)
		body.Close()
		if err != nil || n != want {
			t.Fatalf("wire altered: bytes=%d err=%v", n, err)
		}
	}
	if c.retained != maxCaptureBytes || !strings.Contains(c.RawResponse(), "capture incomplete") {
		t.Fatal("capture budget missing")
	}
	for _, a := range c.Attempts() {
		if !a.Truncated || a.ReceivedBytes != want {
			t.Fatal("incomplete capture disguised as full raw body")
		}
	}
	for i := 0; i < 100; i++ {
		c.TeeResponse(200, io.NopCloser(strings.NewReader(""))).Close()
	}
	if len(c.attempts) != maxCaptureAttempts || !strings.Contains(c.RawResponse(), "further HTTP attempts") {
		t.Fatal("unbounded attempt capture")
	}
}

type captureRepeater struct{}

func (captureRepeater) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
