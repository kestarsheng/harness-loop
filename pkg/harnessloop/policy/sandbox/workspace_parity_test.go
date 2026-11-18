package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// The artifact checker (output_contract) resolves a declared relative path by
// joining it onto the workspace root. The file writer resolves the same
// argument through Resolve. If those two disagreed, a file could be written and
// still be reported missing; this test pins the equivalence.
func TestResolveMatchesContractJoin(t *testing.T) {
	root := t.TempDir()
	cases := []string{
		"cmd/server/healthz.go",
		"a.txt",
		"./x/y.txt",
		"deep/nested/dir/file.md",
	}
	for _, rel := range cases {
		resolved, err := Resolve(root, rel)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", rel, err)
		}
		joined := filepath.Join(root, filepath.FromSlash(rel))
		// Resolve() returns the symlink-evaluated form; compare against the
		// same treatment of the plain join so the assertion survives temp
		// directories that are themselves links.
		if evaluated, err := filepath.EvalSymlinks(filepath.Dir(joined)); err == nil {
			joined = filepath.Join(evaluated, filepath.Base(joined))
		}
		if filepath.Clean(resolved) != filepath.Clean(joined) {
			t.Fatalf("%q: writer resolves to %s, contract join gives %s", rel, resolved, joined)
		}
	}
}

// A href-style or absolute argument must never be silently rebased: the
// contract check would join it differently than the writer resolved it.
func TestResolveRejectsArgumentsTheContractCannotJoin(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"../x", "/etc/passwd", `C:\Windows\x.txt`} {
		if _, err := Resolve(root, rel); err == nil {
			t.Fatalf("%q must be refused instead of rebased", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "cmd")); !os.IsNotExist(err) {
		t.Fatalf("resolution must not create directories, stat err=%v", err)
	}
}
