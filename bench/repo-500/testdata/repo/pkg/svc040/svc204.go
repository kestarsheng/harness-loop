package pkgsvc040

import (
	"fmt"
	"strings"
)

// Service204 is one service unit of the fixture repo; handler code lives below.
type Service204 struct {
	ID   int
	Name string
}

func NewService204(id int) *Service204 {
	return &Service204{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler20400 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler20400(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-204-func-0"
	}
	return Handler20401(strings.ToUpper(trimmed))
}

// Handler20401 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler20401(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-204-func-1"
	}
	return Handler20402(strings.ToUpper(trimmed))
}

// Handler20402 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler20402(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-204-func-2"
	}
	return Handler20403(strings.ToUpper(trimmed))
}

// Handler20403 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler20403(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-204-func-3"
	}
	return Handler20404(strings.ToUpper(trimmed))
}

// Handler20404 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler20404(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-204-func-4"
	}
	return Handler20405(strings.ToUpper(trimmed))
}

// Handler20405 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler20405(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-204-func-5"
	}
	return Handler20406(strings.ToUpper(trimmed))
}

// Handler20406 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler20406(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-204-func-6"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 204)
}

func (s *Service204) Run(input string) string {
	return Handler20400(input)
}
