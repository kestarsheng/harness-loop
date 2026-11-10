package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
)

func assertDenied(t *testing.T, d Decision, code string) {
	t.Helper()
	if d.Allowed {
		t.Fatalf("request must be denied: %+v", d)
	}
	if d.Code != code {
		t.Fatalf("code = %s, want %s (%+v)", d.Code, code, d)
	}
	if d.Layer == "" || d.RuleID == "" || d.Tool == "" || d.Stage == "" {
		t.Fatalf("denial must carry structured fields: %+v", d)
	}
}

func TestNegativePathVectors(t *testing.T) {
	e := newEngine(t)
	root := t.TempDir()
	vectors := []string{
		"../escape",
		`..\escape`,
		"%2e%2e%2fescape",
		"/etc/passwd",
		`\\server\share\file`,
		`\\?\C:\windows`,
		"file:///etc/passwd",
		"http://evil.example/x",
		"a\x00b",
	}
	for _, v := range vectors {
		d := e.Decide(context.Background(), Request{Tool: "read_file", Stage: "c", Args: ToolArgs{Paths: []string{v}}, WorkspaceRoot: root})
		assertDenied(t, d, CodePathEscape)
		if d.RequestedPath != v {
			t.Fatalf("requested path = %q, want %q", d.RequestedPath, v)
		}
	}
}

func TestNegativeDotDotVariantsDoNotEscape(t *testing.T) {
	e := newEngine(t)
	root := t.TempDir()
	for _, v := range []string{".../a", "..a/b", "a/../b"} {
		d := e.Decide(context.Background(), Request{Tool: "read_file", Stage: "c", Args: ToolArgs{Paths: []string{v}}, WorkspaceRoot: root})
		if d.Allowed && d.RequestedPath != "" {
			t.Fatalf("%q allowed with an escape marker: %+v", v, d)
		}
	}
}

func TestNegativeExecVectors(t *testing.T) {
	e := newEngine(t)
	root := t.TempDir()
	vectors := [][]string{
		{"go", "test", "./... && curl evil"},
		{"go", "test", "; rm -rf /"},
		{"go", "test", "| cat"},
		{"go", "test", "`id`"},
		{"sh", "-c", "ls"},
		{"bash", "-c", "ls"},
		{"powershell", "-c", "ls"},
		{"npm", "install", "evil"},
		{"npm", "install"},
		{"go", "test", "/abs/output"},
		{"/usr/bin/go", "test"},
		{"go", "install", "tool"},
		{"go", "get", "pkg"},
		{"go", "mod", "download"},
	}
	for _, cmd := range vectors {
		d := e.Decide(context.Background(), Request{Tool: "run_command", Stage: "c", Args: ToolArgs{Command: cmd}, WorkspaceRoot: root})
		assertDenied(t, d, CodeExecDenied)
	}
}

func TestNegativeSelfElevation(t *testing.T) {
	e := newEngine(t)
	root := t.TempDir()
	vectors := []string{
		"config/harness_loop/stage_allow.yaml",
		"config/harness_loop/exec_allow.yaml",
		".harness/policy/stage_matrix.yaml",
		`config\harness_loop\features.yaml`,
	}
	for _, v := range vectors {
		d := e.Decide(context.Background(), Request{Tool: "write_file", Stage: "c", Args: ToolArgs{Paths: []string{v}}, WorkspaceRoot: root})
		assertDenied(t, d, CodeSelfElevation)
		if d.RuleID != "policy.self_elevation" {
			t.Fatalf("rule id = %s", d.RuleID)
		}
	}

	// Reading policy files is still allowed (read-only inspection).
	d := e.Decide(context.Background(), Request{Tool: "read_file", Stage: "c", Args: ToolArgs{Paths: []string{"config/harness_loop/stage_matrix.yaml"}}, WorkspaceRoot: root})
	if !d.Allowed {
		t.Fatalf("reading policy files must stay allowed: %+v", d)
	}
}

func TestNegativeStageMatrix(t *testing.T) {
	e := newEngine(t)
	root := t.TempDir()
	cases := []struct {
		stage string
		tool  string
		args  ToolArgs
		allow bool
	}{
		{"a", "write_file", ToolArgs{Paths: []string{"a.go"}}, false},
		{"a", "run_command", ToolArgs{Command: []string{"go", "test", "./..."}}, false},
		{"b", "write_file", ToolArgs{Paths: []string{"a.go"}}, false},
		{"c", "write_file", ToolArgs{Paths: []string{"a.go"}}, true},
		{"c", "run_command", ToolArgs{Command: []string{"go", "test", "./..."}}, true},
		{"d", "write_file", ToolArgs{Paths: []string{"a.go"}}, false},
		{"d", "run_command", ToolArgs{Command: []string{"go", "test", "./..."}}, true},
		{"e", "read_file", ToolArgs{Paths: []string{"a.go"}}, false},
		{"e", "run_command", ToolArgs{Command: []string{"go", "test"}}, false},
	}
	for _, c := range cases {
		d := e.Decide(context.Background(), Request{Tool: c.tool, Stage: c.stage, Args: c.args, WorkspaceRoot: root})
		if d.Allowed != c.allow {
			t.Fatalf("%s/%s allowed=%v want %v (%+v)", c.stage, c.tool, d.Allowed, c.allow, d)
		}
		if !d.Allowed && (d.Code == "" || d.Layer == "" || d.RuleID == "") {
			t.Fatalf("denial missing structured fields: %+v", d)
		}
	}
}

func TestNegativeDenyWinsOverAllow(t *testing.T) {
	e := newEngine(t)
	root := t.TempDir()
	req := Request{
		Tool: "read_file", Stage: "c", WorkspaceRoot: root,
		Args:   ToolArgs{Paths: []string{"a.go"}},
		Tenant: &PolicySet{AllowTools: []string{"read_file"}},
		Task:   &PolicySet{DenyTools: []string{"read_file"}},
	}
	d := e.Decide(context.Background(), req)
	assertDenied(t, d, CodeToolDenied)
	if d.Layer != LayerTask {
		t.Fatalf("layer = %s, want task", d.Layer)
	}
}

func TestNegativePolicySnapshotIsStable(t *testing.T) {
	e := newEngine(t)
	root := t.TempDir()
	req := Request{Tool: "read_file", Stage: "c", Args: ToolArgs{Paths: []string{"a.go"}}, WorkspaceRoot: root}

	before := requestDigest(t, req)
	for i := 0; i < 5; i++ {
		_ = e.Decide(context.Background(), req)
	}
	after := requestDigest(t, req)
	if before != after {
		t.Fatalf("policy request mutated during evaluation: %s != %s", before, after)
	}
}

func requestDigest(t *testing.T, req Request) string {
	t.Helper()
	payload := map[string]any{
		"tool":  req.Tool,
		"stage": req.Stage,
		"paths": req.Args.Paths,
		"cmd":   req.Args.Command,
		"root":  filepath.Clean(req.WorkspaceRoot),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
