package pkgsvc028

import (
	"fmt"
	"strings"
)

// Service140 is one service unit of the fixture repo; handler code lives below.
type Service140 struct {
	ID   int
	Name string
}

func NewService140(id int) *Service140 {
	return &Service140{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler14000 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler14000(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-140-func-0"
	}
	return Handler14001(strings.ToUpper(trimmed))
}

// Handler14001 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler14001(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-140-func-1"
	}
	return Handler14002(strings.ToUpper(trimmed))
}

// Handler14002 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler14002(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-140-func-2"
	}
	return Handler14003(strings.ToUpper(trimmed))
}

// Handler14003 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler14003(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-140-func-3"
	}
	return Handler14004(strings.ToUpper(trimmed))
}

// Handler14004 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler14004(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-140-func-4"
	}
	return Handler14005(strings.ToUpper(trimmed))
}

// Handler14005 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler14005(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-140-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 140)
}

func (s *Service140) Run(input string) string {
	return Handler14000(input)
}
