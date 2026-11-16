package tool

import (
	"fmt"
	"os"
	"sync"
)

// Enforce the file budget in the writer, rather than allowing arbitrary growth
// between watchdog polls. The process watcher owns termination and reaping.
type taskOutputWriter struct {
	mu       sync.Mutex
	file     *os.File
	written  int64
	overflow bool
	exceeded chan struct{}
}

func (w *taskOutputWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := int(min(int64(len(data)), max(0, int64(maxTaskOutputBytes)-w.written)))
	written, err := w.file.Write(data[:n])
	w.written += int64(written)
	if err != nil {
		return written, err
	}
	if n < len(data) {
		if !w.overflow {
			w.overflow = true
			close(w.exceeded)
		}
		return written, fmt.Errorf("output_limit_exceeded")
	}
	return written, nil
}
func (w *taskOutputWriter) overflowed() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.overflow }
