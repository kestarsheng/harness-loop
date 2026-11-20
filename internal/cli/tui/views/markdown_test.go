package views

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestRenderChatFinalRendersMarkdown(t *testing.T) {
	out := RenderChat([]ChatMessage{{
		Role: "assistant",
		Text: "# 标题\n\n**bold**\n\n- one\n- two\n\n`inline`\n\n```go\nfmt.Println(1)\n```",
	}}, 80, 40, NewTheme(true))

	for _, want := range []string{"标题", "bold", "one", "two", "inline", "fmt.Println"} {
		if !strings.Contains(out, want) {
			t.Fatalf("markdown render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "**bold**") {
		t.Fatalf("final reply must not keep raw bold markers:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("NO_COLOR markdown must stay ANSI-free:\n%q", out)
	}
}

func TestRenderChatStreamingStaysPlain(t *testing.T) {
	out := RenderChat([]ChatMessage{{
		Role:      "assistant",
		Text:      "**bold**",
		Streaming: true,
	}}, 80, 40, NewTheme(true))
	if !strings.Contains(out, "**bold**") {
		t.Fatalf("streaming token must stay verbatim:\n%s", out)
	}
	if !strings.Contains(out, "▍") {
		t.Fatalf("streaming cursor missing:\n%s", out)
	}
}

func TestRenderChatMarkdownFitsNarrowWidth(t *testing.T) {
	text := "# 很长的标题\n\n**加粗的一段很长的文字，用来确认窄终端不会横向溢出**\n\n- 列表一\n- 列表二\n\n```\nvery-long-code-line-without-breaks\n```"
	out := RenderChat([]ChatMessage{{Role: "assistant", Text: text}}, 60, 40, NewTheme(true))
	for _, line := range strings.Split(out, "\n") {
		if lipgloss.Width(line) > 60 {
			t.Fatalf("markdown line overflows 60 columns (%d): %q", lipgloss.Width(line), line)
		}
	}
}

func TestThemeHasOnlySemanticFields(t *testing.T) {
	typ := reflect.TypeOf(Theme{})
	want := map[string]bool{"NoColor": true, "Primary": true, "Secondary": true, "Border": true, "Err": true}
	if typ.NumField() != len(want) {
		t.Fatalf("Theme has %d fields, want %d: %v", typ.NumField(), len(want), typ)
	}
	for i := 0; i < typ.NumField(); i++ {
		if !want[typ.Field(i).Name] {
			t.Fatalf("unexpected Theme field %q", typ.Field(i).Name)
		}
	}
	theme := NewTheme(false)
	if theme.Border.GetForeground() == nil {
		t.Fatal("Border must carry the dim colour")
	}
	if theme.Border.GetForeground() != lipgloss.Color("8") {
		t.Fatalf("Border colour = %v, want dim 8", theme.Border.GetForeground())
	}
	box := RenderBoxWithTheme(NewTheme(true), "", "hello", 40)
	if !strings.Contains(box, "╭") {
		t.Fatalf("NO_COLOR box must keep the border:\n%s", box)
	}
}

func TestWordmarkWideAndNarrow(t *testing.T) {
	if WordmarkWidth != 72 {
		t.Fatalf("WordmarkWidth = %d, want 72 (T-P2-49 re-measured threshold)", WordmarkWidth)
	}
	wide := RenderWordmark(80, true)
	rows := strings.Split(wide, "\n")
	if len(rows) != 6 {
		// The figlet "standard" face renders "harness loop" in six rows: the
		// descender of the trailing "p" needs its own row.
		t.Fatalf("wide wordmark has %d rows, want 6:\n%s", len(rows), wide)
	}
	for _, want := range []string{"| |__", "|_| |_|", "_ __"} {
		if !strings.Contains(wide, want) {
			t.Fatalf("wide wordmark missing %q:\n%s", want, wide)
		}
	}
	if strings.Contains(wide, "\x1b[") {
		t.Fatalf("NO_COLOR wordmark must stay ANSI-free:\n%q", wide)
	}
	for _, row := range rows {
		if lipgloss.Width(row) > WordmarkWidth {
			t.Fatalf("wordmark row wider than %d: %q", WordmarkWidth, row)
		}
	}

	// 72 shows the banner; 71, 60 and 59 fall back to the single bold line.
	for _, width := range []int{WordmarkWidth - 1, 60, 59} {
		narrow := RenderWordmark(width, true)
		if narrow != LogoText {
			t.Fatalf("width %d wordmark = %q, want %q", width, narrow, LogoText)
		}
		if strings.Contains(narrow, "\n") {
			t.Fatalf("width %d wordmark must be a single line: %q", width, narrow)
		}
	}
	for _, width := range []int{WordmarkWidth, 80, 120} {
		got := RenderWordmark(width, true)
		if !strings.Contains(got, "\n") {
			t.Fatalf("width %d wordmark must use the banner: %q", width, got)
		}
	}
	if coloured := RenderWordmark(WordmarkWidth, false); !strings.Contains(coloured, "| |__") {
		t.Fatalf("coloured wordmark lost its glyphs:\n%q", coloured)
	}
}
