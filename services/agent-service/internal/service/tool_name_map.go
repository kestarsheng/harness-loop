package service

// The MCP layer and the harness policy layer name the same capability
// differently: MCP exposes `file_write`, while the stage matrix speaks
// `write_file`. services/mcp-service maps between them for its own gate
// (harnessToolName); the harness preflight sees whatever name arrives on the
// stream, so agent-service publishes the canonical harness name there.
//
// Only the streamed tool-call name is normalised. The actual MCP call keeps the
// model's tool name, so tool routing and results are unaffected.
var mcpToHarnessTool = map[string]string{
	"file_read":    "read_file",
	"file_write":   "write_file",
	"code_execute": "run_command",
}

// harnessToolName returns the harness-policy name for a tool the model called.
// Unmapped tools pass through unchanged: the policy registry then decides on
// its own whether it knows them (deny-by-default).
func harnessToolName(name string) string {
	if mapped, ok := mcpToHarnessTool[name]; ok {
		return mapped
	}
	return name
}
