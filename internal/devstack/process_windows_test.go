//go:build windows

package devstack

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestCreationFlagValues pins the raw Windows constants so a typo in a hex
// literal cannot silently turn into a different (or ignored) flag.
func TestCreationFlagValues(t *testing.T) {
	if detachedProcess != 0x00000008 {
		t.Fatalf("detachedProcess = %#x, want 0x00000008 (DETACHED_PROCESS)", detachedProcess)
	}
	if createNoWindow != 0x08000000 {
		t.Fatalf("createNoWindow = %#x, want 0x08000000 (CREATE_NO_WINDOW)", createNoWindow)
	}
}

// TestServiceCreationFlagsUseOnlyCreateNoWindow encodes the measured outcome:
// both DETACHED_PROCESS variants lost the redirected stdout/stderr (log stayed
// at 0 B), and the OR variant additionally drew a console window in 4 of 5
// runs. CREATE_NO_WINDOW alone was the only variant alive at 10s, windowless,
// and writing its log. Re-adding the detached bit is a regression, not a tweak.
func TestServiceCreationFlagsUseOnlyCreateNoWindow(t *testing.T) {
	if serviceCreationFlags&detachedProcess != 0 {
		t.Errorf("serviceCreationFlags %#x sets DETACHED_PROCESS: measured to drop the log stream", serviceCreationFlags)
	}
	if serviceCreationFlags&createNoWindow == 0 {
		t.Errorf("serviceCreationFlags %#x misses CREATE_NO_WINDOW: a console window would appear", serviceCreationFlags)
	}
	if want := uint32(0x08000000); serviceCreationFlags != want {
		t.Errorf("serviceCreationFlags = %#x, want exactly %#x (CREATE_NO_WINDOW only)", serviceCreationFlags, want)
	}
}

// TestStartDetachedAppliesFlags checks startDetached wires the flags into the
// SysProcAttr that exec.Start hands to CreateProcess.
func TestStartDetachedAppliesFlags(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	startDetached(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil after startDetached")
	}
	if got := cmd.SysProcAttr.CreationFlags; got != serviceCreationFlags {
		t.Fatalf("CreationFlags = %#x, want %#x", got, serviceCreationFlags)
	}
	// HideWindow is a STARTUPINFO show-state flag; the console window is
	// suppressed by CREATE_NO_WINDOW, so setting both would only muddy which
	// mechanism is doing the work.
	if cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow unexpectedly set; CREATE_NO_WINDOW alone hides the window")
	}
}

// TestStartDetachedKeepsStdioRedirection guards the log guarantee: the runner
// attaches the log file to stdout/stderr and startDetached must not replace or
// drop those handles. This is the property the DETACHED_PROCESS variants broke
// in the flag experiment (0 B logs while the process was still alive).
func TestStartDetachedKeepsStdioRedirection(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	var out stubWriter
	cmd.Stdout = &out
	cmd.Stderr = &out
	startDetached(cmd)

	if cmd.Stdout == nil || cmd.Stderr == nil {
		t.Fatal("startDetached dropped the configured stdout/stderr")
	}
	if cmd.SysProcAttr.CreationFlags != serviceCreationFlags {
		t.Fatalf("CreationFlags = %#x, want %#x", cmd.SysProcAttr.CreationFlags, serviceCreationFlags)
	}
}

type stubWriter struct{ n int }

func (s *stubWriter) Write(p []byte) (int, error) { s.n += len(p); return len(p), nil }

// Compile-time assertion that the service launch still goes through the Windows
// SysProcAttr type rather than a shared abstraction with different semantics.
var _ = func() bool {
	var attr *syscall.SysProcAttr = serviceSysProcAttr()
	_ = attr
	return true
}()
