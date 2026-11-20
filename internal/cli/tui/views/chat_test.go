package views

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func TestRenderChatShowsBothTurns(t *testing.T) {
	theme := NewTheme(true)
	out := RenderChat([]ChatMessage{
		{Role: "user", Text: "hi", Time: time.Unix(0, 0)},
		{Role: "assistant", Text: "你好，我在", Streaming: true},
	}, 80, 40, theme)

	for _, want := range []string{"◇ user", "◆ assistant", "hi", "你好，我在", "▍"} {
		if !strings.Contains(out, want) {
			t.Fatalf("chat view missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"⏺", "❯", "▌"} {
		if strings.Contains(out, bad) {
			t.Fatalf("chat view must not use %q any more:\n%s", bad, out)
		}
	}
}

func TestRenderChatErrorStaysVisible(t *testing.T) {
	out := RenderChat([]ChatMessage{
		{Role: "assistant", Text: "agent-service 127.0.0.1:50006: rpc error: code = Unavailable", Err: true},
	}, 80, 40, NewTheme(true))
	if !strings.Contains(out, "error") || !strings.Contains(out, "Unavailable") {
		t.Fatalf("error line not rendered:\n%s", out)
	}
}

func TestRenderChatKeepsNewestLines(t *testing.T) {
	var msgs []ChatMessage
	for i := 0; i < 50; i++ {
		msgs = append(msgs, ChatMessage{Role: "assistant", Text: "line-" + string(rune('a'+i%26))})
	}
	out := RenderChat(msgs, 80, 6, NewTheme(true))
	if lines := strings.Count(out, "\n") + 1; lines > 6 {
		t.Fatalf("rendered %d lines, want <= 6:\n%s", lines, out)
	}
	if !strings.Contains(out, "line-") {
		t.Fatalf("newest content lost:\n%s", out)
	}
}

func TestRenderChatEmptyStateMentionsTaskCommand(t *testing.T) {
	out := RenderChat(nil, 80, 10, NewTheme(true))
	if !strings.Contains(out, "/task") {
		t.Fatalf("empty state must point at /task:\n%s", out)
	}
}

func TestWrapWidthSplitsLongParagraphs(t *testing.T) {
	rows := wrapWidth("abcdefghij\nk", 4)
	if len(rows) != 4 || rows[0] != "abcd" || rows[2] != "ij" || rows[3] != "k" {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestRenderChatSeparatesTurnsWithBlankLine(t *testing.T) {
	out := RenderChat([]ChatMessage{
		{Role: "user", Text: "first"},
		{Role: "assistant", Text: "answer"},
		{Role: "user", Text: "second"},
	}, 80, 40, NewTheme(true))
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if strings.Contains(line, chatUserRole) && i > 0 {
			if strings.TrimSpace(lines[i-1]) != "" {
				t.Fatalf("turn %d starts without a blank separator: %#v", i, lines[i-1])
			}
		}
	}
	if strings.HasSuffix(out, "\n") {
		t.Fatalf("trailing blank row must be trimmed: %q", out)
	}
}

func TestRenderChatToolMarksOutcome(t *testing.T) {
	theme := NewTheme(true)
	pending := RenderChat([]ChatMessage{{Role: "tool", Text: "Read(main.go)", Streaming: true}}, 80, 10, theme)
	if !strings.Contains(pending, chatToolPending+" Read(main.go)") {
		t.Fatalf("pending tool row lost its %s marker:\n%s", chatToolPending, pending)
	}
	done := RenderChat([]ChatMessage{{Role: "tool", Text: "Read(main.go)"}}, 80, 10, theme)
	if !strings.Contains(done, chatToolOK+" Read(main.go)") {
		t.Fatalf("finished tool row lost its %s marker:\n%s", chatToolOK, done)
	}
	failed := RenderChat([]ChatMessage{{Role: "tool", Text: "Read(main.go)", Err: true}}, 80, 10, theme)
	if !strings.Contains(failed, chatToolFail+" Read(main.go)") {
		t.Fatalf("failed tool row lost its %s marker:\n%s", chatToolFail, failed)
	}
	// A row that already carries a mark must not be marked twice.
	twice := RenderChat([]ChatMessage{{Role: "tool", Text: chatToolOK + " Run(go test)"}}, 80, 10, theme)
	if strings.Contains(twice, chatToolOK+" "+chatToolOK) {
		t.Fatalf("tool row marked twice:\n%s", twice)
	}
}

func TestRenderChatNoColorKeepsSymbolsButDropsBackground(t *testing.T) {
	out := RenderChat([]ChatMessage{
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "ok"},
		{Role: "tool", Text: "Read(x)"},
	}, 80, 20, NewTheme(true))
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("NO_COLOR output must not carry ANSI escapes:\n%q", out)
	}
	for _, want := range []string{chatUserRole, chatAssistantRole, chatToolOK + " Read(x)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("NO_COLOR output lost %q:\n%s", want, out)
		}
	}
}

