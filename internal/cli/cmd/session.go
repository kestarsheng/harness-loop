package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sessionIdentity identifies one CLI invocation on the global monitor board.
// A new identity is generated per invocation on purpose: two terminals running
// `harness loop` must show up as two distinct agents even when they share the
// same workspace and tenant.
type sessionIdentity struct {
	ID    string
	Label string
}

func newSessionIdentity() sessionIdentity {
	return sessionIdentity{
		ID:    newSessionID(),
		Label: terminalLabel(),
	}
}

// newSessionID prefers a random 64-bit suffix so concurrent terminals never
// collide. It falls back to a timestamp when the OS entropy source fails.
func newSessionID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "sess-" + time.Now().UTC().Format("20060102T150405.000000")
	}
	return "sess-" + hex.EncodeToString(buf)
}

// terminalLabel renders host + working directory so the board can show where a
// session comes from. Long paths are truncated to keep the card title readable.
func terminalLabel() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown"
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	label := host
	if cwd != "" {
		label = fmt.Sprintf("%s:%s", host, filepath.Clean(cwd))
	}
	const maxLabel = 80
	if len(label) > maxLabel {
		label = label[:maxLabel]
	}
	return label
}
