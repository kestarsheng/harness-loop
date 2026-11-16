package parser

import (
	"errors"
	"strings"
	"testing"

	"agent-platform/pkg/codeindex"
)

const tsSource = `import { readFile } from "node:fs";
import React from "react";

export class UserService {
  constructor(private repo: Repo) {}

  async loadUser(id: string): Promise<User> {
    const raw = await readFile(id);
    return parseUser(raw);
  }
}

export function parseUser(raw: string): User {
  return JSON.parse(raw) as User;
}

export const buildIndex = (rows: string[]) => {
  return rows.map(parseUser);
};
`

const pySource = `import os
from typing import List

class Loader:
    def __init__(self, root: str):
        self.root = root

    def load(self, name: str) -> str:
        path = os.path.join(self.root, name)
        return read_file(path)

def read_file(path: str) -> str:
    with open(path) as f:
        return f.read()

def summarize(files: List[str]) -> int:
    return len(files)
`

func symbolSet(res codeindex.ParseResult) map[string]codeindex.ChunkType {
	out := map[string]codeindex.ChunkType{}
	for _, chunk := range res.Chunks {
		if chunk.ChunkType == codeindex.ChunkHeader || chunk.ChunkType == codeindex.ChunkFile {
			continue
		}
		out[chunk.Symbol] = chunk.ChunkType
	}
	return out
}

func edgeTargets(res codeindex.ParseResult, edgeType string) map[string]bool {
	out := map[string]bool{}
	for _, edge := range res.Edges {
		if edge.EdgeType == edgeType {
			out[edge.Target] = true
		}
	}
	return out
}

func TestMultilangTypeScriptSymbols(t *testing.T) {
	res := NewRegistry().Parse("user_service.ts", []byte(tsSource))
	if res.Degraded {
		t.Fatalf("typescript source must not degrade: %s", Describe(res))
	}
	if res.Capability != string(CapabilityFor(LangTypeScript)) {
		t.Fatalf("capability = %s", res.Capability)
	}
	if TreesitterEnabled() && res.ParserID != "ts_tree_sitter" {
		t.Fatalf("parser id = %s, want ts_tree_sitter", res.ParserID)
	}
	if !TreesitterEnabled() && res.ParserID != "ts_regex" {
		t.Fatalf("parser id = %s, want ts_regex", res.ParserID)
	}

	symbols := symbolSet(res)
	want := map[string]codeindex.ChunkType{
		"UserService":             codeindex.ChunkClass,
		"UserService.constructor": codeindex.ChunkMethod,
		"UserService.loadUser":    codeindex.ChunkMethod,
		"parseUser":               codeindex.ChunkFunction,
		"buildIndex":              codeindex.ChunkFunction,
	}
	for symbol, kind := range want {
		got, ok := symbols[symbol]
		if !ok {
			t.Fatalf("missing symbol %s: %v", symbol, symbols)
		}
		if got != kind {
			t.Fatalf("symbol %s kind = %s, want %s", symbol, got, kind)
		}
	}

	if imports := edgeTargets(res, codeindex.EdgeImport); !imports["node:fs"] || !imports["react"] {
		t.Fatalf("import edges = %v", imports)
	}
	calls := edgeTargets(res, codeindex.EdgeCall)
	for _, want := range []string{"readFile", "parseUser", "parse", "map"} {
		if !calls[want] {
			t.Fatalf("call edges missing %s: %v", want, calls)
		}
	}
	if calls["if"] || calls["function"] || calls["class"] {
		t.Fatalf("control keywords must not become call edges: %v", calls)
	}
}

func TestMultilangPythonSymbols(t *testing.T) {
	res := NewRegistry().Parse("loader.py", []byte(pySource))
	if res.Degraded {
		t.Fatalf("python source must not degrade: %s", Describe(res))
	}
	if res.Capability != string(CapabilityFor(LangPython)) {
		t.Fatalf("capability = %s", res.Capability)
	}
	if TreesitterEnabled() && res.ParserID != "py_tree_sitter" {
		t.Fatalf("parser id = %s, want py_tree_sitter", res.ParserID)
	}

	symbols := symbolSet(res)
	want := map[string]codeindex.ChunkType{
		"Loader":          codeindex.ChunkClass,
		"Loader.__init__": codeindex.ChunkMethod,
		"Loader.load":     codeindex.ChunkMethod,
		"read_file":       codeindex.ChunkFunction,
		"summarize":       codeindex.ChunkFunction,
	}
	for symbol, kind := range want {
		got, ok := symbols[symbol]
		if !ok {
			t.Fatalf("missing symbol %s: %v", symbol, symbols)
		}
		if got != kind {
			t.Fatalf("symbol %s kind = %s, want %s", symbol, got, kind)
		}
	}

	imports := edgeTargets(res, codeindex.EdgeImport)
	for _, want := range []string{"os", "typing"} {
		if !imports[want] {
			t.Fatalf("import edges missing %s: %v", want, imports)
		}
	}
	calls := edgeTargets(res, codeindex.EdgeCall)
	for _, want := range []string{"join", "read_file", "open", "read", "len"} {
		if !calls[want] {
			t.Fatalf("call edges missing %s: %v", want, calls)
		}
	}
	if calls["def"] || calls["with"] || calls["return"] {
		t.Fatalf("control keywords must not become call edges: %v", calls)
	}
}

