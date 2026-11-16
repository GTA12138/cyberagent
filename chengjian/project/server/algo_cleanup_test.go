package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAlgorithmCancellationStopsChildProcesses(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-heartbeat.txt")
	child := "import sys,time\nwhile True:\n with open(sys.argv[1], 'w') as f: f.write(str(time.monotonic_ns()))\n time.sleep(0.02)\n"
	script := fmt.Sprintf("import subprocess,sys,time\nsubprocess.Popen([sys.executable,'-c',%q,%q])\nwhile True: time.sleep(1)\n", child, marker)
	_, err := runAlgorithmScriptWithTimeout(context.Background(), script, json.RawMessage(`{}`), time.Second)
	if err == nil {
		t.Fatal("expected timeout")
	}
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("child fixture did not start: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	after, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("algorithm child continued running after timeout")
	}
}

func TestAlgorithmAlreadyCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runAlgorithmScript(ctx, `print("{}")`, json.RawMessage(`{}`)); err == nil {
		t.Fatal("canceled execution accepted")
	}
}
