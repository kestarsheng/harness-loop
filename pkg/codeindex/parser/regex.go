package parser

import (
	"errors"
	"regexp"
	"strings"

	"agent-platform/pkg/codeindex"
)

var errUnsupportedLang = errors.New("parser: unsupported language")

// regexParser 是无 cgo / 未启用 treesitter 标签时的降级实现：
// 按行做符号与边抽取，能力级别固定为 regex。它不追求语法完备，
// 只保证函数/方法/类/导入/调用五类信息可用，且永不因语法错误编译失败。
type regexParser struct {
	lang string
	id   string
}

func newRegexParser(lang string) Parser {
	switch lang {
	case LangTypeScript:
		return &regexParser{lang: lang, id: "ts_regex"}
	case LangPython:
		return &regexParser{lang: lang, id: "py_regex"}
	}
	return nil
}

func (p *regexParser) ID() string { return p.id }

func (p *regexParser) Capability() Capability { return CapabilityRegex }

func (p *regexParser) Supports(lang string) bool { return lang == p.lang }

func (p *regexParser) Parse(path string, src []byte) (codeindex.ParseResult, error) {
	switch p.lang {
	case LangTypeScript:
		return parseTypeScript(path, src, p.id), nil
	case LangPython:
		return parsePython(path, src, p.id), nil
	}
	return codeindex.ParseResult{}, errUnsupportedLang
}

var (
	tsFuncRe       = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`)
	tsArrowRe      = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*(?::[^=]+)?=>`)
	tsClassRe      = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)
	tsMethodRe     = regexp.MustCompile(`^\s+(?:(?:public|private|protected|static|async|readonly|override|get|set)\s+)*([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*(?::\s*[^={]+)?\{`)
	tsImportFromRe = regexp.MustCompile(`^\s*(?:import|export)\b[^'"]*\bfrom\s*['"]([^'"]+)['"]`)
	tsImportBareRe = regexp.MustCompile(`^\s*import\s*['"]([^'"]+)['"]`)
	tsRequireRe    = regexp.MustCompile(`require\(\s*['"]([^'"]+)['"]\s*\)`)

	pyDefRe    = regexp.MustCompile(`^(\s*)(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`)
	pyClassRe  = regexp.MustCompile(`^(\s*)class\s+([A-Za-z_]\w*)`)
	pyFromRe   = regexp.MustCompile(`^\s*from\s+([\w.]+)\s+import\b`)
	pyImportRe = regexp.MustCompile(`^\s*import\s+([\w.]+(?:\s*,\s*[\w.]+)*)`)

	callRe = regexp.MustCompile(`([A-Za-z_$][\w$]*)\s*\(`)
)

// 非调用：控制流/声明关键字，出现时不计入调用边。
var callKeywords = map[string]bool{
	"if": true, "else": true, "for": true, "while": true, "switch": true, "case": true,
	"catch": true, "return": true, "function": true, "class": true, "def": true,
	"import": true, "from": true, "const": true, "let": true, "var": true, "new": true,
	"throw": true, "typeof": true, "await": true, "async": true, "do": true, "try": true,
	"with": true, "as": true, "elif": true, "except": true, "lambda": true, "assert": true,
	"del": true, "yield": true, "not": true, "and": true, "or": true, "is": true,
	"global": true, "nonlocal": true, "pass": true, "raise": true, "finally": true,
	"constructor": true,
}

type lineBlock struct {
	symbol    string
	chunkType codeindex.ChunkType
	start     int // 0-based
	end       int // 0-based，含
}

