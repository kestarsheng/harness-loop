package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-platform/pkg/harnessloop/policy"
	"agent-platform/pkg/harnessloop/policy/sandbox"
	"agent-platform/services/mcp-service/internal/audit"
)

// callConfig builds the tool config exactly as mcp-service receives it: the
// JSON string from CallToolRequest.tool_config unmarshalled into a map.
func callConfig(t *testing.T, workspaceRoot string) map[string]interface{} {
	t.Helper()
	raw := `{"__policy_context__":{"stage":"c","workspace_root":"` + filepath.ToSlash(workspaceRoot) + `"}}`
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal tool config: %v", err)
	}
	return cfg
}

func TestFileWriteToolWritesIntoWorkspace(t *testing.T) {
	audit.Reset()
	root := t.TempDir()
	tool := NewFileWriteTool("")

	out, err := tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
		"path":    "cmd/server/healthz.go",
		"content": "package main\n",
	}, callConfig(t, root))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	stored := filepath.Join(root, "cmd", "server", "healthz.go")
	got, err := os.ReadFile(stored)
	if err != nil {
		t.Fatalf("artifact not on disk at %s: %v", stored, err)
	}
	if string(got) != "package main\n" {
		t.Fatalf("content = %q", got)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("result is not json: %v (%s)", err, out)
	}
	if result["path"] != "cmd/server/healthz.go" {
		t.Fatalf("result path = %v, want the contract-relative path", result["path"])
	}
	if result["status"] != "written" {
		t.Fatalf("result status = %v", result["status"])
	}
	if result["bytes"].(float64) != float64(len("package main\n")) {
		t.Fatalf("result bytes = %v", result["bytes"])
	}
	if !strings.HasPrefix(result["sha256"].(string), "sha256:") {
		t.Fatalf("result sha256 = %v", result["sha256"])
	}
	if len(audit.Records()) != 0 {
		t.Fatalf("a successful write must not audit a denial, got %v", audit.Records())
	}
}

// The MCP process cwd is the service directory, so a tool that ignored the
// per-call root would drop the artifact next to the binary instead of in the
// task workspace. Both halves are asserted here.
func TestFileWriteToolTargetsWorkspaceNotProcessCwd(t *testing.T) {
	audit.Reset()
	root := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	tool := NewFileWriteTool("")
	if _, err := tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
		"path":    "artifact-cwd-probe.txt",
		"content": "x",
	}, callConfig(t, root)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "artifact-cwd-probe.txt")); err != nil {
		t.Fatalf("artifact missing from workspace root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "artifact-cwd-probe.txt")); !os.IsNotExist(err) {
		t.Fatalf("artifact leaked into process cwd %s (err=%v)", cwd, err)
	}
}

func TestFileWriteToolRejectsEscapes(t *testing.T) {
	cases := []struct {
		target string
		code   string
	}{
		{"../outside.txt", policy.CodePathEscape},
		{"cmd/../../outside.txt", policy.CodePathEscape},
		{"/etc/passwd", "E_UNSAFE_PATH"},
		{`\\server\share\x.txt`, "E_UNSAFE_PATH"},
		{"http://evil.example/x.txt", "E_UNSAFE_PATH"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.target, func(t *testing.T) {
			audit.Reset()
			parent := t.TempDir()
			root := filepath.Join(parent, "ws")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			tool := NewFileWriteTool("")
			_, err := tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
				"path":    tc.target,
				"content": "leak",
			}, callConfig(t, root))
			if err == nil {
				t.Fatalf("write %q must be refused", tc.target)
			}
			if !errors.Is(err, sandbox.ErrPathEscape) && !errors.Is(err, sandbox.ErrUnsafePath) {
				t.Fatalf("error must be a sandbox sentinel, got %v", err)
			}

			if _, statErr := os.Stat(filepath.Join(parent, "outside.txt")); !os.IsNotExist(statErr) {
				t.Fatalf("escape created a file outside the workspace: %v", statErr)
			}

			records := audit.Records()
			if len(records) != 1 {
				t.Fatalf("denial must leave exactly one audit record, got %d", len(records))
			}
			if records[0].Decision != audit.DecisionDeny || records[0].Code != tc.code {
				t.Fatalf("audit record = %+v", records[0])
			}
			if records[0].Tool != "file_write" || records[0].Path != tc.target {
				t.Fatalf("audit record does not name the refused call: %+v", records[0])
			}
		})
	}
}

func TestFileWriteToolRefusesWithoutWorkspaceRoot(t *testing.T) {
	audit.Reset()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	tool := NewFileWriteTool("")
	_, err = tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
		"path":    "no-root-probe.txt",
		"content": "x",
	}, nil)
	if err == nil {
		t.Fatal("a call without a workspace root must fail closed")
	}
	if _, statErr := os.Stat(filepath.Join(cwd, "no-root-probe.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("fail-closed call wrote next to the process, err=%v", statErr)
	}
	if len(audit.Records()) != 1 {
		t.Fatalf("missing-root refusal must be audited, got %v", audit.Records())
	}
}

func TestFileReadToolReadsInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tool := NewFileReadTool("")
	out, err := tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
		"path": "docs/a.txt",
	}, callConfig(t, root))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("read output = %q", out)
	}

	audit.Reset()
	if _, err := tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
		"path": "../escape.txt",
	}, callConfig(t, root)); err == nil {
		t.Fatal("read escape must be refused")
	}
	if len(audit.Records()) != 1 || audit.Records()[0].Tool != "file_read" {
		t.Fatalf("read denial must be audited, got %v", audit.Records())
	}
}

