package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// InjectedItem is one explicit runtime injection for the receiver. Only
// contract fields and runtime.injection:* sources are legal; anything that
// would replay the sender's transcript is rejected (REQ-132).
type InjectedItem struct {
	Source  string
	Content string
}

// ExecutionMessage is one message of the receiver's freshly built context.
type ExecutionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Source  string `json:"source"`
}

// HandoffContext is the receiver input snapshot. It is built from zero; the
// sender's messages are never copied.
type HandoffContext struct {
	HandoffID    string             `json:"handoff_id"`
	Namespace    string             `json:"namespace"`
	Messages     []ExecutionMessage `json:"messages"`
	InjectedFrom []string           `json:"injected_from"`
	InputDigest  string             `json:"input_digest"`
}

// HandoffEnvelope is what the a2a-service adapter receives. The orchestrator
// never opens its own channel: transport is a port implemented over
// a2a-service SendTask (design 4.13.4).
type HandoffEnvelope struct {
	HandoffID    string
	ParentTaskID string
	SubtaskID    string
	FromAgentID  string
	ToAgentID    string
	TenantID     string
	ProjectID    string
	Metadata     map[string]string
}

type HandoffTransport interface {
	SendHandoff(ctx context.Context, envelope HandoffEnvelope) error
}

var forbiddenInjectionTokens = []string{
	"execution_history",
	"history",
	"checkpoint",
	"workspace_snapshot",
	"transcript",
	"raw_messages",
	"conversation",
}

// BuildHandoffContext creates the receiver snapshot from the contract only.
func BuildHandoffContext(ctx context.Context, h *HandoffContract, injected []InjectedItem, emitter Emitter) (*HandoffContext, error) {
	if h == nil {
		return nil, newError(CodeHandoffIncomplete, "handoff contract is nil")
	}
	namespace := HandoffNamespace(h.HandoffID)
	summary, err := json.Marshal(h.ContextSummary)
	if err != nil {
		return nil, newError(CodeHandoffIncomplete, "encode context summary: %v", err)
	}
	unfinished, err := json.Marshal(h.Unfinished)
	if err != nil {
		return nil, newError(CodeHandoffIncomplete, "encode unfinished list: %v", err)
	}
	constraints, err := json.Marshal(h.Constraints)
	if err != nil {
		return nil, newError(CodeHandoffIncomplete, "encode constraints: %v", err)
	}

	out := &HandoffContext{
		HandoffID: h.HandoffID,
		Namespace: namespace,
		Messages: []ExecutionMessage{
			{Role: "system", Content: string(constraints), Source: "contract.constraints"},
			{Role: "user", Content: string(summary), Source: "contract.context_summary"},
			{Role: "user", Content: string(unfinished), Source: "contract.unfinished"},
		},
		InjectedFrom: []string{"contract.constraints", "contract.context_summary", "contract.unfinished"},
	}
	for _, item := range injected {
		if err := validateInjectionSource(h, item.Source); err != nil {
			if emitter != nil {
				if emitErr := emitSecurityViolation(ctx, emitter, h.ParentTaskID, h.SubtaskID, h.HandoffID,
					CodeHandoffHistoryForbid, "receiver context tried to read sender history"); emitErr != nil {
					return nil, emitErr
				}
			}
			return nil, err
		}
		out.Messages = append(out.Messages, ExecutionMessage{
			Role:    "user",
			Content: item.Content,
			Source:  item.Source,
		})
		out.InjectedFrom = append(out.InjectedFrom, item.Source)
	}
	digest, err := digestMessages(out.Messages)
	if err != nil {
		return nil, err
	}
	out.InputDigest = digest
	return out, nil
}