func TestChatUserBlockStyleIsDarkAndNotInTheme(t *testing.T) {
	style := chatUserStyle()
	bg, ok := style.GetBackground().(lipgloss.Color)
	if !ok {
		t.Fatalf("user block background = %T, want lipgloss.Color", style.GetBackground())
	}
	if string(bg) != chatUserBg {
		t.Fatalf("user block background = %q, want %q", string(bg), chatUserBg)
	}
	theme := NewTheme(false)
	for name, fg := range map[string]interface{}{
		"Primary":   theme.Primary.GetForeground(),
		"Secondary": theme.Secondary.GetForeground(),
		"Border":    theme.Border.GetForeground(),
		"Err":       theme.Err.GetForeground(),
	} {
		if c, ok := fg.(lipgloss.Color); ok && string(c) == chatUserBg {
			t.Fatalf("chat background leaked into Theme.%s", name)
		}
	}
}

func TestRenderChatTimestampIsDim(t *testing.T) {
	out := RenderChat([]ChatMessage{{Role: "user", Text: "hi", Time: time.Date(2026, 11, 20, 9, 5, 0, 0, time.UTC)}}, 80, 20, NewTheme(true))
	if !strings.Contains(out, "09:05") {
		t.Fatalf("timestamp missing://n%s", out)
	}
	if fg := NewTheme(false).Secondary.GetForeground(); fg == nil {
		t.Fatal("Secondary must carry the dim colour used by the timestamp")
	}
}

func TestChatRoleMarkersAreSingleWidthAndAligned(t *testing.T) {
	for _, mark := range []string{"◇", "◆", chatToolPending, chatToolOK, chatToolFail} {
		if w := lipgloss.Width(mark); w != 1 {
			t.Fatalf("glyph %q is %d cells wide, want 1", mark, w)
		}
	}
	// ◇ and ◆ occupy the same cell, so both labels start in the same column.
	if lipgloss.Width("◇") != lipgloss.Width("◆") {
		t.Fatal("user and assistant markers must be the same width")
	}

	stamp := time.Date(2026, 11, 20, 9, 5, 0, 0, time.UTC)
	out := RenderChat([]ChatMessage{
		{Role: "user", Text: "hi", Time: stamp},
		{Role: "assistant", Text: "ok", Time: stamp},
	}, 80, 20, NewTheme(true))

	userCol, asstCol := -1, -1
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, chatUserRole); i >= 0 {
			userCol = lipgloss.Width(line[:i]) + lipgloss.Width("◇")
		}
		if i := strings.Index(line, "◆ assistant"); i >= 0 {
			asstCol = lipgloss.Width(line[:i]) + lipgloss.Width("◆")
		}
	}
	if userCol < 0 || asstCol < 0 {
		t.Fatalf("role headers missing:/n%s", out)
	}
	if userCol != asstCol {
		t.Fatalf("label columns differ: user=%d assistant=%d\n%s", userCol, asstCol, out)
	}
	if !strings.Contains(out, "09:05") {
		t.Fatalf("timestamp must stay on the header row:/n%s", out)
	}
}

