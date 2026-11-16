// Command parser-bench measures the multi-language parser against human
// annotated samples for Go, TypeScript and Python.
//
// Usage (from the repository root):
//
//	go run ./bench/parser                    # regex/ast path (CGO disabled build)
//	go run -tags=treesitter ./bench/parser   # full path (cgo + tree-sitter)
//
// The exit code is non-zero when any language falls below the regression
// threshold, so the script itself carries the gate used by验收.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
)

type labelSymbol struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type labelCall struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type labelFile struct {
	File    string        `json:"file"`
	Lang    string        `json:"lang"`
	Symbols []labelSymbol `json:"symbols"`
	Imports []string      `json:"imports"`
	Calls   []labelCall   `json:"calls"`
	Note    string        `json:"note,omitempty"`
}

type score struct {
	Want int `json:"want"`
	Got  int `json:"got"`
	Hit  int `json:"hit"`
}

func (s score) precision() float64 {
	if s.Got == 0 {
		return 1
	}
	return float64(s.Hit) / float64(s.Got)
}

func (s score) recall() float64 {
	if s.Want == 0 {
		return 1
	}
	return float64(s.Hit) / float64(s.Want)
}

func (s score) f1() float64 {
	p, r := s.precision(), s.recall()
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
}

func (s *score) add(other score) {
	s.Want += other.Want
	s.Got += other.Got
	s.Hit += other.Hit
}

type fileResult struct {
	File       string   `json:"file"`
	Lang       string   `json:"lang"`
	ParserID   string   `json:"parser_id"`
	Capability string   `json:"capability"`
	Degraded   bool     `json:"degraded"`
	Symbols    score    `json:"symbols"`
	Imports    score    `json:"imports"`
	Calls      score    `json:"calls"`
	Misses     []string `json:"misses,omitempty"`
}

type langResult struct {
	Lang      string   `json:"lang"`
	Files     int      `json:"files"`
	Symbols   score    `json:"symbols"`
	Imports   score    `json:"imports"`
	Calls     score    `json:"calls"`
	SymbolF1  float64  `json:"symbol_f1"`
	ImportF1  float64  `json:"import_f1"`
	CallF1    float64  `json:"call_f1"`
	MinF1     float64  `json:"min_f1"`
	Pass      bool     `json:"pass"`
	ParserIDs []string `json:"parser_ids"`
}

type report struct {
	Labels     string       `json:"labels"`
	Treesitter bool         `json:"treesitter"`
	Threshold  float64      `json:"threshold"`
	Files      []fileResult `json:"files"`
	Languages  []langResult `json:"languages"`
	Pass       bool         `json:"pass"`
}

func runAccuracy(args []string) int {
	fs := flag.NewFlagSet("parser", flag.ExitOnError)
	labels := fs.String("labels", filepath.Join("bench", "parser", "labels.jsonl"), "annotation file (jsonl)")
	root := fs.String("root", "", "sample root directory (default: directory of the labels file)")
	threshold := fs.Float64("min-f1", 0.90, "regression threshold applied to symbol/import/call F1 per language")
	jsonOut := fs.String("json", "", "write the machine readable report to this path")
	verbose := fs.Bool("v", false, "print every miss")
	_ = fs.Parse(args)

	cases, err := loadLabels(*labels)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load labels: %v\n", err)
		return 2
	}
	if *root == "" {
		*root = filepath.Dir(*labels)
	}

	registry := parser.NewRegistry()
	rep := report{Labels: *labels, Treesitter: parser.TreesitterEnabled(), Threshold: *threshold}

	byLang := map[string]*langResult{}
	for _, tc := range cases {
		path := filepath.Join(*root, tc.File)
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read sample %s: %v\n", path, err)
			return 2
		}
		res := registry.Parse(path, src)
		fr := compareOne(tc, res)
		rep.Files = append(rep.Files, fr)

		lr := byLang[tc.Lang]
		if lr == nil {
			lr = &langResult{Lang: tc.Lang}
			byLang[tc.Lang] = lr
		}
		lr.Files++
		lr.Symbols.add(fr.Symbols)
		lr.Imports.add(fr.Imports)
		lr.Calls.add(fr.Calls)
		lr.ParserIDs = appendUnique(lr.ParserIDs, fr.ParserID)
	}

	for _, lr := range byLang {
		lr.SymbolF1 = lr.Symbols.f1()
		lr.ImportF1 = lr.Imports.f1()
		lr.CallF1 = lr.Calls.f1()
		lr.MinF1 = min3(lr.SymbolF1, lr.ImportF1, lr.CallF1)
		lr.Pass = lr.MinF1+1e-9 >= *threshold
		rep.Languages = append(rep.Languages, *lr)
	}
	sort.Slice(rep.Languages, func(i, j int) bool { return rep.Languages[i].Lang < rep.Languages[j].Lang })
	rep.Pass = len(rep.Languages) > 0
	for _, lr := range rep.Languages {
		if !lr.Pass {
			rep.Pass = false
		}
	}

	printReport(rep, *verbose)
	if *jsonOut != "" {
		if err := writeJSON(*jsonOut, rep); err != nil {
			fmt.Fprintf(os.Stderr, "write json: %v\n", err)
			return 2
		}
	}
	if !rep.Pass {
		fmt.Printf("\nFAIL: at least one language is below the %.2f threshold\n", *threshold)
		return 1
	}
	fmt.Printf("\nPASS: every language is at or above the %.2f threshold\n", *threshold)
	return 0
}

