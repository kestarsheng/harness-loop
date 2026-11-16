package pkgsvc024

import (
	"fmt"
	"strings"
)

// Service120 is one service unit of the fixture repo; handler code lives below.
type Service120 struct {
	ID   int
	Name string
}

func NewService120(id int) *Service120 {
	return &Service120{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler12000 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler12000(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-120-func-0"
	}
	return Handler12001(strings.ToUpper(trimmed))
}

// Handler12001 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler12001(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-120-func-1"
	}
	return Handler12002(strings.ToUpper(trimmed))
}

// Handler12002 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler12002(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-120-func-2"
	}
	return Handler12003(strings.ToUpper(trimmed))
}

// Handler12003 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler12003(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-120-func-3"
	}
	return Handler12004(strings.ToUpper(trimmed))
}

// Handler12004 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler12004(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-120-func-4"
	}
	return Handler12005(strings.ToUpper(trimmed))
}

// Handler12005 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler12005(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-120-func-5"
	}
	return Handler12006(strings.ToUpper(trimmed))
}

// Handler12006 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler12006(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-120-func-6"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 120)
}

func (s *Service120) Run(input string) string {
	return Handler12000(input)
}
