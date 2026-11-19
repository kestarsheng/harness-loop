//go:build windows

package devstack

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Windows CreateProcess creation flags used when launching a dev-stack service.
const (
	// detachedProcess is DETACHED_PROCESS: the child is created without a
	// console attached to the caller, so it keeps running after harness-cli
	// exits instead of dying with the parent's console.
	detachedProcess = 0x00000008

	// createNoWindow is CREATE_NO_WINDOW: the console the child gets is
	// created already hidden. Without it, cmd.exe runs detached but every
	// console program it spawns allocates a fresh visible console, which
	// pops a black window on screen while the stack starts.
	createNoWindow = 0x08000000
)

// detachedCreationFlags is the exact flag set applied to detached service
// launches: no visible window, and survival across the parent's exit.
const detachedCreationFlags uint32 = detachedProcess | createNoWindow

// detachedSysProcAttr returns the attributes applied to detached service
// launches.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachedCreationFlags}
}

// startDetached applies detachedSysProcAttr to cmd.
func startDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = detachedSysProcAttr()
}

func killProcessTree(pid int, grace time.Duration) error {
	if !pidAlive(pid) {
		return nil
	}
	// Windows console programs do not receive SIGTERM; taskkill /T /F takes the
	// whole tree, which is what dev-lib.sh does for the same reason.
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("devstack: process %d still alive after %s", pid, grace)
}

func pidAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "\""+strconv.Itoa(pid)+"\"")
}
