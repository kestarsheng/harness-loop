package tui

import "strings"

const (
	// DefaultWebBaseURL is the local web console base used by the fixed board.
	DefaultWebBaseURL = "http://localhost:5174"
	// MonitorPath is the globally shared monitoring board path.
	MonitorPath = "/harness-loop/monitor"
)

// URLFor returns the fixed, terminal-independent monitor URL for webBase.
// The result never carries query parameters or fragments: every terminal that
// uses the same web base resolves to the exact same string.
func URLFor(webBase string) string {
	base := strings.TrimSpace(webBase)
	if base == "" {
		base = DefaultWebBaseURL
	}
	if idx := strings.IndexAny(base, "?#"); idx >= 0 {
		base = base[:idx]
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, MonitorPath) {
		return base
	}
	return base + MonitorPath
}
