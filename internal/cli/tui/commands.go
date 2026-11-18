package tui

import "strings"

// SessionAction is what a line typed in a resident session means.
type SessionAction int

const (
	// SessionNoop means the line carried no command (for example an empty line).
	SessionNoop SessionAction = iota
	// SessionSubmit means the line is a goal and must create one task.
	SessionSubmit
	// SessionExit means the line asks to end the current terminal session.
	SessionExit
)

// SessionCommand is the single dispatch result consumed by both the TUI input
// box and the CLI execution layer.
type SessionCommand struct {
	Action SessionAction
	Name   string
	Goal   string
}

// DispatchSessionLine is the one place that decides what a typed line means.
// The resident TUI input box and the terminal fallback prompt both call it, so
// slash commands never grow a second parser.
func DispatchSessionLine(line string) SessionCommand {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return SessionCommand{Action: SessionNoop, Name: "empty"}
	}
	if isSessionExit(trimmed) {
		return SessionCommand{Action: SessionExit, Name: "exit"}
	}
	return SessionCommand{Action: SessionSubmit, Name: "goal", Goal: trimmed}
}

func isSessionExit(line string) bool {
	normalized := strings.ToLower(line)
	normalized = strings.ReplaceAll(normalized, "\\", "/")
	slashed := strings.HasPrefix(normalized, "/")
	normalized = strings.TrimPrefix(normalized, "/")
	normalized = strings.ReplaceAll(normalized, "harness-loop", "harness loop")
	normalized = strings.ReplaceAll(normalized, "harnessloop", "harness loop")
	normalized = strings.Join(strings.Fields(normalized), " ")
	switch normalized {
	case "exit", "harness loop exit", "loop exit", "harness exit":
		return true
	case "quit", "q":
		// Bare words stay valid goals ("quit smoking"); only the slash form is
		// treated as a command.
		return slashed
	}
	return false
}
