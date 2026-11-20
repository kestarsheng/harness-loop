package views

import (
	"fmt"
	"strings"
	"time"
)

type ModelSelectionItem struct {
	Stage         string
	Attempt       int
	ModelID       string
	Strategy      string
	ResolvedFrom  string
	Reason        string
	Operator      string
	ApprovalID    string
	Fallback      bool
	ContextWindow int
	TokensIn      int64
	TokensOut     int64
	Cost          float64
	CreatedAt     time.Time
}

type ModelData struct {
	Selections  []ModelSelectionItem
	Fallbacks   int
	Switches    int
	Recovered   int
	Unavailable int
	Rejected    int
	Error       string
}

func modelTone(item ModelSelectionItem) string {
	if item.Fallback {
		return "warn"
	}
	switch item.Strategy {
	case "fallback", "degraded":
		return "warn"
	case "primary", "":
		return "muted"
	default:
		return "accent"
	}
}

func RenderModels(d ModelData, width, maxLines int, theme Theme) string {
	fallbacks := d.Fallbacks
	if fallbacks == 0 {
		for _, item := range d.Selections {
			if item.Fallback {
				fallbacks++
			}
		}
	}
	title := fmt.Sprintf("Models · %d selections · %d fallbacks · %d switches · %d recovered · %d unavailable · %d rejected",
		len(d.Selections), fallbacks, d.Switches, d.Recovered, d.Unavailable, d.Rejected)
	lines := []string{theme.paint(title, theme.Primary)}
	if d.Error != "" {
		lines = append(lines, theme.paint(" "+d.Error, theme.Err))
	}
	if len(d.Selections) == 0 {
		lines = append(lines, theme.paint(" no model selection recorded yet (waiting for model.selected)", theme.Secondary))
		return strings.Join(limitLines(lines, maxLines), "\n")
	}
	for _, item := range d.Selections {
		stage := item.Stage
		if stage == "" {
			stage = "-"
		}
		model := item.ModelID
		if model == "" {
			model = "model?"
		}
		text := fmt.Sprintf(" %s#%d %-28s strategy=%-10s", stage, item.Attempt, model, item.Strategy)
		if item.ResolvedFrom != "" {
			text += " from=" + item.ResolvedFrom
		}
		if item.Fallback {
			text += " FALLBACK"
		}
		if item.ContextWindow > 0 {
			text += fmt.Sprintf(" ctx=%d", item.ContextWindow)
		}
		if item.TokensIn > 0 || item.TokensOut > 0 {
			text += " tokens=" + formatTokens(item.TokensIn) + "/" + formatTokens(item.TokensOut)
		}
		if item.Cost > 0 {
			text += fmt.Sprintf(" cost=%.4f", item.Cost)
		}
		if item.Reason != "" {
			text += " reason=" + item.Reason
		}
		if item.Operator != "" {
			text += " by=" + item.Operator
		}
		lines = append(lines, paintTone(theme, modelTone(item), fit(text, width)))
	}
	return strings.Join(limitLines(lines, maxLines), "\n")
}
