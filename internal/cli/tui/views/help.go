package views

import "strings"

func RenderHelp(theme Theme) string {
	lines := []string{
		theme.paint("Keys", theme.Title),
		" q    quit the TUI",
		" a    abort the running task (idempotent)",
		" l    toggle the full event log",
		" m    memory view",
		" ?    this help",
		" esc  return to the main view",
		"",
		" The TUI subscribes to the gRPC WatchEvents stream and refreshes task state",
		" through a read-only GetTask poll, so status queries never block execution.",
		"",
		" Acceptance failures show at most 3 evidence entries and the failed_criteria",
		" list. When the stream drops the header shows reconnecting… and events are",
		" replayed from last_seq before the stream resumes.",
	}
	return strings.Join(lines, "\n")
}

func RenderMemory(theme Theme) string {
	lines := []string{
		theme.paint("Memory", theme.Title),
		"",
		" The memory view is not wired in this build: the CLI memory commands belong",
		" to T-P1-02 and are outside T-P1-27. Stage memory injection and degradation",
		" events are still surfaced in the Events panel (memory.index_degraded, etc.).",
	}
	return strings.Join(lines, "\n")
}

func RenderFooter(mode string, width int, theme Theme) string {
	var text string
	switch mode {
	case "logs":
		text = " Esc back   q quit   ? help"
	case "help", "memory":
		text = " Esc back   q quit"
	default:
		text = " q quit   a abort   l logs   m memory   ? help"
	}
	return theme.paint(fit(text, width), theme.Muted)
}
