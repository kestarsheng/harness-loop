package views

import (
	"fmt"
	"strings"
	"time"
)

type Incident struct {
	Time     time.Time
	Seq      int64
	Type     string
	Category string
	Stage    string
	Summary  string
	Severity string
}

type HealthData struct {
	TaskID     string
	Status     string
	Connected  bool
	LastEvent  time.Time
	EventsSeen int

	Reconnecting     bool
	ReconnectAttempt int

	Errors            int
	Warnings          int
	RetryCount        int
	AcceptanceFailed  int
	PendingApprovals  int
	FailedSubtasks    int
	CircuitOpen       int
	ModelFallbacks    int
	ModelSwitches     int
	ModelUnavailable  int
	CheckpointDrift   int
	MemoryDegraded    int
	CompactionDegrade int
	StateRebuilds     int
	SecurityDenied    int
	ToolDenied        int

	BudgetTokens   int64
	ConsumedTokens int64
	BudgetCost     float64
	ConsumedCost   float64

	Error     string
	Incidents []Incident
}

func (d HealthData) budgetRatio() float64 {
	if d.BudgetTokens > 0 {
		return float64(d.ConsumedTokens) / float64(d.BudgetTokens)
	}
	if d.BudgetCost > 0 {
		return d.ConsumedCost / d.BudgetCost
	}
	return 0
}

func (d HealthData) incidentCounts() (security, tool, approval, other int) {
	for _, item := range d.Incidents {
		switch item.Category {
		case "security":
			security++
		case "tool":
			tool++
		case "approval":
			approval++
		default:
			other++
		}
	}
	return
}

func HealthLevel(d HealthData) (string, []string) {
	if !d.Connected && d.Reconnecting {
		return "critical", []string{"event stream disconnected"}
	}
	reasons := make([]string, 0, 4)
	if d.Errors > 0 {
		reasons = append(reasons, fmt.Sprintf("%d error events", d.Errors))
	}
	if d.SecurityDenied > 0 || d.ToolDenied > 0 {
		reasons = append(reasons, fmt.Sprintf("security/tool denials: %d/%d", d.SecurityDenied, d.ToolDenied))
	}
	if d.PendingApprovals > 0 {
		reasons = append(reasons, fmt.Sprintf("%d approvals pending", d.PendingApprovals))
	}
	if d.FailedSubtasks > 0 || d.CircuitOpen > 0 {
		reasons = append(reasons, fmt.Sprintf("subtasks failed=%d circuit_open=%d", d.FailedSubtasks, d.CircuitOpen))
	}
	if d.CheckpointDrift > 0 || d.MemoryDegraded > 0 || d.CompactionDegrade > 0 {
		reasons = append(reasons, fmt.Sprintf("degraded: drift=%d memory=%d compaction=%d",
			d.CheckpointDrift, d.MemoryDegraded, d.CompactionDegrade))
	}
	if d.ModelUnavailable > 0 {
		reasons = append(reasons, fmt.Sprintf("%d models unavailable", d.ModelUnavailable))
	}
	if ratio := d.budgetRatio(); ratio >= 0.9 {
		reasons = append(reasons, fmt.Sprintf("budget at %.0f%%", ratio*100))
	}
	if len(reasons) == 0 {
		if d.EventsSeen == 0 {
			return "waiting", []string{"no events yet"}
		}
		return "healthy", nil
	}
	return "degraded", reasons
}

func healthTone(level string) string {
	switch level {
	case "healthy":
		return "ok"
	case "waiting":
		return "muted"
	case "critical":
		return "err"
	default:
		return "warn"
	}
}

func RenderHealth(d HealthData, width, maxLines int, theme Theme) string {
	level, reasons := HealthLevel(d)
	lines := []string{theme.paint("Health · "+level, theme.Primary)}
	if d.Error != "" {
		lines = append(lines, theme.paint(" "+d.Error, theme.Err))
	}
	stream := "connected"
	if d.Reconnecting {
		stream = fmt.Sprintf("reconnecting (attempt %d)", d.ReconnectAttempt)
	} else if !d.Connected {
		stream = "disconnected"
	}
	last := "-"
	if !d.LastEvent.IsZero() {
		last = d.LastEvent.Format("15:04:05")
	}
	lines = append(lines, fmt.Sprintf(" stream      %s · last event %s · %d events seen", stream, last, d.EventsSeen))
	lines = append(lines, fmt.Sprintf(" signals     errors=%d warnings=%d retries=%d acceptance_failed=%d",
		d.Errors, d.Warnings, d.RetryCount, d.AcceptanceFailed))
	lines = append(lines, fmt.Sprintf(" approvals   pending=%d · subtasks failed=%d circuit_open=%d",
		d.PendingApprovals, d.FailedSubtasks, d.CircuitOpen))
	lines = append(lines, fmt.Sprintf(" models      fallbacks=%d switches=%d unavailable=%d",
		d.ModelFallbacks, d.ModelSwitches, d.ModelUnavailable))
	lines = append(lines, fmt.Sprintf(" integrity   checkpoint_drift=%d memory_degraded=%d compaction_degraded=%d state_rebuild=%d",
		d.CheckpointDrift, d.MemoryDegraded, d.CompactionDegrade, d.StateRebuilds))
	lines = append(lines, " budget      "+renderHealthBudget(d))
	if len(reasons) > 0 {
		lines = append(lines, paintTone(theme, healthTone(level), " "+strings.Join(reasons, "; ")))
	}
	security, tool, approval, other := d.incidentCounts()
	drawerTitle := fmt.Sprintf("Incident drawer · %d (security=%d tool=%d approval=%d other=%d)",
		len(d.Incidents), security, tool, approval, other)
	lines = append(lines, theme.paint(drawerTitle, theme.Primary))
	if len(d.Incidents) == 0 {
		lines = append(lines, theme.paint(" no incidents recorded (security.violation / tool.denied / approval.*)", theme.Secondary))
	} else {
		for i := len(d.Incidents) - 1; i >= 0; i-- {
			item := d.Incidents[i]
			stage := item.Stage
			if stage == "" {
				stage = "-"
			}
			text := fmt.Sprintf(" ! %s", item.Time.Format("15:04:05"))
			if item.Seq > 0 {
				text += fmt.Sprintf(" #%d", item.Seq)
			}
			text += fmt.Sprintf(" [%s] %s %s", item.Category, item.Type, stage)
			if item.Summary != "" {
				text += "  " + item.Summary
			}
			tone := "warn"
			if item.Category == "security" || item.Category == "tool" || item.Severity == "error" {
				tone = "err"
			}
			lines = append(lines, paintTone(theme, tone, fit(text, width)))
		}
	}
	return strings.Join(limitLines(lines, maxLines), "\n")
}

func renderHealthBudget(d HealthData) string {
	tokens := "tokens " + formatTokens(d.ConsumedTokens)
	if d.BudgetTokens > 0 {
		tokens += "/" + formatTokens(d.BudgetTokens)
	}
	cost := fmt.Sprintf("cost %.4f", d.ConsumedCost)
	if d.BudgetCost > 0 {
		cost += fmt.Sprintf("/%.4f", d.BudgetCost)
	}
	ratio := d.budgetRatio()
	if ratio > 0 {
		tokens += fmt.Sprintf(" (%.0f%%)", ratio*100)
	}
	return tokens + "   " + cost
}