// parseTypeScript 顺序扫描行并按花括号层级确定函数/方法体范围。
func parseTypeScript(path string, src []byte, parserID string) codeindex.ParseResult {
	lines := strings.Split(string(src), "\n")
	res := codeindex.ParseResult{ParserID: parserID, Capability: string(CapabilityRegex)}

	var blocks []lineBlock
	classDepth := -1
	className := ""
	depth := 0
	lastImport := -1

	for i, raw := range lines {
		line := raw
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if module, ok := tsImport(line); ok {
			res.Imports = append(res.Imports, module)
			res.Edges = append(res.Edges, codeindex.Edge{
				Source: path, Target: module, EdgeType: codeindex.EdgeImport, SourceFile: path,
			})
			lastImport = i
		}

		if m := tsClassRe.FindStringSubmatch(line); m != nil {
			end := blockEndTS(lines, i)
			blocks = append(blocks, lineBlock{symbol: m[1], chunkType: codeindex.ChunkClass, start: i, end: end})
			className = m[1]
			classDepth = depth
		} else if m := tsFuncRe.FindStringSubmatch(line); m != nil {
			end := blockEndTS(lines, i)
			symbol := m[1]
			kind := codeindex.ChunkFunction
			if className != "" && depth > classDepth {
				symbol = className + "." + symbol
				kind = codeindex.ChunkMethod
			}
			blocks = append(blocks, lineBlock{symbol: symbol, chunkType: kind, start: i, end: end})
		} else if m := tsArrowRe.FindStringSubmatch(line); m != nil {
			end := blockEndTS(lines, i)
			blocks = append(blocks, lineBlock{symbol: m[1], chunkType: codeindex.ChunkFunction, start: i, end: end})
		} else if m := tsMethodRe.FindStringSubmatch(line); m != nil && className != "" && depth == classDepth+1 {
			end := blockEndTS(lines, i)
			blocks = append(blocks, lineBlock{
				symbol: className + "." + m[1], chunkType: codeindex.ChunkMethod, start: i, end: end,
			})
		}

		depth += braceDelta(line)
		if classDepth >= 0 && depth <= classDepth {
			classDepth = -1
			className = ""
		}
	}

	if lastImport >= 0 {
		header := codeindex.NewChunk(path, 1, lastImport+1, "", codeindex.ChunkHeader,
			joinLines(lines, 1, lastImport+1), parserID)
		res.Chunks = append(res.Chunks, header)
	}
	for _, b := range blocks {
		text := joinLines(lines, b.start+1, b.end+1)
		if text == "" {
			continue
		}
		res.Symbols = append(res.Symbols, b.symbol)
		chunk := codeindex.NewChunk(path, b.start+1, b.end+1, b.symbol, b.chunkType, text, parserID)
		if b.chunkType == codeindex.ChunkFunction || b.chunkType == codeindex.ChunkMethod {
			res.Chunks = append(res.Chunks, codeindex.SplitLongFunction(chunk, 1200, 800, 100)...)
			calls := collectCalls(lines, b.start+1, b.end)
			res.Calls = append(res.Calls, calls...)
			for _, target := range calls {
				res.Edges = append(res.Edges, codeindex.Edge{
					Source: b.symbol, Target: target, EdgeType: codeindex.EdgeCall, SourceFile: path,
				})
			}
			continue
		}
		res.Chunks = append(res.Chunks, chunk)
	}
	return res
}

func tsImport(line string) (string, bool) {
	if m := tsImportFromRe.FindStringSubmatch(line); m != nil {
		return m[1], true
	}
	if m := tsImportBareRe.FindStringSubmatch(line); m != nil {
		return m[1], true
	}
	if m := tsRequireRe.FindStringSubmatch(line); m != nil {
		return m[1], true
	}
	return "", false
}

// braceDelta 统计一行的花括号净增量，跳过字符串与行注释。
func braceDelta(line string) int {
	delta := 0
	var quote rune
	escaped := false
	for _, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			switch r {
			case '\\':
				escaped = true
			case quote:
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"', '`':
			quote = r
		case '/':
			// 行注释由调用方前置判断，这里只需处理字符串
		case '{':
			delta++
		case '}':
			delta--
		}
	}
	return delta
}

// blockEndTS 返回从 start 行开始的代码块最后一行（0-based）。
func blockEndTS(lines []string, start int) int {
	depth := 0
	seen := false
	for i := start; i < len(lines); i++ {
		depth += braceDelta(lines[i])
		if strings.Contains(lines[i], "{") {
			seen = true
		}
		if seen && depth <= 0 {
			return i
		}
	}
	return len(lines) - 1
}

