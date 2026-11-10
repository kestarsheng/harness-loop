package fsm

import (
	"encoding/json"
	"fmt"
	"sync"
)

const (
	StageFinishToolName = "stage_finish"

	ErrInvalidFinishPayload = "E_INVALID_FINISH_PAYLOAD"
	ErrStaleFinish          = "E_STALE_FINISH"
	ErrStageMismatch        = "E_STAGE_MISMATCH"
	ErrFinishOnTerminal     = "E_FINISH_ON_TERMINAL"
)

type ArtifactRef struct {
	Path string `json:"path"`
	Hash string `json:"hash,omitempty"`
	Size int64  `json:"size,omitempty"`
}

type StageFinishArgs struct {
	AttemptID string        `json:"attempt_id"`
	Stage     string        `json:"stage"`
	Summary   string        `json:"summary"`
	Artifacts []ArtifactRef `json:"artifacts,omitempty"`
}

func IsStageFinishTool(name string) bool { return name == StageFinishToolName }

func ParseStageFinish(raw []byte) (StageFinishArgs, error) {
	if len(raw) == 0 {
		return StageFinishArgs{}, &Error{Code: ErrInvalidFinishPayload, Msg: "stage_finish payload is empty"}
	}
	var args StageFinishArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return StageFinishArgs{}, &Error{Code: ErrInvalidFinishPayload, Msg: "stage_finish payload is not valid json"}
	}
	if args.AttemptID == "" {
		return StageFinishArgs{}, &Error{Code: ErrInvalidFinishPayload, Msg: "attempt_id is required"}
	}
	if args.Stage == "" {
		return StageFinishArgs{}, &Error{Code: ErrInvalidFinishPayload, Msg: "stage is required"}
	}
	return args, nil
}

type FinishResult struct {
	Accepted  bool
	Duplicate bool
	AttemptID string
	Stage     Stage
	Summary   string
}

type FinishGuard struct {
	mu        sync.Mutex
	machine   *Machine
	attemptID string
	received  bool
}

func NewFinishGuard(m *Machine, attemptID string) *FinishGuard {
	return &FinishGuard{machine: m, attemptID: attemptID}
}

func (g *FinishGuard) AttemptID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.attemptID
}

func (g *FinishGuard) Received() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.received
}

func (g *FinishGuard) Validate(args StageFinishArgs) (FinishResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	current := g.machine.Stage()
	if current == StageDone {
		return FinishResult{}, &Error{Code: ErrFinishOnTerminal, Msg: "task already reached stage e"}
	}
	if args.AttemptID != g.attemptID {
		return FinishResult{}, &Error{
			Code: ErrStaleFinish,
			Msg:  fmt.Sprintf("attempt_id %q does not belong to the current attempt %q", args.AttemptID, g.attemptID),
		}
	}
	stage, err := ParseStage(args.Stage)
	if err != nil {
		return FinishResult{}, err
	}
	if stage != current {
		return FinishResult{}, &Error{
			Code: ErrStageMismatch,
			From: current,
			To:   stage,
			Msg:  fmt.Sprintf("finish stage %q does not match current stage %q", stage, current),
		}
	}
	if g.received {
		return FinishResult{Accepted: true, Duplicate: true, AttemptID: args.AttemptID, Stage: stage, Summary: args.Summary}, nil
	}
	g.received = true
	return FinishResult{Accepted: true, AttemptID: args.AttemptID, Stage: stage, Summary: args.Summary}, nil
}

func InvalidFinishEvent(attemptID string, stage Stage, reason string) PendingEvent {
	return PendingEvent{
		Type:     "invalid_finish",
		Severity: "warn",
		From:     stage,
		To:       stage,
		Detail:   fmt.Sprintf("attempt_id=%s reason=%s", attemptID, reason),
	}
}
