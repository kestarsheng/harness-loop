package orchestrator

import (
	"context"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/policy"
	"agent-platform/pkg/harnessloop/subtask"
)

func validHandoff() *HandoffContract {
	return &HandoffContract{
		HandoffID:    "handoff-1",
		ParentTaskID: "task-1",
		SubtaskID:    "st-1",
		TenantID:     "tenant-1",
		ProjectID:    "project-1",
		From:         HandoffParty{AgentID: "agent-a", Role: "coder", SubtaskID: "st-1"},
		To:           HandoffParty{AgentID: "agent-b", Role: "verifier"},
		Artifacts: []subtask.ArtifactRef{
			{Path: "src/a/x.go", Hash: "sha256:abc", Size: 42},
		},
		ContextSummary: ContextSummary{
			GoalRef:     "实现 A 模块",
			Done:        "已实现主体逻辑",
			Todo:        "补齐边界测试",
			Constraints: "不得修改公共接口",
			Background:  "父任务处于 c 阶段",
			SourceRefs:  map[string][]string{"files": {"src/a/x.go"}},
		},
		Unfinished: []UnfinishedItem{
			{Item: "补 x_test.go 的边界用例", Reason: "无写权限", Evidence: []string{"file:src/a/x.go"}},
		},
		Constraints: HandoffConstraints{
			Role:              "verifier",
			Tools:             []string{"read_file"},
			WorkspaceSubpaths: []string{"src/a/"},
			TokenQuota:        12000,
			Acceptance:        []AcceptanceSpec{{Type: "test_pass", Expr: "go test ./src/a/..."}},
		},
		CreatedAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}
}

func handoffReq() HandoffRequest {
	return HandoffRequest{ParentTaskID: "task-1", TenantID: "tenant-1", ProjectID: "project-1", Stage: "c"}
}

type fakeTransport struct {
	envelopes []HandoffEnvelope
}

func (f *fakeTransport) SendHandoff(_ context.Context, envelope HandoffEnvelope) error {
	f.envelopes = append(f.envelopes, envelope)
	return nil
}

