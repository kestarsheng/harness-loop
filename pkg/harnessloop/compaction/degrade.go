package compaction

import (
	"fmt"
	"strings"

	"agent-platform/pkg/llm"
)

const (
	DegradeLevelL1 = "L1"
	DegradeLevelL2 = "L2"

	DegradeReasonTemplateFailed = "template_failed"
	DegradeReasonPairing        = "pairing"
	DegradeReasonMissingElement = "missing_element"
	DegradeReasonEmergency      = "emergency"
)

type DegradeLimits struct {
	KeepRecent      int
	SystemChars     int
	RecentChars     int
	OldChars        int
	EmergencyRecent int
}

func DefaultDegradeLimits() DegradeLimits {
	return DegradeLimits{KeepRecent: 8, SystemChars: 12000, RecentChars: 6000, OldChars: 1000, EmergencyRecent: 2}
}

func (l DegradeLimits) normalized() DegradeLimits {
	out := l
	if out.KeepRecent <= 0 {
		out.KeepRecent = 8
	}
	if out.SystemChars <= 0 {
		out.SystemChars = 12000
	}
	if out.RecentChars <= 0 {
		out.RecentChars = 6000
	}
	if out.OldChars <= 0 {
		out.OldChars = 1000
	}
	if out.EmergencyRecent <= 0 {
		out.EmergencyRecent = 2
	}
	return out
}

type DegradeResult struct {
	Messages     []ChatMessage
	Lines        []Line
	Level        string
	Reason       string
	TokensBefore int
	TokensAfter  int
	Ratio        float64
}

func PlaceholderLines(h History) []Line {
	return []Line{
		{Type: LineGoal, Phase: h.Phase, Summary: firstNonEmpty(h.Goal, "(goal unavailable)"), SourceRefs: []string{"task:goal"}},
		{Type: LineDone, Phase: h.Phase, Summary: joinOr(h.Done, "(no confirmed progress)"), SourceRefs: []string{"event:stage"}},
		{Type: LineTodo, Phase: h.Phase, Summary: joinOr(h.Todo, "(todo unavailable)"), SourceRefs: []string{"plan:current"}},
		{Type: LineConstraints, Phase: h.Phase, Summary: joinOr(h.Constraints, "(no active constraint)"), SourceRefs: []string{"memory:constraint"}},
		{Type: LineBackground, Phase: h.Phase, Summary: joinOr(h.Background, "(no new background)"), SourceRefs: []string{"context:history"}},
	}
}

func firstNonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func joinOr(items []string, fallback string) string {
	if len(items) == 0 {
		return fallback
	}
	return strings.Join(items, "; ")
}

func DegradeL1(messages []ChatMessage, h History, limits DegradeLimits) (DegradeResult, error) {
	limits = limits.normalized()
	before := estimateMessages(messages)
	lines := PlaceholderLines(h)

	var out []ChatMessage
	systemSeen := false
	for _, msg := range messages {
		if msg.Role == "system" && !systemSeen {
			msg.Content = truncateRunes(msg.Content, limits.SystemChars)
			out = append(out, msg)
			systemSeen = true
			break
		}
	}
	out = append(out, ChatMessage{Role: "user", Content: "goal: " + firstNonEmpty(h.Goal, "(unavailable)")})
	out = append(out, RenderLinesAsMessage(lines))

	recent := tailMessages(messages, limits.KeepRecent)
	for i, msg := range recent {
		limit := limits.OldChars
		if i >= len(recent)/2 {
			limit = limits.RecentChars
		}
		msg.Content = truncateRunes(msg.Content, limit)
		out = append(out, msg)
	}

	if issues := ValidatePairing(out); len(issues) > 0 {
		return DegradeResult{}, PairingError(issues)
	}
	after := estimateMessages(out)
	return DegradeResult{
		Messages: out, Lines: lines, Level: DegradeLevelL1, Reason: DegradeReasonTemplateFailed,
		TokensBefore: before, TokensAfter: after, Ratio: ratio(before, after),
	}, nil
}

