//go:build windows

package procguard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestGuardHelper(t *testing.T) {
	if os.Getenv("CHENGJIAN_GUARD_TEST_HELPER") != "1" {
		return
	}
	switch os.Getenv("CHENGJIAN_GUARD_TEST_MODE") {
	case "memory":
		// Request explicit committed memory, independent of the Go heap allocator.
		// The fixture never touches the region, even if containment regresses.
		address, err := windows.VirtualAlloc(0, 128<<20, windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE)
		if err != nil { os.Exit(42) }
		_ = windows.VirtualFree(address, 0, windows.MEM_RELEASE)
		os.Exit(0)
	case "parent":
		c := exec.Command(os.Args[0], "-test.run=^TestGuardHelper$")
		c.Env = append(os.Environ(), "CHENGJIAN_GUARD_TEST_MODE=sleep")
		if err := c.Start(); err != nil {
			os.Exit(20)
		}
		_ = os.WriteFile(os.Getenv("CHENGJIAN_GUARD_TEST_PIDFILE"), []byte(strconv.Itoa(c.Process.Pid)), 0o600)
		os.Exit(0)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

func helperCommand(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^TestGuardHelper$")
	c.Env = append(os.Environ(), "CHENGJIAN_GUARD_TEST_HELPER=1", "CHENGJIAN_GUARD_TEST_MODE="+mode)
	var out Buffer
	out.Limit = 64 << 10
	c.Stdout, c.Stderr = &out, &out
	return c
}

func TestJobMemoryLimitRejectsOversizedAllocation(t *testing.T) {
	c := helperCommand(t, "memory")
	p, err := StartWithLimits(c, Limits{64 << 20, 4, 50})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err == nil || c.ProcessState.ExitCode() != 42 {
		p.Close("failure")
		t.Fatalf("128MiB commit was not denied by a 64MiB job: %v", err)
	}
	peak := p.Close("memory_limit_test")
	if peak == 0 {
		t.Fatal("job did not record a memory counter")
	}
}

func TestClosingJobReclaimsDescendantAfterParentExit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	c := helperCommand(t, "parent")
	c.Env = append(c.Env, "CHENGJIAN_GUARD_TEST_PIDFILE="+pidFile)
	p, err := Start(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil {
		p.Close("failure")
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		p.Close("failure")
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(data))
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		p.Close("failure")
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	p.Close("parent_completed")
	state, err := windows.WaitForSingleObject(h, 3000)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant survived job close: %d %v", state, err)
	}
}

func TestRunCancellationKillsOwnedJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c := helperCommand(t, "sleep")
	if err := Run(ctx, c); err == nil || ctx.Err() == nil {
		t.Fatalf("cancellation failed: %v", err)
	}
}
