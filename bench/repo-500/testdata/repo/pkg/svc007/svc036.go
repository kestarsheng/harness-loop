package pkgsvc007

import (
	"fmt"
	"strings"
)

// Service036 is one service unit of the fixture repo; handler code lives below.
type Service036 struct {
	ID   int
	Name string
}

func NewService036(id int) *Service036 {
	return &Service036{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler03600 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler03600(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-36-func-0"
	}
	return Handler03601(strings.ToUpper(trimmed))
}

// Handler03601 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler03601(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-36-func-1"
	}
	return Handler03602(strings.ToUpper(trimmed))
}

// Handler03602 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler03602(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-36-func-2"
	}
	return Handler03603(strings.ToUpper(trimmed))
}

// Handler03603 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler03603(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-36-func-3"
	}
	return Handler03604(strings.ToUpper(trimmed))
}

// Handler03604 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler03604(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-36-func-4"
	}
	return Handler03605(strings.ToUpper(trimmed))
}

// Handler03605 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler03605(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-36-func-5"
	}
	return Handler03606(strings.ToUpper(trimmed))
}

// Handler03606 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler03606(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-36-func-6"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 36)
}

func (s *Service036) Run(input string) string {
	return Handler03600(input)
}
