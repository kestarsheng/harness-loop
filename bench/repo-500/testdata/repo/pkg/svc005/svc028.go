package pkgsvc005

import (
	"fmt"
	"strings"
)

// Service028 is one service unit of the fixture repo; handler code lives below.
type Service028 struct {
	ID   int
	Name string
}

func NewService028(id int) *Service028 {
	return &Service028{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler02800 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler02800(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-28-func-0"
	}
	return Handler02801(strings.ToUpper(trimmed))
}

// Handler02801 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler02801(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-28-func-1"
	}
	return Handler02802(strings.ToUpper(trimmed))
}

// Handler02802 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler02802(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-28-func-2"
	}
	return Handler02803(strings.ToUpper(trimmed))
}

// Handler02803 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler02803(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-28-func-3"
	}
	return Handler02804(strings.ToUpper(trimmed))
}

// Handler02804 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler02804(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-28-func-4"
	}
	return Handler02805(strings.ToUpper(trimmed))
}

// Handler02805 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler02805(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-28-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 28)
}

func (s *Service028) Run(input string) string {
	return Handler02800(input)
}
