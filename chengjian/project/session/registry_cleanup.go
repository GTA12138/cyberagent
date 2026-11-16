package session

import (
	"context"
	"errors"
)

// ErrSessionReplaced means cleanup finished for an older driver while another
// driver was registered under the same ID. Its persistent row must be retained.
var ErrSessionReplaced = errors.New("session was replaced during cleanup; retry against the current session")

type sessionCloseCall struct {
	sess    Session
	done    chan struct{}
	removed bool
	err     error
}

// CloseAndRemove removes a driver only after Close confirms success. Failed or
// canceled cleanup retains the driver for a retry. No registry lock is held
// while contacting the driver, and concurrent callers share a cleanup result.
func (r *Registry) CloseAndRemove(ctx context.Context, id int64) (bool, error) {
	r.mu.Lock()
	sess, ok := r.sessions[id]
	if !ok {
		r.mu.Unlock()
		return false, nil
	}
	if call := r.closing[id]; call != nil && call.sess == sess {
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-call.done:
			return call.removed, call.err
		}
	}
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return false, err
	}
	call := &sessionCloseCall{sess: sess, done: make(chan struct{})}
	if r.closing == nil {
		r.closing = make(map[int64]*sessionCloseCall)
	}
	r.closing[id] = call
	r.mu.Unlock()

	err := sess.Close(ctx)
	r.mu.Lock()
	if err == nil {
		if current, exists := r.sessions[id]; exists && current != sess {
			err = ErrSessionReplaced
		} else {
			delete(r.sessions, id)
			call.removed = true
		}
	}
	call.err = err
	if r.closing[id] == call {
		delete(r.closing, id)
	}
	close(call.done)
	r.mu.Unlock()
	return call.removed, call.err
}
