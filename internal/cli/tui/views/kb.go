package views

import (
	"fmt"
	"strings"
	"time"
)

type KBItem struct {
	Title      string
	Snippet    string
	Source     string
	DocumentID string
	ChunkID    string
	Score      float64
}

type KBData struct {
	Query   string
	Items   []KBItem
	Total   int64
	Latency time.Duration
	Loading bool
	Loaded  bool
	Error   string
	Hint    string
}

func RenderKB(d KBData, width, maxLines int, theme Theme) string {
	title := "Knowledge"
	if strings.TrimSpace(d.Query) != "" {
		title += " · " + kbTruncate(d.Query, 60)
	}
	lines := []string{theme.paint(title, theme.Title)}
	if d.Hint != "" {
		lines = append(lines, theme.paint(" "+d.Hint, theme.Muted))
	}
	if d.Error != "" {
		lines = append(lines, theme.paint(" "+d.Error, theme.Err))
	}
	switch {
	case d.Loading:
		lines = append(lines, theme.paint(" searching…", theme.Muted))
	case !d.Loaded:
		lines = append(lines, theme.paint(" type a query and press Enter (Esc back)", theme.Muted))
	case len(d.Items) == 0:
		lines = append(lines, theme.paint(" no results", theme.Muted))
	default:
		meta := fmt.Sprintf(" %d results · %.0f ms", d.Total, float64(d.Latency.Milliseconds()))
		lines = append(lines, theme.paint(meta, theme.Muted))
		for i, item := range d.Items {
			head := fmt.Sprintf(" %d. %.4f  %s", i+1, item.Score, kbTitle(item))
			if item.Source != "" && item.Source != kbTitle(item) {
				head += "  (" + item.Source + ")"
			}
			lines = append(lines, fit(head, width))
			if item.Snippet != "" {
				lines = append(lines, theme.paint(fit("    "+kbTruncate(item.Snippet, 160), width), theme.Muted))
			}
		}
	}
	return strings.Join(limitLines(lines, maxLines), "\n")
}

func kbTruncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

func kbTitle(item KBItem) string {
	if item.Title != "" {
		return item.Title
	}
	if item.DocumentID != "" {
		return item.DocumentID
	}
	return "-"
}
