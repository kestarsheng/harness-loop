package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestResolverLayerPriority(t *testing.T) {
	r := NewResolver(nil)
	root := t.TempDir()
	req := Request{
		Tool:          "write_file",
		Stage:         "c",
		Args:          ToolArgs{Paths: []string{"a.go"}},
		WorkspaceRoot: root,
		TenantID:      "t-1",
		Tenant:        &PolicySet{DenyTools: []string{"write_file"}},
		Role:          &PolicySet{AllowTools: []string{"write_file"}},
		Task:          &PolicySet{AllowTools: []string{"write_file"}},
	}
	d := r.Decide(context.Background(), req)
	if d.Allowed || d.Layer != LayerTenant || d.RuleID != "tenant.deny" {
		t.Fatalf("tenant deny must win over task allow: %+v", d)
	}
	if d.Effect != EffectDeny {
		t.Fatalf("effect = %q, want deny", d.Effect)
	}
}

func TestResolverSystemHardLimitNotOverridable(t *testing.T) {
	r := NewResolver(nil)
	req := Request{
		Tool:          "write_file",
		Stage:         "c",
		Args:          ToolArgs{Paths: []string{"config/harness_loop/approval.yaml"}},
		WorkspaceRoot: t.TempDir(),
		Tenant:        &PolicySet{AllowTools: []string{"write_file"}},
		Role:          &PolicySet{AllowTools: []string{"write_file"}},
		Task:          &PolicySet{AllowTools: []string{"write_file"}},
	}
	d := r.Decide(context.Background(), req)
	if d.Allowed || d.Layer != LayerSystem || d.RuleID != "policy.self_elevation" {
		t.Fatalf("system hard limit must not be overridable: %+v", d)
	}
}

func TestResolverDeclaredLayersMustAllow(t *testing.T) {
	r := NewResolver(nil)
	root := t.TempDir()
	base := Request{
		Tool:          "read_file",
		Stage:         "c",
		Args:          ToolArgs{Paths: []string{"a.go"}},
		WorkspaceRoot: root,
	}

	req := base
	req.Role = &PolicySet{AllowTools: []string{"read_file"}}
	if d := r.Decide(context.Background(), req); !d.Allowed || d.Layer != LayerRole {
		t.Fatalf("role allow must permit: %+v", d)
	}

	req = base
	req.Role = &PolicySet{AllowTools: []string{"write_file"}}
	if d := r.Decide(context.Background(), req); d.Allowed || d.Layer != LayerRole || d.RuleID != "policy.role.no_match" {
		t.Fatalf("declared role without allow must deny: %+v", d)
	}

	req = base
	req.Task = &PolicySet{AllowTools: []string{"write_file"}}
	if d := r.Decide(context.Background(), req); d.Allowed || d.Layer != LayerTask || d.RuleID != "policy.task.no_match" {
		t.Fatalf("declared task without allow must deny: %+v", d)
	}

	req = base
	req.Role = &PolicySet{AllowTools: []string{"read_file"}}
	req.Task = &PolicySet{AllowTools: []string{"read_file"}}
	if d := r.Decide(context.Background(), req); !d.Allowed {
		t.Fatalf("role and task both allow must permit: %+v", d)
	}
}

func TestResolverRequiresApproval(t *testing.T) {
	r := NewResolver(nil)
	req := Request{
		Tool:          "run_command",
		Stage:         "c",
		Args:          ToolArgs{Command: []string{"go", "test", "./..."}},
		WorkspaceRoot: t.TempDir(),
		Tenant: &PolicySet{Rules: []Rule{{
			ID:               "tenant.approval.remote_test",
			Tool:             "run_command",
			Effect:           EffectAllow,
			RequiresApproval: true,
		}}},
	}
	d := r.Decide(context.Background(), req)
	if d.Allowed || !d.RequiresApproval || d.Effect != EffectRequireApproval {
		t.Fatalf("requires_approval must not allow inline: %+v", d)
	}
	if d.RuleID != "tenant.approval.remote_test" || d.Code != CodeApprovalRequired {
		t.Fatalf("approval decision must be traceable: %+v", d)
	}
}

