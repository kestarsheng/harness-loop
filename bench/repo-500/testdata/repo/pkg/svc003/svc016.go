package pkgsvc003

import (
	"fmt"
	"strings"
)

// Service016 is one service unit of the fixture repo; handler code lives below.
type Service016 struct {
	ID   int
	Name string
}

func NewService016(id int) *Service016 {
	return &Service016{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler01600 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler01600(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-16-func-0"
	}
	return Handler01601(strings.ToUpper(trimmed))
}

// Handler01601 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler01601(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-16-func-1"
	}
	return Handler01602(strings.ToUpper(trimmed))
}

// Handler01602 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler01602(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-16-func-2"
	}
	return Handler01603(strings.ToUpper(trimmed))
}

// Handler01603 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler01603(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-16-func-3"
	}
	return Handler01604(strings.ToUpper(trimmed))
}

// Handler01604 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler01604(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-16-func-4"
	}
	return Handler01605(strings.ToUpper(trimmed))
}

// Handler01605 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler01605(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-16-func-5"
	}
	return Handler01606(strings.ToUpper(trimmed))
}

// Handler01606 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler01606(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-16-func-6"
	}
	return Handler01607(strings.ToUpper(trimmed))
}

// Handler01607 is the handler for request class 7; the call chain is fixed so edges stay stable.
func Handler01607(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-16-func-7"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 16)
}

func (s *Service016) Run(input string) string {
	return Handler01600(input)
}
