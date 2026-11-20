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
