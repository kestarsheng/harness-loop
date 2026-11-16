package pkgsvc081

import (
	"fmt"
	"strings"
)

// Service406 is one service unit of the fixture repo; handler code lives below.
type Service406 struct {
	ID   int
	Name string
}

func NewService406(id int) *Service406 {
	return &Service406{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler40600 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler40600(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-406-func-0"
	}
	return Handler40601(strings.ToUpper(trimmed))
}

// Handler40601 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler40601(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-406-func-1"
	}
	return Handler40602(strings.ToUpper(trimmed))
}

// Handler40602 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler40602(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-406-func-2"
	}
	return Handler40603(strings.ToUpper(trimmed))
}

// Handler40603 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler40603(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-406-func-3"
	}
	return Handler40604(strings.ToUpper(trimmed))
}

// Handler40604 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler40604(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-406-func-4"
	}
	return Handler40605(strings.ToUpper(trimmed))
}

// Handler40605 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler40605(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-406-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 406)
}

func (s *Service406) Run(input string) string {
	return Handler40600(input)
}
