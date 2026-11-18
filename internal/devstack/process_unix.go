//go:build !windows

package devstack

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

func startDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func killProcessTree(pid int, grace time.Duration) error {
	if !pidAlive(pid) {
		return nil
	}
	// Setsid made the child a process-group leader; signal the whole group.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("devstack: process %d still alive after %s", pid, grace)
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
