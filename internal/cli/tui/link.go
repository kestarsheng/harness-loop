package tui

import "strings"

const (
	osc8Prefix = "\x1b]8;;"
	osc8Suffix = "\x1b\\"
)

// OSC8Link wraps text in an OSC 8 hyperlink that points at url.
func OSC8Link(url, text string) string {
	if text == "" {
		text = url
	}
	return osc8Prefix + url + osc8Suffix + text + osc8Prefix + osc8Suffix
}

// RenderHyperlink emits an OSC 8 hyperlink when the terminal supports it and
// falls back to plain text otherwise.
func RenderHyperlink(url string, supported bool) string {
	if url == "" {
		return ""
	}
	if supported {
		return OSC8Link(url, url)
	}
	return url
}

// HyperlinkSupported reports whether the current terminal is expected to
// render OSC 8 hyperlinks. getenv is injected for testability.
func HyperlinkSupported(isTTY bool, getenv func(string) string) bool {
	if !isTTY || getenv == nil {
		return false
	}
	term := strings.ToLower(strings.TrimSpace(getenv("TERM")))
	if term == "dumb" {
		return false
	}
	if strings.TrimSpace(getenv("WT_SESSION")) != "" {
		return true
	}
	program := strings.ToLower(strings.TrimSpace(getenv("TERM_PROGRAM")))
	switch program {
	case "apple_terminal":
		return false
	case "iterm.app", "wezterm", "vscode", "vscode-insiders", "hyper":
		return true
	}
	for _, marker := range []string{"xterm", "screen", "tmux", "kitty", "alacritty", "wezterm", "foot", "rio"} {
		if strings.Contains(term, marker) {
			return true
		}
	}
	return false

}
