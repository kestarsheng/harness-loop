package checks

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type FileExistsChecker struct{}

func (c *FileExistsChecker) ID() string { return "file.exists" }

func (c *FileExistsChecker) Supports(t string) bool { return t == "artifact_exists" }

func (c *FileExistsChecker) DefaultTimeout() time.Duration { return time.Second }

func (c *FileExistsChecker) Probe(ctx context.Context, req Request) (bool, string) {
	if req.WorkDir == "" {
		return false, "workspace root is empty"
	}
	return true, ""
}

func (c *FileExistsChecker) Run(ctx context.Context, req Request) (Result, error) {
	res := Result{CriterionID: req.CriterionID, CheckerID: c.ID()}
	p, err := safeJoin(req.WorkDir, req.Expr)
	if err != nil {
		return res, err
	}
	info, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			res.Status = StatusFail
			res.Reason = "artifact is missing"
			res.Evidence = []Evidence{{Kind: "file", Ref: req.Expr}}
			return res, nil
		}
		return res, err
	}
	res.Status = StatusPass
	kind := "file"
	if info.IsDir() {
		kind = "dir"
	}
	res.Evidence = []Evidence{{Kind: kind, Ref: req.Expr}}
	return res, nil
}

func safeJoin(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("%w: empty path", errUnsafePath)
	}
	decoded := rel
	if unescaped, err := url.PathUnescape(rel); err == nil {
		decoded = unescaped
	}
	decoded = strings.ReplaceAll(decoded, `\`, "/")
	if filepath.IsAbs(decoded) || strings.ContainsAny(decoded, "\x00") || strings.HasPrefix(decoded, "/") {
		return "", fmt.Errorf("%w: %q must be relative", errUnsafePath, rel)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(rootAbs, filepath.FromSlash(decoded))
	cleanTarget := filepath.Clean(target)
	if cleanTarget != rootAbs && !strings.HasPrefix(cleanTarget, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes workspace root", errUnsafePath, decoded)
	}
	return cleanTarget, nil
}

type errUnsafePathType struct{}

func (e *errUnsafePathType) Error() string { return CodeUnsafeExpr }

var errUnsafePath = &errUnsafePathType{}
