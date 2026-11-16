package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionErrorPreservesCancellationCause(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		err := opError("http", "request failed: %v", fmt.Errorf("transport: %w", cause))
		if !errors.Is(err, cause) {
			t.Fatalf("lost typed cause: %v", err)
		}
		want := "session http: request failed: transport: " + cause.Error()
		if err.Error() != want {
			t.Fatalf("changed legacy diagnostic: %q", err.Error())
		}
	}
	if errors.Unwrap(opError("test", "target responded HTTP %d", 404)) != nil {
		t.Fatal("protocol failure acquired a fabricated cause")
	}
}

func TestHTTPShellTestPreservesCanceledRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	sh, err := NewHTTPShell(srv.URL, "cmd", "php")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-started; cancel() }()
	if err := sh.Test(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("HTTP driver lost cancellation: %v", err)
	}
}
