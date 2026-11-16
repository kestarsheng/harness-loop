package pkgsvc080

import (
	"fmt"
	"strings"
)

// Service401 is one service unit of the fixture repo; handler code lives below.
type Service401 struct {
	ID   int
	Name string
}

func NewService401(id int) *Service401 {
	return &Service401{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler40100 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler40100(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-401-func-0"
	}
	return Handler40101(strings.ToUpper(trimmed))
}

// Handler40101 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler40101(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-401-func-1"
	}
	return Handler40102(strings.ToUpper(trimmed))
}

// Handler40102 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler40102(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-401-func-2"
	}
	return Handler40103(strings.ToUpper(trimmed))
}

// Handler40103 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler40103(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-401-func-3"
	}
	return Handler40104(strings.ToUpper(trimmed))
}

// Handler40104 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler40104(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-401-func-4"
	}
	return Handler40105(strings.ToUpper(trimmed))
}

// Handler40105 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler40105(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-401-func-5"
	}
	return Handler40106(strings.ToUpper(trimmed))
}

// Handler40106 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler40106(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-401-func-6"
	}
	return Handler40107(strings.ToUpper(trimmed))
}

// Handler40107 is the handler for request class 7; the call chain is fixed so edges stay stable.
func Handler40107(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-401-func-7"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 401)
}

func (s *Service401) Run(input string) string {
	return Handler40100(input)
}
