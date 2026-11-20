package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"agent-platform/internal/cli/tui/views"
	harnesspb "agent-platform/pkg/pb/harnessloop"
)

// TestViewEndsWithInputLine pins the T-P2-49 layout: logo → body → status bar →
// hint → input, with the input as the very last row so the terminal caret and
// the IME candidate window attach to the bottom edge.
func TestViewEndsWithInputLine(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	m = typeLine(t, m, "修复 bug")
	lines := strings.Split(m.View(), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, promptPrefix+"修复 bug") {
		t.Fatalf("last view row is not the input line: %q", last)
	}
	if strings.Contains(last, "? help") {
		t.Fatalf("status bar must not sit below the input: %q", last)
	}
	for _, row := range lines[:len(lines)-1] {
		if strings.Contains(row, promptPrefix+"修复 bug") {
			t.Fatalf("input rendered above the last row: %q", row)
		}
	}
	if len(lines) != 40 {
		t.Fatalf("view has %d rows, want 40", len(lines))
	}
}

// TestStatusBarSitsAboveHintAndInput checks the middle of the bottom stack.
func TestStatusBarSitsAboveHintAndInput(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	status, hint, input := -1, -1, -1
	for i, row := range strings.Split(m.View(), "\n") {
		switch {
		case strings.Contains(row, "? help"):
			status = i
		case strings.Contains(row, "Enter to send"):
			hint = i
		case strings.Contains(row, promptPrefix):
			input = i
		}
	}
	if status < 0 || hint < 0 || input < 0 {
		t.Fatalf("bottom stack incomplete: status=%d hint=%d input=%d", status, hint, input)
	}
	if !(status < hint && hint < input) {
		t.Fatalf("bottom stack out of order: status=%d hint=%d input=%d", status, hint, input)
	}
}

// TestComposerOpensWhileFocusedAndClosesOnSubmit covers the open composer: only
// the top rule while the user types, the complete frame once the input is
// submitted and the composer is blurred.
func TestComposerOpensWhileFocusedAndClosesOnSubmit(t *testing.T) {
	factory := func(ctx context.Context, goal string) (*harnesspb.TaskView, error) {
		return &harnesspb.TaskView{TaskId: "task-1", Status: "running", CurrentStage: "a"}, nil
	}
	m := sessionModel(t, SessionConfig{CreateTask: factory})
	focused := m.View()
	if !strings.Contains(focused, "╭") {
		t.Fatalf("focused composer must draw a top rule:\n%s", focused)
	}
	if strings.Contains(focused, "╰") {
		t.Fatalf("focused composer must stay open (no bottom frame):\n%s", focused)
	}

	m = typeLine(t, m, "/task ship it")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(*Model)
	if cmd == nil {
		t.Fatal("submit produced no command")
	}
	next, _ = m.Update(cmd()) // promptSubmitMsg -> task creation starts
	m = next.(*Model)
	if !m.submitting {
		t.Fatalf("submit did not enter the creating state: submitting=%v awaiting=%v", m.submitting, m.awaitingInput)
	}
	if !strings.Contains(m.View(), "╰") {
		t.Fatalf("submitting must restore the complete frame:\n%s", m.View())
	}
}