func validateInjectionSource(h *HandoffContract, source string) error {
	normalized := strings.ToLower(strings.TrimSpace(source))
	if normalized == "" {
		return newError(CodeHandoffHistoryForbid, "injected source must be explicit")
	}
	for _, token := range forbiddenInjectionTokens {
		if strings.Contains(normalized, token) {
			return newError(CodeHandoffHistoryForbid,
				"injected source %q would share sender execution history", source)
		}
	}
	if strings.HasPrefix(normalized, "memory:") || strings.HasPrefix(normalized, "memory/") {
		own := strings.ToLower(HandoffNamespace(h.HandoffID))
		namespace := strings.TrimPrefix(strings.TrimPrefix(normalized, "memory:"), "memory/")
		if !strings.HasPrefix(namespace, own) {
			return newError(CodeHandoffHistoryForbid,
				"injected source %q reads a memory namespace outside %s", source, own)
		}
		return nil
	}
	if strings.HasPrefix(normalized, "contract.") || strings.HasPrefix(normalized, "runtime.injection:") {
		return nil
	}
	return newError(CodeHandoffHistoryForbid,
		"injected source %q is not on the handoff whitelist", source)
}

// HandoffNamespace is the receiver's isolated memory namespace.
func HandoffNamespace(handoffID string) string {
	return "handoff/" + handoffID + "/"
}

// crossStageTransitions is the legal cross-stage handoff matrix (design 4.13.4:
// "由 verifier 接手 coder 的产物" is the canonical c→d case). Anything not
// listed is refused rather than guessed.
var crossStageTransitions = map[string]map[string]struct{}{
	"a": {"b": {}},
	"b": {"c": {}},
	"c": {"d": {}},
}

// CrossStageAllowed reports whether a handoff may move from one stage to another.
func CrossStageAllowed(fromStage, toStage string) bool {
	from := strings.ToLower(strings.TrimSpace(fromStage))
	to := strings.ToLower(strings.TrimSpace(toStage))
	if from == "" || to == "" {
		return false
	}
	if from == to {
		return true
	}
	targets, ok := crossStageTransitions[from]
	if !ok {
		return false
	}
	_, ok = targets[to]
	return ok
}

// StageAwareHandoff is the cross-stage half of a handoff request. The receiver
// (to stage) may differ from the sender's stage; the contract's five elements
// are validated exactly as for a same-stage handoff.
type StageAwareHandoff struct {
	FromStage string
	ToStage   string
}

// ValidateCrossStageHandoff applies the stage-transition rules. A same-stage
// handoff is always acceptable; a cross-stage one must be a listed transition
// and must name both stages explicitly (no implicit "next stage").
func ValidateCrossStageHandoff(req HandoffRequest) error {
	from := strings.ToLower(strings.TrimSpace(req.Stage))
	to := strings.ToLower(strings.TrimSpace(req.ToStage))
	if to == "" || to == from {
		return nil
	}
	if from == "" {
		return newError(CodeCrossStageDenied,
			"a cross-stage handoff to %q must declare the sender stage", req.ToStage)
	}
	if !CrossStageAllowed(from, to) {
		return newError(CodeCrossStageDenied,
			"handoff %s→%s is not a declared cross-stage transition", from, to)
	}
	return nil
}

// BuildCrossStageEnvelope extends the a2a metadata with the stage pair so the
// receiver can resolve stage-level permissions without reading the sender's
// history (REQ-132).
func BuildCrossStageEnvelope(base HandoffEnvelope, fromStage, toStage string) HandoffEnvelope {
	if base.Metadata == nil {
		base.Metadata = map[string]string{}
	}
	if fromStage != "" {
		base.Metadata["from_stage"] = fromStage
	}
	if toStage != "" {
		base.Metadata["to_stage"] = toStage
	}
	return base
}

// ValidateMemoryAccess rejects reads of another handoff's namespace.
func ValidateMemoryAccess(ctx context.Context, h *HandoffContract, namespace string, emitter Emitter) error {
	own := HandoffNamespace(h.HandoffID)
	if strings.HasPrefix(namespace, own) {
		return nil
	}
	if emitter != nil {
		if err := emitSecurityViolation(ctx, emitter, h.ParentTaskID, h.SubtaskID, h.HandoffID,
			CodeHandoffHistoryForbid, "cross-namespace memory read"); err != nil {
			return err
		}
	}
	return newError(CodeHandoffHistoryForbid, "namespace %q is outside %q", namespace, own)
}

func digestMessages(messages []ExecutionMessage) (string, error) {
	raw, err := json.Marshal(messages)
	if err != nil {
		return "", fmt.Errorf("orchestrator: digest handoff messages: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
