package service

import (
	"strings"
	"testing"
)

func hasFindingsOf(t *testing.T, findings []Finding, name string, severity Severity) bool {
	t.Helper()
	for _, f := range findings {
		if f.Name == name && f.Severity == severity {
			return true
		}
	}
	return false
}

// healthyInput is a wiring that must pass every check.
func healthyInput() SelfCheckInput {
	return SelfCheckInput{
		ListenAddress:       "127.0.0.1:50013",
		AgentTarget:         "127.0.0.1:50006",
		MetricsAddress:      ":9108",
		OrchestratorEnabled: true,
		SubtaskExecutor:     true,
		SubtaskRepo:         true,
		StageRunner:         true,
		PolicyLayersEnabled: true,
		ApprovalGate:        true,
		MemoryRecall:        true,
	}
}

func TestStartupSelfCheckHealthy(t *testing.T) {
	if findings := StartupSelfCheck(healthyInput()); len(findings) != 0 {
		t.Fatalf("healthy wiring reported %v", findings)
	}
	if fatal := FatalFindings(nil); len(fatal) != 0 {
		t.Fatalf("fatal findings on nil input = %v", fatal)
	}
}

// The three fatal cases are the ones that used to surface as "服务秒退" with no
// way to tell them apart from a crash.
func TestStartupSelfCheckFatalCases(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*SelfCheckInput)
		find   string
	}{
		{"empty listen address", func(in *SelfCheckInput) { in.ListenAddress = "  " }, "listen_address"},
		{"unparseable listen address", func(in *SelfCheckInput) { in.ListenAddress = "127.0.0.1" }, "listen_address"},
		{"agent target loops back to us", func(in *SelfCheckInput) { in.AgentTarget = "127.0.0.1:50013" }, "agent_target"},
		{"metrics collides with grpc port", func(in *SelfCheckInput) { in.MetricsAddress = ":50013" }, "metrics_address"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := healthyInput()
			tc.mutate(&in)
			findings := StartupSelfCheck(in)
			if !hasFindingsOf(t, findings, tc.find, SeverityFatal) {
				t.Fatalf("findings = %v, want a fatal %s", findings, tc.find)
			}
			if fatal := FatalFindings(findings); len(fatal) == 0 {
				t.Fatal("FatalFindings dropped a fatal finding")
			}
		})
	}
}

// A capability that is switched on but has nothing wired to it must be reported,
// not silently no-op'd: that is how "b accepted but subtasks=0" stayed hidden.
func TestStartupSelfCheckOrchestratorWarnings(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*SelfCheckInput)
		find   string
	}{
		{"no executor", func(in *SelfCheckInput) { in.SubtaskExecutor = false }, "orchestrator_executor"},
		{"no stage runner", func(in *SelfCheckInput) { in.StageRunner = false }, "orchestrator_stages"},
		{"no subtask repo", func(in *SelfCheckInput) { in.SubtaskRepo = false }, "orchestrator_subtask_repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := healthyInput()
			tc.mutate(&in)
			findings := StartupSelfCheck(in)
			if !hasFindingsOf(t, findings, tc.find, SeverityWarn) {
				t.Fatalf("findings = %v, want a warn %s", findings, tc.find)
			}
			if fatal := FatalFindings(findings); len(fatal) != 0 {
				t.Fatalf("a dark capability must not block startup: %v", fatal)
			}
		})
	}
}

func TestStartupSelfCheckPolicyWithoutApprovalGate(t *testing.T) {
	in := healthyInput()
	in.ApprovalGate = false
	if findings := StartupSelfCheck(in); !hasFindingsOf(t, findings, "policy_approval_gate", SeverityWarn) {
		t.Fatalf("findings = %v, want a warn about the missing approval gate", findings)
	}
}

// Flags off means the whole P2 block is irrelevant; the checks must not invent
// findings for paths that are not running.
func TestStartupSelfCheckLegacyPathIsQuiet(t *testing.T) {
	in := SelfCheckInput{ListenAddress: "127.0.0.1:50013", AgentTarget: "127.0.0.1:50006", MetricsAddress: "off"}
	if findings := StartupSelfCheck(in); len(findings) != 0 {
		t.Fatalf("legacy path reported %v", findings)
	}
}

func TestFormatFindings(t *testing.T) {
	if got := FormatFindings(nil); got != "selfcheck: all checks passed" {
		t.Fatalf("empty format = %q", got)
	}
	got := FormatFindings([]Finding{{Name: "listen_address", Severity: SeverityFatal, Detail: "boom"}})
	if !strings.Contains(got, "fatal[listen_address]") {
		t.Fatalf("format = %q, want the severity and check name", got)
	}
}
