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

// TestComposerOpensWhileFocusedAndClosesOnSubmit covers the open composer: the
// ╭───╮ rule plus the ╰─ … ─╯ input row while the user types, the complete frame
// once the input is submitted and the composer is blurred.
func TestComposerOpensWhileFocusedAndClosesOnSubmit(t *testing.T) {
	factory := func(ctx context.Context, goal string) (*harnesspb.TaskView, error) {
		return &harnesspb.TaskView{TaskId: "task-1", Status: "running", CurrentStage: "a"}, nil
	}
	m := sessionModel(t, SessionConfig{CreateTask: factory})
	focused := m.View()
	if !strings.Contains(focused, "╭") {
		t.Fatalf("focused composer must draw a top rule:\n%s", focused)
	}
	if !strings.Contains(focused, views.ComposerCap) {
		t.Fatalf("focused composer must carry the %q cap:\n%q", views.ComposerCap, focused)
	}
	// The close is on the input row itself; no second border row may follow it.
	lines := strings.Split(focused, "\n")
	if !strings.Contains(lines[len(lines)-1], views.ComposerTail) {
		t.Fatalf("focused composer must close on the input row:\n%s", focused)
	}
	for _, row := range lines[:len(lines)-1] {
		if strings.Contains(row, "╯") {
			t.Fatalf("the close must only appear on the last row: %q", row)
		}
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
	if !strings.Contains(m.View(), "╰") || !strings.Contains(m.View(), "╯") {
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
	x := views.ComposerPromptColumn(0) + lipgloss.Width(promptPrefix) + 2
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

// TestComposerInputLineCarriesTheCap checks the visual close of the open
// composer: the last row is the input line itself, it starts with the ╰─ cap and
// ends with ─╯, so the field reads as finished without any row below it.
func TestComposerInputLineCarriesTheCap(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	m = typeLine(t, m, "你好")
	lines := strings.Split(m.View(), "\n")
	last := lines[len(lines)-1]

	if !strings.Contains(last, views.ComposerCap) {
		t.Fatalf("input row must start with the %q cap: %q", views.ComposerCap, last)
	}
	if !strings.Contains(last, promptPrefix+"你好") {
		t.Fatalf("last row is not the input line: %q", last)
	}
	if idx := strings.Index(last, views.ComposerCap); idx != views.ComposerIndent {
		t.Fatalf("cap must sit at column %d: %q", views.ComposerIndent, last)
	}
	if !strings.Contains(last, views.ComposerTail) {
		t.Fatalf("input row must end with the %q close: %q", views.ComposerTail, last)
	}
	if !strings.HasSuffix(last, "╯") {
		t.Fatalf("╯ must be the very last glyph of the input row: %q", last)
	}
	if lines[len(lines)-1] == "" || strings.TrimSpace(last) == "" {
		t.Fatal("the last row must hold the input, not padding")
	}
}

// TestComposerKeepsHeightOnFocusChange makes sure blurring the composer does not
// shift the layout: the reserved row above the rule absorbs the closed box.
func TestComposerKeepsHeightOnFocusChange(t *testing.T) {
	factory := func(ctx context.Context, goal string) (*harnesspb.TaskView, error) {
		return &harnesspb.TaskView{TaskId: "task-1", Status: "running", CurrentStage: "a"}, nil
	}
	m := sessionModel(t, SessionConfig{CreateTask: factory})
	focused := len(m.composerBlock(m.viewWidth()))

	m = typeLine(t, m, "/task ship it")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(*Model)
	next, _ = m.Update(cmd())
	m = next.(*Model)

	if blurred := len(m.composerBlock(m.viewWidth())); blurred != focused {
		t.Fatalf("composer height jumps on blur: focused=%d blurred=%d", focused, blurred)
	}
}

// TestComposerFitsNarrowTerminals keeps the composer inside the screen width;
// the input row must shrink rather than wrap onto an extra row.
func TestComposerFitsNarrowTerminals(t *testing.T) {
	for _, width := range []int{34, 40, 50} {
		m := sizedSessionModel(t, width, 20, Options{NoColor: true, WorkDir: "/w", GitBranch: "b", ModelName: "m"})
		m = typeLine(t, m, "这是一段很长很长的中文输入，用来确认窄终端不会溢出")
		lines := strings.Split(m.View(), "\n")
		for _, line := range lines {
			if lipgloss.Width(line) > width {
				t.Fatalf("width %d: line overflows (%d cells): %q", width, lipgloss.Width(line), line)
			}
		}
		if !strings.Contains(lines[len(lines)-1], promptPrefix) {
			// A long input wraps: the caret then sits on the last wrapped row,
			// which is still the last row of the view and still input.
			capAt := -1
			for i, line := range lines {
				if strings.Contains(line, views.ComposerCap) {
					capAt = i
				}
			}
			if capAt < 0 {
				t.Fatalf("width %d: composer cap missing:\n%s", width, m.View())
			}
			for _, line := range lines[capAt:] {
				if strings.TrimSpace(line) == "" {
					t.Fatalf("width %d: blank row inside the input block:\n%s", width, m.View())
				}
			}
			if strings.Contains(lines[len(lines)-1], "? help") {
				t.Fatalf("width %d: status bar below the input:\n%s", width, m.View())
			}
		}
	}
}

// TestComposerClosesOnlyTheLastInputRow pins the T-P2-51 shape: the ╭───╮ rule,
// then rows opened by ╰─, and exactly one ╯ on the final row. Wrapped
// continuation rows stay open like a field still being typed into, and no row
// may follow the input.
func TestComposerClosesOnlyTheLastInputRow(t *testing.T) {
	for _, tc := range []struct {
		width int
		text  string
	}{
		{76, "你好"},
		{60, "这是一段很长很长的中文输入，用来确认窄终端换行时只有最后一行带右收尾"},
		{40, "emoji 🙂 也要能换行"},
	} {
		m := sizedSessionModel(t, tc.width, 20, Options{NoColor: true, WorkDir: "/w", GitBranch: "b", ModelName: "m"})
		m = typeLine(t, m, tc.text)
		lines := strings.Split(m.View(), "\n")

		ruleAt := -1
		for i, line := range lines {
			if strings.Contains(line, "╭") {
				ruleAt = i
			}
		}
		if ruleAt < 0 {
			t.Fatalf("width %d: no top rule:/n%s", tc.width, m.View())
		}
		if ruleAt+1 >= len(lines) {
			t.Fatalf("width %d: rule is the last row:/n%s", tc.width, m.View())
		}
		if !strings.Contains(lines[ruleAt], "╮") {
			t.Fatalf("width %d: rule not closed on the right: %q", tc.width, lines[ruleAt])
		}

		// The block runs to the end of the view: nothing may follow the input.
		block := lines[ruleAt:]
		lastRow := block[len(block)-1]
		if !strings.Contains(lastRow, views.ComposerTail) {
			t.Fatalf("width %d: last input row must carry %q: %q", tc.width, views.ComposerTail, lastRow)
		}
		if !strings.HasSuffix(lastRow, "╯") {
			t.Fatalf("width %d: ╯ must be the final glyph: %q", tc.width, lastRow)
		}
		for _, row := range block[:len(block)-1] {
			if strings.Contains(row, "╯") {
				t.Fatalf("width %d: only the last input row may close: %q", tc.width, row)
			}
		}
		for _, row := range lines {
			if lipgloss.Width(row) > tc.width {
				t.Fatalf("width %d: row overflows (%d cells): %q", tc.width, lipgloss.Width(row), row)
			}
		}
	}
}

// TestComposerCloseAlignsWithTheTopRule keeps the frame square: the ─╯ row and
// the ╭───╮ row must end in the same column.
func TestComposerCloseAlignsWithTheTopRule(t *testing.T) {
	m := sessionModel(t, SessionConfig{})
	m = typeLine(t, m, "hi")
	lines := strings.Split(m.View(), "\n")
	last := lines[len(lines)-1]
	rule := lines[len(lines)-2]
	if lipgloss.Width(last) != lipgloss.Width(rule) {
		t.Fatalf("frame is ragged: rule=%d close=%d\n%q\n%q",
			lipgloss.Width(rule), lipgloss.Width(last), rule, last)
	}
}

// TestComposerClickOnCloseClampsToTextEnd keeps a click over the ─╯ right edge
// from pushing the caret past the last rune.
func TestComposerClickOnCloseClampsToTextEnd(t *testing.T) {
	const typed = "你好ab"
	m := sessionModel(t, SessionConfig{})
	m = typeLine(t, m, typed)
	top, _, inputRow := m.composerRegion()
	width := m.viewWidth()

	for _, x := range []int{
		views.ComposerIndent + views.ComposerInnerWidth(width) - 1, // inside ─╯
		width + 8, // past the frame entirely
	} {
		next, _ := m.Update(tea.MouseMsg{
			X:      x,
			Y:      top + inputRow,
			Button: tea.MouseButtonLeft,
			Action: tea.MouseActionPress,
		})
		m = next.(*Model)
		if want := len([]rune(typed)); m.input.cursor != want {
			t.Fatalf("x=%d: caret = %d, want %d (end of text)", x, m.input.cursor, want)
		}
	}
}

// TestComposerClickOnWrappedRowMapsRunes covers a click on a continuation row:
// the column is measured from the row's own indent, so the caret still lands on
// the rune under the pointer.
func TestComposerClickOnWrappedRowMapsRunes(t *testing.T) {
	const typed = "这是一段很长很长的中文输入，用来确认窄终端换行时落点仍然正确"
	m := sizedSessionModel(t, 40, 20, Options{NoColor: true, WorkDir: "/w", GitBranch: "b", ModelName: "m"})
	m = typeLine(t, m, typed)
	top, _, inputRow := m.composerRegion()

	textWidth := views.ComposerContentWidth(m.viewWidth())
	runes := []rune(typed)

	// Click the second display row, one cell in: that is the rune right after
	// the first row's worth of text.
	x := views.ComposerPromptColumn(1) + 1
	next, _ := m.Update(tea.MouseMsg{
		X:      x,
		Y:      top + inputRow + 1,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	})
	m = next.(*Model)

	cells := 0
	want := 0
	for want < len(runes) && cells+lipgloss.Width(string(runes[want])) <= textWidth {
		cells += lipgloss.Width(string(runes[want]))
		want++
	}
	if m.input.cursor != want {
		t.Fatalf("caret = %d, want %d (first rune of the second row)", m.input.cursor, want)
	}
}
