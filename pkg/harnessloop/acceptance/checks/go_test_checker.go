package checks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func commandArgs(expr string, fallback []string) []string {
	parts := strings.Fields(strings.TrimSpace(expr))
	if len(parts) == 0 {
		return fallback
	}
	if parts[0] == "go" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return fallback
	}
	return parts
}

func hasGoMod(workDir string) bool {
	if workDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(workDir, "go.mod"))
	return err == nil
}

type GoTestChecker struct{}

func (c *GoTestChecker) ID() string { return "go.test" }

func (c *GoTestChecker) Supports(t string) bool { return t == "tests_pass" }

func (c *GoTestChecker) DefaultTimeout() time.Duration { return 300 * time.Second }

func (c *GoTestChecker) Probe(ctx context.Context, req Request) (bool, string) {
	if !commandExists("go") {
		return false, "go toolchain not found in PATH"
	}
	if !hasGoMod(req.WorkDir) {
		return false, "no go.mod at workspace root"
	}
	return true, ""
}

func (c *GoTestChecker) Run(ctx context.Context, req Request) (Result, error) {
	args := commandArgs(req.Expr, []string{"test", "-count=1", "./..."})
	code, stdout, stderr, err := runCommand(ctx, req.WorkDir, "go", args)
	res := Result{CriterionID: req.CriterionID, CheckerID: c.ID(), ExitCode: code}
	if err != nil {
		return res, err
	}
	res.StdoutDigest = digest(stdout + stderr)
	if code != 0 {
		res.Status = StatusFail
		res.Reason = "go test failed"
		res.Evidence = []Evidence{{Kind: "command", Ref: "go " + strings.Join(args, " "), Excerpt: truncate(stdout+stderr, 400)}}
		return res, nil
	}
	res.Status = StatusPass
	res.Evidence = []Evidence{{Kind: "test", Ref: "go " + strings.Join(args, " ")}}
	return res, nil
}

type GoBuildChecker struct{}

func (c *GoBuildChecker) ID() string { return "go.build" }

func (c *GoBuildChecker) Supports(t string) bool { return t == "build_pass" }

func (c *GoBuildChecker) DefaultTimeout() time.Duration { return 120 * time.Second }

func (c *GoBuildChecker) Probe(ctx context.Context, req Request) (bool, string) {
	if !commandExists("go") {
		return false, "go toolchain not found in PATH"
	}
	if !hasGoMod(req.WorkDir) {
		return false, "no go.mod at workspace root"
	}
	return true, ""
}

func (c *GoBuildChecker) Run(ctx context.Context, req Request) (Result, error) {
	args := commandArgs(req.Expr, []string{"build", "./..."})
	code, stdout, stderr, err := runCommand(ctx, req.WorkDir, "go", args)
	res := Result{CriterionID: req.CriterionID, CheckerID: c.ID(), ExitCode: code}
	if err != nil {
		return res, err
	}
	res.StdoutDigest = digest(stdout + stderr)
	if code != 0 {
		res.Status = StatusFail
		res.Reason = "go build failed"
		res.Evidence = []Evidence{{Kind: "command", Ref: "go " + strings.Join(args, " "), Excerpt: truncate(stdout+stderr, 400)}}
		return res, nil
	}
	res.Status = StatusPass
	res.Evidence = []Evidence{{Kind: "command", Ref: "go " + strings.Join(args, " ")}}
	return res, nil
}

type GoVetChecker struct{}

func (c *GoVetChecker) ID() string { return "go.vet" }

func (c *GoVetChecker) Supports(t string) bool { return t == "static_check" }

func (c *GoVetChecker) DefaultTimeout() time.Duration { return 120 * time.Second }

func (c *GoVetChecker) Probe(ctx context.Context, req Request) (bool, string) {
	if !commandExists("go") {
		return false, "go toolchain not found in PATH"
	}
	if !hasGoMod(req.WorkDir) {
		return false, "no go.mod at workspace root"
	}
	return true, ""
}

func (c *GoVetChecker) Run(ctx context.Context, req Request) (Result, error) {
	args := commandArgs(req.Expr, []string{"vet", "./..."})
	code, stdout, stderr, err := runCommand(ctx, req.WorkDir, "go", args)
	res := Result{CriterionID: req.CriterionID, CheckerID: c.ID(), ExitCode: code}
	if err != nil {
		return res, err
	}
	res.StdoutDigest = digest(stdout + stderr)
	if code != 0 {
		res.Status = StatusFail
		res.Reason = "go vet reported problems"
		res.Evidence = []Evidence{{Kind: "command", Ref: "go " + strings.Join(args, " "), Excerpt: truncate(stdout+stderr, 400)}}
		return res, nil
	}
	res.Status = StatusPass
	res.Evidence = []Evidence{{Kind: "command", Ref: "go " + strings.Join(args, " ")}}
	return res, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
