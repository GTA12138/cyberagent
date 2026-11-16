package server

import (
	"chengjian.local/chengjian/db"
	"sync"
)

type activitySubscription struct {
	events chan db.Activity
	resync chan struct{}
}

// Broadcaster provides live delivery after persistence. Overflow invalidates a
// subscription; the client must reconnect and replay from its last received ID.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[string]map[*activitySubscription]struct{}
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: map[string]map[*activitySubscription]struct{}{}}
}
func (b *Broadcaster) Subscribe(task string) (<-chan db.Activity, func()) {
	ch, _, unsub := b.SubscribeWithResync(task)
	return ch, unsub
}
func (b *Broadcaster) SubscribeWithResync(task string) (<-chan db.Activity, <-chan struct{}, func()) {
	sub := &activitySubscription{events: make(chan db.Activity, 256), resync: make(chan struct{})}
	b.mu.Lock()
	if b.subs[task] == nil {
		b.subs[task] = map[*activitySubscription]struct{}{}
	}
	b.subs[task][sub] = struct{}{}
	b.mu.Unlock()
	return sub.events, sub.resync, func() { b.mu.Lock(); defer b.mu.Unlock(); b.removeLocked(task, sub) }
}
func (b *Broadcaster) removeLocked(task string, sub *activitySubscription) {
	if _, ok := b.subs[task][sub]; !ok {
		return
	}
	delete(b.subs[task], sub)
	close(sub.events)
	if len(b.subs[task]) == 0 {
		delete(b.subs, task)
	}
}
func (b *Broadcaster) Publish(task string, a db.Activity) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs[task] {
		select {
		case sub.events <- a:
		default:
			close(sub.resync)
			b.removeLocked(task, sub)
		}
	}
}