func loadLabels(path string) ([]labelFile, error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()

	var out []labelFile
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "//") {
			continue
		}
		var item labelFile
		if err := json.Unmarshal([]byte(text), &item); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, item)
	}
	return out, scanner.Err()
}

// compareOne diffs one annotated sample against the parser output. Symbols are
// matched on name+kind, imports on the module string and call edges on the
// (caller, callee-last-segment) pair, which is the convention the indexer uses
// when it writes graph edges.
func compareOne(tc labelFile, res codeindex.ParseResult) fileResult {
	fr := fileResult{
		File:       tc.File,
		Lang:       tc.Lang,
		ParserID:   res.ParserID,
		Capability: res.Capability,
		Degraded:   res.Degraded,
	}

	wantSymbols := map[string]bool{}
	for _, sym := range tc.Symbols {
		wantSymbols[sym.Name+"|"+sym.Kind] = true
	}
	gotSymbols := map[string]bool{}
	for _, chunk := range res.Chunks {
		switch chunk.ChunkType {
		case codeindex.ChunkHeader, codeindex.ChunkFile, codeindex.ChunkFunctionPart:
			continue
		}
		gotSymbols[chunk.Symbol+"|"+string(chunk.ChunkType)] = true
	}
	fr.Symbols = diff(wantSymbols, gotSymbols, &fr.Misses, "symbol")

	wantImports := map[string]bool{}
	for _, module := range tc.Imports {
		wantImports[module] = true
	}
	gotImports := map[string]bool{}
	for _, module := range res.Imports {
		gotImports[module] = true
	}
	fr.Imports = diff(wantImports, gotImports, &fr.Misses, "import")

	wantCalls := map[string]bool{}
	for _, call := range tc.Calls {
		wantCalls[call.From+"->"+call.To] = true
	}
	gotCalls := map[string]bool{}
	for _, edge := range res.Edges {
		if edge.EdgeType != codeindex.EdgeCall || edge.Source == "" || edge.Target == "" {
			continue
		}
		gotCalls[edge.Source+"->"+edge.Target] = true
	}
	fr.Calls = diff(wantCalls, gotCalls, &fr.Misses, "call")
	return fr
}

func diff(want, got map[string]bool, misses *[]string, family string) score {
	out := score{Want: len(want), Got: len(got)}
	for key := range want {
		if got[key] {
			out.Hit++
		} else {
			*misses = append(*misses, family+" missing: "+key)
		}
	}
	for key := range got {
		if !want[key] {
			*misses = append(*misses, family+" extra: "+key)
		}
	}
	return out
}

func printReport(rep report, verbose bool) {
	mode := "regex/ast"
	if rep.Treesitter {
		mode = "cgo+tree-sitter"
	}
	fmt.Printf("parser accuracy report\n")
	fmt.Printf("labels=%s treesitter=%v mode=%s threshold=%.2f\n\n", rep.Labels, rep.Treesitter, mode, rep.Threshold)

	fmt.Printf("%-34s %-14s %-7s %-15s %-15s %-15s\n",
		"file", "parser", "cap", "symbols P/R/F1", "imports P/R/F1", "calls P/R/F1")
	for _, fr := range rep.Files {
		fmt.Printf("%-34s %-14s %-7s %-15s %-15s %-15s\n",
			shorten(fr.File, 34), shorten(fr.ParserID, 14), fr.Capability,
			triple(fr.Symbols), triple(fr.Imports), triple(fr.Calls))
		if verbose {
			for _, miss := range fr.Misses {
				fmt.Printf("    %s\n", miss)
			}
		}
	}

	fmt.Printf("\n%-12s %-6s %-15s %-15s %-15s %-7s %s\n",
		"language", "files", "symbols P/R/F1", "imports P/R/F1", "calls P/R/F1", "min", "verdict")
	for _, lr := range rep.Languages {
		verdict := "PASS"
		if !lr.Pass {
			verdict = "FAIL"
		}
		fmt.Printf("%-12s %-6d %-15s %-15s %-15s %-7.3f %s\n",
			lr.Lang, lr.Files, triple(lr.Symbols), triple(lr.Imports), triple(lr.Calls), lr.MinF1, verdict)
	}
}

func triple(s score) string {
	return fmt.Sprintf("%.3f/%.3f/%.3f", s.precision(), s.recall(), s.f1())
}

func shorten(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return "…" + text[len(text)-limit+1:]
}

func appendUnique(list []string, value string) []string {
	for _, item := range list {
		if item == value {
			return list
		}
	}
	return append(list, value)
}

func min3(a, b, c float64) float64 {
	out := a
	if b < out {
		out = b
	}
	if c < out {
		out = c
	}
	return out
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
