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

// DETACHED_PROCESS keeps the service running after harness-cli exits.
const detachedProcess = 0x00000008

func startDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}
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
