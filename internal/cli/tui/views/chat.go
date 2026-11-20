package views

import (
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/glamour"
)

// ansiEscape matches the colour/cursor sequences glamour's dark style emits so
// NO_COLOR output can be reduced to plain text.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// ChatMessage is one turn of the direct-chat conversation rendered on the
// session's default screen. Role is "user" or "assistant"; Err marks a line
// that must stay visible (stream failure, error chunk) instead of being
// silently dropped.
type ChatMessage struct {
	Role      string
	Text      string
	Err       bool
	Streaming bool
	Time      time.Time
}

const (
	chatIndent   = "  "
	chatBodyPad  = "  "
	chatCursor   = "▍"
	chatMinWidth = 20
)

// RenderChat draws the conversation transcript with the newest turns kept when
// the terminal is too short. width is the terminal width and maxLines bounds
// the rendered area so the composer and footer stay on screen.
func RenderChat(messages []ChatMessage, width, maxLines int, theme Theme) string {
	return RenderChatWithCursor(messages, width, maxLines, theme, chatCursor)
}

// RenderChatWithCursor is RenderChat with an explicit streaming cursor, letting
// the model blink it without changing the view contract.
func RenderChatWithCursor(messages []ChatMessage, width, maxLines int, theme Theme, cursor string) string {
	if cursor == "" {
		cursor = chatCursor
	}
	if width < chatMinWidth {
		width = chatMinWidth
	}
	lines := []string{fit(theme.paint("Conversation", theme.Primary), width)}
	if len(messages) == 0 {
		hint := chatIndent + "send a message, or /task <goal> to run the a~e loop"
		for _, row := range wrapWidth(hint, width) {
			lines = append(lines, theme.paint(row, theme.Secondary))
		}
	}

	bodyWidth := width - len([]rune(chatIndent)) - len([]rune(chatBodyPad))
	if bodyWidth < chatMinWidth/2 {
		bodyWidth = chatMinWidth / 2
	}

	for _, msg := range messages {
		if msg.Role == "tool" {
			style := theme.Secondary
			if msg.Err {
				style = theme.Err
			}
			lines = append(lines, fit(theme.paint(chatIndent+strings.TrimRight(msg.Text, "\n"), style), width))
			continue
		}
		label := "you"
		style := theme.Primary
		switch {
		case msg.Err:
			label = "error"
			style = theme.Err
		case msg.Role == "assistant":
			label = "assistant"
			style = theme.Primary
		}
		header := chatIndent + label
		if !msg.Time.IsZero() {
			header += " " + msg.Time.Format("15:04:05")
		}
		lines = append(lines, fit(theme.paint(header, style), width))

		body := strings.TrimRight(msg.Text, "\n")
		if body == "" {
			if msg.Streaming {
				body = "…"
			}
		}
		if msg.Streaming && body != "" && !msg.Err {
			body += cursor
		}
		var rows []string
		if msg.Role == "assistant" && !msg.Streaming && !msg.Err && strings.TrimSpace(body) != "" {
			// Only the final reply is markdown: tokens stay verbatim while
			// streaming, then the whole reply is re-rendered once on final.
			rows = markdownRows(body, bodyWidth, theme.NoColor)
		} else {
			rows = wrapWidth(body, bodyWidth)
		}
		for _, row := range rows {
			text := chatIndent + chatBodyPad + row
			if msg.Err {
				text = theme.paint(text, theme.Err)
			}
			if strings.Contains(row, "\x1b[") {
				// Markdown rows already carry ANSI styling; truncating them
				// rune-wise would cut escape sequences.
				lines = append(lines, text)
				continue
			}
			lines = append(lines, fit(text, width))
		}
	}

	if maxLines > 0 && len(lines) > maxLines {
		// Keep the newest turns: the transcript scrolls up off the top.
		lines = lines[len(lines)-maxLines:]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// markdownRows renders a finished assistant reply with glamour. NO_COLOR uses
// the notty style so the output stays free of ANSI escapes.
func markdownRows(text string, width int, noColor bool) []string {
	wrap := width - 4
	if wrap < 10 {
		wrap = 10
	}
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(wrap),
	)
	if err != nil {
		return wrapWidth(text, width)
	}
	out, err := renderer.Render(text)
	if err != nil {
		return wrapWidth(text, width)
	}
	if noColor {
		out = ansiEscape.ReplaceAllString(out, "")
	}
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for len(rows) > 0 && strings.TrimSpace(rows[0]) == "" {
		rows = rows[1:]
	}
	for len(rows) > 0 && strings.TrimSpace(rows[len(rows)-1]) == "" {
		rows = rows[:len(rows)-1]
	}
	if len(rows) == 0 {
		return []string{""}
	}
	return rows
}

// wrapWidth breaks text on newlines and then hard-wraps each paragraph to
// width runes. Markdown markers are left untouched: the composer shows the
// reply verbatim.
func wrapWidth(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		runes := []rune(para)
		if len(runes) == 0 {
			out = append(out, "")
			continue
		}
		for len(runes) > width {
			out = append(out, string(runes[:width]))
			runes = runes[width:]
		}
		out = append(out, string(runes))
	}
	return out
}
