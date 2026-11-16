package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type cleanupSession struct {
	id    int64
	close func(context.Context) error
}

func (s *cleanupSession) ID() int64                { return s.id }
func (*cleanupSession) Kind() string               { return "fixture" }
func (*cleanupSession) Test(context.Context) error { return nil }
func (*cleanupSession) Exec(context.Context, string, time.Duration) (string, string, error) {
	return "", "", nil
}
func (*cleanupSession) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (*cleanupSession) WriteFile(context.Context, string, []byte) error  { return nil }
func (s *cleanupSession) Close(ctx context.Context) error                { return s.close(ctx) }

func TestRegistryCleanupFailureRetainsDriverForRetry(t *testing.T) {
	r := NewRegistry()
	cause := errors.New("cleanup result unavailable")
	sess := &cleanupSession{id: 3, close: func(context.Context) error { return cause }}
	r.Add(sess)
	if removed, err := r.CloseAndRemove(context.Background(), 3); removed || !errors.Is(err, cause) {
		t.Fatalf("failed cleanup: removed=%v err=%v", removed, err)
	}
	if got, ok := r.Get(3); !ok || got != sess {
		t.Fatal("failed cleanup discarded retryable driver")
	}
	sess.close = func(context.Context) error { return nil }
	if removed, err := r.CloseAndRemove(context.Background(), 3); !removed || err != nil {
		t.Fatalf("retry: removed=%v err=%v", removed, err)
	}
}

func TestRegistryCleanupDoesNotBlockRegistryAndSharesWork(t *testing.T) {
	r := NewRegistry()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	sess := &cleanupSession{id: 4, close: func(context.Context) error {
		calls.Add(1)
		close(started)
		<-release
		return nil
	}}
	r.Add(sess)
	finished := make(chan error, 1)
	go func() { _, err := r.CloseAndRemove(context.Background(), 4); finished <- err }()
	<-started
	if got, ok := r.Get(4); !ok || got != sess {
		t.Fatal("pending cleanup hid driver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if removed, err := r.CloseAndRemove(ctx, 4); removed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second caller must be able to abandon wait: removed=%v err=%v", removed, err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("cleanup called %d times", calls.Load())
	}
	if _, ok := r.Get(4); ok {
		t.Fatal("confirmed cleanup left driver registered")
	}
}

func TestRegistryCleanupPreservesConcurrentReplacement(t *testing.T) {
	r := NewRegistry()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	old := &cleanupSession{id: 5, close: func(context.Context) error {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return nil
	}}
	r.Add(old)
	finished := make(chan error, 1)
	go func() { _, err := r.CloseAndRemove(context.Background(), 5); finished <- err }()
	<-started
	replacement := &cleanupSession{id: 5, close: func(context.Context) error { return nil }}
	r.Add(replacement)
	close(release)
	if err := <-finished; !errors.Is(err, ErrSessionReplaced) {
		t.Fatalf("replacement should invalidate cleanup: %v", err)
	}
	if got, ok := r.Get(5); !ok || got != replacement {
		t.Fatal("cleanup removed the replacement")
	}
}
