package parser

import (
	"os"
	"strings"

	"agent-platform/pkg/codeindex"
)

// Parser 是单语言解析器。Capability 用于在索引状态里声明解析保真度。
type Parser interface {
	ID() string
	Capability() Capability
	Supports(lang string) bool
	Parse(path string, src []byte) (codeindex.ParseResult, error)
}

type Registry struct {
	parsers []Parser
}

// NewRegistry 按「精确解析优先、正则兜底」注册解析器：
//
//	go         -> go/ast
//	typescript -> tree-sitter（cgo+treesitter 构建）-> regex
//	python     -> tree-sitter（cgo+treesitter 构建）-> regex
//
// 无 cgo 或未启用 tags 时 newTreeSitterParser 返回 nil，注册表只剩 regex，
// 保证 `go build ./...` 与无标签测试恒可通过。
func NewRegistry() *Registry {
	registry := &Registry{}
	registry.Register(&GoASTParser{})
	for _, lang := range []string{LangTypeScript, LangPython} {
		if p := newTreeSitterParser(lang); p != nil {
			registry.Register(p)
		}
		if p := newRegexParser(lang); p != nil {
			registry.Register(p)
		}
	}
	return registry
}

func (r *Registry) Register(p Parser) {
	if p == nil {
		return
	}
	r.parsers = append(r.parsers, p)
}

// Capabilities 返回注册表可见的语言能力（按当前构建）。
func (r *Registry) Capabilities() map[string]Capability { return Capabilities() }

func (r *Registry) ParseFile(path string) (codeindex.ParseResult, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return codeindex.ParseResult{}, err
	}
	return r.Parse(path, src), nil
}

// Parse 依次尝试支持该语言的解析器：语法级解析失败时自动降级到正则，
// 全部失败才产出文件级 degraded chunk（不返回 error，避免中断索引批）。
func (r *Registry) Parse(path string, src []byte) codeindex.ParseResult {
	lang := Detect(path)
	for _, p := range r.parsers {
		if !p.Supports(lang) {
			continue
		}
		res, err := p.Parse(path, src)
		if err != nil {
			continue
		}
		res.ParserID = p.ID()
		res.Capability = string(p.Capability())
		return res
	}
	return degradedResult(path, src, "no_parser_or_parse_failed")
}

func degradedResult(path string, src []byte, reason string) codeindex.ParseResult {
	text := string(src)
	lines := strings.Count(text, "\n") + 1
	chunk := codeindex.NewChunk(path, 1, lines, "", codeindex.ChunkFile, text, "fallback")
	chunk.Degraded = true
	return codeindex.ParseResult{
		Chunks:   []codeindex.Chunk{chunk},
		ParserID: "fallback:" + reason,
		Degraded: true,
	}
}

// Detect 返回语言名，未登记的语言统一为 unknown。
func Detect(path string) string { return LanguageOfPath(path) }
