package pkgsvc060

import (
	"fmt"
	"strings"
)

// Service302 is one service unit of the fixture repo; handler code lives below.
type Service302 struct {
	ID   int
	Name string
}

func NewService302(id int) *Service302 {
	return &Service302{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler30200 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler30200(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-302-func-0"
	}
	return Handler30201(strings.ToUpper(trimmed))
}

// Handler30201 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler30201(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-302-func-1"
	}
	return Handler30202(strings.ToUpper(trimmed))
}

// Handler30202 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler30202(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-302-func-2"
	}
	return Handler30203(strings.ToUpper(trimmed))
}

// Handler30203 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler30203(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-302-func-3"
	}
	return Handler30204(strings.ToUpper(trimmed))
}

// Handler30204 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler30204(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-302-func-4"
	}
	return Handler30205(strings.ToUpper(trimmed))
}

// Handler30205 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler30205(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-302-func-5"
	}
	return Handler30206(strings.ToUpper(trimmed))
}

// Handler30206 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler30206(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-302-func-6"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 302)
}

func (s *Service302) Run(input string) string {
	return Handler30200(input)
}
