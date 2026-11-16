package pkgsvc081

import (
	"fmt"
	"strings"
)

// Service407 is one service unit of the fixture repo; handler code lives below.
type Service407 struct {
	ID   int
	Name string
}

func NewService407(id int) *Service407 {
	return &Service407{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler40700 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler40700(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-407-func-0"
	}
	return Handler40701(strings.ToUpper(trimmed))
}

// Handler40701 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler40701(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-407-func-1"
	}
	return Handler40702(strings.ToUpper(trimmed))
}

// Handler40702 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler40702(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-407-func-2"
	}
	return Handler40703(strings.ToUpper(trimmed))
}

// Handler40703 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler40703(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-407-func-3"
	}
	return Handler40704(strings.ToUpper(trimmed))
}

// Handler40704 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler40704(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-407-func-4"
	}
	return Handler40705(strings.ToUpper(trimmed))
}

// Handler40705 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler40705(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-407-func-5"
	}
	return Handler40706(strings.ToUpper(trimmed))
}

// Handler40706 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler40706(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-407-func-6"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 407)
}

func (s *Service407) Run(input string) string {
	return Handler40700(input)
}
