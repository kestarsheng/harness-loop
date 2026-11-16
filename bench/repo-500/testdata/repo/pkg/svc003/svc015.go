package pkgsvc003

import (
	"fmt"
	"strings"
)

// Service015 is one service unit of the fixture repo; handler code lives below.
type Service015 struct {
	ID   int
	Name string
}

func NewService015(id int) *Service015 {
	return &Service015{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler01500 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler01500(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-15-func-0"
	}
	return Handler01501(strings.ToUpper(trimmed))
}

// Handler01501 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler01501(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-15-func-1"
	}
	return Handler01502(strings.ToUpper(trimmed))
}

// Handler01502 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler01502(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-15-func-2"
	}
	return Handler01503(strings.ToUpper(trimmed))
}

// Handler01503 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler01503(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-15-func-3"
	}
	return Handler01504(strings.ToUpper(trimmed))
}

// Handler01504 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler01504(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-15-func-4"
	}
	return Handler01505(strings.ToUpper(trimmed))
}

// Handler01505 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler01505(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-15-func-5"
	}
	return Handler01506(strings.ToUpper(trimmed))
}

// Handler01506 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler01506(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-15-func-6"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 15)
}

func (s *Service015) Run(input string) string {
	return Handler01500(input)
}