// TestComposerHintAndErrorSitAboveTheInput keeps validation text out of the
// rows below the input line.
func TestComposerHintAndErrorSitAboveTheInput(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	if strings.Contains(m.input.InputLine(), "Enter to send") {
		t.Fatalf("hint must not live inside the input line: %q", m.input.InputLine())
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // empty submit
	m = next.(*Model)
	errText := m.input.ErrorText()
	if errText == "" {
		t.Fatal("empty submit must raise a validation error")
	}
	lines := strings.Split(m.View(), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, promptPrefix) {
		t.Fatalf("input must stay the last row after a validation error: %q", last)
	}
	found := -1
	for i, row := range lines {
		if strings.Contains(row, errText) {
			found = i
		}
	}
	if found < 0 || found >= len(lines)-1 {
		t.Fatalf("error text must render above the input: idx=%d of %d", found, len(lines))
	}
}

func TestBlinkIntervalIsUnderASecond(t *testing.T) {
	if cursorBlinkInterval != 530*time.Millisecond {
		t.Fatalf("cursor blink interval = %v, want 530ms", cursorBlinkInterval)
	}
}

func TestBlinkTickTogglesComposerCaret(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	if !m.cursorOn {
		t.Fatal("a fresh session must start with a visible caret")
	}
	next, cmd := m.Update(blinkTickMsg{})
	m = next.(*Model)
	if m.cursorOn || strings.Contains(m.input.InputLine(), promptCursorMark) {
		t.Fatalf("blink did not hide the caret: on=%v line=%q", m.cursorOn, m.input.InputLine())
	}
	if cmd == nil {
		t.Fatal("blink tick must reschedule itself")
	}
	next, _ = m.Update(cmd())
	m = next.(*Model)
	if !m.cursorOn || !strings.Contains(m.input.InputLine(), promptCursorMark) {
		t.Fatalf("blink did not restore the caret: on=%v line=%q", m.cursorOn, m.input.InputLine())
	}
}

func TestKeyPressResetsBlinkToVisible(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	m.setCursorVisible(false)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = next.(*Model)
	if !m.cursorOn || !strings.Contains(m.input.InputLine(), promptCursorMark) {
		t.Fatalf("typing must show the caret again: on=%v line=%q", m.cursorOn, m.input.InputLine())
	}
}

// TestMouseColumnMapsToRuneIndex covers the wide-character rule: a click column
// is a display column, so CJK and emoji consume two cells and the caret must
// land on a rune boundary rather than on a byte/column count.
func TestMouseColumnMapsToRuneIndex(t *testing.T) {
	const typed = "你好a🙂b"
	m := sessionModel(t, SessionConfig{})
	m = typeLine(t, m, typed)

	contentWidth := views.ComposerContentWidth(m.viewWidth())
	prefixCells := lipgloss.Width(promptPrefix)
	runes := []rune(typed)

	cells := 0
	for idx := 0; idx <= len(runes); idx++ {
		m.input.SetCursorByCell(0, cells+prefixCells, contentWidth)
		if m.input.cursor != idx {
			t.Fatalf("cell %d maps to rune %d, want %d", cells, m.input.cursor, idx)
		}
		if idx < len(runes) {
			cells += lipgloss.Width(string(runes[idx]))
		}
	}
}

// TestMouseClickOnInputMovesCaret drives the whole path through Update.
func TestMouseClickOnInputMovesCaret(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	m = typeLine(t, m, "你好ab")
	top, _, inputRow := m.composerRegion()
	if inputRow < 0 {
		t.Fatal("composer region has no input row")
	}
	// "你" occupies cells 2-3 and "好" cells 4-5 inside the content, after the
	// "> " prefix: clicking the cell just left of "好" puts the caret at rune 1.
	x := views.ComposerIndent + lipgloss.Width(promptPrefix) + 2
	next, _ := m.Update(tea.MouseMsg{
		X:      x,
		Y:      top + inputRow,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	})
	m = next.(*Model)
	if m.input.cursor != 1 {
		t.Fatalf("caret = %d, want 1 (click after the first CJK rune)", m.input.cursor)
	}
}

// TestMouseOutsideComposerIsIgnored keeps a stray click over the transcript from
// touching the input.
func TestMouseOutsideComposerIsIgnored(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	m = typeLine(t, m, "hello")
	m.setCursorVisible(false)
	m.input.SetCursorByCell(0, 0, views.ComposerContentWidth(m.viewWidth()))
	next, _ := m.Update(tea.MouseMsg{
		X:      3,
		Y:      1,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	})
	m = next.(*Model)
	if m.cursorOn {
		t.Fatal("a click above the composer must not focus it")
	}
	if m.input.cursor != 0 {
		t.Fatalf("a click above the composer moved the caret to %d", m.input.cursor)
	}
}

// TestMouseOnHintRowOnlyFocuses checks the non-input rows of the composer.
func TestMouseOnHintRowOnlyFocuses(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	m = typeLine(t, m, "abc")
	m.input.SetCursorByCell(0, 0, views.ComposerContentWidth(m.viewWidth()))
	m.setCursorVisible(false)
	top, _, inputRow := m.composerRegion()
	next, _ := m.Update(tea.MouseMsg{
		X:      6,
		Y:      top + inputRow - 2, // the hint row
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	})
	m = next.(*Model)
	if !m.cursorOn {
		t.Fatal("clicking the hint must focus the composer")
	}
	if m.input.cursor != 0 {
		t.Fatalf("clicking the hint moved the caret to %d", m.input.cursor)
	}
}

// TestBlinkChainKeepsOneTickInFlight makes sure the caret chain is a single
// chain: a path that hands the input back must not start a second one, or the
// caret would toggle at half the interval.
func TestBlinkChainKeepsOneTickInFlight(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	if !m.blinkRunning {
		t.Fatal("Init must start the caret blink chain")
	}
	if cmd := m.blinkTickCmd(); cmd != nil {
		t.Fatal("a second blink chain must not be started while one is in flight")
	}
	next, cmd := m.Update(blinkTickMsg{})
	m = next.(*Model)
	if cmd == nil || !m.blinkRunning {
		t.Fatalf("blink tick must re-arm exactly one chain: cmd=%v running=%v", cmd != nil, m.blinkRunning)
	}
}

// TestSeededGoalSessionStillBlinksCaret guards the --goal path: the input is
// hidden while the seeded task runs, and the chain must start when the task
// finishes so the caret blinks again.
func TestSeededGoalSessionStillBlinksCaret(t *testing.T) {
	factory := func(ctx context.Context, goal string) (*harnesspb.TaskView, error) {
		return &harnesspb.TaskView{TaskId: "task-seeded", Status: "running", CurrentStage: "a"}, nil
	}
	m := NewModel(TaskState{}, nil, Options{NoColor: true, Session: &SessionConfig{InitialGoal: "/task seeded", CreateTask: factory}})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(*Model)
	if cmd := m.Init(); cmd == nil {
		t.Fatal("seeded session must create its task")
	}
	if m.blinkRunning {
		t.Fatal("no blink chain may run while the seeded task owns the screen")
	}
	next, _ = completeTask(t, m, "task-seeded")
	m = next.(*Model)
	if !m.awaitingInput || !m.blinkRunning {
		t.Fatalf("the input must come back with a blinking caret: awaiting=%v running=%v", m.awaitingInput, m.blinkRunning)
	}
}
