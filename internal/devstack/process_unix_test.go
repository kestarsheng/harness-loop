//go:build !windows

package devstack

import (
	"os/exec"
	"testing"
)

// TestStartDetachedUnixUnchanged locks the non-Windows contract: the child
// becomes a session leader (setsid) so it survives harness-cli exiting and can
// be signalled as a process group later. No Windows-style creation flags must
// leak into this path.
func TestStartDetachedUnixUnchanged(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	startDetached(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil after startDetached")
	}
	if !cmd.SysProcAttr.Setsid {
		t.Fatal("Setsid = false, want true: detached child would die or lose its process group")
	}
	if cmd.SysProcAttr.Noctty || cmd.SysProcAttr.Setctty {
		t.Error("ctty flags set on non-Windows: would detach the child from its controlling terminal")
	}
}
