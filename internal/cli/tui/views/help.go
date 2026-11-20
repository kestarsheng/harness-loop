package views

import "strings"

func RenderHelp(theme Theme) string {
	lines := []string{
		theme.paint("Keys", theme.Primary),
		" q    quit the TUI",
		" a    abort the running task (idempotent)",
		" l    toggle the full event log",
		" m    memory view",
		" s    subtask tree / DAG progress + handoff chain",
		" x    model selection, fallback and recovery",
		" h    health plus the incident drawer (alias: !)",
		" c    artifacts and checkpoints",
		" k    knowledge sidebar (type a query, Enter to search)",
		" o    open the shared monitor board in the system browser",
		" ?    this help",
		" esc  return to the main view",
		"",
		" The TUI subscribes to the gRPC WatchEvents stream and refreshes task state",
		" through a read-only GetTask poll, so status queries never block execution.",
		"",
		" Subtasks, handoffs, model selections and checkpoints reuse the existing",
		" ListSubtasks/ListHandoffs/ListModelSelections/ListCheckpoints RPCs.",
		" The incident drawer highlights security.violation, tool.denied and",
		" approval.*; all 73 harness event types render a readable summary.",
		" Acceptance failures show at most 3 evidence entries plus failed_criteria.",
	}
	return strings.Join(lines, "\n")
}

func RenderMemory(theme Theme) string {
	lines := []string{
		theme.paint("Memory", theme.Primary),
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
	case "logs", "memory", "models", "artifacts":
		text = " Esc back   q quit   ? help"
	case "help":
		text = " Esc back   q quit"
	case "subtasks":
		text = " Esc back   q quit   c artifacts   ? help"
	case "health":
		text = " Esc back   q quit   ? help"
	case "knowledge":
		text = " type query   Enter search   Esc back   ctrl+c quit"
	default:
		text = " q quit   a abort   l logs   s subtasks   x models   h health   c artifacts   k knowledge   ? help"
	}
	return theme.paint(fit(text, width), theme.Secondary)
}
