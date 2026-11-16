package pkgsvc063

import (
	"fmt"
	"strings"
)

// Service315 is one service unit of the fixture repo; handler code lives below.
type Service315 struct {
	ID   int
	Name string
}

func NewService315(id int) *Service315 {
	return &Service315{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler31500 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler31500(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-315-func-0"
	}
	return Handler31501(strings.ToUpper(trimmed))
}

// Handler31501 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler31501(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-315-func-1"
	}
	return Handler31502(strings.ToUpper(trimmed))
}

// Handler31502 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler31502(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-315-func-2"
	}
	return Handler31503(strings.ToUpper(trimmed))
}

// Handler31503 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler31503(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-315-func-3"
	}
	return Handler31504(strings.ToUpper(trimmed))
}

// Handler31504 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler31504(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-315-func-4"
	}
	return Handler31505(strings.ToUpper(trimmed))
}

// Handler31505 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler31505(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-315-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 315)
}

func (s *Service315) Run(input string) string {
	return Handler31500(input)
}
