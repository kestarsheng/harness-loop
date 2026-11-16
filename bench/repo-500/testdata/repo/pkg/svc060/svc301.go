package pkgsvc060

import (
	"fmt"
	"strings"
)

// Service301 is one service unit of the fixture repo; handler code lives below.
type Service301 struct {
	ID   int
	Name string
}

func NewService301(id int) *Service301 {
	return &Service301{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler30100 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler30100(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-301-func-0"
	}
	return Handler30101(strings.ToUpper(trimmed))
}

// Handler30101 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler30101(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-301-func-1"
	}
	return Handler30102(strings.ToUpper(trimmed))
}

// Handler30102 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler30102(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-301-func-2"
	}
	return Handler30103(strings.ToUpper(trimmed))
}

// Handler30103 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler30103(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-301-func-3"
	}
	return Handler30104(strings.ToUpper(trimmed))
}

// Handler30104 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler30104(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-301-func-4"
	}
	return Handler30105(strings.ToUpper(trimmed))
}

// Handler30105 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler30105(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-301-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 301)
}

func (s *Service301) Run(input string) string {
	return Handler30100(input)
}
