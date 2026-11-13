package parser

import (
	"os"
	"path/filepath"
	"strings"

	"agent-platform/pkg/codeindex"
)

type Parser interface {
	ID() string
	Supports(lang string) bool
	Parse(path string, src []byte) (codeindex.ParseResult, error)
}

type Registry struct {
	parsers []Parser
}

func NewRegistry() *Registry {
	return &Registry{parsers: []Parser{&GoASTParser{}}}
}

func (r *Registry) ParseFile(path string) (codeindex.ParseResult, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return codeindex.ParseResult{}, err
	}
	return r.Parse(path, src), nil
}

func (r *Registry) Parse(path string, src []byte) codeindex.ParseResult {
	lang := Detect(path)
	for _, p := range r.parsers {
		if !p.Supports(lang) {
			continue
		}
		res, err := p.Parse(path, src)
		if err == nil {
			res.ParserID = p.ID()
			return res
		}
		break
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

func Detect(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".py":
		return "python"
	default:
		return "unknown"
	}
}
