package harnessloop

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func fieldByName(t *testing.T, md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	t.Helper()
	fd := md.Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		t.Fatalf("%s: field %q missing", md.FullName(), name)
	}
	return fd
}

func TestHarnessEventRequiredFields(t *testing.T) {
	md := (&HarnessEvent{}).ProtoReflect().Descriptor()
	required := map[string]protoreflect.Kind{
		"event_id":       protoreflect.StringKind,
		"event_type":     protoreflect.StringKind,
		"schema_version": protoreflect.StringKind,
		"task_id":        protoreflect.StringKind,
		"seq":            protoreflect.Int64Kind,
		"timestamp_ms":   protoreflect.Int64Kind,
		"severity":       protoreflect.StringKind,
		"tenant_id":      protoreflect.StringKind,
		"project_id":     protoreflect.StringKind,
		"payload":        protoreflect.BytesKind,
	}
	for name, kind := range required {
		if got := fieldByName(t, md, name).Kind(); got != kind {
			t.Fatalf("HarnessEvent.%s kind = %v, want %v", name, got, kind)
		}
	}
}

func TestHarnessEventOptionalFields(t *testing.T) {
	md := (&HarnessEvent{}).ProtoReflect().Descriptor()
	optional := []string{
		"trace_id", "span_id", "stage", "attempt", "agent_id", "payload_ref",
		"parent_task_id", "subtask_id", "handoff_id", "dag_id", "dag_node_id",
	}
	for _, name := range optional {
		fd := fieldByName(t, md, name)
		if name == "stage" || name == "attempt" {
			if !fd.HasPresence() {
				t.Fatalf("HarnessEvent.%s must use optional (explicit presence)", name)
			}
		}
	}
}

func TestServiceMethods(t *testing.T) {
	file := File_proto_harnessloop_harnessloop_proto
	svc := file.Services().ByName("HarnessLoopService")
	if svc == nil {
		t.Fatal("HarnessLoopService missing")
	}
	want := []string{
		"CreateTask", "GetTask", "ListTasks", "PauseTask", "ResumeTask", "AbortTask",
		"ListEvents", "WatchEvents", "ListCheckpoints", "RestoreFromCheckpoint",
		"ListModels", "ResolveModel", "PreflightTool",
	}
	got := make(map[string]bool, svc.Methods().Len())
	for i := 0; i < svc.Methods().Len(); i++ {
		got[string(svc.Methods().Get(i).Name())] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("rpc %s missing", name)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d rpcs, got %d", len(want), len(got))
	}
	if m := svc.Methods().ByName("WatchEvents"); m == nil || !m.IsStreamingServer() {
		t.Fatal("WatchEvents must be server-streaming")
	}
}

func TestBudgetHasWallClock(t *testing.T) {
	md := (&Budget{}).ProtoReflect().Descriptor()
	if f := fieldByName(t, md, "wall_clock_seconds"); f.Kind() != protoreflect.Int64Kind {
		t.Fatalf("wall_clock_seconds kind = %v", f.Kind())
	}
}

func TestCriterionFields(t *testing.T) {
	md := (&Criterion{}).ProtoReflect().Descriptor()
	for _, name := range []string{"criterion_id", "type", "expr", "required"} {
		fieldByName(t, md, name)
	}
}

func TestTaskViewCoreFields(t *testing.T) {
	md := (&TaskView{}).ProtoReflect().Descriptor()
	for _, name := range []string{
		"task_id", "tenant_id", "project_id", "workspace_root", "goal", "goal_hash",
		"current_stage", "status", "terminal_reason", "attempt_no_by_stage",
		"budget", "consumed_tokens", "consumed_cost", "fsm_enabled", "version",
	} {
		fieldByName(t, md, name)
	}
}
