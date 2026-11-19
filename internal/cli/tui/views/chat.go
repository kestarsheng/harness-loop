package views

import (
	"strings"
	"time"
)

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
	lines := []string{fit(theme.paint("Conversation", theme.Title), width)}
	if len(messages) == 0 {
		hint := chatIndent + "send a message, or /task <goal> to run the a~e loop"
		for _, row := range wrapWidth(hint, width) {
			lines = append(lines, theme.paint(row, theme.Muted))
		}
	}

	bodyWidth := width - len([]rune(chatIndent)) - len([]rune(chatBodyPad))
	if bodyWidth < chatMinWidth/2 {
		bodyWidth = chatMinWidth / 2
	}

	for _, msg := range messages {
		if msg.Role == "tool" {
			style := theme.Muted
			if msg.Err {
				style = theme.Err
			}
			lines = append(lines, fit(theme.paint(chatIndent+strings.TrimRight(msg.Text, "\n"), style), width))
			continue
		}
		label := "you"
		style := theme.Accent
		switch {
		case msg.Err:
			label = "error"
			style = theme.Err
		case msg.Role == "assistant":
			label = "assistant"
			style = theme.OK
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
		for _, row := range wrapWidth(body, bodyWidth) {
			text := chatIndent + chatBodyPad + row
			if msg.Err {
				text = theme.paint(text, theme.Err)
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