func DegradeL2(messages []ChatMessage, h History, limits DegradeLimits) (DegradeResult, error) {
	limits = limits.normalized()
	before := estimateMessages(messages)
	lines := PlaceholderLines(h)

	var out []ChatMessage
	for _, msg := range messages {
		if msg.Role == "system" {
			out = append(out, ChatMessage{Role: "system", Content: truncateRunes(msg.Content, limits.SystemChars)})
			break
		}
	}
	out = append(out, ChatMessage{Role: "user", Content: "goal: " + firstNonEmpty(h.Goal, "(unavailable)")})
	out = append(out, RenderLinesAsMessage(lines))
	out = append(out, tailMessages(messages, limits.EmergencyRecent)...)

	if issues := ValidatePairing(out); len(issues) > 0 {
		// Emergency drop: remove tool-only remnants to keep pairing valid.
		out = dropUnpairedToolMessages(out)
		if issues = ValidatePairing(out); len(issues) > 0 {
			return DegradeResult{}, PairingError(issues)
		}
	}
	after := estimateMessages(out)
	return DegradeResult{
		Messages: out, Lines: lines, Level: DegradeLevelL2, Reason: DegradeReasonEmergency,
		TokensBefore: before, TokensAfter: after, Ratio: ratio(before, after),
	}, nil
}

func tailMessages(messages []ChatMessage, n int) []ChatMessage {
	if n <= 0 || len(messages) == 0 {
		return nil
	}
	start := len(messages) - n
	if start < 0 {
		start = 0
	}
	tail := messages[start:]
	if issues := ValidatePairing(tail); len(issues) > 0 {
		tail = dropUnpairedToolMessages(tail)
		if issues := ValidatePairing(tail); len(issues) > 0 {
			var keep []ChatMessage
			for _, msg := range tail {
				if msg.Role != "tool" && len(msg.ToolCalls) == 0 {
					keep = append(keep, msg)
				}
			}
			tail = keep
		}
	}
	out := make([]ChatMessage, len(tail))
	copy(out, tail)
	return out
}

func dropUnpairedToolMessages(messages []ChatMessage) []ChatMessage {
	issues := ValidatePairing(messages)
	orphanCalls := map[string]bool{}
	orphanResults := map[string]bool{}
	for _, issue := range issues {
		switch issue.Kind {
		case PairingOrphanCall:
			orphanCalls[issue.CallID] = true
		case PairingOrphanResult:
			orphanResults[issue.CallID] = true
		}
	}
	var out []ChatMessage
	for _, msg := range messages {
		if msg.Role == "tool" && orphanResults[msg.ToolCallID] {
			continue
		}
		if len(msg.ToolCalls) > 0 {
			hasOrphan := false
			for _, call := range msg.ToolCalls {
				if orphanCalls[call.ID] {
					hasOrphan = true
				}
			}
			if hasOrphan {
				continue
			}
		}
		out = append(out, msg)
	}
	return out
}

func estimateMessages(messages []ChatMessage) int {
	total := 0
	for _, msg := range messages {
		total += llm.EstimateTokens(msg.Content)
		total += len(msg.ToolCalls) * 8
	}
	return total
}

func ratio(before, after int) float64 {
	if before <= 0 {
		return 0
	}
	return float64(after) / float64(before)
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…[truncated]"
}

func ValidateDegradeResult(result DegradeResult) error {
	if result.Level != DegradeLevelL1 && result.Level != DegradeLevelL2 {
		return fmt.Errorf("compaction: invalid degrade level %q", result.Level)
	}
	if result.Reason == "" {
		return fmt.Errorf("compaction: degrade reason is required")
	}
	if err := ValidateFiveElements(result.Lines); err != nil {
		return err
	}
	if issues := ValidatePairing(result.Messages); len(issues) > 0 {
		return PairingError(issues)
	}
	return nil
}
