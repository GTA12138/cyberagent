package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"time"

	"chengjian.local/chengjian/db"
)

// Keep the runtime independently testable without connecting to an operator's DB.
type tunnelStore interface {
	Create(context.Context, *db.TunnelRecord) (int64, error)
	Get(context.Context, int64) (*db.TunnelRecord, error)
	UpdateState(context.Context, int64, string, string) error
	UpdateDeployParams(context.Context, int64, int, json.RawMessage) error
	ListActive(context.Context) ([]*db.TunnelRecord, error)
	ListByTask(context.Context, int64) ([]*db.TunnelRecord, error)
	Delete(context.Context, int64) error
}

var errTunnelStopping = errors.New("隧道正在停止")

type tunnelOperation struct {
	token      chan struct{}
	refs       int
	stopping   bool
	activeStop bool
	cancel     context.CancelFunc
}

// Serialize mutation per tunnel, not per manager. Stop cancels the in-flight
// operation before waiting; the context also bounds the wait for this gate.
func (m *Manager) beginOperation(ctx context.Context, id int64, stop bool) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	m.opsMu.Lock()
	if m.ops == nil {
		m.ops = make(map[int64]*tunnelOperation)
	}
	op := m.ops[id]
	if op == nil {
		op = &tunnelOperation{token: make(chan struct{}, 1)}
		op.token <- struct{}{}
		m.ops[id] = op
	}
	op.refs++
	if stop {
		op.stopping = true
		if op.cancel != nil && !op.activeStop {
			op.cancel()
		}
	}
	m.opsMu.Unlock()

	drop := func(held bool) {
		m.opsMu.Lock()
		if held {
			op.cancel = nil
			op.token <- struct{}{}
		}
		op.refs--
		if op.refs == 0 {
			delete(m.ops, id)
		}
		m.opsMu.Unlock()
	}
	select {
	case <-ctx.Done():
		drop(false)
		return ctx, nil, ctx.Err()
	case <-op.token:
	}
	m.opsMu.Lock()
	if err := ctx.Err(); err != nil {
		m.opsMu.Unlock()
		drop(true)
		return ctx, nil, err
	}
	if op.stopping && !stop {
		m.opsMu.Unlock()
		drop(true)
		return ctx, nil, errTunnelStopping
	}
	opCtx, cancel := context.WithCancel(ctx)
	op.cancel = cancel
	op.activeStop = stop
	m.opsMu.Unlock()
	return opCtx, func() { cancel(); drop(true) }, nil
}

type managedProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (m *Manager) trackProcess(id int64, cmd *exec.Cmd, logFile io.Closer) *managedProcess {
	p := &managedProcess{cmd: cmd, done: make(chan struct{})}
	m.procsMu.Lock()
	if m.procs == nil {
		m.procs = make(map[int64]*managedProcess)
	}
	m.procs[id] = p
	m.procsMu.Unlock()
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
		close(p.done)
	}()
	return p
}

// Rollback must retain the caller's values but survive its canceled deadline.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

// This path has not started any resources. Obtain the same gate before dropping
// its initial row so an explicit teardown cannot be overwritten by cancellation.
func (m *Manager) discardUnstarted(ctx context.Context, id int64) {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	ctx, release, err := m.beginOperation(ctx, id, true)
	if err != nil {
		return
	}
	defer release()
	rec, err := m.store.Get(ctx, id)
	if err == nil && rec != nil && rec.State == db.TunnelDeploying {
		_ = m.store.Delete(ctx, id)
	}
}
