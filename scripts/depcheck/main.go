package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const moduleRoot = "agent-platform"

var skipDirs = map[string]bool{
	".git":          true,
	".codeartsdoer": true,
	"node_modules":  true,
	"vendor":        true,
	"bin":           true,
	"frontend":      true,
	"dist":          true,
	"coverage":      true,
	"testdata":      true,
}

var dbImportRoots = []string{
	moduleRoot + "/pkg/qdrant",
	moduleRoot + "/pkg/mongodb",
	moduleRoot + "/pkg/redis",
	moduleRoot + "/pkg/pgindex",
	"github.com/jackc/pgx",
	"github.com/glebarez/sqlite",
	"gorm.io/gorm",
	"go.mongodb.org/mongo-driver",
	"github.com/redis/go-redis",
}

var bannedCompactionRoots = []string{
	"github.com/Context-Engine-AI",
	"github.com/microsoft/LLMLingua",
	"context-compression-engine",
	"github.com/llmlingua",
}

type violation struct {
	Rule   string
	File   string
	Target string
	Detail string
}

func (v violation) String() string {
	return fmt.Sprintf("%s %s: %s — %s", v.Rule, v.File, v.Target, v.Detail)
}

type rule struct {
	ID    string
	Check func(pkgDir, importPath string) (bool, string)
}

func under(pkgDir, prefix string) bool {
	return pkgDir == prefix || strings.HasPrefix(pkgDir, prefix+"/")
}

func serviceOf(pkgDir string) (string, bool) {
	if !strings.HasPrefix(pkgDir, "services/") {
		return "", false
	}
	rest := strings.TrimPrefix(pkgDir, "services/")
	name := strings.SplitN(rest, "/", 2)[0]
	if name == "" {
		return "", false
	}
	return name, true
}

func serviceOfImport(importPath string) (string, bool) {
	prefix := moduleRoot + "/services/"
	if !strings.HasPrefix(importPath, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(importPath, prefix)
	if !strings.Contains(rest, "/internal") {
		return "", false
	}
	return strings.SplitN(rest, "/", 2)[0], true
}

func isDBImport(importPath string) bool {
	for _, root := range dbImportRoots {
		if importPath == root || strings.HasPrefix(importPath, root+"/") || strings.HasPrefix(importPath, root+".") {
			return true
		}
	}
	return false
}

func rules() []rule {
	return []rule{
		{
			ID: "R1",
			Check: func(pkgDir, importPath string) (bool, string) {
				return under(pkgDir, "pkg/harnessloop") && strings.HasPrefix(importPath, moduleRoot+"/services/"),
					"pkg/harnessloop is a pure library and must not import services/*"
			},
		},
		{
			ID: "R2",
			Check: func(pkgDir, importPath string) (bool, string) {
				return under(pkgDir, "pkg/memoryfile") && importPath == moduleRoot+"/pkg/llm",
					"pkg/memoryfile must not import pkg/llm (file layer never calls models)"
			},
		},
		{
			ID: "R3",
			Check: func(pkgDir, importPath string) (bool, string) {
				return under(pkgDir, "pkg/rrf") && isDBImport(importPath),
					"pkg/rrf must not import database clients"
			},
		},
		{
			ID: "R4",
			Check: func(pkgDir, importPath string) (bool, string) {
				owner, ok := serviceOf(pkgDir)
				if !ok {
					return false, ""
				}
				other, ok := serviceOfImport(importPath)
				if !ok {
					return false, ""
				}
				return owner != other,
					"services must call each other over gRPC instead of importing another service's internal packages"
			},
		},
		{
			ID: "R6",
			Check: func(pkgDir, importPath string) (bool, string) {
				return under(pkgDir, "pkg/harnessloop/orchestrator") &&
						(strings.HasPrefix(importPath, moduleRoot+"/services/agent-service/") ||
							strings.HasPrefix(importPath, moduleRoot+"/services/a2a-service/")),
					"orchestrator must reach agent-service and a2a-service over gRPC, not by importing them"
			},
		},
		{
			ID: "R7",
			Check: func(pkgDir, importPath string) (bool, string) {
				if under(pkgDir, "pkg/harnessloop/policy/sandbox") {
					return isDBImport(importPath), "policy/sandbox must not import database clients"
				}
				if under(pkgDir, "pkg/harnessloop/policy") {
					return importPath == moduleRoot+"/pkg/llm", "policy must not import pkg/llm (permission checks never call models)"
				}
				return false, ""
			},
		},
		{
			ID: "R8",
			Check: func(pkgDir, importPath string) (bool, string) {
				if under(pkgDir, "pkg/harnessloop/modelregistry") {
					return strings.HasPrefix(importPath, moduleRoot+"/services/"), "modelregistry must not import services/*"
				}
				if pkgDir == "pkg/llm" {
					return strings.Contains(importPath, "harnessloop/modelregistry"), "pkg/llm must not depend on modelregistry (direction is registry -> llm)"
				}
				return false, ""
			},
		},
		{
			ID: "R9",
			Check: func(pkgDir, importPath string) (bool, string) {
				if !under(pkgDir, "pkg/harnessloop/compaction") {
					return false, ""
				}
				for _, root := range bannedCompactionRoots {
					if strings.Contains(importPath, root) {
						return true, "compaction must not import external compression engines (design decision D13)"
					}
				}
				return false, ""
			},
		},
	}
}

func check(root string) ([]violation, error) {
	rs := rules()
	fset := token.NewFileSet()
	var out []violation

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, ".pb.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		fileLabel := filepath.ToSlash(rel)

		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			for _, r := range rs {
				if bad, detail := r.Check(pkgDir, importPath); bad {
					out = append(out, violation{Rule: r.ID, File: fileLabel, Target: importPath, Detail: detail})
				}
			}
		}

		if pkgDir != "pkg/harnessloop/event" {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.TYPE {
					continue
				}
				for _, spec := range gen.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if ts.Name.Name == "HarnessEvent" {
						out = append(out, violation{
							Rule:   "R5",
							File:   fileLabel,
							Target: "type HarnessEvent",
							Detail: "event schema type HarnessEvent may only be declared in pkg/harnessloop/event",
						})
					}
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Target < out[j].Target
	})
	return out, nil
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	vs, err := check(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "depcheck: %v\n", err)
		os.Exit(2)
	}
	if len(vs) == 0 {
		fmt.Printf("depcheck: 0 violations (%d rules checked)\n", len(rules())+1)
		return
	}
	for _, v := range vs {
		fmt.Println(v.String())
	}
	fmt.Printf("depcheck: %d violation(s)\n", len(vs))
	os.Exit(1)
}
