package views

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type SubtaskNode struct {
	ID          string
	ParentID    string
	DAGID       string
	NodeID      string
	ParallelSet string
	Role        string
	Status      string
	Depth       int
	Index       int
	Attempt     int
	DependsOn   []string
	HandoffID   string
	AllocTokens int64
	Tokens      int64
	Cost        float64
	Artifacts   []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type HandoffLink struct {
	ID        string
	ParentID  string
	SubtaskID string
	From      string
	To        string
	Status    string
	Reason    string
	UpdatedAt time.Time
}

type CheckpointItem struct {
	ID        string
	Kind      string
	Stage     string
	Attempt   int
	Watermark int64
	Payload   string
	CreatedAt time.Time
}

type SubagentData struct {
	Subtasks    []SubtaskNode
	Handoffs    []HandoffLink
	Checkpoints []CheckpointItem
	Error       string
}

func subtaskTone(status string) string {
	switch status {
	case "accepted", "completed", "succeeded":
		return "ok"
	case "failed", "rejected", "circuit_open", "cancelled", "canceled":
		return "err"
	case "running", "started", "handoff_waiting", "in_progress":
		return "accent"
	default:
		return "muted"
	}
}

func paintTone(theme Theme, tone, s string) string {
	switch tone {
	case "ok":
		return theme.paint(s, theme.OK)
	case "err":
		return theme.paint(s, theme.Err)
	case "accent":
		return theme.paint(s, theme.Accent)
	case "warn":
		return theme.paint(s, theme.Warn)
	default:
		return theme.paint(s, theme.Muted)
	}
}

func limitLines(lines []string, maxLines int) []string {
	if maxLines > 0 && len(lines) > maxLines {
		return lines[:maxLines]
	}
	return lines
}

func subtaskCounts(nodes []SubtaskNode) (accepted, running, failed, queued int) {
	for _, node := range nodes {
		switch node.Status {
		case "accepted", "completed", "succeeded":
			accepted++
		case "running", "started", "in_progress", "handoff_waiting":
			running++
		case "failed", "rejected", "circuit_open", "cancelled", "canceled":
			failed++
		default:
			queued++
		}
	}
	return accepted, running, failed, queued
}

func progressBar(done, total, width int) string {
	if width <= 0 {
		width = 24
	}
	if total <= 0 {
		return strings.Repeat("-", width) + "   0%"
	}
	ratio := float64(done) / float64(total)
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	filled := int(ratio * float64(width))
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]" + fmt.Sprintf(" %3.0f%%", ratio*100)
}

