package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"agent-platform/internal/cli/tui/views"
)

const (
	promptCursorMark = "▏"
	maxGoalRunes     = 8192
	// PromptPlaceholder is shown while the buffer is empty.
	PromptPlaceholder = "Send a message…  (Enter to send · Shift+Enter newline)"
)

// PromptOptions configures the single-line goal prompt shown by the loop
// command when --goal is omitted.
type PromptOptions struct {
	NoColor   bool
	Hyperlink bool
	Width     int
	Height    int
	// Embedded switches the prompt into the resident-session input box:
	// Enter emits promptSubmitMsg instead of quitting, Esc clears the line, and
	// Ctrl+C asks the owning model to end the session.
	Embedded bool
}

// promptSubmitMsg carries a goal submitted from the embedded input box.
type promptSubmitMsg struct{ Value string }

// promptQuitMsg asks the owning session model to end the current terminal.
type promptQuitMsg struct{}

// PromptModel is a minimal single-line text input built directly on Bubble
// Tea so the CLI does not need the bubbles dependency.
type PromptModel struct {
	title    string
	noColor  bool
	embedded bool

	value  []rune
	cursor int
	width  int
	height int

	history []string
	histIdx int

	errText  string
	done     bool
	canceled bool
}

// NewPromptModel builds the goal prompt model. webURL and opts.Hyperlink are
// accepted for API compatibility with the monitor-board era: the Claude-style
// layout (T-P2-42) no longer renders a URL bar, the session view keeps the
// board reachable through the "o" key instead.
func NewPromptModel(title, webURL string, opts PromptOptions) *PromptModel {
	if strings.TrimSpace(title) == "" {
		title = "Harness Loop"
	}
	width := opts.Width
	if width <= 0 {
		width = 100
	}
	height := opts.Height
	if height <= 0 {
		height = 24
	}
	return &PromptModel{
		title:    title,
		noColor:  opts.NoColor,
		embedded: opts.Embedded,
		width:    width,
		height:   height,
	}
}

// Init implements tea.Model.
func (m *PromptModel) Init() tea.Cmd { return nil }

// Update implements tea.Model.
func (m *PromptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width > 0 {
			m.width = msg.Width
		}
		if msg.Height > 0 {
			m.height = msg.Height
		}
		return m, nil

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}
	return m, nil
}

func (m *PromptModel) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyCtrlC:
		if m.embedded {
			return func() tea.Msg { return promptQuitMsg{} }
		}
		m.canceled = true
		return tea.Quit
	case tea.KeyEsc:
		if m.embedded {
			m.reset()
			return nil
		}
		m.canceled = true
		return tea.Quit
	case tea.KeyEnter:
		// Shift+Enter (reported by bubbletea as alt+enter) inserts a newline
		// instead of submitting.
		if msg.Alt {
			m.insertRunes([]rune{'\n'}, true)
			return nil
		}
		if strings.TrimSpace(string(m.value)) == "" {
			m.errText = "goal cannot be empty"
			return nil
		}
		value := m.Value()
		m.pushHistory(value)
		if m.embedded {
			m.reset()
			return func() tea.Msg { return promptSubmitMsg{Value: value} }
		}
		m.done = true
		return tea.Quit
	case tea.KeyCtrlJ:
		m.insertRunes([]rune{'\n'}, true)
	case tea.KeyUp:
		m.historyPrev()
	case tea.KeyDown:
		m.historyNext()
	case tea.KeyBackspace:
		if m.cursor > 0 {
			m.deleteRange(m.cursor-1, m.cursor)
		}
	case tea.KeyDelete:
		if m.cursor < len(m.value) {
			m.deleteRange(m.cursor, m.cursor+1)
		}
	case tea.KeyLeft:
		if m.cursor > 0 {
			m.cursor--
		}
	case tea.KeyRight:
		if m.cursor < len(m.value) {
			m.cursor++
		}
	case tea.KeyHome:
		m.cursor = 0
	case tea.KeyEnd:
		m.cursor = len(m.value)
	case tea.KeyCtrlU:
		m.value = nil
		m.cursor = 0
		m.errText = ""
	case tea.KeyCtrlW:
		m.deleteWord()
	case tea.KeySpace:
		m.insertRunes([]rune{' '}, false)
	case tea.KeyRunes:
		// A paste may legitimately contain newlines; keep them as-is.
		m.insertRunes(msg.Runes, msg.Paste)
	}
	return nil
}

func (m *PromptModel) pushHistory(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if n := len(m.history); n > 0 && m.history[n-1] == value {
		m.histIdx = 0
		return
	}
	m.history = append(m.history, value)
	if len(m.history) > 100 {
		m.history = m.history[len(m.history)-100:]
	}
	m.histIdx = 0
}

