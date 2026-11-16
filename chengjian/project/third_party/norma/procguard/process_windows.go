//go:build windows

package procguard

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type platformGuard struct {
	mu  sync.Mutex
	job windows.Handle
}

func startPlatform(cmd *exec.Cmd, limits Limits) (*platformGuard, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	g := &platformGuard{job: job}
	fail := func(err error) (*platformGuard, error) {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		g.close()
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS | windows.JOB_OBJECT_LIMIT_JOB_MEMORY
	info.BasicLimitInformation.ActiveProcessLimit = limits.Processes
	info.JobMemoryLimit = uintptr(limits.MemoryBytes)
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fail(err)
	}
	// JOBOBJECT_CPU_RATE_CONTROL_INFORMATION: ENABLE | HARD_CAP; rate is 1/100%.
	cpu := struct{ Flags, Rate uint32 }{1 | 4, limits.CPUPercent * 100}
	if _, err := windows.SetInformationJobObject(job, 15, uintptr(unsafe.Pointer(&cpu)), uint32(unsafe.Sizeof(cpu))); err != nil {
		return fail(err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fail(err)
	}
	err = windows.AssignProcessToJobObject(job, process)
	windows.CloseHandle(process)
	if err != nil {
		return fail(fmt.Errorf("assign job: %w", err))
	}
	// os/exec closes the primary-thread handle. Enumerate that suspended process's
	// threads so the job is attached BEFORE any user code or descendants can run.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fail(err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := false
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(cmd.Process.Pid) {
			continue
		}
		thread, e := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if e != nil {
			return fail(e)
		}
		_, e = windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		if e != nil {
			return fail(e)
		}
		resumed = true
	}
	if !resumed {
		return fail(fmt.Errorf("primary thread unavailable"))
	}
	return g, nil
}

func (g *platformGuard) kill(_ *exec.Cmd) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(g.job, 1)
}
func (g *platformGuard) peakMemory() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if g.job != 0 {
		_ = windows.QueryInformationJobObject(g.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	}
	return uint64(info.PeakJobMemoryUsed)
}
func (g *platformGuard) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != 0 {
		_ = windows.CloseHandle(g.job)
		g.job = 0
	}
}