func TestResolverCacheInvalidation(t *testing.T) {
	r := NewResolver(nil)
	r.SetCache(NewDecisionCache(100, time.Minute))
	root := t.TempDir()
	req := Request{
		Tool:          "read_file",
		Stage:         "c",
		Args:          ToolArgs{Paths: []string{"a.go"}},
		WorkspaceRoot: root,
		TenantID:      "t-1",
		Tenant:        &PolicySet{AllowTools: []string{"read_file"}},
	}

	first := r.Decide(context.Background(), req)
	if !first.Allowed || first.CacheHit {
		t.Fatalf("first decision must be a miss and allow: %+v", first)
	}
	second := r.Decide(context.Background(), req)
	if !second.Allowed || !second.CacheHit {
		t.Fatalf("second decision must be a cache hit: %+v", second)
	}

	req.Tenant = &PolicySet{DenyTools: []string{"read_file"}}
	r.BumpPolicyVersion()
	third := r.Decide(context.Background(), req)
	if third.Allowed || third.CacheHit || third.Layer != LayerTenant {
		t.Fatalf("policy change must take effect on the next call: %+v", third)
	}
}

func TestResolverWriteToolNotCached(t *testing.T) {
	r := NewResolver(nil)
	req := Request{
		Tool:          "write_file",
		Stage:         "c",
		Args:          ToolArgs{Paths: []string{"a.go"}},
		WorkspaceRoot: t.TempDir(),
	}
	for i := 0; i < 3; i++ {
		d := r.Decide(context.Background(), req)
		if !d.Allowed || d.CacheHit {
			t.Fatalf("write tool decision must never be cached: %+v", d)
		}
	}
}

func TestResolverTenantTightening(t *testing.T) {
	r := NewResolver(nil)
	platform := PolicySet{
		DenyTools: []string{"delete"},
		Rules:     []Rule{{ID: "system.protected_path", Tool: "write_file", Effect: EffectDeny}},
	}

	tightened := PolicySet{DenyTools: []string{"delete", "network"}}
	if err := r.ValidateTenantPolicy("t-1", platform, tightened); err != nil {
		t.Fatalf("tightening must be allowed: %v", err)
	}

	loosened := PolicySet{AllowTools: []string{"delete"}}
	err := r.ValidateTenantPolicy("t-1", platform, loosened)
	if !errors.Is(err, ErrPolicyLoosening) || !strings.Contains(err.Error(), CodePolicyLoosening) {
		t.Fatalf("loosening must be rejected: %v", err)
	}

	loosenedRules := PolicySet{Rules: []Rule{{ID: "tenant.allow_write", Tool: "write_file", Effect: EffectAllow}}}
	if err := r.ValidateTenantPolicy("t-1", platform, loosenedRules); err == nil {
		t.Fatal("rule-level loosening must be rejected")
	}

	r.SetTenantLooseningApprover(func(tenantID, policyHash string) (bool, string) {
		return tenantID == "t-1" && policyHash != "", "approval-1"
	})
	if err := r.ValidateTenantPolicy("t-1", platform, loosened); err != nil {
		t.Fatalf("platform-approved loosening must pass: %v", err)
	}
	if err := r.ValidateTenantPolicy("t-2", platform, loosened); err == nil {
		t.Fatal("approval must be scoped to the tenant")
	}
}

