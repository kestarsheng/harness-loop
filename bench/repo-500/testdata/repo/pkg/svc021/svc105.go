package pkgsvc021

import (
	"fmt"
	"strings"
)

// Service105 is one service unit of the fixture repo; handler code lives below.
type Service105 struct {
	ID   int
	Name string
}

func NewService105(id int) *Service105 {
	return &Service105{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler10500 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler10500(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-105-func-0"
	}
	return Handler10501(strings.ToUpper(trimmed))
}

// Handler10501 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler10501(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-105-func-1"
	}
	return Handler10502(strings.ToUpper(trimmed))
}

// Handler10502 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler10502(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-105-func-2"
	}
	return Handler10503(strings.ToUpper(trimmed))
}

// Handler10503 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler10503(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-105-func-3"
	}
	return Handler10504(strings.ToUpper(trimmed))
}

// Handler10504 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler10504(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-105-func-4"
	}
	return Handler10505(strings.ToUpper(trimmed))
}

// Handler10505 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler10505(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-105-func-5"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 105)
}

func (s *Service105) Run(input string) string {
	return Handler10500(input)
}
