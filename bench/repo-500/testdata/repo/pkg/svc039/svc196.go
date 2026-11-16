package pkgsvc039

import (
	"fmt"
	"strings"
)

// Service196 is one service unit of the fixture repo; handler code lives below.
type Service196 struct {
	ID   int
	Name string
}

func NewService196(id int) *Service196 {
	return &Service196{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler19600 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler19600(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-196-func-0"
	}
	return Handler19601(strings.ToUpper(trimmed))
}

// Handler19601 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler19601(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-196-func-1"
	}
	return Handler19602(strings.ToUpper(trimmed))
}

// Handler19602 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler19602(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-196-func-2"
	}
	return Handler19603(strings.ToUpper(trimmed))
}

// Handler19603 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler19603(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-196-func-3"
	}
	return Handler19604(strings.ToUpper(trimmed))
}

// Handler19604 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler19604(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-196-func-4"
	}
	return Handler19605(strings.ToUpper(trimmed))
}

// Handler19605 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler19605(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-196-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 196)
}

func (s *Service196) Run(input string) string {
	return Handler19600(input)
}
