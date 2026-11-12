package subtask

import (
	"context"
	"errors"
	"testing"

	"agent-platform/pkg/harnessloop/policy"
)

type fakeCatalog struct {
	roles map[string][]string
	tools map[string][]string
}

func (f *fakeCatalog) AgentRoles(_ context.Context, agentID string) ([]string, error) {
	return f.roles[agentID], nil
}

func (f *fakeCatalog) AgentTools(_ context.Context, agentID string) ([]string, error) {
	return f.tools[agentID], nil
}

type fakeTags struct {
	tags map[string][]string
}

func (f *fakeTags) ToolPermissionTags(_ context.Context, tool string) ([]string, error) {
	return f.tags[tool], nil
}

func newTestRegistry() *Registry {
	catalog := &fakeCatalog{
		roles: map[string][]string{
			"coder":    {"coder"},
			"verifier": {"verifier"},
			"orphan":   nil,
		},
		tools: map[string][]string{
			"coder":    {"read_file", "write_file", "edit_file", "deploy_service"},
			"verifier": {"read_file", "write_file"},
			"orphan":   {"read_file"},
		},
	}
	tags := &fakeTags{tags: map[string][]string{
		"read_file":  {"fs.read"},
		"write_file": {"fs.write"},
		"go":         {"exec"},
	}}
	return NewRegistry(catalog, policy.DefaultMatrix(), tags)
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", want)
	}
	var ce *CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CodedError %s, got %T: %v", want, err, err)
	}
	if ce.Code != want {
		t.Fatalf("expected code %s, got %s (%v)", want, ce.Code, err)
	}
}

func TestRoleToolsResolveGrantsIntersectionOnly(t *testing.T) {
	reg := newTestRegistry()
	binding, err := reg.Resolve(context.Background(), "coder", "c")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(binding.Roles) != 1 || binding.Roles[0] != "coder" {
		t.Fatalf("unexpected roles: %v", binding.Roles)
	}
	granted := map[string]bool{}
	for _, tool := range binding.GrantedTools {
		granted[tool] = true
	}
	if !granted["read_file"] || !granted["write_file"] {
		t.Fatalf("expected read/write tools granted, got %v", binding.GrantedTools)
	}
	if granted["deploy_service"] {
		t.Fatalf("deploy_service must not be granted in stage c")
	}
	if len(binding.DeniedTools) != 1 || binding.DeniedTools[0] != "deploy_service" {
		t.Fatalf("expected deploy_service denied, got %v", binding.DeniedTools)
	}
	if len(binding.MissingTags) != 1 || binding.MissingTags[0] != "edit_file" {
		t.Fatalf("expected edit_file missing tags, got %v", binding.MissingTags)
	}
}

func TestRoleToolsVerifierCannotWrite(t *testing.T) {
	reg := newTestRegistry()
	if err := reg.CheckTool(context.Background(), "verifier", "d", "write_file"); err == nil {
		t.Fatal("verifier write in stage d must be rejected")
	} else {
		assertCode(t, err, CodeToolNotPermitted)
	}
	if err := reg.CheckTool(context.Background(), "verifier", "d", "read_file"); err != nil {
		t.Fatalf("verifier read in stage d should pass: %v", err)
	}
}

func TestRoleToolsUndeclaredRoleRejected(t *testing.T) {
	reg := newTestRegistry()
	_, err := reg.Resolve(context.Background(), "orphan", "c")
	assertCode(t, err, CodeAgentRoleUndeclared)
}

func TestRoleToolsMissingTagsRejected(t *testing.T) {
	reg := newTestRegistry()
	binding, err := reg.Resolve(context.Background(), "coder", "c")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(binding.GrantedTools) != 2 {
		t.Fatalf("only tagged read_file/write_file should be granted, got %v", binding.GrantedTools)
	}
	if err := reg.CheckTool(context.Background(), "coder", "c", "edit_file"); err == nil {
		t.Fatal("untagged edit_file must be rejected")
	} else {
		assertCode(t, err, CodeToolTagMissing)
	}
}

func TestRoleToolsValidateToolTags(t *testing.T) {
	reg := newTestRegistry()
	if err := reg.ValidateToolTags(context.Background(), "read_file"); err != nil {
		t.Fatalf("tagged tool should pass: %v", err)
	}
	assertCode(t, reg.ValidateToolTags(context.Background(), "untagged_tool"), CodeToolTagMissing)
}

func TestRoleToolsUnknownStage(t *testing.T) {
	reg := newTestRegistry()
	_, err := reg.Resolve(context.Background(), "coder", "z")
	assertCode(t, err, CodeUnknownStage)
}