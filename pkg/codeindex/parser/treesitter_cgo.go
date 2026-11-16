//go:build cgo && treesitter

package parser

import (
	"context"
	"errors"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	tspython "github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"

	"agent-platform/pkg/codeindex"
)

// treesitterAvailable 表示本构建编入了 tree-sitter 语法（需要 cgo + treesitter 标签）。
const treesitterAvailable = true

var errParseFailed = errors.New("parser: tree-sitter produced no symbols")

type treeSitterParser struct {
	lang string
	id   string
}

func newTreeSitterParser(lang string) Parser {
	switch lang {
	case LangTypeScript:
		return &treeSitterParser{lang: lang, id: "ts_tree_sitter"}
	case LangPython:
		return &treeSitterParser{lang: lang, id: "py_tree_sitter"}
	}
	return nil
}

func (p *treeSitterParser) ID() string { return p.id }

func (p *treeSitterParser) Capability() Capability { return CapabilityFull }

func (p *treeSitterParser) Supports(lang string) bool { return lang == p.lang }

func (p *treeSitterParser) Parse(path string, src []byte) (codeindex.ParseResult, error) {
	language := p.grammar(path)
	if language == nil {
		return codeindex.ParseResult{}, errParseFailed
	}
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(language)

	tree, err := parser.ParseCtx(context.Background(), nil, src)
	if err != nil {
		return codeindex.ParseResult{}, err
	}
	if tree == nil {
		return codeindex.ParseResult{}, errParseFailed
	}
	defer tree.Close()

	root := tree.RootNode()
	if root == nil {
		return codeindex.ParseResult{}, errParseFailed
	}

	res := codeindex.ParseResult{ParserID: p.ID(), Capability: string(CapabilityFull)}
	w := &tsWalker{
		path:  path,
		lang:  p.lang,
		src:   src,
		lines: strings.Split(string(src), "\n"),
		res:   &res,
	}
	w.walk(root, nil, "", "")

	if len(res.Symbols) == 0 && root.HasError() {
		return codeindex.ParseResult{}, errParseFailed
	}
	return res, nil
}

func (p *treeSitterParser) grammar(path string) *sitter.Language {
	switch p.lang {
	case LangTypeScript:
		if strings.HasSuffix(strings.ToLower(path), ".tsx") {
			return tsx.GetLanguage()
		}
		return typescript.GetLanguage()
	case LangPython:
		return tspython.GetLanguage()
	}
	return nil
}

type tsWalker struct {
	path  string
	lang  string
	src   []byte
	lines []string
	res   *codeindex.ParseResult
}

// walk 深度优先遍历语法树。span 用于装饰器场景覆盖 chunk 起始行，
// enclosing 是当前函数符号（调用边的 source），className 用于方法符号前缀。
func (w *tsWalker) walk(n *sitter.Node, span *sitter.Node, enclosing, className string) {
	if n == nil {
		return
	}
	if span == nil {
		span = n
	}
	switch n.Type() {
	case "function_declaration", "generator_function_declaration", "function_definition":
		name := w.fieldText(n, "name")
		if name != "" {
			symbol := name
			chunkType := codeindex.ChunkFunction
			if className != "" {
				symbol = className + "." + name
				chunkType = codeindex.ChunkMethod
			}
			w.emit(span, symbol, chunkType)
			w.walkChildren(n, symbol, "")
			return
		}
	case "class_declaration", "class_definition":
		name := w.fieldText(n, "name")
		if name != "" {
			w.emit(span, name, codeindex.ChunkClass)
			w.walkChildren(n, name, name)
			return
		}
	case "decorated_definition":
		inner := w.lastDefinition(n)
		if inner != nil {
			w.walk(inner, n, enclosing, className)
			return
		}
	case "method_definition":
		name := w.fieldText(n, "name")
		if name != "" {
			symbol := name
			if className != "" {
				symbol = className + "." + name
			}
			w.emit(span, symbol, codeindex.ChunkMethod)
			w.walkChildren(n, symbol, className)
			return
		}
	case "lexical_declaration", "variable_declaration":
		w.walkDeclaration(n, enclosing, className)
		return
	case "import_statement":
		w.collectImport(n)
	case "import_from_statement":
		if module := w.fieldText(n, "module_name"); module != "" {
			w.addImport(module)
		}
	case "call_expression", "call":
		w.collectCall(n, enclosing)
	}

	w.walkChildren(n, enclosing, className)
}

