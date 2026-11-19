package views

import (
	"strings"
	"testing"
	"time"
)

func TestRenderChatShowsBothTurns(t *testing.T) {
	theme := NewTheme(true)
	out := RenderChat([]ChatMessage{
		{Role: "user", Text: "hi", Time: time.Unix(0, 0)},
		{Role: "assistant", Text: "你好，我在", Streaming: true},
	}, 80, 40, theme)

	for _, want := range []string{"Conversation", "you", "hi", "assistant", "你好，我在", "▍"} {
		if !strings.Contains(out, want) {
			t.Fatalf("chat view missing %q:\n%s", want, out)
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
