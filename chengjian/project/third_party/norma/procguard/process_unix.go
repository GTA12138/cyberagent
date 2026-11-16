//go:build !windows

package procguard

import (
	"os/exec"
	"syscall"
)

// Unix keeps process-group cancellation. Windows is the supported resource-
// limited host; Linux deployments should additionally use cgroup/container limits.
type platformGuard struct{ cmd *exec.Cmd }

func startPlatform(cmd *exec.Cmd, _ Limits) (*platformGuard, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &platformGuard{cmd: cmd}, nil
}
func (g *platformGuard) kill(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
func (g *platformGuard) peakMemory() uint64 { return 0 }
func (g *platformGuard) close()             { _ = g.kill(g.cmd) }