func TestResolverAuditAndEvents(t *testing.T) {
	sink := &MemoryAuditSink{}
	r := NewResolver(nil)
	r.SetAuditSink(sink)
	root := t.TempDir()

	deniedReq := Request{
		Tool:          "write_file",
		Stage:         "a",
		Args:          ToolArgs{Paths: []string{"a.go"}},
		WorkspaceRoot: root,
		TaskID:        "task-1",
	}
	denyDecision := r.Decide(context.Background(), deniedReq)
	if denyDecision.Allowed {
		t.Fatalf("write in stage a must be denied: %+v", denyDecision)
	}
	denyEvents := denyDecision.Events("call-1", deniedReq)
	if len(denyEvents) != 1 || denyEvents[0].Type != "tool.denied" {
		t.Fatalf("deny must only emit tool.denied: %+v", denyEvents)
	}

	allowedReq := Request{
		Tool:          "read_file",
		Stage:         "c",
		Args:          ToolArgs{Paths: []string{"a.go"}},
		WorkspaceRoot: root,
		TaskID:        "task-1",
	}
	allowDecision := r.Decide(context.Background(), allowedReq)
	if !allowDecision.Allowed {
		t.Fatalf("read must be allowed: %+v", allowDecision)
	}
	allowEvents := allowDecision.Events("call-2", allowedReq)
	if len(allowEvents) != 2 || allowEvents[0].Type != "tool.started" || allowEvents[1].Type != "tool.result" {
		t.Fatalf("allow must emit started+result: %+v", allowEvents)
	}

	records := sink.Records()
	if len(records) != 2 {
		t.Fatalf("audit records = %d, want 2", len(records))
	}
	if records[0].Decision != EffectDeny || records[0].RuleID != denyDecision.RuleID {
		t.Fatalf("audit and event must share the same decision: %+v vs %+v", records[0], denyDecision)
	}
	if denyEvents[0].RuleID != records[0].RuleID || denyEvents[0].Decision != records[0].Decision {
		t.Fatalf("event/audit mismatch: %+v vs %+v", denyEvents[0], records[0])
	}

	stats := sink.ByRule()
	byRule := make(map[string]RuleStats, len(stats))
	for _, s := range stats {
		byRule[s.RuleID] = s
	}
	denyStats, ok := byRule[denyDecision.RuleID]
	if !ok || denyStats.Total != 1 || denyStats.Denied != 1 || denyStats.DenyRate != 1 {
		t.Fatalf("rule aggregation for %s = %+v", denyDecision.RuleID, denyStats)
	}
	allowStats, ok := byRule[allowDecision.RuleID]
	if !ok || allowStats.Total != 1 || allowStats.Denied != 0 || allowStats.DenyRate != 0 {
		t.Fatalf("rule aggregation for %s = %+v", allowDecision.RuleID, allowStats)
	}
}

func TestResolverDenyIsDeterministic(t *testing.T) {
	r := NewResolver(nil)
	req := Request{
		Tool:          "write_file",
		Stage:         "b",
		Args:          ToolArgs{Paths: []string{"a.go"}},
		WorkspaceRoot: t.TempDir(),
	}
	first := r.Decide(context.Background(), req)
	for i := 0; i < 5; i++ {
		next := r.Decide(context.Background(), req)
		if next.RuleID != first.RuleID || next.Allowed != first.Allowed {
			t.Fatalf("retry changed the decision: %+v vs %+v", next, first)
		}
	}
}

func TestResolverPerformance(t *testing.T) {
	cache := NewDecisionCache(10000, time.Minute)
	r := NewResolver(nil)
	r.SetCache(cache)
	root := t.TempDir()
	req := Request{
		Tool:          "read_file",
		Stage:         "c",
		Args:          ToolArgs{Paths: []string{"stable.go"}},
		WorkspaceRoot: root,
	}
	r.Decide(context.Background(), req)

	var hit, miss []time.Duration
	for i := 0; i < 1000; i++ {
		if i%10 >= 7 {
			req.Args.Paths = []string{fmt.Sprintf("file-%d.go", i)}
		} else {
			req.Args.Paths = []string{"stable.go"}
		}
		start := time.Now()
		d := r.Decide(context.Background(), req)
		elapsed := time.Since(start)
		if d.CacheHit {
			hit = append(hit, elapsed)
		} else {
			miss = append(miss, elapsed)
		}
	}
	if len(hit) == 0 || len(miss) == 0 {
		t.Fatalf("expected both cache hits and misses, got %d/%d", len(hit), len(miss))
	}
	if p95 := percentile95(hit); p95 > 5*time.Millisecond {
		t.Fatalf("cache hit P95 = %s, want <= 5ms", p95)
	}
	if p95 := percentile95(miss); p95 > 20*time.Millisecond {
		t.Fatalf("cache miss P95 = %s, want <= 20ms", p95)
	}
}

func percentile95(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted))*0.95 + 0.9999)
	if idx > 0 {
		idx--
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
