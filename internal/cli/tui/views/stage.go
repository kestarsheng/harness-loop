package views

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

type StageData struct {
	TaskID         string
	Status         string
	TerminalReason string
	CurrentStage   string
	Attempt        int

	Attempts            map[string]int
	RetryByCategory     map[string]int
	RetryCount          int
	AcceptanceFailCount int

	Verdicts   map[string]string
	VerifiedBy map[string]string

	Tokens       int64
	BudgetTokens int64
	Cost         float64
	BudgetCost   float64
	Elapsed      time.Duration

	Connected        bool
	Reconnecting     bool
	ReconnectAttempt int
	Aborting         bool
	Error            string
}

var stageNames = map[string]string{
	"a": "需求分析",
	"b": "方案设计",
	"c": "执行编码",
	"d": "验证交付",
	"e": "复盘沉淀",
}

var stageOrder = []string{"a", "b", "c", "d", "e"}

func StageName(stage string) string {
	if stage == "" {
		return "-"
	}
	if name, ok := stageNames[stage]; ok {
		return stage + " " + name
	}
	return stage
}

func RenderHeader(d StageData) string {
	status := d.Status
	if status == "" {
		status = "unknown"
	}
	line := fmt.Sprintf(" Harness Loop · task %s · %s", shortID(d.TaskID), status)
	if d.TerminalReason != "" {
		line += " · " + d.TerminalReason
	}
	out := line
	if d.Reconnecting {
		out += fmt.Sprintf("\n reconnecting… (attempt %d) — events will be replayed from last_seq", d.ReconnectAttempt)
	}
	if d.Aborting {
		out += "\n abort requested, waiting for the runtime to acknowledge…"
	}
	if d.Error != "" {
		out += "\n " + d.Error
	}
	return out
}

func RenderStage(d StageData) string {
	stage := StageName(d.CurrentStage)
	attempt := d.Attempt
	if attempt == 0 {
		attempt = d.Attempts[d.CurrentStage]
	}
	lines := []string{
		fmt.Sprintf(" Stage      %-16s attempt #%d   elapsed %s", stage, attempt, formatDuration(d.Elapsed)),
		" Acceptance " + renderAcceptance(d),
		" Retry      " + renderRetry(d),
		" Budget     " + renderBudget(d),
	}
	return strings.Join(lines, "\n")
}

func renderAcceptance(d StageData) string {
	parts := make([]string, 0, len(stageOrder))
	for _, stage := range stageOrder {
		verdict := d.Verdicts[stage]
		if verdict == "" {
			if stage == d.CurrentStage {
				verdict = "pending"
			} else if d.Attempts[stage] > 0 {
				verdict = "pending"
			} else {
				continue
			}
		}
		item := stage + ":" + verdict
		if by := d.VerifiedBy[stage]; by != "" {
			item += " (" + by + ")"
		}
		parts = append(parts, item)
	}
	if len(parts) == 0 {
		return "no verdict yet"
	}
	return strings.Join(parts, "  ")
}

func renderRetry(d StageData) string {
	parts := []string{fmt.Sprintf("total=%d", d.RetryCount)}
	keys := make([]string, 0, len(d.RetryByCategory))
	for k := range d.RetryByCategory {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, d.RetryByCategory[k]))
	}
	parts = append(parts, fmt.Sprintf("acceptance_fail=%d", d.AcceptanceFailCount))
	return strings.Join(parts, "  ")
}

func renderBudget(d StageData) string {
	tokens := fmt.Sprintf("tokens %s", formatTokens(d.Tokens))
	if d.BudgetTokens > 0 {
		tokens += "/" + formatTokens(d.BudgetTokens)
	}
	cost := fmt.Sprintf("cost %.4f", d.Cost)
	if d.BudgetCost > 0 {
		cost += fmt.Sprintf("/%.4f", d.BudgetCost)
	}
	return tokens + "   " + cost
}

func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func fit(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	for i := len(runes); i > 0; i-- {
		cand := string(runes[:i]) + "…"
		if lipgloss.Width(cand) <= width {
			return cand
		}
	}
	return ""
}
