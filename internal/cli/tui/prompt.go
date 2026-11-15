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
}

// PromptModel is a minimal single-line text input built directly on Bubble
// Tea so the CLI does not need the bubbles dependency.
type PromptModel struct {
	title     string
	webURL    string
	hyperlink bool
	noColor   bool
	theme     Theme

	value  []rune
	cursor int
	width  int
	height int

	errText  string
	done     bool
	canceled bool
}

// NewPromptModel builds the goal prompt model.
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
		title:     title,
		webURL:    webURL,
		hyperlink: opts.Hyperlink,
		noColor:   opts.NoColor,
		theme:     NewTheme(opts.NoColor),
		width:     width,
		height:    height,
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
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *PromptModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyEsc:
		m.canceled = true
		return m, tea.Quit
	case tea.KeyEnter:
		if strings.TrimSpace(string(m.value)) == "" {
			m.errText = "goal cannot be empty"
			return m, nil
		}
		m.done = true
		return m, tea.Quit
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
	return m, nil
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

// View implements tea.Model.
func (m *PromptModel) View() string {
	parts := make([]string, 0, 6)
	if bar := views.RenderURLBar(RenderHyperlink(m.webURL, m.hyperlink), m.theme); bar != "" {
		parts = append(parts, bar)
	}
	parts = append(parts,
		m.paintTitle(m.title),
		"  Describe the goal for this loop session (single line).",
		"",
		"  > "+m.displayLine(),
	)
	if m.errText != "" {
		parts = append(parts, "  "+m.errText)
	}
	parts = append(parts, "", "  Enter start   Esc cancel")
	return strings.Join(parts, "\n")
}

func (m *PromptModel) paintTitle(s string) string {
	if m.noColor {
		return s
	}
	return m.theme.Title.Render(s)
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
