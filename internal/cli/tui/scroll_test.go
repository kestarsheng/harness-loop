package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// chatScrollModel builds a resident session whose transcript overflows the
// body, renders one frame so chatTotal is filled, and hands it back at the
// bottom with the composer focused.
func chatScrollModel(t *testing.T, rows int) *Model {
	t.Helper()
	m := sessionModel(t, SessionConfig{})
	m.chatMode = true
	for i := 0; i < rows; i++ {
		m.chat = append(m.chat, ChatLine{Role: "tool", Text: fmt.Sprintf("row-%03d", i)})
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = next.(*Model)
	_ = m.View()
	return m
}

func wheelUp(t *testing.T, m *Model) *Model {
	t.Helper()
	next, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress, X: 10, Y: 5})
	return next.(*Model)
}

func wheelDown(t *testing.T, m *Model) *Model {
	t.Helper()
	next, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress, X: 10, Y: 5})
	return next.(*Model)
}

func pressKey(t *testing.T, m *Model, typ tea.KeyType) *Model {
	t.Helper()
	next, _ := m.Update(tea.KeyMsg{Type: typ})
	return next.(*Model)
}

// statusLine returns the footer line, the one carrying the help hint.
func statusLine(view string) string {
	lines := strings.Split(view, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "? help") {
			return lines[i]
		}
	}
	return ""
}

func TestWheelScrollsTheTranscript(t *testing.T) {
	m := chatScrollModel(t, 60)
	if m.chatTotal <= m.chatViewportHeight() {
		t.Fatalf("fixture must overflow: total=%d visible=%d", m.chatTotal, m.chatViewportHeight())
	}
	if m.chatScroll != 0 {
		t.Fatalf("a fresh transcript must start at the bottom, offset=%d", m.chatScroll)
	}

	m = wheelUp(t, m)
	if m.chatScroll != chatWheelRows {
		t.Fatalf("one wheel notch up = offset %d, want %d", m.chatScroll, chatWheelRows)
	}
	m = wheelUp(t, m)
	if m.chatScroll != 2*chatWheelRows {
		t.Fatalf("two notches = offset %d, want %d", m.chatScroll, 2*chatWheelRows)
	}
	m = wheelDown(t, m)
	if m.chatScroll != chatWheelRows {
		t.Fatalf("wheel down = offset %d, want %d", m.chatScroll, chatWheelRows)
	}
	// Scrolling past the bottom clamps instead of going negative.
	m = wheelDown(t, m)
	m = wheelDown(t, m)
	if m.chatScroll != 0 {
		t.Fatalf("wheel past the bottom = offset %d, want 0", m.chatScroll)
	}
}

func TestPageKeysHomeAndEnd(t *testing.T) {
	m := chatScrollModel(t, 80)
	page := m.chatPageRows()
	if page < 1 {
		t.Fatalf("page step = %d, want >= 1", page)
	}
	if page != m.chatViewportHeight()-2 {
		t.Fatalf("page step = %d, want visible-2 = %d", page, m.chatViewportHeight()-2)
	}

	m = pressKey(t, m, tea.KeyPgUp)
	if m.chatScroll != page {
		t.Fatalf("PgUp = offset %d, want %d", m.chatScroll, page)
	}
	m = pressKey(t, m, tea.KeyPgDown)
	if m.chatScroll != 0 {
		t.Fatalf("PgDn = offset %d, want 0", m.chatScroll)
	}

	m = pressKey(t, m, tea.KeyHome)
	if want := m.chatMaxScroll(); m.chatScroll != want || want == 0 {
		t.Fatalf("Home = offset %d, want the top %d", m.chatScroll, want)
	}
	m = pressKey(t, m, tea.KeyEnd)
	if m.chatScroll != 0 {
		t.Fatalf("End = offset %d, want 0", m.chatScroll)
	}
}

// TestHomeAndEndStayCaretKeysWhileTyping: the composer owns Home/End as soon as
// there is text, so editing a line is never hijacked by the scrollback.
func TestHomeAndEndStayCaretKeysWhileTyping(t *testing.T) {
	m := chatScrollModel(t, 60)
	m = wheelUp(t, m)
	scroll := m.chatScroll

	m = typeLine(t, m, "abc")
	if m.input.cursor != 3 {
		t.Fatalf("caret = %d after typing, want 3", m.input.cursor)
	}
	m = pressKey(t, m, tea.KeyHome)
	if m.input.cursor != 0 {
		t.Fatalf("Home must move the caret, caret = %d", m.input.cursor)
	}
	if m.chatScroll != scroll {
		t.Fatalf("Home must not scroll while the line has text: offset %d, want %d", m.chatScroll, scroll)
	}
	m = pressKey(t, m, tea.KeyEnd)
	if m.input.cursor != 3 {
		t.Fatalf("End must move the caret, caret = %d", m.input.cursor)
	}
	if m.chatScroll != scroll {
		t.Fatalf("End must not scroll while the line has text: offset %d, want %d", m.chatScroll, scroll)
	}
}

