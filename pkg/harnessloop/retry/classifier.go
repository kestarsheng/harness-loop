package retry

import (
	"errors"
	"fmt"
	"time"
)

type Category string

const (
	Network           Category = "network"
	Runtime           Category = "runtime"
	System            Category = "system"
	Quota             Category = "quota"
	Timeout           Category = "timeout"
	NoFinish          Category = "no_finish"
	InvalidToolResult Category = "invalid_tool_result"
	VerificationError Category = "verification_error"
	ContentFilter     Category = "content_filter"
)

var allCategories = [9]Category{
	Network, Runtime, System, Quota, Timeout, NoFinish, InvalidToolResult, VerificationError, ContentFilter,
}

func (c Category) Valid() bool {
	for _, v := range allCategories {
		if c == v {
			return true
		}
	}
	return false
}

func Categories() []Category {
	out := make([]Category, len(allCategories))
	copy(out, allCategories[:])
	return out
}

type ErrorKind string

const (
	KindNet      ErrorKind = "net"
	KindGRPC     ErrorKind = "grpc"
	KindHTTP     ErrorKind = "http"
	KindOS       ErrorKind = "os"
	KindPanic    ErrorKind = "panic"
	KindJSON     ErrorKind = "json"
	KindAgent    ErrorKind = "agent"
	KindVerifier ErrorKind = "verifier"
	KindTool     ErrorKind = "tool"
	KindFinish   ErrorKind = "finish_reason"
	KindState    ErrorKind = "state"
)

type ErrorEnvelope struct {
	Kind         ErrorKind     `json:"kind"`
	Code         string        `json:"code,omitempty"`
	HTTPStatus   int           `json:"http_status,omitempty"`
	GRPCStatus   string        `json:"grpc_status,omitempty"`
	RetryAfter   time.Duration `json:"retry_after,omitempty"`
	FinishReason string        `json:"finish_reason,omitempty"`
	EmptyResult  bool          `json:"empty_result,omitempty"`
}

type Classification struct {
	Category  Category
	Retryable bool
	Fallback  bool
	Reason    string
}

func Classify(env ErrorEnvelope) Classification {
	switch env.Kind {
	case KindPanic, KindJSON:
		return Classification{Category: Runtime, Retryable: true, Reason: string(env.Kind)}
	case KindNet:
		return Classification{Category: Network, Retryable: true, Reason: "net error"}
	case KindOS:
		return Classification{Category: System, Retryable: true, Reason: "os error"}
	case KindVerifier:
		return Classification{Category: VerificationError, Retryable: true, Reason: "verifier failure"}
	case KindTool:
		if env.EmptyResult || env.Code != "" {
			return Classification{Category: InvalidToolResult, Retryable: true, Reason: "tool returned empty result or error code"}
		}
		return Classification{Category: InvalidToolResult, Retryable: true, Reason: "tool result rejected by validator"}
	case KindFinish:
		switch normalizeRaw(env.FinishReason) {
		case FinishContentFilter:
			return Classification{Category: ContentFilter, Retryable: false, Reason: "model refused to generate"}
		case FinishLength:
			return Classification{Category: Runtime, Retryable: true, Reason: "context exhausted"}
		case FinishOther:
			return Classification{Category: System, Retryable: true, Reason: "unknown finish reason"}
		}
		return Classification{Category: System, Retryable: true, Reason: "unhandled finish reason"}
	case KindAgent:
		if env.Code == "E_STAGE_NOT_FINISHED" {
			return Classification{Category: NoFinish, Retryable: true, Reason: "stage finished without stage_finish tool call"}
		}
		return Classification{Category: Runtime, Retryable: true, Reason: "agent runtime error"}
	case KindState:
		return Classification{Category: System, Retryable: true, Reason: "state inconsistency"}
	case KindGRPC:
		return classifyGRPC(env)
	case KindHTTP:
		return classifyHTTP(env)
	}

	if env.HTTPStatus > 0 {
		return classifyHTTP(env)
	}
	if env.GRPCStatus != "" {
		return classifyGRPC(env)
	}
	return Classification{Category: System, Retryable: true, Fallback: true, Reason: "unclassified error, fell back to system"}
}

func classifyGRPC(env ErrorEnvelope) Classification {
	switch env.GRPCStatus {
	case "Unavailable", "ResourceExhausted", "Aborted", "Internal":
		return Classification{Category: Network, Retryable: true, Reason: "grpc " + env.GRPCStatus}
	case "DeadlineExceeded":
		return Classification{Category: Timeout, Retryable: true, Reason: "grpc deadline exceeded"}
	case "PermissionDenied", "Unauthenticated", "InvalidArgument":
		return Classification{Category: System, Retryable: false, Reason: "grpc " + env.GRPCStatus}
	}
	return Classification{Category: System, Retryable: true, Fallback: true, Reason: "unknown grpc status"}
}

func classifyHTTP(env ErrorEnvelope) Classification {
	switch {
	case env.HTTPStatus == 429 && isQuotaCode(env.Code):
		return Classification{Category: Quota, Retryable: false, Reason: "insufficient quota"}
	case env.HTTPStatus == 429:
		return Classification{Category: Network, Retryable: true, Reason: "rate limited"}
	case env.HTTPStatus == 408 || env.HTTPStatus == 504:
		return Classification{Category: Timeout, Retryable: true, Reason: "http gateway timeout"}
	case env.HTTPStatus >= 500:
		return Classification{Category: Network, Retryable: true, Reason: "http 5xx"}
	case env.HTTPStatus == 401 || env.HTTPStatus == 403:
		return Classification{Category: System, Retryable: false, Reason: "http auth failure"}
	case env.HTTPStatus >= 400:
		return Classification{Category: System, Retryable: false, Reason: "http 4xx"}
	}
	return Classification{Category: System, Retryable: true, Fallback: true, Reason: "unexpected http status"}
}

func isQuotaCode(code string) bool {
	return code == "insufficient_quota" || code == "quota_exceeded" || code == "E_QUOTA_EXCEEDED"
}

var ErrInvalidEnvelope = errors.New("retry: invalid error envelope")

func (e ErrorEnvelope) Validate() error {
	if e.Kind == "" {
		return fmt.Errorf("%w: kind is required", ErrInvalidEnvelope)
	}
	if e.HTTPStatus > 0 || e.GRPCStatus != "" || e.Kind != "" {
		return nil
	}
	return ErrInvalidEnvelope
}

type ErrorEvent struct {
	TraceID              string
	TaskID               string
	Stage                string
	AttemptID            string
	Category             Category
	Retryable            bool
	RetryIndex           int
	MaxRetries           int
	ClassificationReason string
	Fallback             bool
	OriginalError        string
	OriginalFinishReason string
}

func (e ErrorEvent) Payload() map[string]any {
	return map[string]any{
		"trace_id":               e.TraceID,
		"task_id":                e.TaskID,
		"stage":                  e.Stage,
		"attempt_id":             e.AttemptID,
		"category":               string(e.Category),
		"retryable":              e.Retryable,
		"retry_index":            e.RetryIndex,
		"max_retries":            e.MaxRetries,
		"classification":         classificationLabel(e.Fallback),
		"classification_reason":  e.ClassificationReason,
		"original_error":         e.OriginalError,
		"original_finish_reason": e.OriginalFinishReason,
	}
}

func classificationLabel(fallback bool) string {
	if fallback {
		return "fallback"
	}
	return "exact"
}