func TestThemePrimaryIsLogoColour(t *testing.T) {
	theme := NewTheme(false)
	fg, ok := theme.Primary.GetForeground().(lipgloss.Color)
	if !ok {
		t.Fatalf("Primary foreground = %T, want lipgloss.Color", theme.Primary.GetForeground())
	}
	if string(fg) != LogoColor {
		t.Fatalf("Primary = %q, want LogoColor %q", string(fg), LogoColor)
	}
	if !theme.Primary.GetBold() {
		t.Fatal("Primary must stay bold")
	}
}

// TestUserBlockLeavesGapsAndPadding pins the block geometry: the background
// starts after the left indent, carries one cell of padding on each side and
// stops one cell before the right edge, so it never spans the whole row.
func TestUserBlockLeavesGapsAndPadding(t *testing.T) {
	const width = 40
	theme := NewTheme(true)
	row := userRow(theme, "hi", width)

	if !strings.HasPrefix(row, strings.Repeat(" ", chatIndentWidth)) {
		t.Fatalf("user block lost its left margin: %q", row)
	}
	blockWidth := lipgloss.Width(row) - chatIndentWidth
	wantBlock := userContentWidth(width) + 2*chatUserPadX
	if blockWidth != wantBlock {
		t.Fatalf("block width = %d, want %d (content + padding)", blockWidth, wantBlock)
	}
	if blockWidth >= width-chatIndentWidth {
		t.Fatalf("block width %d leaves no right gap at terminal width %d", blockWidth, width)
	}
	if got := lipgloss.Width(row); got > width-chatUserGapX {
		t.Fatalf("user row is %d cells, want <= %d (right gap %d)", got, width-chatUserGapX, chatUserGapX)
	}
	if chatUserPadX < 1 || chatUserGapX < 1 {
		t.Fatal("padding and gap must both be at least one cell")
	}
	// The text itself must not touch the block edges.
	body := row[chatIndentWidth:]
	if !strings.HasPrefix(body, " ") || !strings.HasSuffix(body, " ") {
		t.Fatalf("text is flush against the block edge: %q", body)
	}
}

// TestUserBlockHugsTheHeader pins the T-P2-51 decision: the "◇ user" label and
// its background block are adjacent, with no blank row in between (a terminal
// row cannot be split, so the half-row gap is not representable).
func TestUserBlockHugsTheHeader(t *testing.T) {
	if chatUserGapRows != 0 {
		t.Fatalf("chatUserGapRows = %d, want 0 (block hugs the header)", chatUserGapRows)
	}
	out := RenderChat([]ChatMessage{{Role: "user", Text: "hi"}}, 80, 20, NewTheme(true))
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("one turn must be header + one block row, got %d rows:/n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], chatUserRole) {
		t.Fatalf("first row is not the header: %q", lines[0])
	}
	if strings.TrimSpace(lines[1]) == "" {
		t.Fatalf("blank row between header and block:/n%s", out)
	}
	if !strings.Contains(lines[1], "hi") {
		t.Fatalf("second row is not the block: %q", lines[1])
	}
}

// TestUserBlockIsRectangularWithNoRoundedCorners keeps the block square: one
// row per wrapped line, no border glyphs, and exactly one blank row between
// turns (unchanged by the gap decision).
func TestUserBlockIsRectangularWithNoRoundedCorners(t *testing.T) {
	out := RenderChat([]ChatMessage{
		{Role: "user", Text: "one"},
		{Role: "user", Text: "two"},
	}, 80, 20, NewTheme(false))
	lines := strings.Split(out, "\n")
	if len(lines) != 5 {
		t.Fatalf("two turns must be 2*(header+block)+1 separator = 5 rows, got %d:/n%s", len(lines), out)
	}
	for _, row := range lines {
		if strings.ContainsAny(row, "╭╮╰╯│─") {
			t.Fatalf("user block must stay rectangular, found a border glyph: %q", row)
		}
	}
	if strings.TrimSpace(lines[2]) != "" {
		t.Fatalf("turns must still be separated by exactly one blank row: %q", lines[2])
	}
	if !strings.Contains(lines[3], chatUserRole) {
		t.Fatalf("third row should start the second turn: %q", lines[3])
	}
}

