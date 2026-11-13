package parser

import (
	"os"
	"strings"
	"testing"

	"agent-platform/pkg/codeindex"
)

const sampleSource = `package sample

import (
	"fmt"
	"os"
	"strings"
	"errors"
	"time"
)

type User struct{}

func A() {
	fmt.Println()
	os.Getenv("X")
	strings.TrimSpace(" y ")
	errors.New("e")
	time.Now()
}

func B() {
	fmt.Println()
	os.Getenv("X")
	strings.TrimSpace(" y ")
	errors.New("e")
	time.Now()
}

func C() {
	fmt.Println()
	os.Getenv("X")
	strings.TrimSpace(" y ")
	errors.New("e")
	time.Now()
}

func D() {}
func E() {}
func F() {}
func G() {}
func H() {}

func (u *User) GetName() string { return "" }

func (u *User) SetName(name string) {}
`

func parseSample(t *testing.T, path, src string) codeindex.ParseResult {
	t.Helper()
	return NewRegistry().Parse(path, []byte(src))
}

func TestGoASTCountsMatchAnnotation(t *testing.T) {
	res := parseSample(t, "sample.go", sampleSource)
	if res.Degraded {
		t.Fatalf("valid go source must not degrade: %+v", res)
	}
	if res.ParserID != "go_ast" {
		t.Fatalf("parser id = %s", res.ParserID)
	}
	if len(res.Imports) != 5 {
		t.Fatalf("imports = %d, want 5", len(res.Imports))
	}

	functions := 0
	for _, chunk := range res.Chunks {
		if chunk.ChunkType == codeindex.ChunkFunction || chunk.ChunkType == codeindex.ChunkMethod {
			functions++
		}
	}
	if functions != 10 {
		t.Fatalf("function chunks = %d, want 10", functions)
	}

	callEdges := 0
	importEdges := 0
	for _, edge := range res.Edges {
		switch edge.EdgeType {
		case codeindex.EdgeCall:
			callEdges++
		case codeindex.EdgeImport:
			importEdges++
		}
	}
	if callEdges != 15 {
		t.Fatalf("call edges = %d, want 15", callEdges)
	}
	if importEdges != 5 {
		t.Fatalf("import edges = %d, want 5", importEdges)
	}
}

func TestChunkHasRequiredFields(t *testing.T) {
	res := parseSample(t, "sample.go", sampleSource)
	for _, chunk := range res.Chunks {
		if chunk.FilePath == "" || chunk.StartLine <= 0 || chunk.EndLine < chunk.StartLine {
			t.Fatalf("chunk missing position: %+v", chunk)
		}
		if chunk.Text == "" {
			t.Fatalf("chunk %s has empty text", chunk.Symbol)
		}
		if !strings.HasPrefix(chunk.ContentHash, "sha256:") {
			t.Fatalf("chunk %s hash = %s", chunk.Symbol, chunk.ContentHash)
		}
		if chunk.ParserID != "go_ast" {
			t.Fatalf("chunk %s parser id = %s", chunk.Symbol, chunk.ParserID)
		}
	}
}

func TestMethodSymbolUsesReceiver(t *testing.T) {
	res := parseSample(t, "sample.go", sampleSource)
	symbols := map[string]bool{}
	for _, chunk := range res.Chunks {
		symbols[chunk.Symbol] = true
	}
	if !symbols["User.GetName"] || !symbols["User.SetName"] {
		t.Fatalf("method symbols missing: %v", symbols)
	}
	if !symbols["A"] || !symbols["H"] {
		t.Fatalf("function symbols missing: %v", symbols)
	}
}

