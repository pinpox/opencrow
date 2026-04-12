//go:build !linux

package main

import (
	"os/exec"
	"syscall"
)

// configurePiSysProcAttr: non-Linux builds get a new process group but
// no Pdeathsig (Linux-only). Good enough for dev on darwin; production
// runs in NixOS containers.
func configurePiSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessTree(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}
