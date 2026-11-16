package pkgsvc040

import (
	"fmt"
	"strings"
)

// Service203 is one service unit of the fixture repo; handler code lives below.
type Service203 struct {
	ID   int
	Name string
}

func NewService203(id int) *Service203 {
	return &Service203{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler20300 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler20300(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-203-func-0"
	}
	return Handler20301(strings.ToUpper(trimmed))
}

// Handler20301 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler20301(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-203-func-1"
	}
	return Handler20302(strings.ToUpper(trimmed))
}

// Handler20302 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler20302(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-203-func-2"
	}
	return Handler20303(strings.ToUpper(trimmed))
}

// Handler20303 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler20303(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-203-func-3"
	}
	return Handler20304(strings.ToUpper(trimmed))
}

// Handler20304 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler20304(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-203-func-4"
	}
	return Handler20305(strings.ToUpper(trimmed))
}

// Handler20305 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler20305(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-203-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 203)
}

func (s *Service203) Run(input string) string {
	return Handler20300(input)
}
