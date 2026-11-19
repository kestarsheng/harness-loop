package cmd

import (
	"bytes"
	"os"
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

// loopSource is the static surface of the T-P2-40 decoupling. The command owns
// a TUI wired to goroutines and a live 50013 stack, so the split is asserted
// against the source rather than by driving the terminal.
func loopSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("loop.go")
	if err != nil {
		t.Fatalf("read loop.go: %v", err)
	}
	return string(raw)
}

// loop must not compute or hand a board URL to the TUI: `loop start` is the
// only command that prints it.
func TestLoopNeverPassesBoardURLToTUI(t *testing.T) {
	src := loopSource(t)
	if strings.Contains(src, "tui.URLFor") {
		t.Fatal("loop.go must not call tui.URLFor (the board URL belongs to `loop start`)")
	}
	if strings.Contains(src, "monitor=%s") {
		t.Fatal("loop.go must not print monitor=<url>")
	}
	if strings.Contains(src, "resolveWebBaseURL") {
		t.Fatal("loop.go must not resolve the web base URL")
	}
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "WebURL:") {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(trimmed, "WebURL:")) != `"",` {
			t.Fatalf("loop.go:%d passes a non-empty WebURL to the TUI: %q", i+1, trimmed)
		}
	}
	// Both TUI entry points must still exist, and both must be URL-free.
	if got := strings.Count(src, "WebURL:"); got != 2 {
		t.Fatalf("WebURL assignments = %d, want 2 (RunSession + Run)", got)
	}
}

// A cold `loop` starts infra+agent only, so it never launches vite.
func TestLoopEnsureBackendStartsInfraAndAgentGroups(t *testing.T) {
	src := loopSource(t)
	want := "m.EnsureGroups(cmd.Context(), devstack.GroupInfra, devstack.GroupAgent)"
	if !strings.Contains(src, want) {
		t.Fatalf("loop.go must call %s", want)
	}
	if strings.Contains(src, "m.Ensure(cmd.Context())") {
		t.Fatal("loop.go must not call the whole-stack m.Ensure")
	}
}

// The help text is the contract users read: loop is the agent session only,
// loop start is the board and its URL.
func TestLoopHelpTextsAreDecoupled(t *testing.T) {
	root := NewRootCmd()

	loop, _, err := root.Find([]string{"loop"})
	if err != nil {
		t.Fatalf("find loop: %v", err)
	}
	if strings.Contains(loop.Short, "monitor board") {
		t.Fatalf("loop Short still advertises the board: %q", loop.Short)
	}
	if !strings.Contains(loop.Long, "TUI") {
		t.Fatalf("loop Long must say it opens the agent session TUI:\n%s", loop.Long)
	}
	if !strings.Contains(loop.Long, "loop start") {
		t.Fatalf("loop Long must point at `loop start` for the board:\n%s", loop.Long)
	}

	start, _, err := root.Find([]string{"loop", "start"})
	if err != nil || start == nil || start.Name() != "start" {
		t.Fatalf("loop start missing: %v %v", start, err)
	}
	if !strings.Contains(start.Short, "URL") {
		t.Fatalf("loop start Short must advertise the URL: %q", start.Short)
	}
	if !strings.Contains(start.Long, "does not open the agent session TUI") {
		t.Fatalf("loop start Long must say it does not open the TUI:\n%s", start.Long)
	}
}
