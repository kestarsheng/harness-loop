package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-platform/pkg/harnessloop/policy"
)

func TestStageToolsFromDefaultMatrix(t *testing.T) {
	matrix := policy.DefaultMatrix()
	cases := []struct {
		stage string
		want  []string
	}{
		// a/b are read-only: the executor only exposes file_read among the
		// matrix entries (list_dir/grep have no MCP counterpart).
		{"a", []string{"file_read"}},
		{"b", []string{"file_read"}},
		// c may write and exec.
		{"c", []string{"file_read", "file_write", "code_execute"}},
		// d may read and exec, but not write.
		{"d", []string{"file_read", "code_execute"}},
		// e denies everything.
		{"e", nil},
	}
	for _, tc := range cases {
		got := stageToolsFromMatrix(matrix, tc.stage)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("stage %s tools = %v, want %v", tc.stage, got, tc.want)
		}
	}
}

func TestStageToolsFromShippedMatrix(t *testing.T) {
	// The engine must offer exactly what the shipped matrix allows, otherwise a
	// stage either gets no tool loop or calls something the gate rejects.
	path := filepath.Join("..", "..", "..", "..", "pkg", "config", "harness_loop", "stage_matrix.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("stage matrix not available: %v", err)
	}
	matrix, err := policy.LoadMatrix(path)
	if err != nil {
		t.Fatalf("load matrix: %v", err)
	}
	for stage, rule := range matrix.Stages {
		tools := stageToolsFromMatrix(matrix, stage)
		if rule.DenyAll {
			if len(tools) != 0 {
				t.Fatalf("stage %s denies all tools but got %v", stage, tools)
			}
			continue
		}
		if len(rule.Read) > 0 && !containsString(tools, "file_read") {
			t.Fatalf("stage %s allows read_file but the engine offers %v", stage, tools)
		}
		if len(rule.Write) == 0 && containsString(tools, "file_write") {
			t.Fatalf("stage %s must not offer file_write: %v", stage, tools)
		}
		if len(rule.Exec) == 0 && containsString(tools, "code_execute") {
			t.Fatalf("stage %s must not offer code_execute: %v", stage, tools)
		}
	}
}

func TestHarnessToMCPToolIsInverseOfStreamName(t *testing.T) {
	// agent-service publishes the harness name on the stream (harnessToolName)
	// and the MCP name to the tool registry. They must agree in both directions.
	for mcp, harness := range mcpToHarnessTool {
		back, ok := mcpToolForHarnessName(harness)
		if !ok || back != mcp {
			t.Fatalf("round trip %s -> %s -> (ok=%v, %s)", mcp, harness, ok, back)
		}
	}
	if _, ok := mcpToolForHarnessName("list_dir"); ok {
		t.Fatal("list_dir has no MCP tool and must not be granted")
	}
}

func TestAnnotateStageContextVars(t *testing.T) {
	matrix := policy.DefaultMatrix()

	vars := map[string]any{stageVar: "c"}
	annotateStageContextVars(vars, matrix)
	if got := vars[stageToolListVar]; got != "file_read,file_write,code_execute" {
		t.Fatalf("stage c tool list = %v", got)
	}

	// Not stage mode: nothing added.
	plain := map[string]any{}
	annotateStageContextVars(plain, matrix)
	if _, ok := plain[stageToolListVar]; ok {
		t.Fatalf("non-stage request must not carry a stage tool list: %v", plain)
	}

	// A caller-provided value wins.
	preset := map[string]any{stageVar: "a", stageToolListVar: "file_write"}
	annotateStageContextVars(preset, matrix)
	if preset[stageToolListVar] != "file_write" {
		t.Fatalf("preset tool list overwritten: %v", preset[stageToolListVar])
	}

	// Unknown stage: empty list, never a guess.
	unknown := map[string]any{stageVar: "z"}
	annotateStageContextVars(unknown, matrix)
	if unknown[stageToolListVar] != "" {
		t.Fatalf("unknown stage must yield an empty list: %v", unknown[stageToolListVar])
	}
}

func TestLoadStageMatrixFallsBackToDefault(t *testing.T) {
	matrix := loadStageMatrix(filepath.Join("does", "not", "exist.yaml"))
	if _, ok := matrix.Rule("a"); !ok {
		t.Fatalf("fallback matrix lost its stages: %+v", matrix)
	}
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
