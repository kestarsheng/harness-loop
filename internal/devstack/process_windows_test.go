//go:build windows

package devstack

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestCreationFlagValues pins the raw Windows constants so a typo in the hex
// literal cannot silently turn into a different (or ignored) flag.
func TestCreationFlagValues(t *testing.T) {
	if detachedProcess != 0x00000008 {
		t.Fatalf("detachedProcess = %#x, want 0x00000008 (DETACHED_PROCESS)", detachedProcess)
	}
	if createNoWindow != 0x08000000 {
		t.Fatalf("createNoWindow = %#x, want 0x08000000 (CREATE_NO_WINDOW)", createNoWindow)
	}
}

// TestDetachedCreationFlagsCombination asserts the launched services get both
// bits at once: no visible console window, and survival after harness-cli
// exits. Losing either bit is a regression.
func TestDetachedCreationFlagsCombination(t *testing.T) {
	flags := detachedCreationFlags

	if flags&detachedProcess == 0 {
		t.Errorf("flags %#x missing DETACHED_PROCESS: service would die with harness-cli", flags)
	}
	if flags&createNoWindow == 0 {
		t.Errorf("flags %#x missing CREATE_NO_WINDOW: service would flash a console window", flags)
	}
	if want := uint32(0x08000008); flags != want {
		t.Errorf("detachedCreationFlags = %#x, want %#x (exact OR of both flags)", flags, want)
	}
}

// TestStartDetachedAppliesFlags checks startDetached wires the flags into the
// SysProcAttr that exec.Start will hand to CreateProcess.
func TestStartDetachedAppliesFlags(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	startDetached(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil after startDetached")
	}
	if got := cmd.SysProcAttr.CreationFlags; got != detachedCreationFlags {
		t.Fatalf("CreationFlags = %#x, want %#x", got, detachedCreationFlags)
	}
	// HideWindow must stay off: it only affects the STARTUPINFO show-state and
	// is not what suppresses the console window here.
	if cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow unexpectedly set; flags alone should hide the window")
	}
}

// TestStartDetachedKeepsStdioRedirection guards the log guarantee: detached
// processes still write through the file handles the runner attached, i.e.
// nothing in startDetached replaces them with a console.
func TestStartDetachedKeepsStdioRedirection(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	var out stubWriter
	cmd.Stdout = &out
	cmd.Stderr = &out
	startDetached(cmd)
	if cmd.Stdout == nil || cmd.Stderr == nil {
		t.Fatal("startDetached dropped the configured stdout/stderr")
	}
	if cmd.SysProcAttr.CreationFlags != detachedCreationFlags {
		t.Fatalf("CreationFlags = %#x, want %#x", cmd.SysProcAttr.CreationFlags, detachedCreationFlags)
	}
}

type stubWriter struct{ n int }

func (s *stubWriter) Write(p []byte) (int, error) { s.n += len(p); return len(p), nil }

// TestSysProcAttrIsWindowsAttr is a compile-time assertion that the detached
// launch still uses the Windows SysProcAttr type, not a shared abstraction that
// might have quietly changed semantics.
var _ = func() bool {
	var attr *syscall.SysProcAttr = detachedSysProcAttr()
	_ = attr
	return true
}()
