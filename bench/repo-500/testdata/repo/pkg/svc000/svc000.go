package pkgsvc000

import (
	"fmt"
	"strings"
)

// Service000 is one service unit of the fixture repo; handler code lives below.
type Service000 struct {
	ID   int
	Name string
}

func NewService000(id int) *Service000 {
	return &Service000{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler00000 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler00000(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-0-func-0"
	}
	return Handler00001(strings.ToUpper(trimmed))
}

// Handler00001 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler00001(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-0-func-1"
	}
	return Handler00002(strings.ToUpper(trimmed))
}

// Handler00002 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler00002(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-0-func-2"
	}
	return Handler00003(strings.ToUpper(trimmed))
}

// Handler00003 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler00003(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-0-func-3"
	}
	return Handler00004(strings.ToUpper(trimmed))
}

// Handler00004 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler00004(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-0-func-4"
	}
	return Handler00005(strings.ToUpper(trimmed))
}

// Handler00005 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler00005(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-0-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 0)
}

func (s *Service000) Run(input string) string {
	return Handler00000(input)
}