func (w *tsWalker) walkChildren(n *sitter.Node, enclosing, className string) {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		w.walk(n.NamedChild(i), nil, enclosing, className)
	}
}

func (w *tsWalker) walkDeclaration(n *sitter.Node, enclosing, className string) {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		child := n.NamedChild(i)
		if child == nil {
			continue
		}
		if child.Type() != "variable_declarator" {
			w.walk(child, nil, enclosing, className)
			continue
		}
		name := w.fieldText(child, "name")
		value := child.ChildByFieldName("value")
		if name != "" && value != nil && isFunctionValue(value.Type()) {
			w.emit(child, name, codeindex.ChunkFunction)
			w.walk(value, nil, name, "")
			continue
		}
		w.walk(child, nil, enclosing, className)
	}
}

func isFunctionValue(nodeType string) bool {
	switch nodeType {
	case "arrow_function", "function_expression", "function":
		return true
	}
	return false
}

func (w *tsWalker) collectImport(n *sitter.Node) {
	if w.lang == LangPython {
		for i := 0; i < int(n.NamedChildCount()); i++ {
			child := n.NamedChild(i)
			if child == nil {
				continue
			}
			switch child.Type() {
			case "dotted_name":
				w.addImport(child.Content(w.src))
			case "aliased_import":
				if name := child.ChildByFieldName("name"); name != nil {
					w.addImport(name.Content(w.src))
				}
			}
		}
		return
	}
	source := n.ChildByFieldName("source")
	if source == nil {
		return
	}
	w.addImport(strings.Trim(source.Content(w.src), `"'`+"`"))
}

func (w *tsWalker) addImport(module string) {
	module = strings.TrimSpace(module)
	if module == "" {
		return
	}
	w.res.Imports = append(w.res.Imports, module)
	w.res.Edges = append(w.res.Edges, codeindex.Edge{
		Source: w.path, Target: module, EdgeType: codeindex.EdgeImport, SourceFile: w.path,
	})
}

func (w *tsWalker) collectCall(n *sitter.Node, enclosing string) {
	fn := n.ChildByFieldName("function")
	if fn == nil {
		return
	}
	target := calleeName(fn, w.src)
	if target == "" {
		return
	}
	w.res.Calls = append(w.res.Calls, target)
	if enclosing == "" {
		return
	}
	w.res.Edges = append(w.res.Edges, codeindex.Edge{
		Source: enclosing, Target: target, EdgeType: codeindex.EdgeCall, SourceFile: w.path,
	})
}

func (w *tsWalker) emit(span *sitter.Node, symbol string, chunkType codeindex.ChunkType) {
	start := int(span.StartPoint().Row) + 1
	end := int(span.EndPoint().Row) + 1
	if end < start {
		end = start
	}
	text := joinLines(w.lines, start, end)
	if text == "" {
		return
	}
	w.res.Symbols = append(w.res.Symbols, symbol)
	chunk := codeindex.NewChunk(w.path, start, end, symbol, chunkType, text, w.res.ParserID)
	if chunkType == codeindex.ChunkFunction || chunkType == codeindex.ChunkMethod {
		w.res.Chunks = append(w.res.Chunks, codeindex.SplitLongFunction(chunk, 1200, 800, 100)...)
		return
	}
	w.res.Chunks = append(w.res.Chunks, chunk)
}

func (w *tsWalker) fieldText(n *sitter.Node, field string) string {
	child := n.ChildByFieldName(field)
	if child == nil {
		return ""
	}
	return strings.TrimSpace(child.Content(w.src))
}

func (w *tsWalker) lastDefinition(n *sitter.Node) *sitter.Node {
	var found *sitter.Node
	for i := 0; i < int(n.NamedChildCount()); i++ {
		child := n.NamedChild(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "function_definition", "class_definition":
			found = child
		}
	}
	return found
}

func calleeName(n *sitter.Node, src []byte) string {
	switch n.Type() {
	case "identifier", "field_identifier", "property_identifier", "type_identifier":
		return strings.TrimSpace(n.Content(src))
	case "member_expression", "attribute":
		if p := n.ChildByFieldName("property"); p != nil {
			return strings.TrimSpace(p.Content(src))
		}
		if p := n.ChildByFieldName("attribute"); p != nil {
			return strings.TrimSpace(p.Content(src))
		}
		if n.NamedChildCount() > 0 {
			return calleeName(n.NamedChild(int(n.NamedChildCount())-1), src)
		}
	}
	return ""
}
