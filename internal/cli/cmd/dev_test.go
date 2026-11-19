package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"agent-platform/internal/cli/output"
	"agent-platform/internal/devstack"
)

func TestDevCmdRegistered(t *testing.T) {
	resetOpts()
	t.Cleanup(resetOpts)
	root := NewRootCmd()

	dev, _, err := root.Find([]string{"dev"})
	if err != nil || dev.Name() != "dev" {
		t.Fatalf("dev command not registered: %v", err)
	}
	for _, sub := range []string{"up", "down", "status"} {
		child, _, err := root.Find([]string{"dev", sub})
		if err != nil || child.Name() != sub {
			t.Fatalf("dev %s not registered: %v", sub, err)
		}
	}
	for _, flag := range []string{"contract", "root"} {
		if dev.PersistentFlags().Lookup(flag) == nil {
			t.Fatalf("dev must expose --%s", flag)
		}
	}
	if down, _, _ := root.Find([]string{"dev", "down"}); down.Flags().Lookup("keep-db") == nil {
		t.Fatal("dev down must expose --keep-db")
	}
}

func TestNewDevManagerUsesContract(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	m, err := newDevManager(root, devstack.DefaultContractPath)
	if err != nil {
		t.Fatalf("newDevManager: %v", err)
	}
	if len(m.Contract.Items) != 14 {
		t.Fatalf("items = %d", len(m.Contract.Items))
	}
	if m.Dirs.Run != filepath.Join(root, ".harness", "dev", "run") {
		t.Fatalf("run dir = %s", m.Dirs.Run)
	}
}

func TestNewDevManagerMissingContract(t *testing.T) {
	if _, err := newDevManager(t.TempDir(), "scripts/dev-stack.json"); err == nil {
		t.Fatal("missing contract must fail")
	}
}

func TestEmitDevReportText(t *testing.T) {
	resetOpts()
	t.Cleanup(resetOpts)
	cmd := NewRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	rep := &devstack.Report{Items: []devstack.ItemStatus{
		{Name: "harnessloop", Kind: "process", Port: 50013, Healthy: true, Reused: true, Detail: "127.0.0.1:50013 reachable"},
		{Name: "vite", Kind: "frontend", Port: 5174, Healthy: false, Detail: "connection refused"},
	}}
	err := emitDevReport(cmd, "ensure", rep, devstack.ErrNotHealthy)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != output.ExitFailed {
		t.Fatalf("expected ExitError(%d), got %v", output.ExitFailed, err)
	}
	text := buf.String()
	for _, want := range []string{"dev ensure: 2 item(s)", "harnessloop", "FAIL", "vite", "stack is not healthy"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

func TestEmitDevReportSuccess(t *testing.T) {
	resetOpts()
	t.Cleanup(resetOpts)
	cmd := NewRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	rep := &devstack.Report{Reused: true, Items: []devstack.ItemStatus{{Name: "harnessloop", Kind: "process", Port: 50013, Healthy: true}}}
	if err := emitDevReport(cmd, "ensure", rep, nil); err != nil {
		t.Fatalf("emitDevReport: %v", err)
	}
	if !strings.Contains(buf.String(), "reused running stack") {
		t.Fatalf("output = %s", buf.String())
	}
}

// loop 自举（T-P2-21 遗留）：50013 已起时必须直接复用，不要求用户先 dev-up。
func TestEnsureBackendReusesHealthyHarnessLoop(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	writeContract(t, root, fmt.Sprintf(`{"version":1,"items":[{"name":"harnessloop","kind":"process",`+
		`"group":"agent","port":50013,"command":"true","health":{"type":"http","url":%q,"expect_status":200}}]}`, srv.URL))

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var buf bytes.Buffer
	if err := ensureBackend(cmd, root, devstack.DefaultContractPath, &buf); err != nil {
		t.Fatalf("ensureBackend: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "already up") || !strings.Contains(out, "reusing the running stack") {
		t.Fatalf("a healthy stack must be reused, output:\n%s", out)
	}

	// Working directory of the probe is the fixture, so nothing was launched.
	if _, err := os.Stat(filepath.Join(root, ".harness", "dev", "logs", "harnessloop.log")); !os.IsNotExist(err) {
		t.Fatalf("reuse path must not launch anything: %v", err)
	}
}

// 50013 探测失败时必须走 devstack.Ensure（这里用一个必然启动失败的命令冷启动）。
func TestEnsureBackendStartsColdStack(t *testing.T) {
	root := t.TempDir()
	writeContract(t, root, `{"version":1,"items":[{"name":"harnessloop","kind":"process","group":"agent","port":50099,`+
		`"command":"hlp-no-such-binary-xyz","health":{"type":"port","address":"127.0.0.1:50099"}}]}`)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var buf bytes.Buffer
	err := ensureBackend(cmd, root, devstack.DefaultContractPath, &buf)
	if err == nil {
		t.Fatal("a cold stack whose process cannot start must report the failure")
	}
	out := buf.String()
	if !strings.Contains(out, "is down, starting the stack") {
		t.Fatalf("cold path must announce the ensure, output:\n%s", out)
	}
	if !strings.Contains(out, "50099") && !strings.Contains(out, "harnessloop") {
		t.Fatalf("cold path must report the failing item, output:\n%s", out)
	}
}

func writeContract(t *testing.T, root, body string) {
	t.Helper()
	path := filepath.Join(root, devstack.DefaultContractPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
