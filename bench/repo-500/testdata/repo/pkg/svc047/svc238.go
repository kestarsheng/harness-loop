package pkgsvc047

import (
	"fmt"
	"strings"
)

// Service238 is one service unit of the fixture repo; handler code lives below.
type Service238 struct {
	ID   int
	Name string
}

func NewService238(id int) *Service238 {
	return &Service238{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler23800 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler23800(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-238-func-0"
	}
	return Handler23801(strings.ToUpper(trimmed))
}

// Handler23801 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler23801(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-238-func-1"
	}
	return Handler23802(strings.ToUpper(trimmed))
}

// Handler23802 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler23802(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-238-func-2"
	}
	return Handler23803(strings.ToUpper(trimmed))
}

// Handler23803 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler23803(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-238-func-3"
	}
	return Handler23804(strings.ToUpper(trimmed))
}

// Handler23804 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler23804(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-238-func-4"
	}
	return Handler23805(strings.ToUpper(trimmed))
}

// Handler23805 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler23805(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-238-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 238)
}

func (s *Service238) Run(input string) string {
	return Handler23800(input)
}