// parsePython 用缩进层级确定块范围，同时维护 class 上下文用于方法命名。
func parsePython(path string, src []byte, parserID string) codeindex.ParseResult {
	lines := strings.Split(string(src), "\n")
	res := codeindex.ParseResult{ParserID: parserID, Capability: string(CapabilityRegex)}

	type scope struct {
		indent  int
		symbol  string
		isClass bool
	}
	var stack []scope
	var blocks []lineBlock
	lastImport := -1

	for i, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if m := pyFromRe.FindStringSubmatch(raw); m != nil {
			res.Imports = append(res.Imports, m[1])
			res.Edges = append(res.Edges, codeindex.Edge{
				Source: path, Target: m[1], EdgeType: codeindex.EdgeImport, SourceFile: path,
			})
			lastImport = i
			continue
		}
		if m := pyImportRe.FindStringSubmatch(raw); m != nil {
			for _, module := range strings.Split(m[1], ",") {
				module = strings.TrimSpace(module)
				if module == "" {
					continue
				}
				res.Imports = append(res.Imports, module)
				res.Edges = append(res.Edges, codeindex.Edge{
					Source: path, Target: module, EdgeType: codeindex.EdgeImport, SourceFile: path,
				})
			}
			lastImport = i
			continue
		}

		indent := lineIndent(raw)
		for len(stack) > 0 && indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}

		kind := codeindex.ChunkType("")
		symbol := ""
		if m := pyClassRe.FindStringSubmatch(raw); m != nil {
			kind = codeindex.ChunkClass
			symbol = m[2]
		} else if m := pyDefRe.FindStringSubmatch(raw); m != nil {
			kind = codeindex.ChunkFunction
			symbol = m[2]
			if len(stack) > 0 && stack[len(stack)-1].isClass {
				kind = codeindex.ChunkMethod
				symbol = stack[len(stack)-1].symbol + "." + symbol
			}
		}
		if kind == "" {
			continue
		}
		end := blockEndPy(lines, i, indent)
		blocks = append(blocks, lineBlock{symbol: symbol, chunkType: kind, start: i, end: end})
		stack = append(stack, scope{indent: indent, symbol: symbol, isClass: kind == codeindex.ChunkClass})
	}

	if lastImport >= 0 {
		header := codeindex.NewChunk(path, 1, lastImport+1, "", codeindex.ChunkHeader,
			joinLines(lines, 1, lastImport+1), parserID)
		res.Chunks = append(res.Chunks, header)
	}
	for _, b := range blocks {
		text := joinLines(lines, b.start+1, b.end+1)
		if text == "" {
			continue
		}
		res.Symbols = append(res.Symbols, b.symbol)
		chunk := codeindex.NewChunk(path, b.start+1, b.end+1, b.symbol, b.chunkType, text, parserID)
		if b.chunkType == codeindex.ChunkFunction || b.chunkType == codeindex.ChunkMethod {
			res.Chunks = append(res.Chunks, codeindex.SplitLongFunction(chunk, 1200, 800, 100)...)
			calls := collectCalls(lines, b.start+1, b.end)
			res.Calls = append(res.Calls, calls...)
			for _, target := range calls {
				res.Edges = append(res.Edges, codeindex.Edge{
					Source: b.symbol, Target: target, EdgeType: codeindex.EdgeCall, SourceFile: path,
				})
			}
			continue
		}
		res.Chunks = append(res.Chunks, chunk)
	}
	return res
}

func blockEndPy(lines []string, start, indent int) int {
	end := start
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if lineIndent(lines[i]) <= indent {
			return end
		}
		end = i
	}
	return end
}

func lineIndent(line string) int {
	width := 0
	for _, r := range line {
		switch r {
		case ' ':
			width++
		case '\t':
			width += 4
		case '\r':
			continue
		default:
			return width
		}
	}
	return width
}

// collectCalls 在 [from, to] 行区间内抽取调用名（0-based，含端点）。
func collectCalls(lines []string, from, to int) []string {
	if to >= len(lines) {
		to = len(lines) - 1
	}
	var out []string
	for i := from; i <= to; i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, m := range callRe.FindAllStringSubmatch(line, -1) {
			name := m[1]
			if callKeywords[name] || isNumericToken(name) {
				continue
			}
			out = append(out, name)
		}
	}
	return out
}

func isNumericToken(s string) bool {
	if s == "" {
		return true
	}
	first := s[0]
	return first >= '0' && first <= '9'
}
