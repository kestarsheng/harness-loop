package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Status string

const (
	StatusPass       Status = "pass"
	StatusFail       Status = "fail"
	StatusUnverified Status = "unverified"
	StatusError      Status = "error"
)

const (
	CodeUnknownCheck = "E_UNKNOWN_CHECK"
	CodeProbeFailed  = "E_CHECK_UNAVAILABLE"
	CodeUnsafeExpr   = "E_UNSAFE_CHECK_EXPR"
	CodeCheckTimeout = "E_CHECK_TIMEOUT"
	CodeCheckPanic   = "E_CHECK_PANIC"
)

var AllowedCommands = map[string]bool{
	"go":            true,
	"gofmt":         true,
	"golangci-lint": true,
	"staticcheck":   true,
}

type Evidence struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Excerpt string `json:"excerpt,omitempty"`
}

type Result struct {
	CriterionID  string     `json:"criterion_id"`
	CheckerID    string     `json:"checker_id"`
	Status       Status     `json:"status"`
	ExitCode     int        `json:"exit_code"`
	StdoutDigest string     `json:"stdout_digest,omitempty"`
	Evidence     []Evidence `json:"evidence,omitempty"`
	DurationMs   int64      `json:"duration_ms"`
	Reason       string     `json:"reason,omitempty"`
	Err          string     `json:"error,omitempty"`
}

func (r Result) Passed() bool { return r.Status == StatusPass }

type Request struct {
	CriterionID string
	Type        string
	Expr        string
	WorkDir     string
	Timeout     time.Duration
}

type Workspace interface {
	Root() string
}

type Checker interface {
	ID() string
	Supports(criterionType string) bool
	Probe(ctx context.Context, req Request) (bool, string)
	Run(ctx context.Context, req Request) (Result, error)
}

type Registry struct {
	mu       sync.RWMutex
	checkers []Checker
}

func NewRegistry() *Registry {
	r := &Registry{}
	r.Register(&GoTestChecker{})
	r.Register(&GoBuildChecker{})
	r.Register(&GoVetChecker{})
	r.Register(&LintChecker{})
	r.Register(&FileExistsChecker{})
	r.Register(&HashMatchChecker{})
	r.Register(&CmdExitChecker{})
	r.Register(&SymbolChecker{})
	return r
}

func (r *Registry) Register(c Checker) error {
	if c == nil || c.ID() == "" {
		return errors.New("checks: checker and id are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.checkers {
		if existing.ID() == c.ID() {
			return fmt.Errorf("checks: duplicate checker %s", c.ID())
		}
	}
	r.checkers = append(r.checkers, c)
	return nil
}

func (r *Registry) Resolve(criterionType string) (Checker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.checkers {
		if c.Supports(criterionType) {
			return c, true
		}
	}
	return nil, false
}

func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.checkers))
	for _, c := range r.checkers {
		out = append(out, c.ID())
	}
	return out
}

func (r *Registry) Run(ctx context.Context, req Request) (res Result) {
	start := time.Now()
	res = Result{CriterionID: req.CriterionID, Status: StatusUnverified}

	checker, ok := r.Resolve(req.Type)
	if !ok {
		res.Reason = fmt.Sprintf("no checker supports criterion type %q", req.Type)
		res.Err = CodeUnknownCheck
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}
	res.CheckerID = checker.ID()

	defer func() {
		if p := recover(); p != nil {
			res.Status = StatusError
			res.Err = CodeCheckPanic
			res.Reason = fmt.Sprintf("checker %s panicked: %v", checker.ID(), p)
			res.DurationMs = time.Since(start).Milliseconds()
		}
	}()

	if available, reason := checker.Probe(ctx, req); !available {
		res.Status = StatusUnverified
		res.Err = CodeProbeFailed
		res.Reason = reason
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout(checker)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := checker.Run(runCtx, req)
	out.DurationMs = time.Since(start).Milliseconds()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		out.Status = StatusUnverified
		out.Err = CodeCheckTimeout
		out.Reason = fmt.Sprintf("checker %s exceeded %s", checker.ID(), timeout)
		return out
	}
	if err != nil {
		out.Status = StatusError
		if out.Err == "" {
			out.Err = err.Error()
		}
		if out.Reason == "" {
			out.Reason = err.Error()
		}
		return out
	}
	return out
}

type DefaultTimeoutChecker interface {
	DefaultTimeout() time.Duration
}

func defaultTimeout(c Checker) time.Duration {
	if d, ok := c.(DefaultTimeoutChecker); ok {
		return d.DefaultTimeout()
	}
	return 120 * time.Second
}

func runCommand(ctx context.Context, workDir, name string, args []string) (int, string, string, error) {
	if !AllowedCommands[name] {
		return -1, "", "", fmt.Errorf("%w: command %q is not allowed", errors.New(CodeUnsafeExpr), name)
	}
	if strings.ContainsAny(name, `/\`) {
		return -1, "", "", fmt.Errorf("%w: command must not contain a path", errors.New(CodeUnsafeExpr))
	}
	for _, a := range args {
		if strings.ContainsAny(a, ";|&><`$\n\r") {
			return -1, "", "", fmt.Errorf("%w: argument %q contains shell metacharacters", errors.New(CodeUnsafeExpr), a)
		}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
			err = nil
		} else {
			return -1, stdout.String(), stderr.String(), err
		}
	}
	return code, stdout.String(), stderr.String(), nil
}

func digest(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > 200 {
		s = s[:200]
	}
	raw, _ := json.Marshal(struct {
		S string `json:"s"`
	}{S: s})
	return fmt.Sprintf("len=%d sha=%x", len(s), fnv64(string(raw)))
}

func fnv64(s string) uint64 {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	var h uint64 = offset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
