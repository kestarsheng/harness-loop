package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/openai/openai-go"
)

// ErrorKind is the provider-independent classification of an LLM failure.
type ErrorKind string

const (
	ErrorKindCanceled       ErrorKind = "canceled"
	ErrorKindTimeout        ErrorKind = "timeout"
	ErrorKindAuth           ErrorKind = "auth"
	ErrorKindPermission     ErrorKind = "permission"
	ErrorKindRateLimit      ErrorKind = "rate_limit"
	ErrorKindInvalidRequest ErrorKind = "invalid_request"
	ErrorKindNotFound       ErrorKind = "not_found"
	ErrorKindServer         ErrorKind = "server"
	ErrorKindNetwork        ErrorKind = "network"
	ErrorKindStream         ErrorKind = "stream"
	ErrorKindUnknown        ErrorKind = "unknown"
)

// APIError is the normalized error returned by the adapter layer. It preserves
// the underlying error for errors.Is/errors.As and exposes a stable Kind.
type APIError struct {
	Kind         ErrorKind
	StatusCode   int
	ProviderCode string
	ProviderType string
	Message      string
	Err          error
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("llm: %s (status %d): %s", e.Kind, e.StatusCode, msg)
	}
	return fmt.Sprintf("llm: %s: %s", e.Kind, msg)
}

func (e *APIError) Unwrap() error { return e.Err }

// IsKind reports whether err is an APIError with the given kind.
func IsKind(err error, kind ErrorKind) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Kind == kind
}

// classifyError converts SDK, transport and context errors into APIError while
// keeping errors.Is(err, context.Canceled/DeadlineExceeded) working through
// Unwrap.
func classifyError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return err
	}

	if errors.Is(err, context.Canceled) {
		return &APIError{Kind: ErrorKindCanceled, Message: "request canceled", Err: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &APIError{Kind: ErrorKindTimeout, Message: "request timed out", Err: err}
	}

	var sdkErr *openai.Error
	if errors.As(err, &sdkErr) {
		msg := sdkErr.Message
		if msg == "" {
			msg = http.StatusText(sdkErr.StatusCode)
		}
		return &APIError{
			Kind:         classifyStatus(sdkErr.StatusCode),
			StatusCode:   sdkErr.StatusCode,
			ProviderCode: sdkErr.Code,
			ProviderType: sdkErr.Type,
			Message:      msg,
			Err:          err,
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		kind := ErrorKindNetwork
		if netErr.Timeout() {
			kind = ErrorKindTimeout
		}
		return &APIError{Kind: kind, Message: netErr.Error(), Err: err}
	}

	// The SSE decoder surfaces in-band stream errors without a status code.
	if strings.Contains(err.Error(), "received error while streaming") {
		return &APIError{Kind: ErrorKindStream, Message: err.Error(), Err: err}
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.Canceled) {
			return &APIError{Kind: ErrorKindCanceled, Message: "request canceled", Err: ctxErr}
		}
		return &APIError{Kind: ErrorKindTimeout, Message: "request timed out", Err: ctxErr}
	}

	return &APIError{Kind: ErrorKindUnknown, Message: err.Error(), Err: err}
}

func classifyStatus(status int) ErrorKind {
	switch {
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return ErrorKindTimeout
	case status == http.StatusUnauthorized:
		return ErrorKindAuth
	case status == http.StatusForbidden:
		return ErrorKindPermission
	case status == http.StatusTooManyRequests:
		return ErrorKindRateLimit
	case status == http.StatusNotFound:
		return ErrorKindNotFound
	case status >= 500:
		return ErrorKindServer
	case status >= 400:
		return ErrorKindInvalidRequest
	default:
		return ErrorKindUnknown
	}
}
