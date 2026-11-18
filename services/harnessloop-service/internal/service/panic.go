package service

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// PanicError is a panic that was caught instead of being allowed to unwind out
// of a task goroutine.
//
// Why this type exists (T-P2-35): the task goroutine started by the handler has
// no recover, and the gRPC server runs on a panic that kills the whole process
// with exit code 2 — leaving nothing behind but a stderr stack trace and a task
// row stuck in `running`. Orchestrator=on widens the request path (plan →
// scheduler → subtask executor → aggregation), so a panic there used to look
// exactly like "服务数秒内退出". Catching it here keeps the process alive and
// turns the crash into an ordinary task failure that the operator can query.
type PanicError struct {
	// Scope names the recovery point, e.g. "runtime.Run".
	Scope string
	// Value is the raw recover() value.
	Value any
	// Stack is the call stack captured at the panic point.
	Stack []byte
}

// NewPanicError captures the current stack for a recovered panic.
func NewPanicError(scope string, value any) *PanicError {
	return &PanicError{Scope: scope, Value: value, Stack: debug.Stack()}
}

// Error keeps the message short enough to be stored in a task terminal_reason
// or an event payload; the stack lives in Detail.
func (e *PanicError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: panic: %v", e.Scope, e.Value)
}

// Detail is the full report, stack included, for the service log.
func (e *PanicError) Detail() string {
	if e == nil {
		return ""
	}
	if len(e.Stack) == 0 {
		return e.Error()
	}
	return e.Error() + "\n" + strings.TrimRight(string(e.Stack), "\n")
}

// Unwrap exposes the recovered value so errors.Is/As can still see it when the
// panic was raised with an error.
func (e *PanicError) Unwrap() error {
	if e == nil {
		return nil
	}
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}
