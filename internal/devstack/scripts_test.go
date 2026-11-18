package devstack

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The contract is shared with scripts/dev-lib.sh and scripts/dev-down.sh.
// These checks fail when one side drifts, without running anything.
func TestShellScriptsStayInSync(t *testing.T) {
	c := loadContract(t)
	scriptsDir := filepath.Join("..", "..", "scripts")
	lib := readScript(t, filepath.Join(scriptsDir, "dev-lib.sh"))
	down := readScript(t, filepath.Join(scriptsDir, "dev-down.sh"))
	up := readScript(t, filepath.Join(scriptsDir, "dev-up.sh"))
	if !strings.Contains(up, "dev-stack.json") {
		t.Fatalf("scripts/dev-up.sh must reference dev-stack.json as its data source")
	}

	for _, it := range c.Items {
		if port := strconv.Itoa(it.Port); !strings.Contains(lib, port) {
			t.Fatalf("%s port %d is missing from scripts/dev-lib.sh", it.Name, it.Port)
		}
		if it.HTTPPort > 0 {
			httpPort := strconv.Itoa(it.HTTPPort)
			if !strings.Contains(lib, httpPort) {
				t.Fatalf("%s http_port %d is missing from scripts/dev-lib.sh", it.Name, it.HTTPPort)
			}
		}
		if it.IsProcess() && !strings.Contains(down, it.PIDFileStem()) {
			t.Fatalf("%s pid name %q is missing from scripts/dev-down.sh", it.Name, it.PIDFileStem())
		}
	}

	// Every declared container command must be a docker invocation.
	for _, it := range c.Items {
		if it.Kind != KindContainer {
			continue
		}
		if !strings.HasPrefix(it.Command, "docker ") {
			t.Fatalf("container %s command is not a docker invocation: %q", it.Name, it.Command)
		}
	}

	// The contract must keep the documented 14-item shape.
	if got := len(c.Items); got != 14 {
		t.Fatalf("items = %d, want 14", got)
	}
}

func readScript(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
