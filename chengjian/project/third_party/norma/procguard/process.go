// Package procguard owns resource-bounded local tool processes. Limits are host
// policy, never parameters supplied by generated scripts or tool calls.
package procguard

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Limits struct {
	MemoryBytes uint64
	Processes   uint32
	CPUPercent  uint32
}

func DefaultLimits() Limits {
	return Limits{MemoryBytes: uint64(hostInt("CHENGJIAN_EXEC_MEMORY_MB", 512, 64, 16384)) << 20, Processes: uint32(hostInt("CHENGJIAN_EXEC_PROCESSES", 16, 1, 64)), CPUPercent: uint32(hostInt("CHENGJIAN_EXEC_CPU_PERCENT", 50, 1, 100))}
}

func hostInt(key string, fallback, low, high int) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n < low || n > high {
		return fallback
	}
	return n
}

var slots = make(chan struct{}, 4)

type Process struct {
	cmd      *exec.Cmd
	platform *platformGuard
	started  time.Time
	once     sync.Once
	peak     uint64
}

// Start acquires a server-wide execution slot and starts the child with its
// descendants contained before generated code is allowed to run on Windows.
func Start(cmd *exec.Cmd) (*Process, error) { return StartWithLimits(cmd, DefaultLimits()) }

func StartWithLimits(cmd *exec.Cmd, limits Limits) (*Process, error) {
	select {
	case slots <- struct{}{}:
	default:
		return nil, fmt.Errorf("execution_capacity_exceeded: 4 local commands are active; wait for or stop an owned task before retrying")
	}
	p := &Process{cmd: cmd, started: time.Now()}
	g, err := startPlatform(cmd, limits)
	if err != nil {
		<-slots
		return nil, fmt.Errorf("execution_guard_unavailable: %w", err)
	}
	p.platform = g
	argHash := sha256.Sum256([]byte(strings.Join(cmd.Args, "\x00")))
	log.Printf("[execution] start pid=%d executable=%q cwd=%q args_sha256=%x memory_limit=%d cpu_percent=%d process_limit=%d", cmd.Process.Pid, filepath.Base(cmd.Path), cmd.Dir, argHash, limits.MemoryBytes, limits.CPUPercent, limits.Processes)
	for _, arg := range cmd.Args[1:] {
		if strings.EqualFold(filepath.Ext(arg), ".py") {
			path := arg
			if !filepath.IsAbs(path) {
				path = filepath.Join(cmd.Dir, path)
			}
			if info, e := os.Stat(path); e == nil && info.Size() <= 2<<20 {
				if data, e := os.ReadFile(path); e == nil {
					h := sha256.Sum256(data)
					log.Printf("[execution] script pid=%d path=%q sha256=%x", cmd.Process.Pid, path, h)
				}
			}
		}
	}
	return p, nil
}

// Kill addresses the owned process group/job, never a process-name match.
func (p *Process) Kill() error { return p.platform.kill(p.cmd) }

// Close must follow Wait. It also terminates descendants left behind by an
// exited shell, records peak job memory, and releases capacity exactly once.
func (p *Process) Close(reason string) uint64 {
	p.once.Do(func() {
		p.peak = p.platform.peakMemory()
		p.platform.close()
		exit := -1
		if p.cmd.ProcessState != nil {
			exit = p.cmd.ProcessState.ExitCode()
		}
		log.Printf("[execution] end pid=%d exit=%d reason=%q elapsed_ms=%d peak_job_memory=%d", p.cmd.Process.Pid, exit, reason, time.Since(p.started).Milliseconds(), p.peak)
		<-slots
	})
	return p.peak
}

// Run is for synchronous script tools. Output must be a bounded writer/file.
func Run(ctx context.Context, cmd *exec.Cmd) error {
	p, err := Start(cmd)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Kill()
		case <-done:
		}
	}()
	err = cmd.Wait()
	close(done)
	reason := "completed"
	if err != nil {
		reason = "command_failed_or_resource_limit"
	}
	if ctx.Err() != nil {
		reason = ctx.Err().Error()
	}
	p.Close(reason)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Buffer caps retained output at a hard byte budget and reports overflow to the
// pipe copier, causing exec.Wait to surface failure instead of silently losing it.
type Buffer struct {
	mu       sync.Mutex
	data     []byte
	Limit    int
	Overflow bool
}

func (b *Buffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	limit := b.Limit
	if limit <= 0 {
		limit = 8 << 20
	}
	n := min(len(data), max(0, limit-len(b.data)))
	b.data = append(b.data, data[:n]...)
	if n < len(data) {
		b.Overflow = true
		return n, fmt.Errorf("output_limit_exceeded: %d bytes", limit)
	}
	return n, nil
}

func (b *Buffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data...)
}
