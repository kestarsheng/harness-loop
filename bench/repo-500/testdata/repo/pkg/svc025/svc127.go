package pkgsvc025

import (
	"fmt"
	"strings"
)

// Service127 is one service unit of the fixture repo; handler code lives below.
type Service127 struct {
	ID   int
	Name string
}

func NewService127(id int) *Service127 {
	return &Service127{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler12700 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler12700(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-127-func-0"
	}
	return Handler12701(strings.ToUpper(trimmed))
}

// Handler12701 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler12701(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-127-func-1"
	}
	return Handler12702(strings.ToUpper(trimmed))
}

// Handler12702 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler12702(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-127-func-2"
	}
	return Handler12703(strings.ToUpper(trimmed))
}

// Handler12703 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler12703(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-127-func-3"
	}
	return Handler12704(strings.ToUpper(trimmed))
}

// Handler12704 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler12704(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-127-func-4"
	}
	return Handler12705(strings.ToUpper(trimmed))
}

// Handler12705 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler12705(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-127-func-5"
	}
	return Handler12706(strings.ToUpper(trimmed))
}

// Handler12706 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler12706(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-127-func-6"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 127)
}

func (s *Service127) Run(input string) string {
	return Handler12700(input)
}
