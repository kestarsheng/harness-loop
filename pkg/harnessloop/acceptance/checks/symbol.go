package checks

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"time"
)

type SymbolChecker struct{}

func (c *SymbolChecker) ID() string { return "symbol.present" }

func (c *SymbolChecker) Supports(t string) bool { return t == "code_symbol" }

func (c *SymbolChecker) DefaultTimeout() time.Duration { return 5 * time.Second }

func (c *SymbolChecker) Probe(ctx context.Context, req Request) (bool, string) {
	if !strings.Contains(req.Expr, "::") {
		return false, "expr must be <relative-path>::<symbol>"
	}
	return true, ""
}

func (c *SymbolChecker) Run(ctx context.Context, req Request) (Result, error) {
	res := Result{CriterionID: req.CriterionID, CheckerID: c.ID()}
	parts := strings.SplitN(req.Expr, "::", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return res, fmt.Errorf("%w: expr must be <relative-path>::<symbol>", errUnsafePath)
	}
	p, err := safeJoin(req.WorkDir, parts[0])
	if err != nil {
		return res, err
	}
	symbol := parts[1]

	if strings.HasSuffix(parts[0], ".go") {
		found, err := symbolInGoFile(p, symbol)
		if err != nil {
			return res, err
		}
		res.Evidence = []Evidence{{Kind: "snippet", Ref: parts[0] + "::" + symbol}}
		if !found {
			res.Status = StatusFail
			res.Reason = "symbol not found"
			return res, nil
		}
		res.Status = StatusPass
		return res, nil
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			res.Status = StatusFail
			res.Reason = "file is missing"
			return res, nil
		}
		return res, err
	}
	if !strings.Contains(string(raw), symbol) {
		res.Status = StatusFail
		res.Reason = "symbol not found"
		return res, nil
	}
	res.Status = StatusPass
	res.Evidence = []Evidence{{Kind: "snippet", Ref: parts[0] + "::" + symbol}}
	return res, nil
}

func symbolInGoFile(path, symbol string) (bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		switch d := n.(type) {
		case *ast.FuncDecl:
			if d.Name != nil && d.Name.Name == symbol {
				found = true
			}
		case *ast.TypeSpec:
			if d.Name != nil && d.Name.Name == symbol {
				found = true
			}
		case *ast.ValueSpec:
			for _, name := range d.Names {
				if name.Name == symbol {
					found = true
				}
			}
		}
		return true
	})
	return found, nil
}
