package service

import (
	"context"
	"encoding/json"
	"testing"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/policy"
)

type auditRecorder struct {
	records []policy.AuditRecord
}

func (a *auditRecorder) AppendToolDecision(_ context.Context, rec policy.AuditRecord) error {
	a.records = append(a.records, rec)
	return nil
}

func newTestPolicyBridge(enabled bool, engine *policy.Engine) (*PolicyBridge, *auditRecorder, *modelEventSink) {
	writer := &auditRecorder{}
	sink := &modelEventSink{}
	pub := event.NewPublisher(sink, nil, nil)
	emitter := orchestrator.NewPublisherEmitter(pub, orchestrator.EmitContext{})
	return NewPolicyBridge(enabled, engine, writer, emitter, nil), writer, sink
}

func policyCall(stage, tool, root string) ToolCallInput {
	return ToolCallInput{
		TenantID: "tenant-a", ProjectID: "proj-a", TaskID: "task-1",
		Stage: stage, Attempt: 1, AgentID: "agent-1", WorkspaceRoot: root, Tool: tool,
		Paths: []string{"a.go"},
	}
}

func (s *modelEventSink) find(typ event.EventType) *event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.events {
		if ev.EventType == typ {
			return ev
		}
	}
	return nil
}

func TestPolicyBridgeDisabledMatchesEngine(t *testing.T) {
	engine := policy.NewEngine()
	bridge, writer, sink := newTestPolicyBridge(false, engine)
	root := t.TempDir()
	allowed := bridge.Preflight(context.Background(), policyCall("c", "write_file", root))
	if !allowed.Allowed {
		t.Fatalf("write in c must be allowed: %+v", allowed)
	}
	denied := bridge.Preflight(context.Background(), policyCall("a", "write_file", root))
	if denied.Allowed || denied.Code != policy.CodeToolDenied {
		t.Fatalf("write in a must be denied: %+v", denied)
	}
	if len(writer.records) != 0 || len(sink.types()) != 0 {
		t.Fatalf("disabled bridge must not write audit/events: %+v %+v", writer.records, sink.types())
	}
}

func TestPolicyBridgeAuditAndDeniedEventShareRuleID(t *testing.T) {
	engine := policy.NewEngine()
	bridge, writer, sink := newTestPolicyBridge(true, engine)
	root := t.TempDir()
	res := bridge.Preflight(context.Background(), policyCall("a", "write_file", root))
	if res.Allowed || res.RuleID == "" || res.Layer == "" {
		t.Fatalf("decision = %+v", res)
	}
	if len(writer.records) != 1 {
		t.Fatalf("audit records = %+v", writer.records)
	}
	rec := writer.records[0]
	if rec.Decision != policy.EffectDeny || rec.RuleID != res.RuleID || rec.Layer != res.Layer {
		t.Fatalf("audit/decision mismatch: %+v vs %+v", rec, res)
	}
	ev := sink.find(event.ToolDenied)
	if ev == nil {
		t.Fatalf("tool.denied event missing: %v", sink.types())
	}
	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["rule_id"] != rec.RuleID || payload["layer"] != string(rec.Layer) {
		t.Fatalf("event/audit rule mismatch: %+v vs %+v", payload, rec)
	}
}

func TestPolicyBridgeCrossTenantIsSecurityViolation(t *testing.T) {
	engine := policy.NewEngine()
	bridge, writer, sink := newTestPolicyBridge(true, engine)
	bridge.SetTaskLookup(func(context.Context, string) (string, string, error) {
		return "tenant-b", "proj-b", nil
	})
	res := bridge.Preflight(context.Background(), policyCall("c", "write_file", t.TempDir()))
	if res.Allowed || res.Code != "E_CROSS_TENANT_DENIED" {
		t.Fatalf("cross tenant must be denied: %+v", res)
	}
	if sink.find(event.SecurityViolation) == nil {
		t.Fatalf("security.violation event missing: %v", sink.types())
	}
	if len(writer.records) == 0 || writer.records[0].RuleID != "security.cross_tenant" {
		t.Fatalf("cross tenant audit missing: %+v", writer.records)
	}
}

func TestPolicyBridgeRequiresApprovalStaysPending(t *testing.T) {
	engine := policy.NewEngine()
	bridge, writer, sink := newTestPolicyBridge(true, engine)
	call := policyCall("c", "write_file", t.TempDir())
	call.TenantPolicy = &policy.PolicySet{Rules: []policy.Rule{{
		ID: "tenant.write.approval", Tool: "write_file", Effect: policy.EffectRequireApproval, RequiresApproval: true,
	}}}
	res := bridge.Preflight(context.Background(), call)
	if res.Allowed || !res.RequiresApproval {
		t.Fatalf("approval call must not be allowed: %+v", res)
	}
	if res.Code != policy.CodeApprovalUnavailable {
		t.Fatalf("unavailable gate must hold as pending, code=%s", res.Code)
	}
	if res.ApprovalID != "" {
		t.Fatalf("no approval id without a service: %s", res.ApprovalID)
	}
	if sink.find(event.ApprovalRequested) == nil {
		t.Fatalf("approval.requested event missing: %v", sink.types())
	}
	if len(writer.records) == 0 || writer.records[0].Decision != policy.EffectRequireApproval {
		t.Fatalf("approval audit missing: %+v", writer.records)
	}
}

func TestPolicyBridgeInvalidateCache(t *testing.T) {
	engine := policy.NewEngine()
	bridge, _, _ := newTestPolicyBridge(true, engine)
	root := t.TempDir()
	first := bridge.Preflight(context.Background(), policyCall("c", "read_file", root))
	second := bridge.Preflight(context.Background(), policyCall("c", "read_file", root))
	if first.CacheHit || !second.CacheHit {
		t.Fatalf("cache behaviour: first=%v second=%v", first.CacheHit, second.CacheHit)
	}
	bridge.Invalidate()
	third := bridge.Preflight(context.Background(), policyCall("c", "read_file", root))
	if third.CacheHit {
		t.Fatal("cache must be invalidated after policy/approval change")
	}
}

func TestPolicyBridgeAuthorizeToolDeniesWithoutRetry(t *testing.T) {
	engine := policy.NewEngine()
	bridge, _, _ := newTestPolicyBridge(true, engine)
	err := bridge.AuthorizeTool(context.Background(), policyCall("a", "write_file", t.TempDir()))
	if err == nil {
		t.Fatal("denied tool must return an error")
	}
}