func sortedSubtasks(nodes []SubtaskNode) []SubtaskNode {
	out := make([]SubtaskNode, len(nodes))
	copy(out, nodes)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func RenderSubtasks(d SubagentData, width, maxLines int, theme Theme) string {
	accepted, running, failed, queued := subtaskCounts(d.Subtasks)
	title := fmt.Sprintf("Subtasks · %d nodes · %d accepted · %d running · %d failed · %d queued",
		len(d.Subtasks), accepted, running, failed, queued)
	lines := []string{theme.paint(title, theme.Title)}
	if d.Error != "" {
		lines = append(lines, theme.paint(" "+d.Error, theme.Err))
	}
	if len(d.Subtasks) == 0 {
		lines = append(lines, theme.paint(" no subtasks recorded yet (waiting for subtask.plan_created)", theme.Muted))
		return strings.Join(limitLines(lines, maxLines), "\n")
	}
	lines = append(lines, " DAG "+progressBar(accepted, len(d.Subtasks), 24))
	for _, node := range sortedSubtasks(d.Subtasks) {
		indent := strings.Repeat("  ", node.Depth)
		role := node.Role
		if role == "" {
			role = "role?"
		}
		status := node.Status
		if status == "" {
			status = "unknown"
		}
		text := fmt.Sprintf("%s• %s %-12s %-15s", indent, shortID(node.ID), role, status)
		if node.Attempt > 0 {
			text += fmt.Sprintf(" attempt#%d", node.Attempt)
		}
		if node.ParallelSet != "" {
			text += " set=" + node.ParallelSet
		}
		if len(node.DependsOn) > 0 {
			deps := make([]string, 0, len(node.DependsOn))
			for _, dep := range node.DependsOn {
				deps = append(deps, shortID(dep))
			}
			text += " deps=" + strings.Join(deps, ",")
		}
		if node.HandoffID != "" {
			text += " handoff=" + shortID(node.HandoffID)
		}
		if node.AllocTokens > 0 || node.Tokens > 0 {
			text += " tokens=" + formatTokens(node.Tokens)
			if node.AllocTokens > 0 {
				text += "/" + formatTokens(node.AllocTokens)
			}
		}
		if node.Cost > 0 {
			text += fmt.Sprintf(" cost=%.4f", node.Cost)
		}
		lines = append(lines, paintTone(theme, subtaskTone(node.Status), fit(text, width)))
	}
	return strings.Join(limitLines(lines, maxLines), "\n")
}

func RenderHandoffs(d SubagentData, width, maxLines int, theme Theme) string {
	lines := []string{theme.paint(fmt.Sprintf("Handoffs · %d links", len(d.Handoffs)), theme.Title)}
	if d.Error != "" {
		lines = append(lines, theme.paint(" "+d.Error, theme.Err))
	}
	if len(d.Handoffs) == 0 {
		lines = append(lines, theme.paint(" no handoff recorded yet", theme.Muted))
		return strings.Join(limitLines(lines, maxLines), "\n")
	}
	for _, link := range d.Handoffs {
		from := link.From
		if from == "" {
			from = "-"
		}
		to := link.To
		if to == "" {
			to = "-"
		}
		text := fmt.Sprintf(" %s → %s %-12s", shortID(from), shortID(to), link.Status)
		if link.SubtaskID != "" {
			text += " subtask=" + shortID(link.SubtaskID)
		}
		if link.Reason != "" {
			text += " reason=" + link.Reason
		}
		lines = append(lines, paintTone(theme, handoffTone(link.Status), fit(text, width)))
	}
	return strings.Join(limitLines(lines, maxLines), "\n")
}

func handoffTone(status string) string {
	switch status {
	case "accepted", "completed":
		return "ok"
	case "rejected", "failed", "timeout":
		return "err"
	case "created", "pending", "waiting":
		return "accent"
	default:
		return "muted"
	}
}

func CountArtifacts(d SubagentData) int {
	total := 0
	for _, node := range d.Subtasks {
		total += len(node.Artifacts)
	}
	return total
}

func RenderArtifacts(d SubagentData, width, maxLines int, theme Theme) string {
	title := fmt.Sprintf("Artifacts & checkpoints · %d artifacts · %d checkpoints", CountArtifacts(d), len(d.Checkpoints))
	lines := []string{theme.paint(title, theme.Title)}
	if d.Error != "" {
		lines = append(lines, theme.paint(" "+d.Error, theme.Err))
	}
	artifactLines := make([]string, 0, CountArtifacts(d))
	for _, node := range sortedSubtasks(d.Subtasks) {
		for _, ref := range node.Artifacts {
			text := fmt.Sprintf(" artifact  %s  (%s %s)", ref, shortID(node.ID), node.Status)
			artifactLines = append(artifactLines, fit(text, width))
		}
	}
	if len(artifactLines) == 0 {
		lines = append(lines, theme.paint(" no artifacts recorded yet", theme.Muted))
	} else {
		lines = append(lines, artifactLines...)
	}
	if len(d.Checkpoints) == 0 {
		lines = append(lines, theme.paint(" no checkpoints recorded yet", theme.Muted))
	} else {
		for _, cp := range d.Checkpoints {
			text := fmt.Sprintf(" checkpoint %s %s", shortID(cp.ID), cp.Kind)
			if cp.Stage != "" {
				text += " " + cp.Stage
			}
			if cp.Attempt > 0 {
				text += fmt.Sprintf("#%d", cp.Attempt)
			}
			if cp.Watermark > 0 {
				text += fmt.Sprintf(" seq=%d", cp.Watermark)
			}
			if cp.Payload != "" {
				text += " ref=" + cp.Payload
			}
			if !cp.CreatedAt.IsZero() {
				text += " " + cp.CreatedAt.Format("15:04:05")
			}
			lines = append(lines, theme.paint(fit(text, width), theme.Muted))
		}
	}
	return strings.Join(limitLines(lines, maxLines), "\n")
}
