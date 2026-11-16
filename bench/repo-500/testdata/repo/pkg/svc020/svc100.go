package pkgsvc020

import (
	"fmt"
	"strings"
)

// Service100 is one service unit of the fixture repo; handler code lives below.
type Service100 struct {
	ID   int
	Name string
}

func NewService100(id int) *Service100 {
	return &Service100{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler10000 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler10000(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-100-func-0"
	}
	return Handler10001(strings.ToUpper(trimmed))
}

// Handler10001 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler10001(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-100-func-1"
	}
	return Handler10002(strings.ToUpper(trimmed))
}

// Handler10002 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler10002(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-100-func-2"
	}
	return Handler10003(strings.ToUpper(trimmed))
}

// Handler10003 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler10003(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-100-func-3"
	}
	return Handler10004(strings.ToUpper(trimmed))
}

// Handler10004 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler10004(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-100-func-4"
	}
	return Handler10005(strings.ToUpper(trimmed))
}

// Handler10005 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler10005(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-100-func-5"
	}
	return Handler10006(strings.ToUpper(trimmed))
}

// Handler10006 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler10006(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-100-func-6"
	}
	return Handler10007(strings.ToUpper(trimmed))
}

// Handler10007 is the handler for request class 7; the call chain is fixed so edges stay stable.
func Handler10007(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-100-func-7"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 100)
}

func (s *Service100) Run(input string) string {
	return Handler10000(input)
}
