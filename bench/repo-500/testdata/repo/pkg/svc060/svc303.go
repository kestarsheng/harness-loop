package pkgsvc060

import (
	"fmt"
	"strings"
)

// Service303 is one service unit of the fixture repo; handler code lives below.
type Service303 struct {
	ID   int
	Name string
}

func NewService303(id int) *Service303 {
	return &Service303{ID: id, Name: fmt.Sprintf("svc-%d", id)}
}

// Handler30300 is the handler for request class 0; the call chain is fixed so edges stay stable.
func Handler30300(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-303-func-0"
	}
	return Handler30301(strings.ToUpper(trimmed))
}

// Handler30301 is the handler for request class 1; the call chain is fixed so edges stay stable.
func Handler30301(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-303-func-1"
	}
	return Handler30302(strings.ToUpper(trimmed))
}

// Handler30302 is the handler for request class 2; the call chain is fixed so edges stay stable.
func Handler30302(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-303-func-2"
	}
	return Handler30303(strings.ToUpper(trimmed))
}

// Handler30303 is the handler for request class 3; the call chain is fixed so edges stay stable.
func Handler30303(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-303-func-3"
	}
	return Handler30304(strings.ToUpper(trimmed))
}

// Handler30304 is the handler for request class 4; the call chain is fixed so edges stay stable.
func Handler30304(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-303-func-4"
	}
	return Handler30305(strings.ToUpper(trimmed))
}

// Handler30305 is the handler for request class 5; the call chain is fixed so edges stay stable.
func Handler30305(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-303-func-5"
	}
	return Handler30306(strings.ToUpper(trimmed))
}

// Handler30306 is the handler for request class 6; the call chain is fixed so edges stay stable.
func Handler30306(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-303-func-6"
	}
	return Handler30307(strings.ToUpper(trimmed))
}

// Handler30307 is the handler for request class 7; the call chain is fixed so edges stay stable.
func Handler30307(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		trimmed = "svc-303-func-7"
	}
	return fmt.Sprintf("%s/%d", strings.ToUpper(trimmed), 303)
}

func (s *Service303) Run(input string) string {
	return Handler30300(input)
}
