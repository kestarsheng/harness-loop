package service

import (
	"os"
	"strings"

	"agent-platform/pkg/harnessloop/policy"
)

// Stage-mode wiring between the harness policy layer and the agent engine.
//
// The stage matrix (pkg/config/harness_loop/stage_matrix.yaml) is the single
// source of truth for "which tool tag is allowed in which stage". harnessloop
// enforces it at the gate; agent-service reads the same file and publishes the
// resulting tool subset to the engine, so the model is offered exactly the tools
// that would survive the gate. Offering more would let a legal-looking tool call
// fail the whole attempt at the gate; offering fewer means no tool loop at all.
const (
	// stageToolListVar mirrors agent.StageToolListVar: comma-separated MCP tool
	// names the current stage may call.
	stageToolListVar = "__stage_tools__"
	// stageVar is the stage letter context var the harness loop sends.
	stageVar = "__stage__"
	// stageMatrixEnv points at the stage matrix file. The default matches the
	// path harnessloop uses, so both services read the same policy on a dev box.
	stageMatrixEnv     = "AGENT_STAGE_MATRIX_FILE"
	stageMatrixDefault = "pkg/config/harness_loop/stage_matrix.yaml"
	// execTool is the MCP tool that carries the exec tag (harness name
	// run_command). It is granted whenever a stage's exec list is non-empty.
	execTool = "code_execute"
)

// harnessToMCPTool is the inverse of mcpToHarnessTool in tool_name_map.go. The
// stage matrix speaks harness names (read_file/run_command); the tool registry
// and the model speak MCP names (file_read/code_execute).
var harnessToMCPTool = map[string]string{
	"read_file":   "file_read",
	"write_file":  "file_write",
	"run_command": "code_execute",
}

// mcpToolForHarnessName maps one stage-matrix entry onto a callable MCP tool.
// Entries with no MCP counterpart (list_dir, grep, edit_file, apply_patch,
// mkdir) return false: the registry cannot serve them, so the model must not be
// told they exist.
func mcpToolForHarnessName(name string) (string, bool) {
	mapped, ok := harnessToMCPTool[name]
	if !ok {
		return "", false
	}
	return mapped, true
}

// stageToolsFromMatrix returns the MCP tools a stage may call, derived from the
// matrix's read/write/exec lists. Order is deterministic (read, write, exec) so
// tests and logs stay stable.
//
// Note the asymmetry between the three lists: `read` and `write` name tools,
// while `exec` names *allowed commands* (go, gofmt, ...). A non-empty exec list
// therefore grants the single exec-capable tool, and the command itself is still
// checked by the exec whitelist at the gate.
func stageToolsFromMatrix(matrix policy.Matrix, stage string) []string {
	rule, ok := matrix.Rule(stage)
	if !ok || rule.DenyAll {
		return nil
	}
	out := make([]string, 0, len(rule.Read)+len(rule.Write)+1)
	seen := make(map[string]bool, cap(out))
	for _, group := range [][]string{rule.Read, rule.Write} {
		for _, harnessName := range group {
			tool, ok := mcpToolForHarnessName(harnessName)
			if !ok || seen[tool] {
				continue
			}
			seen[tool] = true
			out = append(out, tool)
		}
	}
	if len(rule.Exec) > 0 && !seen[execTool] {
		out = append(out, execTool)
	}
	return out
}

// loadStageMatrix loads the stage matrix from path, falling back to the built-in
// default when the file is missing or unreadable. A missing file must not block
// stage execution: the matrix default is the same policy harnessloop ships.
func loadStageMatrix(path string) policy.Matrix {
	matrix, err := policy.LoadMatrix(path)
	if err != nil {
		return policy.DefaultMatrix()
	}
	return matrix
}

// stageMatrixPath resolves the matrix path from the environment.
func stageMatrixPath() string {
	if p := strings.TrimSpace(os.Getenv(stageMatrixEnv)); p != "" {
		return p
	}
	return stageMatrixDefault
}

// annotateStageContextVars publishes the stage tool subset into the request's
// context vars so the engine can narrow the agent's tool set. It is a no-op
// outside stage mode, and it never overwrites a value the caller set.
func annotateStageContextVars(vars map[string]any, matrix policy.Matrix) {
	if vars == nil {
		return
	}
	stage, _ := vars[stageVar].(string)
	if strings.TrimSpace(stage) == "" {
		return
	}
	if _, exists := vars[stageToolListVar]; exists {
		return
	}
	tools := stageToolsFromMatrix(matrix, strings.TrimSpace(strings.ToLower(stage)))
	vars[stageToolListVar] = strings.Join(tools, ",")
}