func TestMultilangChunksCarryPositions(t *testing.T) {
	for name, src := range map[string]string{"a.ts": tsSource, "a.py": pySource} {
		res := NewRegistry().Parse(name, []byte(src))
		if len(res.Chunks) == 0 {
			t.Fatalf("%s produced no chunks", name)
		}
		for _, chunk := range res.Chunks {
			if chunk.StartLine <= 0 || chunk.EndLine < chunk.StartLine {
				t.Fatalf("%s chunk %s bad range: %d-%d", name, chunk.Symbol, chunk.StartLine, chunk.EndLine)
			}
			if chunk.Text == "" || !strings.HasPrefix(chunk.ContentHash, "sha256:") {
				t.Fatalf("%s chunk %s incomplete: %+v", name, chunk.Symbol, chunk)
			}
			if chunk.ParserID == "" {
				t.Fatalf("%s chunk %s missing parser id", name, chunk.Symbol)
			}
		}
	}
}

func TestMultilangCapabilityReport(t *testing.T) {
	caps := Capabilities()
	wantTs := CapabilityRegex
	if TreesitterEnabled() {
		wantTs = CapabilityFull
	}
	if caps[LangTypeScript] != wantTs || caps[LangPython] != wantTs {
		t.Fatalf("capabilities = %v, want ts/py=%s", caps, wantTs)
	}
	if caps[LangGo] != CapabilityFull {
		t.Fatalf("go capability = %s", caps[LangGo])
	}
	if caps[LangUnknown] != CapabilityNone {
		t.Fatalf("unknown language must have no capability: %v", caps)
	}
	// 注册表暴露同一份能力视图，供索引状态查询。
	if got := NewRegistry().Capabilities()[LangPython]; got != wantTs {
		t.Fatalf("registry capability = %s", got)
	}
}

type failingParser struct{ lang string }

func (f failingParser) ID() string                { return "failing" }
func (f failingParser) Capability() Capability    { return CapabilityFull }
func (f failingParser) Supports(lang string) bool { return lang == f.lang }
func (f failingParser) Parse(string, []byte) (codeindex.ParseResult, error) {
	return codeindex.ParseResult{}, errors.New("boom")
}

func TestMultilangFallsBackToRegex(t *testing.T) {
	registry := &Registry{}
	registry.Register(failingParser{lang: LangTypeScript})
	registry.Register(newRegexParser(LangTypeScript))

	res := registry.Parse("a.ts", []byte("export function a() { return 1 }\n"))
	if res.Degraded {
		t.Fatalf("regex fallback must succeed: %s", Describe(res))
	}
	if res.ParserID != "ts_regex" || res.Capability != string(CapabilityRegex) {
		t.Fatalf("fallback parser = %s/%s", res.ParserID, res.Capability)
	}
	if len(res.Symbols) != 1 || res.Symbols[0] != "a" {
		t.Fatalf("symbols = %v", res.Symbols)
	}
}

func TestMultilangDegradesWhenAllParsersFail(t *testing.T) {
	registry := &Registry{}
	registry.Register(failingParser{lang: LangTypeScript})
	registry.Register(failingParser{lang: LangTypeScript})

	res := registry.Parse("a.ts", []byte("export function a() {}\n"))
	if !res.Degraded || len(res.Chunks) != 1 || res.Chunks[0].ChunkType != codeindex.ChunkFile {
		t.Fatalf("all-failure must degrade: %s", Describe(res))
	}
	if res.Capability != string(CapabilityNone) {
		t.Fatalf("degraded result must not claim capability: %s", res.Capability)
	}
}
