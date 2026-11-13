package service

import (
	"context"
	"encoding/json"

	"agent-platform/pkg/agent"
)

const stageFinishToolName = "stage_finish"

type stageFinishExecutor struct {
	inner agent.ToolExecutor
}

func (e *stageFinishExecutor) Execute(ctx context.Context, toolName string, arguments json.RawMessage, toolConfig *agent.ToolSpecificConfig) (string, error) {
	if toolName == stageFinishToolName {
		return `{"status":"finished"}`, nil
	}
	if e.inner == nil {
		return "", nil
	}
	return e.inner.Execute(ctx, toolName, arguments, toolConfig)
}

func (e *stageFinishExecutor) ListTools(ctx context.Context) (map[string]any, error) {
	var tools map[string]any
	if e.inner != nil {
		if loaded, err := e.inner.ListTools(ctx); err == nil && loaded != nil {
			tools = loaded
		}
	}
	if tools == nil {
		tools = map[string]any{}
	}
	tools[stageFinishToolName] = map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        stageFinishToolName,
			"description": "Signal that the current harness stage is complete. Required in stage mode; text-only completion is rejected.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"attempt_id": map[string]any{"type": "string", "description": "attempt id from the stage message"},
					"stage":      map[string]any{"type": "string", "description": "current stage letter (a/b/c/d)"},
					"summary":    map[string]any{"type": "string", "description": "what was completed in this stage"},
				},
				"required": []string{"attempt_id", "stage", "summary"},
			},
		},
	}
	return tools, nil
}
