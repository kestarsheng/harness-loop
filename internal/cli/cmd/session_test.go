package cmd

import (
	"strings"
	"testing"
)

func TestNewSessionIdentityIsUniquePerInvocation(t *testing.T) {
	a := newSessionIdentity()
	b := newSessionIdentity()
	if a.ID == "" || !strings.HasPrefix(a.ID, "sess-") {
		t.Fatalf("session id must be prefixed and non-empty: %q", a.ID)
	}
	if a.ID == b.ID {
		t.Fatalf("two invocations must not share a session id: %q", a.ID)
	}
	if a.Label != b.Label {
		t.Fatalf("label should be stable within one host/cwd: %q vs %q", a.Label, b.Label)
	}
}

func TestTerminalLabelIsBoundedAndReadable(t *testing.T) {
	label := terminalLabel()
	if label == "" {
		t.Fatal("terminal label must not be empty")
	}
	if len(label) > 80 {
		t.Fatalf("terminal label must be truncated to 80 bytes, got %d", len(label))
	}
	if strings.ContainsAny(label, "\r\n\t") {
		t.Fatalf("terminal label must not contain control characters: %q", label)
	}
}
