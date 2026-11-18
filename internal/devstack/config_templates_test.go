package devstack

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Every real process template must render without a compose-only host:port.
func TestAllProcessTemplatesRenderCleanly(t *testing.T) {
	c := loadContract(t)
	root := t.TempDir()
	hostPort := regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9-]*):(\d+)\b`)

	for _, it := range c.Items {
		if !it.IsProcess() || it.ConfigTemplate == "" {
			continue
		}
		src, err := filepath.Abs(filepath.Join("..", "..", it.ConfigTemplate))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(src); err != nil {
			t.Fatalf("%s template missing: %v", it.Name, err)
		}
		item := it
		item.ConfigTemplate = src
		item.ConfigOutput = filepath.ToSlash(filepath.Join(".harness", "dev", "conf", it.Name+".yaml"))
		if err := RenderConfig(root, c, item); err != nil {
			t.Fatalf("%s: RenderConfig: %v", it.Name, err)
		}
		raw, err := os.ReadFile(absJoin(root, item.ConfigOutput))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range hostPort.FindAllStringSubmatch(string(raw), -1) {
			t.Fatalf("%s still references %s:%s after rendering:\n%s", it.Name, m[1], m[2], raw)
		}
	}
}
