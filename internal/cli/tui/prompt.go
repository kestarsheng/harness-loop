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
		if strings.TrimSpace(string(m.value)) == "" {
			m.errText = "goal cannot be empty"
			return nil
		}
		if m.embedded {
			value := m.Value()
			m.reset()
			return func() tea.Msg { return promptSubmitMsg{Value: value} }
		}
		m.done = true
		return tea.Quit
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
		m.insertRunes([]rune{' '})
	case tea.KeyRunes:
		m.insertRunes(msg.Runes)
	}
	return nil
}

func (m *PromptModel) reset() {
	m.value = nil
	m.cursor = 0
	m.errText = ""
	m.done = false
	m.canceled = false
}

func (m *PromptModel) insertRunes(runes []rune) {
	for _, r := range runes {
		switch r {
		case '\r', '\n', '\t':
			r = ' '
		}
		if r < 0x20 || r == 0x7f {
			continue
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
	start := m.cursor
	for start > 0 && m.value[start-1] == ' ' {
		start--
	}
	for start > 0 && m.value[start-1] != ' ' {
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
		"  " + m.title + " · describe the goal for this session (single line).",
		"",
		views.RenderBox("", m.InputLine(), m.width),
	}
	if m.errText != "" {
		parts = append(parts, "", "  "+m.errText)
	}
	parts = append(parts, "", "  Enter start   Esc cancel")
	return strings.Join(parts, "\n")
}

func (m *PromptModel) displayLine() string {
	width := m.width - 8
	if width < 20 {
		width = 20
	}
	start := 0
	if m.cursor > width-1 {
		start = m.cursor - (width - 1)
	}
	end := start + width
	if end > len(m.value) {
		end = len(m.value)
	}
	if start > end {
		start = end
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	b.WriteString(string(m.value[start:m.cursor]))
	b.WriteString(promptCursorMark)
	b.WriteString(string(m.value[m.cursor:end]))
	if end < len(m.value) {
		b.WriteString("…")
	}
	return b.String()
}

// HandleKey forwards a key press from an embedding model to the same input
// implementation, returning whatever command the edit produced.
func (m *PromptModel) HandleKey(msg tea.KeyMsg) tea.Cmd { return m.handleKey(msg) }

// Reset clears the input buffer between session tasks.
func (m *PromptModel) Reset() { m.reset() }

// InputLine renders the compact one-line composer used inside the session.
// The leading spaces are gone: the component box supplies its own padding.
func (m *PromptModel) InputLine() string { return "> " + m.displayLine() }

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
