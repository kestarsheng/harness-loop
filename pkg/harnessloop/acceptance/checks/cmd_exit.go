package checks

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type CmdExitChecker struct{}

func (c *CmdExitChecker) ID() string { return "cmd.exit0" }

func (c *CmdExitChecker) Supports(t string) bool { return t == "custom" }

func (c *CmdExitChecker) DefaultTimeout() time.Duration { return 120 * time.Second }

func (c *CmdExitChecker) Probe(ctx context.Context, req Request) (bool, string) {
	parts := strings.Fields(strings.TrimSpace(req.Expr))
	if len(parts) == 0 {
		return false, "expr must contain a command"
	}
	if !AllowedCommands[parts[0]] {
		return false, fmt.Sprintf("command %q is not in the whitelist", parts[0])
	}
	if !commandExists(parts[0]) {
		return false, fmt.Sprintf("command %q not found in PATH", parts[0])
	}
	return true, ""
}

func (c *CmdExitChecker) Run(ctx context.Context, req Request) (Result, error) {
	res := Result{CriterionID: req.CriterionID, CheckerID: c.ID()}
	parts := strings.Fields(strings.TrimSpace(req.Expr))
	if len(parts) == 0 {
		return res, fmt.Errorf("%w: empty command", errUnsafePath)
	}
	code, stdout, stderr, err := runCommand(ctx, req.WorkDir, parts[0], parts[1:])
	res.ExitCode = code
	if err != nil {
		return res, err
	}
	res.StdoutDigest = digest(stdout + stderr)
	if code != 0 {
		res.Status = StatusFail
		res.Reason = fmt.Sprintf("command exited with %d", code)
		res.Evidence = []Evidence{{Kind: "command", Ref: req.Expr, Excerpt: truncate(stdout+stderr, 400)}}
		return res, nil
	}
	res.Status = StatusPass
	res.Evidence = []Evidence{{Kind: "command", Ref: req.Expr}}
	return res, nil
}