// read_file(".") is what an agent does to open its workspace. It has to come
// back as a listing of the root — not as a sandbox refusal, and not as a
// platform errno leaking out of os.ReadFile.
func TestFileReadToolListsDotAsWorkspaceRoot(t *testing.T) {
	audit.Reset()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg", "sub"), 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "sub", "deep.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatalf("seed nested: %v", err)
	}

	tool := NewFileReadTool("")
	out, err := tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
		"path": ".",
	}, callConfig(t, root))
	if err != nil {
		t.Fatalf(`read_file(".") must succeed: %v`, err)
	}

	if !strings.Contains(out, "Directory: .\n") {
		t.Fatalf(`listing does not name the argument as ".": %q`, out)
	}
	if !strings.Contains(out, "Entries: 2\n") {
		t.Fatalf("listing entry count is wrong (non-recursive listing of root): %q", out)
	}
	if !strings.Contains(out, "README.md\tfalse\t5\n") {
		t.Fatalf("file entry must carry is_dir=false and its size: %q", out)
	}
	if !strings.Contains(out, "pkg\ttrue\t0\n") {
		t.Fatalf("directory entry must carry is_dir=true: %q", out)
	}
	// Non-recursive: the nested file is reachable but must not be listed here.
	if strings.Contains(out, "deep.go") {
		t.Fatalf(`listing recursed into subdirectories: %q`, out)
	}
}

func TestFileReadToolEmptyDirectoryIsExplicit(t *testing.T) {
	audit.Reset()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "emptydir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	tool := NewFileReadTool("")
	out, err := tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
		"path": "emptydir",
	}, callConfig(t, root))
	if err != nil {
		t.Fatalf("read empty dir: %v", err)
	}
	if !strings.Contains(out, "Entries: 0\n") || !strings.Contains(out, "(empty directory)") {
		t.Fatalf("empty directory must say so explicitly: %q", out)
	}
	if strings.Contains(out, "name\tis_dir\tsize") {
		t.Fatalf("empty directory must not render a table header: %q", out)
	}
}

func TestFileReadToolTruncatesLargeDirectory(t *testing.T) {
	audit.Reset()
	root := t.TempDir()
	const seeded = maxDirEntries + 5
	for i := 0; i < seeded; i++ {
		name := filepath.Join(root, fmt.Sprintf("f%03d.txt", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	tool := NewFileReadTool("")
	out, err := tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
		"path": ".",
	}, callConfig(t, root))
	if err != nil {
		t.Fatalf("read large dir: %v", err)
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("listing must be marked truncated: %q", out)
	}
	if !strings.Contains(out, fmt.Sprintf("Entries: %d", seeded)) {
		t.Fatalf("truncated listing must still report the real count: %q", out)
	}

	rows := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "" || line == "name\tis_dir\tsize" {
			continue
		}
		if strings.Count(line, "\t") == 2 {
			rows++
		}
	}
	if rows != maxDirEntries {
		t.Fatalf("truncated listing rendered %d entry rows, want %d", rows, maxDirEntries)
	}
}

// A symlink entry is reported as a link, never as its target: following it here
// would let the listing describe something the path guard never approved.
func TestFileReadToolDoesNotFollowSymlinkEntries(t *testing.T) {
	audit.Reset()
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	tool := NewFileReadTool("")
	out, err := tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
		"path": ".",
	}, callConfig(t, root))
	if err != nil {
		t.Fatalf(`read_file("."): %v`, err)
	}
	if !strings.Contains(out, "link\tfalse\t") {
		t.Fatalf("symlink entry must be is_dir=false and not followed: %q", out)
	}
	if strings.Contains(out, "secret.txt") {
		t.Fatalf("listing followed the symlink: %q", out)
	}
}

// The directory branch must not become a bypass: a path the sandbox refuses is
// still refused before any listing happens.
func TestFileReadToolDirectoryDoesNotBypassPathGuard(t *testing.T) {
	audit.Reset()
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	tool := NewFileReadTool("")
	if _, err := tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
		"path": "link",
	}, callConfig(t, root)); err == nil {
		t.Fatal("a symlink that escapes the workspace must not be listed")
	}
	for _, rel := range []string{"..", "../", "/etc"} {
		if _, err := tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
			"path": rel,
		}, callConfig(t, root)); err == nil {
			t.Fatalf("%q must be refused", rel)
		}
	}
}

func TestAuditDenialReachesFile(t *testing.T) {
	audit.Reset()
	t.Cleanup(func() { audit.SetFile("") })
	trail := filepath.Join(t.TempDir(), "nested", "tool-audit.jsonl")
	audit.SetFile(trail)

	root := t.TempDir()
	tool := NewFileWriteTool("")
	if _, err := tool.ExecuteWithConfig(context.Background(), map[string]interface{}{
		"path":    "../leak.txt",
		"content": "x",
	}, callConfig(t, root)); err == nil {
		t.Fatal("escape must be refused")
	}

	raw, err := os.ReadFile(trail)
	if err != nil {
		t.Fatalf("audit file missing: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("audit file has %d lines: %s", len(lines), raw)
	}
	var rec audit.Record
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("audit line is not json: %v", err)
	}
	if filepath.Clean(rec.WorkspaceRoot) != filepath.Clean(root) || rec.Code != policy.CodePathEscape {
		t.Fatalf("audit record = %+v", rec)
	}
}
