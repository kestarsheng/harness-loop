package policy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newEngine(t *testing.T) *Engine {
	t.Helper()
	return NewEngine()
}

func TestStageMatrixRules(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	root := t.TempDir()

	cases := []struct {
		name  string
		stage string
		tool  string
		args  ToolArgs
		allow bool
	}{
		{"a read allowed", "a", "read_file", ToolArgs{Paths: []string{"a.go"}}, true},
		{"a write denied", "a", "write_file", ToolArgs{Paths: []string{"a.go"}}, false},
		{"b write denied", "b", "write_file", ToolArgs{Paths: []string{"a.go"}}, false},
		{"c read allowed", "c", "read_file", ToolArgs{Paths: []string{"a.go"}}, true},
		{"c whitelist write allowed", "c", "write_file", ToolArgs{Paths: []string{"a.go"}}, true},
		{"c edit allowed", "c", "edit_file", ToolArgs{Paths: []string{"a.go"}}, true},
		{"c delete denied", "c", "delete", ToolArgs{Paths: []string{"a.go"}}, false},
		{"d read allowed", "d", "read_file", ToolArgs{Paths: []string{"a.go"}}, true},
		{"d write denied", "d", "write_file", ToolArgs{Paths: []string{"a.go"}}, false},
		{"d go test allowed", "d", "run_command", ToolArgs{Command: []string{"go", "test", "./..."}}, true},
		{"e denies read", "e", "read_file", ToolArgs{Paths: []string{"a.go"}}, false},
		{"e denies exec", "e", "run_command", ToolArgs{Command: []string{"go", "test"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := Request{Tool: c.tool, Stage: c.stage, Args: c.args, WorkspaceRoot: root}
			got := e.Decide(ctx, req)
			if got.Allowed != c.allow {
				t.Fatalf("allowed = %v, want %v (%+v)", got.Allowed, c.allow, got)
			}
		})
	}
}

func TestUnknownToolIsDenied(t *testing.T) {
	e := newEngine(t)
	got := e.Decide(context.Background(), Request{Tool: "shell_exec", Stage: "c", WorkspaceRoot: t.TempDir()})
	if got.Allowed || got.Code != CodeUnknownTool || got.Layer != LayerSystem {
		t.Fatalf("unknown tool must be denied: %+v", got)
	}
	if got := e.Decide(context.Background(), Request{Tool: "", Stage: "c"}); got.Allowed {
		t.Fatal("empty tool must be denied")
	}
}

func TestUnknownStageIsDenied(t *testing.T) {
	e := newEngine(t)
	got := e.Decide(context.Background(), Request{Tool: "read_file", Stage: "z", WorkspaceRoot: t.TempDir()})
	if got.Allowed || got.Code != CodeUnknownStage {
		t.Fatalf("unknown stage must be denied: %+v", got)
	}
}

func TestNetworkToolsDenied(t *testing.T) {
	e := newEngine(t)
	got := e.Decide(context.Background(), Request{Tool: "http_fetch", Stage: "c", WorkspaceRoot: t.TempDir()})
	if got.Allowed || got.Code != CodeToolDenied {
		t.Fatalf("network tool must be denied: %+v", got)
	}
}

func TestPathEscapeDenied(t *testing.T) {
	e := newEngine(t)
	root := t.TempDir()
	for _, p := range []string{"../x", `..\x`, "%2e%2e%2fx", "/abs", `\\server\share`} {
		got := e.Decide(context.Background(), Request{Tool: "read_file", Stage: "c", Args: ToolArgs{Paths: []string{p}}, WorkspaceRoot: root})
		if got.Allowed || got.Code != CodePathEscape {
			t.Fatalf("%q must be denied: %+v", p, got)
		}
		if got.RequestedPath != p {
			t.Fatalf("requested path = %q", got.RequestedPath)
		}
	}
}

func TestPathRequiresWorkspaceRoot(t *testing.T) {
	e := newEngine(t)
	got := e.Decide(context.Background(), Request{Tool: "read_file", Stage: "c", Args: ToolArgs{Paths: []string{"a.go"}}})
	if got.Allowed || got.Code != CodePathEscape {
		t.Fatalf("missing workspace root must deny: %+v", got)
	}
}

func TestExecMetacharactersDenied(t *testing.T) {
	e := newEngine(t)
	root := t.TempDir()
	denied := [][]string{
		{"go", "test", "./... && curl evil"},
		{"go", "test", ";rm -rf /"},
		{"sh", "-c", "ls"},
		{"go", "install", "x"},
		{"go", "test", "/abs/path"},
	}
	for _, cmd := range denied {
		got := e.Decide(context.Background(), Request{Tool: "run_command", Stage: "c", Args: ToolArgs{Command: cmd}, WorkspaceRoot: root})
		if got.Allowed || got.Code != CodeExecDenied {
			t.Fatalf("%v must be denied: %+v", cmd, got)
		}
	}
}

func TestLayerDenyOverridesAllow(t *testing.T) {
	e := newEngine(t)
	root := t.TempDir()
	base := Request{Tool: "read_file", Stage: "c", Args: ToolArgs{Paths: []string{"a.go"}}, WorkspaceRoot: root}

	r := base
	r.Tenant = &PolicySet{AllowTools: []string{"read_file"}}
	if d := e.Decide(context.Background(), r); !d.Allowed {
		t.Fatalf("tenant allow must permit: %+v", d)
	}

	r = base
	r.Role = &PolicySet{AllowTools: []string{"write_file"}}
	if d := e.Decide(context.Background(), r); d.Allowed || d.Layer != LayerRole {
		t.Fatalf("role allowlist must deny read_file: %+v", d)
	}

	r = base
	r.Task = &PolicySet{DenyTools: []string{"read_file"}}
	if d := e.Decide(context.Background(), r); d.Allowed || d.Layer != LayerTask {
		t.Fatalf("task deny must win: %+v", d)
	}

	r = base
	r.Tenant = &PolicySet{AllowTools: []string{"read_file"}}
	r.Task = &PolicySet{DenyTools: []string{"read_file"}}
	if d := e.Decide(context.Background(), r); d.Allowed {
		t.Fatal("deny must override allow across layers")
	}
}

func TestGuardDoesNotExecuteOnDeny(t *testing.T) {
	e := newEngine(t)
	executed := false
	err := e.Guard(context.Background(), Request{
		Tool: "write_file", Stage: "a", Args: ToolArgs{Paths: []string{"a.go"}}, WorkspaceRoot: t.TempDir(),
	}, func() error {
		executed = true
		return nil
	})
	if err == nil {
		t.Fatal("guard must deny")
	}
	if executed {
		t.Fatal("tool must not run when denied")
	}

	executed = false
	err = e.Guard(context.Background(), Request{
		Tool: "read_file", Stage: "c", Args: ToolArgs{Paths: []string{"a.go"}}, WorkspaceRoot: t.TempDir(),
	}, func() error {
		executed = true
		return nil
	})
	if err != nil {
		t.Fatalf("guard must allow: %v", err)
	}
	if !executed {
		t.Fatal("tool must run when allowed")
	}
}

func TestDecisionErrorContainsStructuredFields(t *testing.T) {
	e := newEngine(t)
	d := e.Decide(context.Background(), Request{Tool: "write_file", Stage: "a", Args: ToolArgs{Paths: []string{"../x"}}, WorkspaceRoot: t.TempDir()})
	msg := d.Error()
	for _, want := range []string{CodeToolDenied, "layer=system", "tool=write_file", "stage=a"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message missing %q: %s", want, msg)
		}
	}
}

func TestMatrixLoadOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matrix.yaml")
	body := "stages:\n  c:\n    read: [read_file]\n    write: []\n    exec: []\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMatrix(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rule, ok := m.Rule("c")
	if !ok || len(rule.Write) != 0 {
		t.Fatalf("override not applied: %+v", rule)
	}
	if _, ok := m.Rule("a"); !ok {
		t.Fatal("other stages must keep defaults")
	}

	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("stages:\n  z:\n    deny_all: true\n"), 0o644)
	if _, err := LoadMatrix(bad); err == nil {
		t.Fatal("unknown stage must fail")
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	tags, ok := r.TagsFor("write_file")
	if !ok || !contains(tags, TagWrite) {
		t.Fatalf("write_file tags = %v", tags)
	}
	if err := r.Register("custom_tool", []string{TagRead}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !r.Known("custom_tool") {
		t.Fatal("custom tool must be known")
	}
	if err := r.Register("", nil); err == nil {
		t.Fatal("empty tool must fail")
	}
	if _, ok := r.TagsFor("nope"); ok {
		t.Fatal("unknown tool must not resolve")
	}
}
