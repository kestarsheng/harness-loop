package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type AuditRecord struct {
	AuditID        string
	TS             time.Time
	TenantID       string
	ProjectID      string
	TaskID         string
	SubtaskID      string
	HandoffID      string
	Stage          string
	Attempt        int
	Role           string
	AgentID        string
	Tool           string
	Tags           []string
	Decision       Effect
	RuleID         string
	Layer          Layer
	Code           string
	ArgsDigest     string
	ArgsSummary    map[string]any
	PathNormalized string
	DurationUS     int64
}

type AuditSink interface {
	Write(ctx context.Context, rec AuditRecord) error
}

type RuleStats struct {
	RuleID           string
	Total            int
	Denied           int
	ApprovalRequired int
	DenyRate         float64
}

type MemoryAuditSink struct {
	mu      sync.Mutex
	records []AuditRecord
	next    int64
}

func (s *MemoryAuditSink) Write(ctx context.Context, rec AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.AuditID == "" {
		s.next++
		rec.AuditID = fmt.Sprintf("audit-%d", s.next)
	}
	s.records = append(s.records, rec)
	return nil
}

func (s *MemoryAuditSink) Records() []AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditRecord, len(s.records))
	copy(out, s.records)
	return out
}

func (s *MemoryAuditSink) ByRule() []RuleStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	byRule := make(map[string]*RuleStats)
	for _, rec := range s.records {
		stats, ok := byRule[rec.RuleID]
		if !ok {
			stats = &RuleStats{RuleID: rec.RuleID}
			byRule[rec.RuleID] = stats
		}
		stats.Total++
		switch rec.Decision {
		case EffectDeny:
			stats.Denied++
		case EffectRequireApproval:
			stats.ApprovalRequired++
		}
	}

	out := make([]RuleStats, 0, len(byRule))
	for _, stats := range byRule {
		if stats.Total > 0 {
			stats.DenyRate = float64(stats.Denied) / float64(stats.Total)
		}
		out = append(out, *stats)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuleID < out[j].RuleID })
	return out
}

func (d Decision) EffectiveEffect() Effect {
	if d.Effect != "" {
		return d.Effect
	}
	if d.RequiresApproval {
		return EffectRequireApproval
	}
	if d.Allowed {
		return EffectAllow
	}
	return EffectDeny
}

func (d Decision) AuditRecord(req Request, tags []string, duration time.Duration) AuditRecord {
	return AuditRecord{
		TS:             time.Now(),
		TenantID:       req.TenantID,
		ProjectID:      req.ProjectID,
		TaskID:         req.TaskID,
		SubtaskID:      req.SubtaskID,
		Stage:          req.Stage,
		Attempt:        req.Attempt,
		Role:           req.RoleName,
		AgentID:        req.AgentID,
		Tool:           req.Tool,
		Tags:           append([]string(nil), tags...),
		Decision:       d.EffectiveEffect(),
		RuleID:         d.RuleID,
		Layer:          d.Layer,
		Code:           d.Code,
		ArgsDigest:     "sha256:" + argsDigest(req.Args),
		ArgsSummary:    argsSummary(req.Args),
		PathNormalized: normalizedPath(req.Args.Paths),
		DurationUS:     duration.Microseconds(),
	}
}

type DecisionEvent struct {
	Type      string
	CallID    string
	TaskID    string
	SubtaskID string
	Stage     string
	Tool      string
	Decision  Effect
	RuleID    string
	Layer     Layer
	Code      string
}

func (d Decision) Events(callID string, req Request) []DecisionEvent {
	base := DecisionEvent{
		CallID:    callID,
		TaskID:    req.TaskID,
		SubtaskID: req.SubtaskID,
		Stage:     req.Stage,
		Tool:      req.Tool,
		Decision:  d.EffectiveEffect(),
		RuleID:    d.RuleID,
		Layer:     d.Layer,
		Code:      d.Code,
	}
	switch d.EffectiveEffect() {
	case EffectAllow:
		started := base
		started.Type = "tool.started"
		result := base
		result.Type = "tool.result"
		return []DecisionEvent{started, result}
	case EffectRequireApproval:
		event := base
		event.Type = "approval.requested"
		return []DecisionEvent{event}
	default:
		denied := base
		denied.Type = "tool.denied"
		return []DecisionEvent{denied}
	}
}

func argsSummary(args ToolArgs) map[string]any {
	summary := map[string]any{}
	if len(args.Paths) > 0 {
		summary["paths"] = append([]string(nil), args.Paths...)
	}
	if len(args.Command) > 0 {
		cmd := args.Command
		truncated := false
		if len(cmd) > 3 {
			cmd = cmd[:3]
			truncated = true
		}
		out := make([]string, 0, len(cmd))
		for _, part := range cmd {
			if len(part) > 64 {
				part = part[:64]
				truncated = true
			}
			out = append(out, part)
		}
		summary["command"] = out
		if truncated {
			summary["command_truncated"] = true
		}
	}
	if args.NetworkMethod != "" {
		summary["network_method"] = args.NetworkMethod
	}
	if args.EstimatedCost > 0 {
		summary["estimated_cost"] = args.EstimatedCost
	}
	return summary
}

func normalizedPath(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return filepath.Clean(filepath.FromSlash(paths[0]))
}

func ArgumentsDigest(args ToolArgs) string {
	payload, err := json.Marshal(map[string]any{
		"paths":          args.Paths,
		"command":        args.Command,
		"network_method": args.NetworkMethod,
		"estimated_cost": args.EstimatedCost,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
