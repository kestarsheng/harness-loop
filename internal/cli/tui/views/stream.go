package views

import (
	"fmt"
	"strings"
	"time"
)

type StreamLine struct {
	Time  time.Time
	Stage string
	Text  string
	Error bool
}

func RenderStream(lines []StreamLine, width, maxLines int, theme Theme) string {
	var b strings.Builder
	b.WriteString(theme.paint("Agent Output", theme.Title))
	b.WriteString("\n")
	if len(lines) == 0 {
		b.WriteString(theme.paint(" waiting for agent output…", theme.Muted))
		return b.String()
	}
	start := 0
	if maxLines > 0 && len(lines) > maxLines {
		start = len(lines) - maxLines
	}
	for _, line := range lines[start:] {
		stage := line.Stage
		if stage == "" {
			stage = "-"
		}
		text := fmt.Sprintf(" %s [%s] %s", line.Time.Format("15:04:05"), stage, line.Text)
		if line.Error {
			text = theme.paint(text, theme.Err)
		}
		b.WriteString(fit(text, width))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
