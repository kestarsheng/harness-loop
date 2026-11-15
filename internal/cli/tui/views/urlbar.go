package views

import "strings"

// RenderURLBar renders the always-visible fixed monitor URL line. target may
// already be an OSC 8 hyperlink; when it is empty the bar is omitted.
func RenderURLBar(target string, theme Theme) string {
	if strings.TrimSpace(target) == "" {
		return ""
	}
	return " " + theme.paint("Monitor", theme.Accent) + " " + target + theme.paint("   o open   shared board", theme.Muted)
}
