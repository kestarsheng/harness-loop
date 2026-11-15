package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func newPrompt() *PromptModel {
	return NewPromptModel("Harness Loop", URLFor(DefaultWebBaseURL), PromptOptions{NoColor: true, Width: 100, Height: 24})
}

func typeRunes(t *testing.T, m *PromptModel, text string) *PromptModel {
	t.Helper()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
	return next.(*PromptModel)
}

func TestPromptViewShowsFixedURL(t *testing.T) {
	m := newPrompt()
	view := m.View()
	if !strings.Contains(view, URLFor(DefaultWebBaseURL)) {
		t.Fatalf("prompt must show the fixed monitor URL:\n%s", view)
	}
	if !strings.Contains(view, "Harness Loop") || !strings.Contains(view, "Esc cancel") {
		t.Fatalf("prompt chrome missing:\n%s", view)
	}
}

func TestPromptEmptyGoalRejected(t *testing.T) {
	m := newPrompt()
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(*PromptModel)
	if cmd != nil || m.Done() {
		t.Fatal("empty goal must not be submitted")
	}
	if !strings.Contains(m.View(), "goal cannot be empty") {
		t.Fatalf("empty goal error missing:\n%s", m.View())
	}
}

func TestPromptTypingBackspaceAndSubmit(t *testing.T) {
	m := newPrompt()
	m = typeRunes(t, m, "修复 bug")
	if got := m.Value(); got != "修复 bug" {
		t.Fatalf("value = %q", got)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = next.(*PromptModel)
	if got := m.Value(); got != "修复 bu" {
		t.Fatalf("after backspace = %q", got)
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(*PromptModel)
	if !m.Done() || m.Canceled() {
		t.Fatalf("done=%v canceled=%v", m.Done(), m.Canceled())
	}
	if cmd == nil {
		t.Fatal("submit must quit the prompt")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("submit command = %T", cmd())
	}
	if got := m.Value(); got != "修复 bu" {
		t.Fatalf("submitted value = %q", got)
	}
}

func TestPromptPasteAndCursorEditing(t *testing.T) {
	m := newPrompt()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc\nxyz"), Paste: true})
	m = next.(*PromptModel)
	if got := m.Value(); got != "abc xyz" {
		t.Fatalf("pasted value = %q", got)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft, Runes: nil})
	m = next.(*PromptModel)
	m = typeRunes(t, m, "-")
	if got := m.Value(); got != "abc xy-z" {
		t.Fatalf("cursor insert = %q", got)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
	m = next.(*PromptModel)
	if got := m.Value(); got != "abc z" {
		t.Fatalf("ctrl+w = %q", got)
	}
}

func TestPromptCancel(t *testing.T) {
	m := newPrompt()
	m = typeRunes(t, m, "throwaway")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(*PromptModel)
	if !m.Canceled() || m.Done() {
		t.Fatalf("canceled=%v done=%v", m.Canceled(), m.Done())
	}
	if cmd == nil {
		t.Fatal("cancel must quit the prompt")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("cancel command = %T", cmd())
	}
}

func TestPromptWindowSize(t *testing.T) {
	m := newPrompt()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = next.(*PromptModel)
	if m.width != 40 || m.height != 20 {
		t.Fatalf("size = %dx%d", m.width, m.height)
	}
}
