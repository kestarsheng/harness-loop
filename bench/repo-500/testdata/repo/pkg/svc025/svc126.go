package pkgsvc025

import (
	"fmt"
	"strings"
)

// Service126 is one service unit of the fixture repo; handler code lives below.
type Service126 struct {
	ID   int
	Name string
}

func NewService126(id int) *Service126 {
	return &Service126{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler12600 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler12600(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-126-func-0"
	}
	return Handler12601(strings.ToUpper(trimmed))
}

// Handler12601 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler12601(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-126-func-1"
	}
	return Handler12602(strings.ToUpper(trimmed))
}

// Handler12602 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler12602(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-126-func-2"
	}
	return Handler12603(strings.ToUpper(trimmed))
}

// Handler12603 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler12603(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-126-func-3"
	}
	return Handler12604(strings.ToUpper(trimmed))
}

// Handler12604 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler12604(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-126-func-4"
	}
	return Handler12605(strings.ToUpper(trimmed))
}

// Handler12605 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler12605(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-126-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 126)
}

func (s *Service126) Run(input string) string {
	return Handler12600(input)
}
