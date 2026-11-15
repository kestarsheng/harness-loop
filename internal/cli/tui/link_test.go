package tui

import "testing"

func TestOSC8Link(t *testing.T) {
	url := "http://localhost:5174/harness-loop/monitor"
	want := "\x1b]8;;" + url + "\x1b\\" + url + "\x1b]8;;\x1b\\"
	if got := OSC8Link(url, url); got != want {
		t.Fatalf("OSC8Link = %q, want %q", got, want)
	}
	if got := OSC8Link(url, "monitor"); got != "\x1b]8;;"+url+"\x1b\\monitor\x1b]8;;\x1b\\" {
		t.Fatalf("custom label = %q", got)
	}
}

func TestRenderHyperlinkFallback(t *testing.T) {
	url := "http://localhost:5174/harness-loop/monitor"
	if got := RenderHyperlink(url, true); got != OSC8Link(url, url) {
		t.Fatalf("supported terminal must emit OSC 8: %q", got)
	}
	if got := RenderHyperlink(url, false); got != url {
		t.Fatalf("unsupported terminal must emit plain text: %q", got)
	}
	if got := RenderHyperlink("", true); got != "" {
		t.Fatalf("empty URL = %q", got)
	}
}

func TestHyperlinkSupported(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	cases := []struct {
		name string
		tty  bool
		env  map[string]string
		want bool
	}{
		{name: "not a tty", tty: false, env: map[string]string{"TERM": "xterm-256color"}, want: false},
		{name: "dumb terminal", tty: true, env: map[string]string{"TERM": "dumb"}, want: false},
		{name: "windows terminal", tty: true, env: map[string]string{"WT_SESSION": "abc"}, want: true},
		{name: "iterm", tty: true, env: map[string]string{"TERM": "xterm-256color", "TERM_PROGRAM": "iTerm.app"}, want: true},
		{name: "apple terminal", tty: true, env: map[string]string{"TERM": "xterm-256color", "TERM_PROGRAM": "Apple_Terminal"}, want: false},
		{name: "xterm", tty: true, env: map[string]string{"TERM": "xterm-256color"}, want: true},
		{name: "kitty", tty: true, env: map[string]string{"TERM": "xterm-kitty"}, want: true},
		{name: "unknown conhost", tty: true, env: map[string]string{}, want: false},
	}
	for _, tc := range cases {
		if got := HyperlinkSupported(tc.tty, env(tc.env)); got != tc.want {
			t.Fatalf("%s: HyperlinkSupported = %v, want %v", tc.name, got, tc.want)
		}
	}
	if HyperlinkSupported(true, nil) {
		t.Fatal("nil getenv must be treated as unsupported")
	}
}