// TestScrollHoldsPositionWhileStreamingAndFlagsNewRows is the core follow
// contract: at the bottom the stream is followed, scrolled up the window stays
// put, arriving rows raise "↓ new", and End resumes the follow.
func TestScrollHoldsPositionWhileStreamingAndFlagsNewRows(t *testing.T) {
	m := chatScrollModel(t, 60)
	m = wheelUp(t, m)
	if m.chatScroll != 3 {
		t.Fatalf("offset = %d, want 3", m.chatScroll)
	}
	before := m.View()
	if !strings.Contains(before, "row-056") {
		t.Fatalf("window tail should be row-056 after 3 rows up:\n%s", before)
	}
	if strings.Contains(before, "row-057") {
		t.Fatalf("row-057 must be below the window:\n%s", before)
	}
	if strings.Contains(statusLine(before), chatNewMark) {
		t.Fatalf("no new rows have arrived yet:\n%s", before)
	}

	// The stream keeps appending under the reader.
	m.chat = append(m.chat, ChatLine{Role: "tool", Text: "row-060"})
	m.noteChatGrowth()
	after := m.View()
	if !strings.Contains(after, "row-056") || strings.Contains(after, "row-057") {
		t.Fatalf("the window must not slide down when rows arrive:\n%s", after)
	}
	if m.chatScroll != 4 {
		t.Fatalf("offset = %d, want 4 (absorbing one new row)", m.chatScroll)
	}
	if !strings.Contains(statusLine(after), chatNewMark) {
		t.Fatalf("status bar must advertise %q:\n%s", chatNewMark, after)
	}

	m = pressKey(t, m, tea.KeyEnd)
	live := m.View()
	if m.chatScroll != 0 {
		t.Fatalf("End = offset %d, want 0", m.chatScroll)
	}
	if !strings.Contains(live, "row-060") {
		t.Fatalf("End must jump to the newest row:\n%s", live)
	}
	if strings.Contains(statusLine(live), chatNewMark) {
		t.Fatalf("%q must clear at the bottom:\n%s", chatNewMark, live)
	}
}

// TestFollowKeepsUpWithoutScrolling pins the default: at the bottom every new
// row lands on screen on its own.
func TestFollowKeepsUpWithoutScrolling(t *testing.T) {
	m := chatScrollModel(t, 60)
	m.chat = append(m.chat, ChatLine{Role: "tool", Text: "row-060"})
	m.noteChatGrowth()
	view := m.View()
	if !strings.Contains(view, "row-060") {
		t.Fatalf("a following viewport must show the newest row:\n%s", view)
	}
	if m.chatScroll != 0 {
		t.Fatalf("offset = %d, want 0", m.chatScroll)
	}
	if strings.Contains(statusLine(view), chatNewMark) {
		t.Fatalf("%q must not appear while following:\n%s", chatNewMark, view)
	}
}

// TestScrollBarStaysOutOfTheInputLine keeps the IME guarantee: the input row is
// still the last row of the view and no scrollbar glyph may reach it or the
// status bar.
func TestScrollBarStaysOutOfTheInputLine(t *testing.T) {
	m := chatScrollModel(t, 60)
	m = wheelUp(t, m)
	view := m.View()
	lines := strings.Split(view, "\n")

	if last := lines[len(lines)-1]; !strings.Contains(last, promptPrefix) {
		t.Fatalf("input line is not the last row: %q", last)
	}
	for i, line := range lines {
		if i == len(lines)-1 && strings.ContainsAny(line, "\u2588\u2591") {
			t.Fatalf("the scrollbar must not touch the input line: %q", line)
		}
	}
	if status := statusLine(view); strings.ContainsAny(status, "\u2588\u2591") {
		t.Fatalf("the scrollbar must not touch the status bar: %q", status)
	}
	if !strings.Contains(view, "\u2591") {
		t.Fatalf("an overflowing transcript must draw a scrollbar:\n%s", view)
	}
}

// TestWheelDoesNotStealTheComposerClick: a left click still lands in the
// composer while the transcript is scrolled, and it must not move the window.
func TestWheelDoesNotStealTheComposerClick(t *testing.T) {
	m := chatScrollModel(t, 60)
	m = wheelUp(t, m)
	scroll := m.chatScroll

	top, _, inputRow := m.composerRegion()
	if inputRow < 0 {
		t.Fatal("the composer must hold an input row")
	}
	next, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 6, Y: top + inputRow})
	m = next.(*Model)
	if m.chatScroll != scroll {
		t.Fatalf("a composer click moved the window: offset %d, want %d", m.chatScroll, scroll)
	}
}

// TestScrollIsIgnoredOutsideTheChatPane keeps the wheel and the page keys from
// moving a transcript the user cannot see.
func TestScrollIsIgnoredOutsideTheChatPane(t *testing.T) {
	m := chatScrollModel(t, 60)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m = next.(*Model)
	m = wheelUp(t, m)
	m = pressKey(t, m, tea.KeyPgUp)
	if m.chatScroll != 0 {
		t.Fatalf("a panel above the chat moved the transcript: offset %d", m.chatScroll)
	}
}
