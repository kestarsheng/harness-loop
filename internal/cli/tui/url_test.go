package tui

import (
	"strings"
	"testing"
)

func TestURLFor(t *testing.T) {
	cases := []struct {
		name    string
		webBase string
		want    string
	}{
		{name: "empty uses default", webBase: "", want: "http://localhost:5174/harness-loop/monitor"},
		{name: "plain base", webBase: "http://localhost:5174", want: "http://localhost:5174/harness-loop/monitor"},
		{name: "trailing slash", webBase: "http://localhost:5174/", want: "http://localhost:5174/harness-loop/monitor"},
		{name: "spaces trimmed", webBase: " http://127.0.0.1:5174 ", want: "http://127.0.0.1:5174/harness-loop/monitor"},
		{name: "query stripped", webBase: "http://localhost:5174?project=x&tenant=y", want: "http://localhost:5174/harness-loop/monitor"},
		{name: "fragment stripped", webBase: "http://localhost:5174/#section", want: "http://localhost:5174/harness-loop/monitor"},
		{name: "idempotent", webBase: "http://localhost:5174/harness-loop/monitor", want: "http://localhost:5174/harness-loop/monitor"},
	}
	for _, tc := range cases {
		if got := URLFor(tc.webBase); got != tc.want {
			t.Fatalf("%s: URLFor(%q) = %q, want %q", tc.name, tc.webBase, got, tc.want)
		}
	}
}

func TestURLForIsIdenticalAcrossTerminals(t *testing.T) {
	// Two terminals on the same machine share the same default configuration,
	// but may have different terminal-related environment variables. URLFor is
	// a pure function, so both must resolve to the exact same string.
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("WT_SESSION", "first-terminal")
	first := URLFor(DefaultWebBaseURL)

	t.Setenv("TERM", "dumb")
	t.Setenv("WT_SESSION", "")
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	second := URLFor(DefaultWebBaseURL)

	if first != second {
		t.Fatalf("terminals disagree: %q vs %q", first, second)
	}
	if want := "http://localhost:5174/harness-loop/monitor"; first != want {
		t.Fatalf("got %q, want %q", first, want)
	}
	if strings.ContainsAny(first, "?#") {
		t.Fatalf("monitor URL must not carry query parameters or fragments: %q", first)
	}
}
