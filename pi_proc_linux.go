//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// configurePiSysProcAttr puts pi in its own process group so Kill can
// signal the whole tree, and sets Pdeathsig so pi (and via the pgid
// kill, its children) cannot outlive opencrow even if Kill never runs
// — e.g. main returns before the worker goroutine reaches stopPi, or
// opencrow is SIGKILLed. Without this, leaked tool subprocesses keep
// the systemd cgroup non-empty and container restart blocks on
// TimeoutStopSec.
func configurePiSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGTERM,
	}
}

// killProcessTree signals the entire process group. pid == pgid because
// of Setpgid above; negative pid addresses the group. Errors are
// discarded: ESRCH after the group is gone is the expected end state,
// and there is no recovery for EPERM here.
func killProcessTree(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}