func TestHandoffCreateFreezesReceiverContext(t *testing.T) {
	emitter := NewMemoryEmitter()
	store := NewMemoryHandoffStore()
	transport := &fakeTransport{}
	service := NewHandoffService(nil, transport, store, emitter)

	record, receiverCtx, err := service.Create(context.Background(), validHandoff(), handoffReq(), []InjectedItem{
		{Source: "runtime.injection:workspace", Content: "workspace root is /repo"},
	})
	if err != nil {
		t.Fatalf("create handoff: %v", err)
	}
	if record.Status != HandoffCreated || record.InputDigest == "" {
		t.Fatalf("unexpected record: %+v", record)
	}
	if receiverCtx.Namespace != "handoff/handoff-1/" {
		t.Fatalf("namespace = %q", receiverCtx.Namespace)
	}
	for _, msg := range receiverCtx.Messages {
		switch {
		case msg.Source == "contract.constraints", msg.Source == "contract.context_summary",
			msg.Source == "contract.unfinished", msg.Source == "runtime.injection:workspace":
		default:
			t.Fatalf("receiver context contains a non-whitelisted source %q", msg.Source)
		}
	}
	if len(transport.envelopes) != 1 {
		t.Fatalf("expected one transport send, got %d", len(transport.envelopes))
	}
	env := transport.envelopes[0]
	if env.HandoffID != "handoff-1" || env.Metadata["parent_task_id"] != "task-1" ||
		env.Metadata["subtask_id"] != "st-1" {
		t.Fatalf("a2a envelope metadata is wrong: %+v", env)
	}
	if len(emitter.Find(EventHandoffCreated)) != 1 {
		t.Fatalf("expected handoff.created event")
	}

	if err := service.Accept(context.Background(), "handoff-1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	record, err = store.Get(context.Background(), "handoff-1")
	if err != nil || record.Status != HandoffAccepted {
		t.Fatalf("record should be accepted: %+v err=%v", record, err)
	}
	if len(emitter.Find(EventHandoffAccepted)) != 1 {
		t.Fatalf("expected handoff.accepted event")
	}
}

func TestHandoffRejectsMissingUnfinished(t *testing.T) {
	service := NewHandoffService(nil, nil, NewMemoryHandoffStore(), NewMemoryEmitter())
	h := validHandoff()
	h.Unfinished = nil
	_, _, err := service.Create(context.Background(), h, handoffReq(), nil)
	assertCode(t, err, CodeHandoffIncomplete)
}

func TestHandoffRejectsIncompleteSummary(t *testing.T) {
	service := NewHandoffService(nil, nil, NewMemoryHandoffStore(), NewMemoryEmitter())
	h := validHandoff()
	h.ContextSummary.Done = ""
	_, _, err := service.Create(context.Background(), h, handoffReq(), nil)
	assertCode(t, err, CodeHandoffIncomplete)
}

func TestHandoffRejectsArtifactWithoutHash(t *testing.T) {
	service := NewHandoffService(nil, nil, NewMemoryHandoffStore(), NewMemoryEmitter())
	h := validHandoff()
	h.Artifacts[0].Hash = ""
	_, _, err := service.Create(context.Background(), h, handoffReq(), nil)
	assertCode(t, err, CodeHandoffIncomplete)
}

func TestHandoffRejectsSenderHistory(t *testing.T) {
	emitter := NewMemoryEmitter()
	service := NewHandoffService(nil, nil, NewMemoryHandoffStore(), emitter)
	_, _, err := service.Create(context.Background(), validHandoff(), handoffReq(), []InjectedItem{
		{Source: "execution_history", Content: "A 的原始消息序列"},
	})
	assertCode(t, err, CodeHandoffHistoryForbid)
	if len(emitter.Find(EventSecurityViolation)) != 1 {
		t.Fatalf("expected security.violation for shared history")
	}
}

func TestHandoffRejectsCrossTenant(t *testing.T) {
	emitter := NewMemoryEmitter()
	service := NewHandoffService(nil, nil, NewMemoryHandoffStore(), emitter)
	h := validHandoff()
	h.TenantID = "tenant-2"
	_, _, err := service.Create(context.Background(), h, handoffReq(), nil)
	assertCode(t, err, CodeCrossTenantDenied)
	if len(emitter.Find(EventSecurityViolation)) != 1 {
		t.Fatalf("expected security.violation for cross-tenant handoff")
	}
}

func TestHandoffReusesRoleToolRegistry(t *testing.T) {
	roles := subtask.NewRegistry(
		fakeCatalog{roles: []string{"verifier"}, tools: []string{"read_file"}},
		policy.DefaultMatrix(),
		fakeTags{},
	)
	service := NewHandoffService(roles, nil, NewMemoryHandoffStore(), NewMemoryEmitter())

	if _, _, err := service.Create(context.Background(), validHandoff(), handoffReq(), nil); err != nil {
		t.Fatalf("read_file should be permitted for verifier: %v", err)
	}

	h := validHandoff()
	h.HandoffID = "handoff-2"
	h.Constraints.Tools = []string{"write_file"}
	_, _, err := service.Create(context.Background(), h, handoffReq(), nil)
	assertCode(t, err, subtask.CodeToolNotPermitted)

	h = validHandoff()
	h.HandoffID = "handoff-3"
	h.To.Role = "coder"
	h.Constraints.Role = "coder"
	_, _, err = service.Create(context.Background(), h, handoffReq(), nil)
	assertCode(t, err, subtask.CodeAgentRoleUndeclared)
}

func TestHandoffMemoryNamespace(t *testing.T) {
	emitter := NewMemoryEmitter()
	h := validHandoff()
	if err := ValidateMemoryAccess(context.Background(), h, "handoff/handoff-1/session", nil); err != nil {
		t.Fatalf("own namespace should pass: %v", err)
	}
	err := ValidateMemoryAccess(context.Background(), h, "handoff/other/session", emitter)
	assertCode(t, err, CodeHandoffHistoryForbid)
	if len(emitter.Find(EventSecurityViolation)) != 1 {
		t.Fatalf("expected security.violation for cross-namespace read")
	}
}

func TestHandoffListIsTenantScoped(t *testing.T) {
	store := NewMemoryHandoffStore()
	service := NewHandoffService(nil, nil, store, NewMemoryEmitter())
	if _, _, err := service.Create(context.Background(), validHandoff(), handoffReq(), nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	own, err := service.ListByParent(context.Background(), "task-1", "tenant-1", "project-1")
	if err != nil || len(own) != 1 {
		t.Fatalf("own tenant should see one handoff: %v %v", own, err)
	}
	other, err := service.ListByParent(context.Background(), "task-1", "tenant-2", "project-1")
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-tenant query must return empty: %v %v", other, err)
	}
}
