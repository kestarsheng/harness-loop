package compaction

import (
	"errors"
	"fmt"
	"strings"
)

var ErrPairingBroken = errors.New("compaction: tool call pairing is broken")

type ToolCall struct {
	ID   string
	Name string
}

type ChatMessage struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
}

type PairingIssue struct {
	Kind   string
	CallID string
}

const (
	PairingOrphanCall   = "orphan_call"
	PairingOrphanResult = "orphan_result"
	PairingDuplicateID  = "duplicate_call"
)

func ValidatePairing(messages []ChatMessage) []PairingIssue {
	calls := map[string]int{}
	results := map[string]int{}
	var issues []PairingIssue

	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			if call.ID == "" {
				issues = append(issues, PairingIssue{Kind: PairingOrphanCall, CallID: ""})
				continue
			}
			calls[call.ID]++
			if calls[call.ID] > 1 {
				issues = append(issues, PairingIssue{Kind: PairingDuplicateID, CallID: call.ID})
			}
		}
		if msg.Role == "tool" {
			if msg.ToolCallID == "" {
				issues = append(issues, PairingIssue{Kind: PairingOrphanResult, CallID: ""})
				continue
			}
			results[msg.ToolCallID]++
		}
	}

	for id := range calls {
		if results[id] == 0 {
			issues = append(issues, PairingIssue{Kind: PairingOrphanCall, CallID: id})
		}
	}
	for id := range results {
		if calls[id] == 0 {
			issues = append(issues, PairingIssue{Kind: PairingOrphanResult, CallID: id})
		}
	}
	return issues
}

func PairingError(issues []PairingIssue) error {
	if len(issues) == 0 {
		return nil
	}
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		parts = append(parts, fmt.Sprintf("%s:%s", issue.Kind, issue.CallID))
	}
	return fmt.Errorf("%w: %s", ErrPairingBroken, strings.Join(parts, ","))
}

func ApplyCompaction(messages []ChatMessage, lines []Line) ([]ChatMessage, error) {
	if len(lines) == 0 {
		return nil, errors.New("compaction: empty jsonl")
	}
	var out []ChatMessage
	for _, msg := range messages {
		if msg.Role == "system" {
			out = append(out, msg)
			break
		}
	}
	block := RenderLinesAsMessage(lines)
	out = append(out, block)
	if issues := ValidatePairing(out); len(issues) > 0 {
		return nil, PairingError(issues)
	}
	return out, nil
}

func RenderLinesAsMessage(lines []Line) ChatMessage {
	var b strings.Builder
	b.WriteString("[compacted context]")
	for _, line := range lines {
		b.WriteString("\n")
		b.WriteString(line.Type)
		b.WriteString(": ")
		b.WriteString(line.Summary)
	}
	return ChatMessage{Role: "system", Content: b.String()}
}
