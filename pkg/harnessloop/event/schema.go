package event

import (
	"fmt"
	"regexp"
	"time"
)

const (
	CurrentSchemaVersion = "1.0.0"
	MaxPayloadBytes      = 8 * 1024
)

type Event struct {
	EventID       string
	EventType     EventType
	SchemaVersion string
	TaskID        string
	TraceID       string
	SpanID        string
	Stage         string
	Attempt       int
	Seq           int64
	Timestamp     time.Time
	Severity      Severity
	TenantID      string
	ProjectID     string
	AgentID       string
	Payload       []byte
	PayloadRef    string
	ParentTaskID  string
	SubtaskID     string
	HandoffID     string
	DagID         string
	DagNodeID     string
}

type ValidationError struct {
	Code  string
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s: %s (field=%s)", e.Code, e.Msg, e.Field)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

const (
	CodeMissingField     = "E_EVENT_MISSING_FIELD"
	CodeUnknownType      = "E_EVENT_UNKNOWN_TYPE"
	CodeInvalidSeverity  = "E_EVENT_INVALID_SEVERITY"
	CodeInvalidSeq       = "E_EVENT_INVALID_SEQ"
	CodePayloadTooLarge  = "E_EVENT_PAYLOAD_TOO_LARGE"
	CodePayloadEmpty     = "E_EVENT_PAYLOAD_EMPTY"
	CodeSecretInPayload  = "E_EVENT_SECRET_IN_PAYLOAD"
	CodeInvalidSchemaVer = "E_EVENT_INVALID_SCHEMA_VERSION"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bsk-[a-z0-9]{16,}`),
	regexp.MustCompile(`(?i)"?\b(api[_-]?key|access[_-]?key|secret[_-]?key|password|passwd|token)\b"?\s*[:=]\s*"?[A-Za-z0-9_\-]{12,}`),
	regexp.MustCompile(`-----BEGIN (RSA|OPENSSH|EC|DSA) PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)authorization\s*:\s*bearer\s+\S+`),
}

func (e *Event) Validate() error {
	if e.EventID == "" {
		return &ValidationError{Code: CodeMissingField, Field: "event_id", Msg: "event_id is required"}
	}
	if e.EventType == "" {
		return &ValidationError{Code: CodeMissingField, Field: "event_type", Msg: "event_type is required"}
	}
	if !IsKnownType(e.EventType) {
		return &ValidationError{Code: CodeUnknownType, Field: "event_type", Msg: fmt.Sprintf("event type %q is not registered", e.EventType)}
	}
	if e.SchemaVersion == "" {
		return &ValidationError{Code: CodeMissingField, Field: "schema_version", Msg: "schema_version is required"}
	}
	if e.SchemaVersion != CurrentSchemaVersion {
		return &ValidationError{Code: CodeInvalidSchemaVer, Field: "schema_version", Msg: fmt.Sprintf("schema_version %q is not supported", e.SchemaVersion)}
	}
	if e.TaskID == "" {
		return &ValidationError{Code: CodeMissingField, Field: "task_id", Msg: "task_id is required"}
	}
	if e.Seq <= 0 {
		return &ValidationError{Code: CodeInvalidSeq, Field: "seq", Msg: fmt.Sprintf("seq %d must be positive", e.Seq)}
	}
	if e.Timestamp.IsZero() {
		return &ValidationError{Code: CodeMissingField, Field: "timestamp", Msg: "timestamp is required"}
	}
	if e.Severity == "" {
		return &ValidationError{Code: CodeMissingField, Field: "severity", Msg: "severity is required"}
	}
	if !e.Severity.Valid() {
		return &ValidationError{Code: CodeInvalidSeverity, Field: "severity", Msg: fmt.Sprintf("severity %q is invalid", e.Severity)}
	}
	if e.TenantID == "" {
		return &ValidationError{Code: CodeMissingField, Field: "tenant_id", Msg: "tenant_id is required"}
	}
	if e.ProjectID == "" {
		return &ValidationError{Code: CodeMissingField, Field: "project_id", Msg: "project_id is required"}
	}
	if len(e.Payload) == 0 {
		return &ValidationError{Code: CodePayloadEmpty, Field: "payload", Msg: "payload is required"}
	}
	if len(e.Payload) > MaxPayloadBytes {
		return &ValidationError{
			Code:  CodePayloadTooLarge,
			Field: "payload",
			Msg:   fmt.Sprintf("payload is %d bytes, limit is %d", len(e.Payload), MaxPayloadBytes),
		}
	}
	if e.containsSecret() {
		return &ValidationError{Code: CodeSecretInPayload, Field: "payload", Msg: "payload appears to contain a secret"}
	}
	return nil
}

func (e *Event) containsSecret() bool {
	for _, p := range secretPatterns {
		if p.Match(e.Payload) {
			return true
		}
	}
	return false
}

func (e *Event) Clone() *Event {
	cp := *e
	if e.Payload != nil {
		cp.Payload = append([]byte(nil), e.Payload...)
	}
	return &cp
}
