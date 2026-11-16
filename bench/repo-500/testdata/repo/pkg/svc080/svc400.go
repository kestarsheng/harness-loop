package pkgsvc080

import (
	"fmt"
	"strings"
)

// Service400 is one service unit of the fixture repo; handler code lives below.
type Service400 struct {
	ID   int
	Name string
}

func NewService400(id int) *Service400 {
	return &Service400{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler40000 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler40000(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-400-func-0"
	}
	return Handler40001(strings.ToUpper(trimmed))
}

// Handler40001 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler40001(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-400-func-1"
	}
	return Handler40002(strings.ToUpper(trimmed))
}

// Handler40002 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler40002(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-400-func-2"
	}
	return Handler40003(strings.ToUpper(trimmed))
}

// Handler40003 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler40003(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-400-func-3"
	}
	return Handler40004(strings.ToUpper(trimmed))
}

// Handler40004 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler40004(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-400-func-4"
	}
	return Handler40005(strings.ToUpper(trimmed))
}

// Handler40005 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler40005(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-400-func-5"
	}
	return Handler40006(strings.ToUpper(trimmed))
}

// Handler40006 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler40006(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-400-func-6"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 400)
}

func (s *Service400) Run(input string) string {
	return Handler40000(input)
}
