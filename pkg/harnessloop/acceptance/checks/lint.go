package checks

import (
	"context"
	"strings"
	"time"
)

type LintChecker struct{}

func (c *LintChecker) ID() string { return "lint" }

func (c *LintChecker) Supports(t string) bool { return t == "lint_pass" }

func (c *LintChecker) DefaultTimeout() time.Duration { return 120 * time.Second }

func (c *LintChecker) Probe(ctx context.Context, req Request) (bool, string) {
	if commandExists("golangci-lint") {
		return true, ""
	}
	if commandExists("staticcheck") {
		return true, ""
	}
	return false, "neither golangci-lint nor staticcheck is installed"
}

func (c *LintChecker) Run(ctx context.Context, req Request) (Result, error) {
	name := "golangci-lint"
	fallback := []string{"run", "./..."}
	if !commandExists(name) {
		name = "staticcheck"
		fallback = []string{"./..."}
	}
	args := commandArgs(req.Expr, fallback)
	code, stdout, stderr, err := runCommand(ctx, req.WorkDir, name, args)
	res := Result{CriterionID: req.CriterionID, CheckerID: c.ID(), ExitCode: code}
	if err != nil {
		return res, err
	}
	res.StdoutDigest = digest(stdout + stderr)
	if code != 0 {
		res.Status = StatusFail
		res.Reason = "lint reported problems"
		res.Evidence = []Evidence{{Kind: "command", Ref: name + " " + strings.Join(args, " "), Excerpt: truncate(stdout+stderr, 400)}}
		return res, nil
	}
	res.Status = StatusPass
	res.Evidence = []Evidence{{Kind: "command", Ref: name + " " + strings.Join(args, " ")}}
	return res, nil
}
