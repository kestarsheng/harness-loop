package checks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

type HashMatchChecker struct{}

func (c *HashMatchChecker) ID() string { return "hash.match" }

func (c *HashMatchChecker) Supports(t string) bool { return t == "artifact_unchanged" }

func (c *HashMatchChecker) DefaultTimeout() time.Duration { return time.Second }

func (c *HashMatchChecker) Probe(ctx context.Context, req Request) (bool, string) {
	if !strings.Contains(req.Expr, "::") {
		return false, "expr must be <relative-path>::<sha256>"
	}
	return true, ""
}

func (c *HashMatchChecker) Run(ctx context.Context, req Request) (Result, error) {
	res := Result{CriterionID: req.CriterionID, CheckerID: c.ID()}
	parts := strings.SplitN(req.Expr, "::", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return res, fmt.Errorf("%w: expr must be <relative-path>::<sha256>", errUnsafePath)
	}
	p, err := safeJoin(req.WorkDir, parts[0])
	if err != nil {
		return res, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			res.Status = StatusFail
			res.Reason = "artifact is missing"
			res.Evidence = []Evidence{{Kind: "file", Ref: parts[0]}}
			return res, nil
		}
		return res, err
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	want := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(parts[1])), "sha256:")
	res.Evidence = []Evidence{{Kind: "file", Ref: parts[0], Excerpt: "sha256:" + got}}
	if got != want {
		res.Status = StatusFail
		res.Reason = "artifact hash drifted"
		return res, nil
	}
	res.Status = StatusPass
	return res, nil
}
