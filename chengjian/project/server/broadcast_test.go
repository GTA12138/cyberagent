package server

import (
	"chengjian.local/chengjian/db"
	"sync"
	"testing"
)

func TestBroadcasterOverflowDisconnectsOnlySlowSubscriber(t *testing.T) {
	b := NewBroadcaster()
	slow, resync, unsub := b.SubscribeWithResync("task")
	defer unsub()
	fast, fastResync, unsubFast := b.SubscribeWithResync("task")
	defer unsubFast()
	for id := int64(1); id <= 600; id++ {
		b.Publish("task", db.Activity{ID: id})
		if got := <-fast; got.ID != id {
			t.Fatalf("healthy subscriber lost %d: %d", id, got.ID)
		}
	}
	select {
	case <-resync:
	default:
		t.Fatal("slow subscription did not require resync")
	}
	select {
	case <-fastResync:
		t.Fatal("healthy subscription disconnected")
	default:
	}
	n := 0
	for range slow {
		n++
	}
	if n != 256 {
		t.Fatalf("buffer=%d", n)
	}
	unsub()
	unsub() // Overflow and repeated deferred unsubscribe cannot double-close.
	fresh, _, closeFresh := b.SubscribeWithResync("task")
	defer closeFresh()
	b.Publish("task", db.Activity{ID: 601})
	if got := <-fresh; got.ID != 601 {
		t.Fatal(got.ID)
	}
}

func TestBroadcasterConcurrentOverflowAndUnsubscribe(t *testing.T) {
	b := NewBroadcaster()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 400; j++ {
				_, _, unsub := b.SubscribeWithResync("task")
				b.Publish("task", db.Activity{ID: int64(j + 1)})
				unsub()
				unsub()
			}
		}()
	}
	wg.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subs) != 0 {
		t.Fatal("subscription leak")
	}
}
