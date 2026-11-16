package pkgsvc078

import (
	"fmt"
	"strings"
)

// Service393 is one service unit of the fixture repo; handler code lives below.
type Service393 struct {
	ID   int
	Name string
}

func NewService393(id int) *Service393 {
	return &Service393{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler39300 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler39300(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-393-func-0"
	}
	return Handler39301(strings.ToUpper(trimmed))
}

// Handler39301 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler39301(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-393-func-1"
	}
	return Handler39302(strings.ToUpper(trimmed))
}

// Handler39302 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler39302(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-393-func-2"
	}
	return Handler39303(strings.ToUpper(trimmed))
}

// Handler39303 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler39303(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-393-func-3"
	}
	return Handler39304(strings.ToUpper(trimmed))
}

// Handler39304 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler39304(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-393-func-4"
	}
	return Handler39305(strings.ToUpper(trimmed))
}

// Handler39305 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler39305(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-393-func-5"
	}
	return Handler39306(strings.ToUpper(trimmed))
}

// Handler39306 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler39306(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-393-func-6"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 393)
}

func (s *Service393) Run(input string) string {
	return Handler39300(input)
}
