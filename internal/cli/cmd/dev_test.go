package cmd

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

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
