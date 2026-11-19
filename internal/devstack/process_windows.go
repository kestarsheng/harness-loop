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

// Windows CreateProcess creation flags for dev-stack service launches.
//
// The choice below is not a matter of taste: it was measured on Windows 10/11
// (go1.22, cmd /c ping -n 60 127.0.0.1, 5 runs per variant, parent launcher
// exited before every measurement). Results:
//
//	DETACHED_PROCESS (0x8)                    -> log 0 B, cmd.exe sometimes
//	                                             exits immediately; a console
//	                                             window appeared in 1 of 5 runs
//	DETACHED_PROCESS | CREATE_NO_WINDOW       -> log 0 B in 5 of 5 runs and a
//	                                             visible console window in 4 of
//	                                             5; the process survives but
//	                                             its stdout/stderr never reach
//	                                             the log file
//	CREATE_NO_WINDOW (0x08000000)             -> alive at 2s and 10s in 5 of 5,
//	                                             no console window in 5 of 5,
//	                                             log file written normally
//
// So DETACHED_PROCESS is *not* used here. Note this contradicts the MSDN remark
// that CREATE_NO_WINDOW is ignored when combined with DETACHED_PROCESS: the
// combination measurably did allocate a console (conhost.exe appeared in the
// tree) and did keep the child alive, but it also swallowed the redirected
// stdio, so the combination is not a usable fix either way.
const (
	// detachedProcess is DETACHED_PROCESS. Deliberately unused for services:
	// it gives the child no console at all, which both loses the redirected
	// log stream and lets cmd.exe die on start. Kept named so the flag matrix
	// above stays readable and a future change has to justify itself.
	detachedProcess = 0x00000008

	// createNoWindow is CREATE_NO_WINDOW: the child gets a console that is
	// created without a window. The child therefore keeps its own console
	// (independent of harness-cli's) and survives harness-cli exiting, while
	// nothing is ever drawn on screen.
	createNoWindow = 0x08000000
)

// serviceCreationFlags is the exact flag set applied to dev-stack service
// launches: a windowless console, and survival across the parent's exit.
const serviceCreationFlags uint32 = createNoWindow

// serviceSysProcAttr returns the attributes applied to service launches.
func serviceSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: serviceCreationFlags}
}

// startDetached applies serviceSysProcAttr to cmd.
func startDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = serviceSysProcAttr()
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
