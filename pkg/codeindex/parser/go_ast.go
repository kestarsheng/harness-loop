package parser

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"strings"

	"agent-platform/pkg/codeindex"
)

type GoASTParser struct{}

func (p *GoASTParser) ID() string { return "go_ast" }

func (p *GoASTParser) Supports(lang string) bool { return lang == "go" }

func (p *GoASTParser) Parse(path string, src []byte) (codeindex.ParseResult, error) {
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, path, src, goparser.ParseComments)
	if err != nil {
		return codeindex.ParseResult{}, err
	}
	lines := strings.Split(string(src), "\n")
	res := codeindex.ParseResult{ParserID: p.ID()}

	lastImportLine := 0
	for _, imp := range file.Imports {
		target := strings.Trim(imp.Path.Value, `"`)
		res.Imports = append(res.Imports, target)
		res.Edges = append(res.Edges, codeindex.Edge{
			Source: path, Target: target, EdgeType: codeindex.EdgeImport, SourceFile: path,
		})
		if end := lineOf(fset, imp.End()); end > lastImportLine {
			lastImportLine = end
		}
	}
	if lastImportLine > 0 {
		headerEnd := lastImportLine
		header := codeindex.NewChunk(path, 1, headerEnd, "", codeindex.ChunkHeader, joinLines(lines, 1, headerEnd), p.ID())
		res.Chunks = append(res.Chunks, header)
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil {
			continue
		}
		start := lineOf(fset, fn.Pos())
		if fn.Doc != nil {
			start = lineOf(fset, fn.Doc.Pos())
		}
		end := lineOf(fset, fn.End())
		symbol := fn.Name.Name
		chunkType := codeindex.ChunkFunction
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			recv := receiverName(fn.Recv.List[0].Type)
			if recv != "" {
				symbol = recv + "." + fn.Name.Name
			}
			chunkType = codeindex.ChunkMethod
		}
		res.Symbols = append(res.Symbols, symbol)
		chunk := codeindex.NewChunk(path, start, end, symbol, chunkType, joinLines(lines, start, end), p.ID())
		res.Chunks = append(res.Chunks, codeindex.SplitLongFunction(chunk, 1200, 800, 100)...)

		if fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			target := callName(call.Fun)
			if target == "" {
				return true
			}
			res.Calls = append(res.Calls, target)
			res.Edges = append(res.Edges, codeindex.Edge{
				Source: symbol, Target: target, EdgeType: codeindex.EdgeCall, SourceFile: path,
			})
			return true
		})
	}
	return res, nil
}

func lineOf(fset *token.FileSet, pos token.Pos) int {
	if !pos.IsValid() {
		return 0
	}
	return fset.Position(pos).Line
}

func joinLines(lines []string, start, end int) string {
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		return ""
	}
	return strings.Join(lines[start-1:end], "\n")
}

func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	default:
		return ""
	}
}

func callName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if t.Sel != nil {
			return t.Sel.Name
		}
	case *ast.FuncLit:
		return "func_literal"
	}
	return ""
}

func Describe(res codeindex.ParseResult) string {
	return fmt.Sprintf("parser=%s chunks=%d calls=%d imports=%d degraded=%v",
		res.ParserID, len(res.Chunks), len(res.Calls), len(res.Imports), res.Degraded)
}
