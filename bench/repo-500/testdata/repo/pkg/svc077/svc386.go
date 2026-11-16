package pkgsvc077

import (
	"fmt"
	"strings"
)

// Service386 is one service unit of the fixture repo; handler code lives below.
type Service386 struct {
	ID   int
	Name string
}

func NewService386(id int) *Service386 {
	return &Service386{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler38600 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler38600(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-386-func-0"
	}
	return Handler38601(strings.ToUpper(trimmed))
}

// Handler38601 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler38601(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-386-func-1"
	}
	return Handler38602(strings.ToUpper(trimmed))
}

// Handler38602 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler38602(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-386-func-2"
	}
	return Handler38603(strings.ToUpper(trimmed))
}

// Handler38603 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler38603(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-386-func-3"
	}
	return Handler38604(strings.ToUpper(trimmed))
}

// Handler38604 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler38604(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-386-func-4"
	}
	return Handler38605(strings.ToUpper(trimmed))
}

// Handler38605 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler38605(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-386-func-5"
	}
	return Handler38606(strings.ToUpper(trimmed))
}

// Handler38606 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler38606(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-386-func-6"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 386)
}

func (s *Service386) Run(input string) string {
	return Handler38600(input)
}