func (m *PromptModel) historyPrev() {
	if len(m.history) == 0 {
		return
	}
	if m.histIdx < len(m.history) {
		m.histIdx++
	}
	m.setValue(m.history[len(m.history)-m.histIdx])
}

func (m *PromptModel) historyNext() {
	if m.histIdx <= 0 {
		return
	}
	m.histIdx--
	if m.histIdx == 0 {
		m.setValue("")
		return
	}
	m.setValue(m.history[len(m.history)-m.histIdx])
}

func (m *PromptModel) setValue(s string) {
	m.value = []rune(s)
	m.cursor = len(m.value)
	m.errText = ""
}

func (m *PromptModel) reset() {
	m.value = nil
	m.cursor = 0
	m.errText = ""
	m.done = false
	m.canceled = false
}

func (m *PromptModel) insertRunes(runes []rune, multiline bool) {
	for _, r := range runes {
		switch r {
		case '\r':
			continue
		case '\n':
			if !multiline {
				r = ' '
			}
		case '\t':
			r = ' '
		}
		if r < 0x20 || r == 0x7f {
			if r != '\n' {
				continue
			}
		}
		if len(m.value) >= maxGoalRunes {
			break
		}
		m.value = append(m.value, 0)
		copy(m.value[m.cursor+1:], m.value[m.cursor:])
		m.value[m.cursor] = r
		m.cursor++
	}
	m.errText = ""
}

func (m *PromptModel) deleteRange(from, to int) {
	if from < 0 || to > len(m.value) || from >= to {
		return
	}
	m.value = append(m.value[:from], m.value[to:]...)
	m.cursor = from
	m.errText = ""
}

func (m *PromptModel) deleteWord() {
	isSpace := func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' }
	start := m.cursor
	for start > 0 && isSpace(m.value[start-1]) {
		start--
	}
	for start > 0 && !isSpace(m.value[start-1]) {
		start--
	}
	m.deleteRange(start, m.cursor)
}

// View implements tea.Model. The layout mirrors the Claude-style session
// screen: wordmark, one-line hint, the input inside a rounded component box,
// then the key hints.
func (m *PromptModel) View() string {
	parts := []string{
		views.RenderLogo(m.noColor),
		"",
		"  " + m.title + " · describe the goal (Enter to send, Shift+Enter for a new line).",
		"",
		views.RenderBoxWithTheme(views.NewTheme(m.noColor), "", m.InputLine(), m.width),
	}
	if m.errText != "" {
		parts = append(parts, "", "  "+m.errText)
	}
	parts = append(parts, "", "  Enter start   Esc cancel")
	return strings.Join(parts, "\n")
}

// InputLine renders the multi-line composer. The cursor mark is inserted at
// the logical position; every row after the first is indented under the "> ".
func (m *PromptModel) InputLine() string {
	if len(m.value) == 0 {
		return "> " + PromptPlaceholder
	}
	runes := make([]rune, 0, len(m.value)+1)
	runes = append(runes, m.value[:m.cursor]...)
	runes = append(runes, []rune(promptCursorMark)...)
	runes = append(runes, m.value[m.cursor:]...)
	rows := strings.Split(string(runes), "\n")
	for i, row := range rows {
		if i == 0 {
			rows[i] = "> " + row
		} else {
			rows[i] = "  " + row
		}
	}
	return strings.Join(rows, "\n")
}

// HandleKey forwards a key press from an embedding model to the same input
// implementation, returning whatever command the edit produced.
func (m *PromptModel) HandleKey(msg tea.KeyMsg) tea.Cmd { return m.handleKey(msg) }

// Reset clears the input buffer between session tasks.
func (m *PromptModel) Reset() { m.reset() }

// Empty reports whether the composer buffer holds no non-space text.
func (m *PromptModel) Empty() bool { return strings.TrimSpace(string(m.value)) == "" }

// ErrorText returns the current inline validation error.
func (m *PromptModel) ErrorText() string { return m.errText }

// Value returns the trimmed goal text entered by the user.
func (m *PromptModel) Value() string {
	return strings.TrimSpace(string(m.value))
}

// Canceled reports whether the user dismissed the prompt.
func (m *PromptModel) Canceled() bool { return m.canceled }

// Done reports whether the user submitted a goal.
func (m *PromptModel) Done() bool { return m.done }

// RunPrompt runs the single-line goal prompt until the user submits or cancels.
func RunPrompt(ctx context.Context, title, webURL string, opts PromptOptions) (string, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	model := NewPromptModel(title, webURL, opts)
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		return "", false, err
	}
	if model.canceled || !model.done {
		return "", true, nil
	}
	return model.Value(), false, nil
}
