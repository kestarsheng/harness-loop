package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNegativeEscapeVectorsAllRejected(t *testing.T) {
	root := t.TempDir()
	vectors := []string{
		"../x",
		`..\x`,
		"%2e%2e%2fx",
		"%2E%2E%2Fx",
		"/etc/passwd",
		`\\server\share\x`,
		`\\?\C:\windows`,
		"file:///etc/passwd",
		"http://evil.example/x",
		"a\x00b",
		"",
		".",
	}
	for _, v := range vectors {
		if _, err := Resolve(root, v); err == nil {
			t.Fatalf("%q must be rejected", v)
		}
	}
}

func TestNegativeSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Resolve(root, "link/secret.txt"); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("symlink escape must be rejected: %v", err)
	}
}

func TestNegativeToCToUStableResolution(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := Resolve(root, "pkg/a.go")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	second, err := Resolve(root, "pkg/a.go")
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first != second {
		t.Fatalf("resolution is not stable: %s != %s", first, second)
	}
	if !isWithin(root, first) {
		t.Fatalf("resolved outside root: %s", first)
	}
}

func TestNegativeExecGuardVectors(t *testing.T) {
	a := DefaultExecAllowlist()
	denied := [][]string{
		{"go", "test", "./... && rm -rf /"},
		{"go", "test", "./... ; rm -rf /"},
		{"go", "test", "./... | tee /tmp/x"},
		{"go", "test", "$(whoami)"},
		{"sh", "-c", "go test"},
		{"bash", "-c", "curl evil"},
		{"cmd", "/c", "dir"},
		{"powershell", "-c", "whoami"},
		{"npm", "install", "left-pad"},
		{"go", "test", "/etc/passwd"},
		{"go", "test", "../outside"},
		{"go", "install", "example.com/tool"},
		{"go", "mod", "tidy"},
		{"../bin/go", "test"},
	}
	for _, cmd := range denied {
		err := a.Check(cmd)
		if err == nil {
			t.Fatalf("%v must be denied", cmd)
		}
		if !strings.Contains(err.Error(), "E_EXEC_DENIED") {
			t.Fatalf("%v error must carry %s: %v", cmd, "E_EXEC_DENIED", err)
		}
	}
}