func TestHeaderChunkCoversImports(t *testing.T) {
	res := parseSample(t, "sample.go", sampleSource)
	if len(res.Chunks) == 0 || res.Chunks[0].ChunkType != codeindex.ChunkHeader {
		t.Fatalf("first chunk must be header: %+v", res.Chunks[0])
	}
	if res.Chunks[0].StartLine != 1 {
		t.Fatalf("header start line = %d", res.Chunks[0].StartLine)
	}
	if !strings.Contains(res.Chunks[0].Text, "import (") {
		t.Fatalf("header text must cover imports: %q", res.Chunks[0].Text)
	}
}

func TestParseFailureDegradesToFileChunk(t *testing.T) {
	broken := "package broken\n\nfunc broken( {\n"
	res := parseSample(t, "broken.go", broken)
	if !res.Degraded {
		t.Fatal("broken source must degrade")
	}
	if len(res.Chunks) != 1 || res.Chunks[0].ChunkType != codeindex.ChunkFile || !res.Chunks[0].Degraded {
		t.Fatalf("degraded result = %+v", res.Chunks)
	}
}

func TestUnsupportedLanguageDegrades(t *testing.T) {
	res := parseSample(t, "app.ts", "export function a() { return 1 }\n")
	if !res.Degraded || len(res.Chunks) != 1 || res.Chunks[0].ChunkType != codeindex.ChunkFile {
		t.Fatalf("unsupported language must degrade: %+v", res)
	}
}

func TestDetectLanguage(t *testing.T) {
	cases := map[string]string{
		"a.go": "go", "a.ts": "typescript", "a.tsx": "typescript",
		"a.py": "python", "a.md": "unknown",
	}
	for path, want := range cases {
		if got := Detect(path); got != want {
			t.Fatalf("%s -> %s, want %s", path, got, want)
		}
	}
}

func TestIdentifierTokensMatchCamelAndSnake(t *testing.T) {
	camel := codeindex.IdentifierTokens("getUserProfile")
	snake := codeindex.IdentifierTokens("get_user_profile")
	for _, tokens := range [][]string{camel, snake} {
		joined := strings.Join(tokens, " ")
		if !strings.Contains(joined, "user") || !strings.Contains(joined, "profile") {
			t.Fatalf("tokens = %v", tokens)
		}
	}
	query := codeindex.IdentifierTokens("user profile")
	for _, want := range []string{"user", "profile"} {
		found := false
		for _, token := range query {
			if token == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("query tokens missing %s: %v", want, query)
		}
	}
}

func TestLongFunctionIsSplit(t *testing.T) {
	var body strings.Builder
	body.WriteString("package long\n\nfunc Big() {\n")
	for i := 0; i < 400; i++ {
		body.WriteString("\tprintln(\"line ")
		body.WriteString(strings.Repeat("x", 40))
		body.WriteString("\")\n")
	}
	body.WriteString("}\n")
	res := parseSample(t, "long.go", body.String())
	if res.Degraded {
		t.Fatalf("long function should parse: %+v", res)
	}
	parts := 0
	for _, chunk := range res.Chunks {
		if chunk.ChunkType == codeindex.ChunkFunctionPart && chunk.ParentSymbol == "Big" {
			parts++
		}
	}
	if parts < 2 {
		t.Fatalf("long function must be split into parts, got %d", parts)
	}
	for _, chunk := range res.Chunks {
		if chunk.ChunkType == codeindex.ChunkFunctionPart {
			if chunk.StartLine < 1 || chunk.EndLine < chunk.StartLine {
				t.Fatalf("part position invalid: %+v", chunk)
			}
		}
	}
}

func TestParseFileReadsDisk(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sample.go"
	if err := writeFile(path, sampleSource); err != nil {
		t.Fatal(err)
	}
	res, err := NewRegistry().ParseFile(path)
	if err != nil {
		t.Fatalf("parse file: %v", err)
	}
	if len(res.Chunks) == 0 || res.ParserID != "go_ast" {
		t.Fatalf("unexpected result: %s", Describe(res))
	}
	if _, err := NewRegistry().ParseFile(dir + "/missing.go"); err == nil {
		t.Fatal("missing file must fail")
	}
}
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
