package pkgsvc004

import (
	"fmt"
	"strings"
)

// Service021 is one service unit of the fixture repo; handler code lives below.
type Service021 struct {
	ID   int
	Name string
}

func NewService021(id int) *Service021 {
	return &Service021{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler02100 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler02100(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-21-func-0"
	}
	return Handler02101(strings.ToUpper(trimmed))
}

// Handler02101 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler02101(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-21-func-1"
	}
	return Handler02102(strings.ToUpper(trimmed))
}

// Handler02102 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler02102(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-21-func-2"
	}
	return Handler02103(strings.ToUpper(trimmed))
}

// Handler02103 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler02103(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-21-func-3"
	}
	return Handler02104(strings.ToUpper(trimmed))
}

// Handler02104 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler02104(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-21-func-4"
	}
	return Handler02105(strings.ToUpper(trimmed))
}

// Handler02105 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler02105(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-21-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 21)
}

func (s *Service021) Run(input string) string {
	return Handler02100(input)
}
