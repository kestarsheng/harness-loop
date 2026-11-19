package views

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestRenderLogoKeepsWordmarkInBothModes(t *testing.T) {
	for _, noColor := range []bool{false, true} {
		out := RenderLogo(noColor)
		if !strings.Contains(out, LogoText) {
			t.Fatalf("noColor=%v: wordmark missing from %q", noColor, out)
		}
		if noColor && (strings.Contains(out, "\x1b[38;") || strings.Contains(out, "\x1b[48;")) {
			t.Fatalf("NO_COLOR output must not carry colour escapes: %q", out)
		}
	}
}

func TestLogoStyleIsBoldLightBlue(t *testing.T) {
	style := LogoStyle(false)
	if !style.GetBold() {
		t.Fatal("logo must be bold")
	}
	color, ok := style.GetForeground().(lipgloss.Color)
	if !ok {
		t.Fatalf("logo foreground = %T, want lipgloss.Color", style.GetForeground())
	}
	if string(color) != "#8FD3FF" {
		t.Fatalf("logo colour = %q, want #8FD3FF", string(color))
	}
}

func TestLogoStyleNoColorDropsForegroundKeepsWeight(t *testing.T) {
	style := LogoStyle(true)
	if !style.GetBold() {
		t.Fatal("NO_COLOR must keep the bold wordmark")
	}
	if _, ok := style.GetForeground().(lipgloss.NoColor); !ok {
		t.Fatalf("NO_COLOR foreground = %T, want lipgloss.NoColor", style.GetForeground())
	}
}
