package views

import (
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
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
	chatUserBg   = "236"

	// Role labels use single-width geometric glyphs, never emoji: ◇ for the
	// user and ◆ for the assistant/error turn. Both markers are exactly one
	// cell wide, so the labels start in the same column.
	chatUserRole      = "◇ user"
	chatAssistantRole = "◆ assistant"
	chatErrorRole     = "◆ error"

	// Tool rows carry a single-width mark: ▸ pending, ✓ done, ✗ failed.
	chatToolPending = "▸"
	chatToolOK      = "✓"
	chatToolFail    = "✗"
	// ToolMarkPending is exported for the model, which labels a tool row when
	// the stream starts it; the renderer recognises the mark again below.
	ToolMarkPending = chatToolPending

	// chatUserPadX is the padding inside the user block on each side and
	// chatUserGapX the gap the block leaves on the right, so the background
	// never runs into the edge of the message area.
	chatUserPadX = 1
	chatUserGapX = 1

	// chatUserGapRows is how many blank rows sit between the "◇ user" header
	// and its background block. A terminal row cannot be split, so the
	// quarter-row gap would need a half-height cell the cell grid does not
	// have; the block hugs the header instead. Keeping it a constant makes the
	// zero explicit and testable rather than an accident.
	chatUserGapRows = 0

	// Scrollbar glyphs. Both are one cell wide and survive NO_COLOR: the
	// thumb is a full block, the track a light shade, so the bar reads as a
	// bar even with every colour stripped.
	chatScrollThumb = "█"
	chatScrollTrack = "░"
	// chatScrollCol is the single rightmost column reserved for the bar. The
	// text is wrapped one column narrower so the bar never sits on a rune.
	chatScrollCol = 1
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
	lines := chatRows(messages, width, theme, cursor)
	if maxLines > 0 && len(lines) > maxLines {
		// Keep the newest turns: the transcript scrolls up off the top.
		lines = lines[len(lines)-maxLines:]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// RenderChatViewport draws a window onto the transcript and reports nothing;
// RenderChatViewportSize is the variant the model calls, because it needs the
// total row count to know how far the transcript can scroll.
func RenderChatViewport(messages []ChatMessage, width, visible, offset int, theme Theme, cursor string) string {
	out, _ := RenderChatViewportSize(messages, width, visible, offset, theme, cursor)
	return out
}

// RenderChatViewportSize renders the [total-visible-offset, total-offset) window
// of the transcript and returns it with the full row count. offset is measured
// from the bottom, so 0 keeps the newest row on screen (auto-follow) and the
// clamp ceiling shows the first row. The message area gives up its rightmost
// column to the scrollbar only when the transcript actually overflows, so a
// short conversation keeps the full width and gains no trailing padding.
func RenderChatViewportSize(messages []ChatMessage, width, visible, offset int, theme Theme, cursor string) (string, int) {
	if visible < 1 {
		visible = 1
	}
	if width < chatMinWidth {
		width = chatMinWidth
	}
	textWidth := width - chatScrollCol
	if textWidth < chatMinWidth/2 {
		textWidth = chatMinWidth / 2
	}
	rows := chatRows(messages, textWidth, theme, cursor)
	total := len(rows)
	if total == 0 {
		return "", 0
	}
	offset = clampChatOffset(offset, total, visible)
	start := max(0, total-visible-offset)
	end := min(total, total-offset)
	if end < start {
		end = start
	}
	window := rows[start:end]
	if total <= visible {
		return strings.Join(window, "\n"), total
	}
	top, size := chatScrollBar(total, visible, offset)
	out := make([]string, 0, len(window))
	for i, row := range window {
		thumb := i >= top && i < top+size
		glyph := chatScrollTrack
		if thumb {
			glyph = chatScrollThumb
		}
		out = append(out, padCells(row, textWidth)+theme.scrollBar(glyph, thumb))
	}
	return strings.Join(out, "\n"), total
}

// chatRows renders every transcript row at width and drops the trailing blank
// rows, so the last row is always content.
func chatRows(messages []ChatMessage, width int, theme Theme, cursor string) []string {
	if cursor == "" {
		cursor = chatCursor
	}
	if width < chatMinWidth {
		width = chatMinWidth
	}
	lines := []string{}
	if len(messages) == 0 {
		hint := chatIndent + chatUserRole + " · " + chatAssistantRole +
			" · send a message, or /task <goal> to run the a~e loop"
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
			text := strings.TrimRight(msg.Text, "\n")
			if text == "" {
				continue
			}
			if !hasToolMark(text) {
				text = toolMark(msg) + " " + text
			}
			lines = append(lines, fit(theme.paint(chatIndent+text, style), width))
			continue
		}
		if msg.Role == "user" {
			lines = append(lines, chatHeader(theme, chatUserRole, msg.Time, theme.Primary, width))
			for i := 0; i < chatUserGapRows; i++ {
				lines = append(lines, "")
			}
			for _, row := range wrapWidth(strings.TrimRight(msg.Text, "\n"), userContentWidth(width)) {
				lines = append(lines, userRow(theme, row, width))
			}
			lines = append(lines, "")
			continue
		}
		label := chatAssistantRole
		style := theme.Primary
		if msg.Err {
			label = chatErrorRole
			style = theme.Err
		}
		lines = append(lines, chatHeader(theme, label, msg.Time, style, width))

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
		lines = append(lines, "")
	}

	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// clampChatOffset folds an offset into [0, total-visible]: 0 is the bottom,
// total-visible the top, and a transcript shorter than the window cannot
// scroll at all.
func clampChatOffset(offset, total, visible int) int {
	limit := chatMaxOffset(total, visible)
	if offset < 0 {
		return 0
	}
	if offset > limit {
		return limit
	}
	return offset
}

// chatMaxOffset is the largest scroll offset the transcript allows.
func chatMaxOffset(total, visible int) int {
	if total <= visible || visible < 1 {
		return 0
	}
	return total - visible
}

// chatScrollBar returns the first bar row and the thumb length for an offset
// measured from the bottom. The thumb keeps the classic proportion
// visible*visible/total (at least one cell) and slides by the fraction of the
// transcript that has scrolled past the top, so offset=0 parks it at the
// bottom and the clamp ceiling parks it at the top.
func chatScrollBar(total, visible, offset int) (top, size int) {
	maxStart := chatMaxOffset(total, visible)
	if maxStart == 0 {
		return 0, visible
	}
	size = visible * visible / total
	if size < 1 {
		size = 1
	}
	if size > visible {
		size = visible
	}
	if size >= visible {
		return 0, visible
	}
	fromTop := maxStart - clampChatOffset(offset, total, visible)
	top = (fromTop*(visible-size) + maxStart/2) / maxStart
	top = max(0, min(top, visible-size))
	return top, size
}

// padCells right-pads s with spaces to width cells. lipgloss measures cells, so
// an ANSI-styled row keeps its place under the scrollbar column.
func padCells(s string, width int) string {
	if pad := width - lipgloss.Width(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// scrollBar paints one scrollbar cell. NO_COLOR keeps the glyph and drops the
// colour; the thumb takes the logo accent so the position is readable.
func (t Theme) scrollBar(glyph string, thumb bool) string {
	style := t.Border
	if thumb {
		style = t.Primary
	}
	return t.paint(glyph, style)
}

// chatHeader paints "<label>" in style and the timestamp as dim small text. The
// stamp is dropped, not truncated, when the terminal is too narrow.
func chatHeader(theme Theme, label string, at time.Time, style lipgloss.Style, width int) string {
	head := chatIndent + label
	if at.IsZero() {
		return theme.paint(head, style)
	}
	stamp := at.Format("15:04")
	if lipgloss.Width(head)+1+lipgloss.Width(stamp) > width {
		return theme.paint(head, style)
	}
	return theme.paint(head, style) + " " + theme.paint(stamp, theme.Secondary)
}

// hasToolMark reports whether a tool row already carries its ▸/✓/✗ status.
func hasToolMark(text string) bool {
	for _, mark := range []string{chatToolPending, chatToolOK, chatToolFail} {
		if strings.HasPrefix(text, mark) {
			return true
		}
	}
	return false
}

// toolMark is the symbol a tool row gets when the stream did not supply one:
// pending while running, ✗ on failure, ✓ once finished.
func toolMark(msg ChatMessage) string {
	switch {
	case msg.Err:
		return chatToolFail
	case msg.Streaming:
		return chatToolPending
	default:
		return chatToolOK
	}
}

// chatIndentWidth is the display width of the message area's left margin. It is
// a variable only because lipgloss measures at runtime.
var chatIndentWidth = lipgloss.Width(chatIndent)

// chatUserStyle is the dark block behind the user's own message. It is a chat
// constant on purpose: Theme keeps only Primary/Secondary/Border/Err.
func chatUserStyle() lipgloss.Style {
	return lipgloss.NewStyle().Background(lipgloss.Color(chatUserBg))
}

// userContentWidth is the text width inside the user background block: the
// message area minus the left indent, the right gap and both paddings.
func userContentWidth(width int) int {
	w := width - chatIndentWidth - chatUserGapX - 2*chatUserPadX
	if w < chatMinWidth/2 {
		w = chatMinWidth / 2
	}
	return w
}

// userRow renders one line of user input as an inset block: one cell of padding
// on each side of the text and one cell of gap before the right edge, so the
// background never spans the whole row. NO_COLOR drops the background and keeps
// the same columns; the ◇ on the header line carries the role instead.
func userRow(theme Theme, row string, width int) string {
	contentWidth := userContentWidth(width)
	content := fit(row, contentWidth)
	if pad := contentWidth - lipgloss.Width(content); pad > 0 {
		content += strings.Repeat(" ", pad)
	}
	body := strings.Repeat(" ", chatUserPadX) + content + strings.Repeat(" ", chatUserPadX)
	if theme.NoColor {
		return chatIndent + body
	}
	return chatIndent + chatUserStyle().Render(body)
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
