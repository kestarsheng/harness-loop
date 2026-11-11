package agent

import (
	"encoding/json"
	"strings"
)

// StageFinishMarker is the plain-text escape hatch used when the upstream model
// cannot emit structured tool calls. The marker plus a strict JSON object is
// the only accepted text form; free-form "I am done" never counts as a finish.
const StageFinishMarker = "[stage_finish]"

type stageFinishProtocol struct {
	AttemptID string `json:"attempt_id"`
	Stage     string `json:"stage"`
	Summary   string `json:"summary"`
}

func ParseStageFinishProtocol(content string) (json.RawMessage, bool) {
	idx := strings.Index(content, StageFinishMarker)
	if idx < 0 {
		return nil, false
	}
	rest := content[idx+len(StageFinishMarker):]
	start := strings.Index(rest, "{")
	if start < 0 {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(rest[start:]))
	var payload stageFinishProtocol
	if err := dec.Decode(&payload); err != nil {
		return nil, false
	}
	payload.AttemptID = strings.TrimSpace(payload.AttemptID)
	payload.Stage = strings.TrimSpace(strings.ToLower(payload.Stage))
	payload.Summary = strings.TrimSpace(payload.Summary)
	if payload.AttemptID == "" || payload.Summary == "" {
		return nil, false
	}
	if len(payload.Stage) != 1 || !strings.Contains("abcde", payload.Stage) {
		return nil, false
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func StageFinishProtocolInstruction() string {
	return "\nIf the model cannot emit tool calls, finish the stage by outputting exactly one line: " +
		StageFinishMarker + ` {"attempt_id":"<attempt id from the stage message>","stage":"<a|b|c|d>","summary":"<what you completed>"}` +
		"\nOnly this strict form is accepted; free-form statements such as \"I am done\" are treated as not finished."
}
