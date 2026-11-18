package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoopCommandRegistered(t *testing.T) {
	root := NewRootCmd()
	loop, args, err := root.Find([]string{"loop"})
	if err != nil {
		t.Fatalf("find loop: %v", err)
	}
	if loop == nil || loop.Name() != "loop" {
		t.Fatalf("loop command not registered: %v", loop)
	}
	if len(args) != 0 {
		t.Fatalf("unexpected args: %v", args)
	}
	for _, name := range []string{"goal", "goal-file", "workspace", "project", "budget-tokens", "budget-cost", "timeout", "exit-on-complete"} {
		if loop.Flags().Lookup(name) == nil {
			t.Fatalf("loop flag --%s missing", name)
		}
	}
	if got := loop.Flags().Lookup("timeout").DefValue; got != "0s" {
		t.Fatalf("loop timeout default = %s", got)
	}
	if got := loop.Flags().Lookup("exit-on-complete").DefValue; got != "false" {
		t.Fatalf("loop exit-on-complete default = %s", got)
	}

	run, _, err := root.Find([]string{"run"})
	if err != nil || run == nil || run.Name() != "run" {
		t.Fatalf("run command must be preserved: %v %v", run, err)
	}
}

func TestHelpUsesCurrentDefaults(t *testing.T) {
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("help: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "127.0.0.1:50013") {
		t.Fatalf("help must advertise the harnessloop-service port:\n%s", out)
	}
	if strings.Contains(out, "50008") {
		t.Fatalf("help still contains the stale 50008 default:\n%s", out)
	}
	if !strings.Contains(out, "web-base-url") {
		t.Fatalf("help must expose --web-base-url:\n%s", out)
	}
}

func TestLoopRequiresInteractiveTerminal(t *testing.T) {
	resetOpts()
	t.Cleanup(resetOpts)

	root := NewRootCmd()
	root.SetArgs([]string{"loop", "--goal", "demo"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("non-TTY loop must fail closed, got %v", err)
	}
}
