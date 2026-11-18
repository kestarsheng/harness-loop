package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	cases := []string{
		"../escape.txt",
		`..\escape.txt`,
		"%2e%2e%2fescape.txt",
		"..",
		"../",
		"pkg/../..",
		"/etc/passwd",
		`\\server\share\file`,
		"",
		"a\x00b",
	}
	for _, rel := range cases {
		if _, err := Resolve(root, rel); err == nil {
			t.Fatalf("%q must be rejected", rel)
		}
	}
}

// "." names the workspace root; the empty string names nothing. Pinning both
// halves here keeps a future "just allow relative paths" refactor from
// silently reviving the empty-path hole.
func TestResolveDotIsWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Resolve returns the symlink-evaluated form, and t.TempDir() may sit under
	// a link itself (macOS /var -> /private/var), so compare against the same
	// treatment of the root.
	want := root
	if evaluated, err := filepath.EvalSymlinks(root); err == nil {
		want = evaluated
	}

	got, err := Resolve(root, ".")
	if err != nil {
		t.Fatalf(`Resolve(root, ".") must succeed, got %v`, err)
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf(`Resolve(root, ".") = %s, want workspace root %s`, got, want)
	}
	if !isWithin(root, got) {
		t.Fatalf(`"." resolved outside root: %s`, got)
	}

	cached, err := ResolveCached(root, ".")
	if err != nil {
		t.Fatalf(`ResolveCached(root, ".") must succeed, got %v`, err)
	}
	if filepath.Clean(cached) != filepath.Clean(got) {
		t.Fatalf(`ResolveCached(root, ".") = %s, Resolve gives %s`, cached, got)
	}

	// Subtask c calls read_file(".") / list_dir("."): the resolved argument has
	// to be usable as the workspace root, not merely error-free.
	entries, err := os.ReadDir(got)
	if err != nil {
		t.Fatalf(`reading "." as the workspace root: %v`, err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name() == "README.md" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf(`"." does not point at the populated root, entries=%v`, entries)
	}

	// Allowing "." must not punch through the subpath whitelist: the root is not
	// inside "pkg", so a restricted guard still refuses it.
	restricted := PathGuard{Root: root, AllowedSubpaths: []string{"pkg"}}
	if bad, err := restricted.Check([]string{"."}); err == nil || bad != "." {
		t.Fatalf(`PathGuard must refuse "." under a subpath restriction, echoed %q err %v`, bad, err)
	}
}

func TestResolveEmptyStringStillRejected(t *testing.T) {
	root := t.TempDir()
	if _, err := Resolve(root, ""); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf(`Resolve(root, "") must fail with E_UNSAFE_PATH, got %v`, err)
	}
	if _, err := Resolve(root, "."); err != nil {
		t.Fatalf(`"." must stay accepted while "" is refused, got %v`, err)
	}
	// ".." must still be an escape, not merely "unsafe": callers branch on this.
	for _, rel := range []string{"..", "../", "..\\x", "pkg/../.."} {
		if _, err := Resolve(root, rel); !errors.Is(err, ErrPathEscape) {
			t.Fatalf("%q must fail with E_PATH_ESCAPE, got %v", rel, err)
		}
	}
}

func TestResolveAcceptsInsidePaths(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(root, "pkg/sub/file.go")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !isWithin(root, got) {
		t.Fatalf("resolved outside root: %s", got)
	}

	got, err = Resolve(root, "./pkg/../pkg/sub")
	if err != nil {
		t.Fatalf("clean path: %v", err)
	}
	if !isWithin(root, got) {
		t.Fatalf("cleaned path outside root: %s", got)
	}
}

func TestResolveDetectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not available: %v", err)
	}
	if _, err := Resolve(root, "link/secret.txt"); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("symlink escape must be rejected: %v", err)
	}
}

func TestPathGuardAllowedSubpaths(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "allowed"), 0o755)
	os.MkdirAll(filepath.Join(root, "other"), 0o755)

	g := PathGuard{Root: root, AllowedSubpaths: []string{"allowed"}}
	if _, err := g.Check([]string{"allowed/a.go"}); err != nil {
		t.Fatalf("allowed subpath rejected: %v", err)
	}
	if bad, err := g.Check([]string{"other/b.go"}); err == nil || bad != "other/b.go" {
		t.Fatalf("outside subpath must be rejected: %v %v", bad, err)
	}

	unrestricted := PathGuard{Root: root}
	if _, err := unrestricted.Check([]string{"other/b.go"}); err != nil {
		t.Fatalf("no subpath restriction should allow: %v", err)
	}
	if _, err := unrestricted.Check([]string{"../x"}); err == nil {
		t.Fatal("escape must still be rejected")
	}
}

func TestExecAllowlist(t *testing.T) {
	a := DefaultExecAllowlist()

	allowed := [][]string{
		{"go", "test", "./..."},
		{"go", "build", "./..."},
		{"go", "vet", "./..."},
		{"gofmt", "-l", "."},
	}
	for _, args := range allowed {
		if err := a.Check(args); err != nil {
			t.Fatalf("%v rejected: %v", args, err)
		}
	}

	denied := [][]string{
		{},
		{"rm", "-rf", "/"},
		{"sh", "-c", "curl evil"},
		{"bash", "-c", "ls"},
		{"go", "test", "./... && curl evil"},
		{"go", "test", ";rm"},
		{"go", "test", "|cat"},
		{"go", "test", "`id`"},
		{"go", "test", "$HOME"},
		{"go", "test", "/etc/passwd"},
		{"go", "test", "../outside"},
		{"go", "install", "evil"},
		{"go", "get", "evil"},
		{"go", "mod", "download"},
		{"go", "mod", "tidy"},
		{"/usr/bin/go", "test"},
		{"./go", "test"},
	}
	for _, args := range denied {
		if err := a.Check(args); err == nil {
			t.Fatalf("%v must be denied", args)
		}
	}
}

func TestExecAllowlistLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exec_allow.yaml")
	if err := os.WriteFile(path, []byte("commands: [go, mytool]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := LoadExecAllowlist(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !a.Allows("mytool") || a.Allows("gofmt") {
		t.Fatalf("overrides not applied: %v", a.Commands)
	}
	if _, err := LoadExecAllowlist(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing allowlist must fail")
	}
	if _, err := LoadExecAllowlist(""); err != nil {
		t.Fatalf("empty path must use defaults: %v", err)
	}
}

func TestIsEscape(t *testing.T) {
	if !IsEscape(fmt.Errorf("wrapper: %w", ErrPathEscape)) {
		t.Fatal("wrapped escape must be detected")
	}
	if IsEscape(ErrUnsafePath) {
		t.Fatal("unsafe path is not an escape")
	}
}

func TestResolveNonExistentInsidePath(t *testing.T) {
	root := t.TempDir()
	got, err := Resolve(root, "newdir/new.go")
	if err != nil {
		t.Fatalf("non-existent inside path must resolve: %v", err)
	}
	if !isWithin(root, got) {
		t.Fatalf("resolved outside root: %s", got)
	}
}

func TestLoadExecAllowlistInvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("commands: [go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExecAllowlist(path); err == nil {
		t.Fatal("invalid yaml must fail")
	}
}
