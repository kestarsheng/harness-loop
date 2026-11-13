package views

import (
	"fmt"
	"strings"
	"time"
)

type EventLine struct {
	Seq      int64
	Time     time.Time
	Type     string
	Stage    string
	Attempt  int
	Severity string
	Summary  string
	Error    bool
	Kind     string
}

func (e EventLine) render() string {
	switch e.Kind {
	case "evidence":
		return "     evidence  " + e.Summary
	case "failed_criteria":
		return "     failed    " + e.Summary
	}
	parts := []string{" " + e.Time.Format("15:04:05")}
	if e.Seq > 0 {
		parts = append(parts, fmt.Sprintf("#%d", e.Seq))
	}
	parts = append(parts, e.Type)
	if e.Stage != "" {
		stage := e.Stage
		if e.Attempt > 0 {
			stage = fmt.Sprintf("%s#%d", stage, e.Attempt)
		}
		parts = append(parts, stage)
	}
	line := strings.Join(parts, " ")
	if e.Summary != "" {
		line += "  " + e.Summary
	}
	return line
}

func RenderEvents(lines []EventLine, width, maxLines int, theme Theme) string {
	title := "Events"
	out := renderEventLines(lines, width, maxLines, theme, title)
	return out
}

func RenderLogs(lines []EventLine, width, maxLines int, theme Theme) string {
	title := fmt.Sprintf("Logs · %d events (Esc back, q quit)", len(lines))
	return renderEventLines(lines, width, maxLines, theme, title)
}

func renderEventLines(lines []EventLine, width, maxLines int, theme Theme, title string) string {
	var b strings.Builder
	b.WriteString(theme.paint(title, theme.Title))
	b.WriteString("\n")
	if len(lines) == 0 {
		b.WriteString(theme.paint(" waiting for events…", theme.Muted))
		return b.String()
	}
	start := 0
	if maxLines > 0 && len(lines) > maxLines {
		start = len(lines) - maxLines
	}
	for _, line := range lines[start:] {
		text := line.render()
		switch {
		case line.Kind == "evidence" || line.Kind == "failed_criteria":
			text = theme.paint(text, theme.Warn)
		case line.Error || strings.HasPrefix(line.Severity, "error"):
			text = theme.paint(text, theme.Err)
		}
		b.WriteString(fit(text, width))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
