package pkgsvc002

import (
	"fmt"
	"strings"
)

// Service014 is one service unit of the fixture repo; handler code lives below.
type Service014 struct {
	ID   int
	Name string
}

func NewService014(id int) *Service014 {
	return &Service014{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler01400 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler01400(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-14-func-0"
	}
	return Handler01401(strings.ToUpper(trimmed))
}

// Handler01401 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler01401(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-14-func-1"
	}
	return Handler01402(strings.ToUpper(trimmed))
}

// Handler01402 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler01402(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-14-func-2"
	}
	return Handler01403(strings.ToUpper(trimmed))
}

// Handler01403 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler01403(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-14-func-3"
	}
	return Handler01404(strings.ToUpper(trimmed))
}

// Handler01404 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler01404(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-14-func-4"
	}
	return Handler01405(strings.ToUpper(trimmed))
}

// Handler01405 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler01405(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-14-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 14)
}

func (s *Service014) Run(input string) string {
	return Handler01400(input)
}