// scrollMessages builds n tool rows, one rendered row each, so the geometry
// tests can state the row count exactly.
func scrollMessages(n int) []ChatMessage {
	out := make([]ChatMessage, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ChatMessage{Role: "tool", Text: "row-" + pad2(i)})
	}
	return out
}

func pad2(i int) string {
	if i < 10 {
		return "00" + string(rune('0'+i))
	}
	if i < 100 {
		return "0" + string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	return string(rune('0'+i/100)) + string(rune('0'+i/10%10)) + string(rune('0'+i%10))
}

// TestRenderChatViewportWindowsFromTheBottom pins the window arithmetic:
// offset 0 is the newest rows, the clamp ceiling is the oldest, and the window
// is exactly visible rows tall.
func TestRenderChatViewportWindowsFromTheBottom(t *testing.T) {
	theme := NewTheme(true)
	msgs := scrollMessages(60)
	bottom, total := RenderChatViewportSize(msgs, 80, 10, 0, theme, "")
	if total != 60 {
		t.Fatalf("total = %d, want 60", total)
	}
	lines := strings.Split(bottom, "\n")
	if len(lines) != 10 {
		t.Fatalf("window = %d rows, want 10:\n%s", len(lines), bottom)
	}
	if !strings.Contains(bottom, "row-059") {
		t.Fatalf("offset 0 must show the newest row:\n%s", bottom)
	}
	if strings.Contains(bottom, "row-000") {
		t.Fatalf("offset 0 must not show the oldest row:\n%s", bottom)
	}

	top, total := RenderChatViewportSize(msgs, 80, 10, total-10, theme, "")
	if !strings.Contains(top, "row-000") || strings.Contains(top, "row-059") {
		t.Fatalf("max offset must show the oldest rows:\n%s", top)
	}

	// An out-of-range offset clamps instead of panicking or shifting.
	over, _ := RenderChatViewportSize(msgs, 80, 10, total+50, theme, "")
	if over != top {
		t.Fatalf("offset above the ceiling must clamp to the top:\n%s\n--- got ---\n%s", top, over)
	}
	under, _ := RenderChatViewportSize(msgs, 80, 10, -5, theme, "")
	if under != bottom {
		t.Fatalf("negative offset must clamp to the bottom:\n%s\n--- got ---\n%s", bottom, under)
	}
}

// TestRenderChatViewportScrollBarGeometry checks the bar at the widths the TUI
// actually runs at: the bar owns the last column of every row, the thumb keeps
// its proportion and slides from the bottom (offset 0) to the top.
func TestRenderChatViewportScrollBarGeometry(t *testing.T) {
	theme := NewTheme(true)
	msgs := scrollMessages(200)
	for _, width := range []int{40, 80, 120} {
		out, total := RenderChatViewportSize(msgs, width, 20, 0, theme, "")
		lines := strings.Split(out, "\n")
		if len(lines) != 20 {
			t.Fatalf("%d cols: window = %d rows, want 20", width, len(lines))
		}
		for i, line := range lines {
			if got := lipgloss.Width(line); got != width {
				t.Fatalf("%d cols row %d is %d cells, want %d: %q", width, i, got, width, line)
			}
			last := []rune(line)[len([]rune(line))-1]
			if string(last) != chatScrollThumb && string(last) != chatScrollTrack {
				t.Fatalf("%d cols row %d ends with %q, want the scrollbar", width, i, string(last))
			}
		}
		// 20*20/200 = 2 cells of thumb.
		if n := strings.Count(out, chatScrollThumb); n != 2 {
			t.Fatalf("%d cols: thumb = %d cells, want 2", width, n)
		}
		if !strings.HasSuffix(strings.Split(out, "\n")[19], chatScrollThumb) {
			t.Fatalf("%d cols: at offset 0 the thumb must sit at the bottom", width)
		}

		topOut, _ := RenderChatViewportSize(msgs, width, 20, total-20, theme, "")
		if !strings.HasSuffix(strings.Split(topOut, "\n")[0], chatScrollThumb) {
			t.Fatalf("%d cols: at the top the thumb must sit on row 0", width)
		}
		if strings.HasSuffix(strings.Split(topOut, "\n")[19], chatScrollThumb) {
			t.Fatalf("%d cols: the thumb must not stay at the bottom when scrolled to the top", width)
		}
	}
}

// TestRenderChatViewportScrollBarIsAtLeastOneCell keeps a very long transcript
// from shrinking the thumb out of existence.
func TestRenderChatViewportScrollBarIsAtLeastOneCell(t *testing.T) {
	out, total := RenderChatViewportSize(scrollMessages(5000), 80, 10, 0, NewTheme(true), "")
	if total != 5000 {
		t.Fatalf("total = %d", total)
	}
	if n := strings.Count(out, chatScrollThumb); n != 1 {
		t.Fatalf("thumb = %d cells, want the 1-cell floor", n)
	}
	top, size := chatScrollBar(5000, 10, 0)
	if size != 1 || top != 9 {
		t.Fatalf("chatScrollBar = (%d,%d), want (9,1)", top, size)
	}
	top, size = chatScrollBar(5000, 10, 4990)
	if size != 1 || top != 0 {
		t.Fatalf("chatScrollBar at the top = (%d,%d), want (0,1)", top, size)
	}
}

// TestRenderChatViewportWithoutOverflowKeepsFullWidth: a conversation that fits
// gains no bar column and no trailing padding.
func TestRenderChatViewportWithoutOverflowKeepsFullWidth(t *testing.T) {
	out, total := RenderChatViewportSize(scrollMessages(3), 80, 10, 0, NewTheme(true), "")
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if strings.ContainsAny(out, chatScrollThumb+chatScrollTrack) {
		t.Fatalf("no scrollbar may be drawn without overflow:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, " ") {
			t.Fatalf("short transcripts must not be padded: %q", line)
		}
	}
}

// TestRenderChatViewportNoColorKeepsBarChars: NO_COLOR strips the colour but the
// thumb and track must stay readable glyphs.
func TestRenderChatViewportNoColorKeepsBarChars(t *testing.T) {
	out, _ := RenderChatViewportSize(scrollMessages(200), 60, 10, 0, NewTheme(true), "")
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("NO_COLOR output must not carry ANSI escapes:\n%q", out)
	}
	if !strings.Contains(out, chatScrollThumb) || !strings.Contains(out, chatScrollTrack) {
		t.Fatalf("NO_COLOR must keep the scrollbar glyphs:\n%s", out)
	}
}

// TestRenderChatViewportKeepsTurnsIntact guards the shared row builder: the
// viewport window holds the same rows, in the same order, as the legacy
// tail-truncation path. Only the right-hand cell differs, because the viewport
// reserves one column for the scrollbar.
func TestRenderChatViewportKeepsTurnsIntact(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Text: "hello"},
		{Role: "assistant", Text: "hi there"},
	}
	legacy := strings.Split(RenderChat(msgs, 80, 40, NewTheme(true)), "\n")
	portable, total := RenderChatViewportSize(msgs, 80, 40, 0, NewTheme(true), "")
	if total != len(legacy) {
		t.Fatalf("row counts differ: legacy %d, viewport %d", len(legacy), total)
	}
	rows := strings.Split(portable, "\n")
	for i, want := range legacy {
		if got := strings.TrimRight(rows[i], " "); got != strings.TrimRight(want, " ") {
			t.Fatalf("row %d differs:\n viewport: %q\n legacy:   %q", i, got, want)
		}
	}
}
