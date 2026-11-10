package checks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module minitest\n\ngo 1.22\n",
		"calc.go": `package minitest

func Add(a, b int) int { return a + b }
`,
		"calc_test.go": `package minitest

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatalf("bad math")
	}
}

func TestBroken(t *testing.T) {
	t.Fatal("intentional failure")
}
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestRegistryHasAllCheckers(t *testing.T) {
	r := NewRegistry()
	want := []string{"go.test", "go.build", "go.vet", "lint", "file.exists", "hash.match", "cmd.exit0", "symbol.present"}
	got := r.IDs()
	if len(got) != len(want) {
		t.Fatalf("checkers = %v", got)
	}
	for _, id := range want {
		found := false
		for _, g := range got {
			if g == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing checker %s in %v", id, got)
		}
	}
}

func TestRegistryUnknownTypeIsUnverified(t *testing.T) {
	r := NewRegistry()
	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "does_not_exist", WorkDir: t.TempDir()})
	if res.Status != StatusUnverified || res.Err != CodeUnknownCheck {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestFileExistsChecker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()

	if res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "artifact_exists", Expr: "a.txt", WorkDir: dir}); res.Status != StatusPass {
		t.Fatalf("existing file: %+v", res)
	}
	if res := r.Run(context.Background(), Request{CriterionID: "c-2", Type: "artifact_exists", Expr: "missing.txt", WorkDir: dir}); res.Status != StatusFail {
		t.Fatalf("missing file: %+v", res)
	}
	if res := r.Run(context.Background(), Request{CriterionID: "c-3", Type: "artifact_exists", Expr: "../escape.txt", WorkDir: dir}); res.Status != StatusError {
		t.Fatalf("path escape must error: %+v", res)
	}
	if res := r.Run(context.Background(), Request{CriterionID: "c-4", Type: "artifact_exists", Expr: "/abs/path", WorkDir: dir}); res.Status != StatusError {
		t.Fatalf("absolute path must error: %+v", res)
	}
}

func TestGoBuildChecker(t *testing.T) {
	dir := writeModule(t)
	r := NewRegistry()
	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "build_pass", Expr: "go build ./...", WorkDir: dir})
	if res.Status != StatusPass {
		t.Fatalf("build: %+v", res)
	}
	if res.ExitCode != 0 || res.DurationMs < 0 {
		t.Fatalf("unexpected result: %+v", res)
	}

	broken := t.TempDir()
	os.WriteFile(filepath.Join(broken, "go.mod"), []byte("module broken\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(broken, "x.go"), []byte("package broken\n\nfunc broken() { undefinedCall() }\n"), 0o644)
	res = r.Run(context.Background(), Request{CriterionID: "c-2", Type: "build_pass", WorkDir: broken})
	if res.Status != StatusFail || res.ExitCode == 0 {
		t.Fatalf("broken build: %+v", res)
	}
}

func TestGoTestChecker(t *testing.T) {
	dir := writeModule(t)
	r := NewRegistry()

	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "tests_pass", Expr: "go test -run TestAdd ./...", WorkDir: dir})
	if res.Status != StatusPass {
		t.Fatalf("passing test: %+v", res)
	}
	res = r.Run(context.Background(), Request{CriterionID: "c-2", Type: "tests_pass", Expr: "go test -run TestBroken ./...", WorkDir: dir})
	if res.Status != StatusFail {
		t.Fatalf("failing test must be fail: %+v", res)
	}
	if len(res.Evidence) == 0 {
		t.Fatal("failure must carry evidence")
	}
}

func TestGoTestWithoutGoModIsUnverified(t *testing.T) {
	r := NewRegistry()
	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "tests_pass", WorkDir: t.TempDir()})
	if res.Status != StatusUnverified || res.Err != CodeProbeFailed {
		t.Fatalf("no go.mod must be unverified: %+v", res)
	}
}

func TestGoVetChecker(t *testing.T) {
	dir := writeModule(t)
	r := NewRegistry()
	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "static_check", WorkDir: dir})
	if res.Status != StatusPass {
		t.Fatalf("vet: %+v", res)
	}
}

func TestLintUnavailableIsUnverified(t *testing.T) {
	if commandExists("golangci-lint") || commandExists("staticcheck") {
		t.Skip("lint tool is installed; environment-dependent test")
	}
	r := NewRegistry()
	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "lint_pass", WorkDir: t.TempDir()})
	if res.Status != StatusUnverified || res.Err != CodeProbeFailed {
		t.Fatalf("missing lint tool must be unverified: %+v", res)
	}
}

func TestCmdExitChecker(t *testing.T) {
	r := NewRegistry()
	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "custom", Expr: "go version", WorkDir: t.TempDir()})
	if res.Status != StatusPass {
		t.Fatalf("go version: %+v", res)
	}

	res = r.Run(context.Background(), Request{CriterionID: "c-2", Type: "custom", Expr: "rm -rf /", WorkDir: t.TempDir()})
	if res.Status != StatusUnverified {
		t.Fatalf("disallowed command must be unverified: %+v", res)
	}
}

func TestHashMatchChecker(t *testing.T) {
	dir := t.TempDir()
	body := []byte("hello")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	const sum = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	r := NewRegistry()

	if res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "artifact_unchanged", Expr: "a.txt::" + sum, WorkDir: dir}); res.Status != StatusPass {
		t.Fatalf("matching hash: %+v", res)
	}
	if res := r.Run(context.Background(), Request{CriterionID: "c-2", Type: "artifact_unchanged", Expr: "a.txt::deadbeef", WorkDir: dir}); res.Status != StatusFail {
		t.Fatalf("drifted hash: %+v", res)
	}
}

func TestSymbolChecker(t *testing.T) {
	dir := writeModule(t)
	r := NewRegistry()

	if res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "code_symbol", Expr: "calc.go::Add", WorkDir: dir}); res.Status != StatusPass {
		t.Fatalf("existing symbol: %+v", res)
	}
	if res := r.Run(context.Background(), Request{CriterionID: "c-2", Type: "code_symbol", Expr: "calc.go::Sub", WorkDir: dir}); res.Status != StatusFail {
		t.Fatalf("missing symbol: %+v", res)
	}
}

func TestCheckTimeoutIsUnverified(t *testing.T) {
	dir := writeModule(t)
	r := NewRegistry()
	res := r.Run(context.Background(), Request{
		CriterionID: "c-1",
		Type:        "build_pass",
		WorkDir:     dir,
		Timeout:     time.Nanosecond,
	})
	if res.Status != StatusUnverified || res.Err != CodeCheckTimeout {
		t.Fatalf("timeout must be unverified: %+v", res)
	}
}

type panicChecker struct{}

func (p *panicChecker) ID() string                                          { return "panic" }
func (p *panicChecker) Supports(t string) bool                              { return t == "panic_case" }
func (p *panicChecker) Probe(ctx context.Context, r Request) (bool, string) { return true, "" }
func (p *panicChecker) Run(ctx context.Context, r Request) (Result, error) {
	panic("boom")
}

func TestCheckPanicBecomesError(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&panicChecker{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "panic_case"})
	if res.Status != StatusError || res.Err != CodeCheckPanic {
		t.Fatalf("panic must map to error: %+v", res)
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&FileExistsChecker{}); err == nil {
		t.Fatal("duplicate checker id must be rejected")
	}
	if err := r.Register(nil); err == nil {
		t.Fatal("nil checker must be rejected")
	}
}

func TestUnsafeCommandArgs(t *testing.T) {
	dir := writeModule(t)
	r := NewRegistry()
	res := r.Run(context.Background(), Request{
		CriterionID: "c-1",
		Type:        "build_pass",
		Expr:        "go build ./...; rm -rf /",
		WorkDir:     dir,
	})
	if res.Status != StatusError {
		t.Fatalf("shell metacharacters must be rejected: %+v", res)
	}
	var e error = errors.New(res.Err)
	if e == nil || !strings.Contains(res.Err, CodeUnsafeExpr) {
		t.Fatalf("error must carry %s: %+v", CodeUnsafeExpr, res)
	}
}

func TestCmdExitFailure(t *testing.T) {
	r := NewRegistry()
	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "custom", Expr: "go vet -definitely-not-a-flag", WorkDir: t.TempDir()})
	if res.Status != StatusFail || res.ExitCode == 0 {
		t.Fatalf("non-zero exit must fail: %+v", res)
	}
	if len(res.Evidence) == 0 {
		t.Fatal("failure must carry evidence")
	}
}

func TestHashMatchMissingFile(t *testing.T) {
	r := NewRegistry()
	res := r.Run(context.Background(), Request{
		CriterionID: "c-1",
		Type:        "artifact_unchanged",
		Expr:        "nope.txt::deadbeef",
		WorkDir:     t.TempDir(),
	})
	if res.Status != StatusFail {
		t.Fatalf("missing file must fail: %+v", res)
	}
}

func TestSymbolInNonGoFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello harness loop"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	if res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "code_symbol", Expr: "README.md::harness", WorkDir: dir}); res.Status != StatusPass {
		t.Fatalf("text symbol: %+v", res)
	}
	if res := r.Run(context.Background(), Request{CriterionID: "c-2", Type: "code_symbol", Expr: "README.md::missing", WorkDir: dir}); res.Status != StatusFail {
		t.Fatalf("missing text symbol: %+v", res)
	}
}

func TestFileExistsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "artifact_exists", Expr: "sub", WorkDir: dir})
	if res.Status != StatusPass || res.Evidence[0].Kind != "dir" {
		t.Fatalf("directory: %+v", res)
	}
}
