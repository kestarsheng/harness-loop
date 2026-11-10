package modelregistry

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDefaultRegistryLoadsSeed(t *testing.T) {
	r := DefaultRegistry()
	if r.Version != 3 {
		t.Fatalf("version = %d", r.Version)
	}
	if len(r.IDs()) != 4 {
		t.Fatalf("models = %v", r.IDs())
	}
	if w, err := r.ContextWindowOf("qwen-max"); err != nil || w <= 0 {
		t.Fatalf("window = %d err=%v", w, err)
	}
	if m, ok := r.EmbeddingModel(); !ok || m.EmbeddingDim != 1024 {
		t.Fatalf("embedding model = %+v", m)
	}
	if _, err := r.ContextWindowOf("nope"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unknown model must fail: %v", err)
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")
	body := `
version: 2
models:
  - id: small
    provider: p
    kind: chat
    context_window: 4096
    max_output_tokens: 1024
    tool_call: true
    structured_output: false
    price_in_per_1k: 0.001
    price_out_per_1k: 0.002
    status: available
    api_key_ref: env:KEY
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if r.Version != 2 || len(r.IDs()) != 1 {
		t.Fatalf("unexpected registry: %+v", r.IDs())
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file must fail")
	}
}

func TestRejectIncompleteMeta(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing window", "version: 1\nmodels:\n  - id: m\n    provider: p\n    kind: chat\n    max_output_tokens: 10\n    price_in_per_1k: 0\n    price_out_per_1k: 0\n"},
		{"missing provider", "version: 1\nmodels:\n  - id: m\n    kind: chat\n    context_window: 100\n    max_output_tokens: 10\n    price_in_per_1k: 0\n    price_out_per_1k: 0\n"},
		{"max output exceeds window", "version: 1\nmodels:\n  - id: m\n    provider: p\n    kind: chat\n    context_window: 100\n    max_output_tokens: 200\n    price_in_per_1k: 0\n    price_out_per_1k: 0\n"},
		{"embedding without dim", "version: 1\nmodels:\n  - id: m\n    provider: p\n    kind: embedding\n    context_window: 100\n    max_output_tokens: 1\n    price_in_per_1k: 0\n    price_out_per_1k: 0\n"},
		{"chat with embedding dim", "version: 1\nmodels:\n  - id: m\n    provider: p\n    kind: chat\n    context_window: 100\n    max_output_tokens: 10\n    embedding_dim: 1024\n    price_in_per_1k: 0\n    price_out_per_1k: 0\n"},
		{"bad kind", "version: 1\nmodels:\n  - id: m\n    provider: p\n    kind: vision\n    context_window: 100\n    max_output_tokens: 10\n    price_in_per_1k: 0\n    price_out_per_1k: 0\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "models.yaml")
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid metadata must be rejected")
			}
		})
	}
}

func TestDuplicateModelID(t *testing.T) {
	body := `
version: 1
models:
  - id: dup
    provider: p
    kind: chat
    context_window: 100
    max_output_tokens: 10
    price_in_per_1k: 0
    price_out_per_1k: 0
  - id: dup
    provider: p
    kind: chat
    context_window: 100
    max_output_tokens: 10
    price_in_per_1k: 0
    price_out_per_1k: 0
`
	path := filepath.Join(t.TempDir(), "models.yaml")
	os.WriteFile(path, []byte(body), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), CodeDuplicateID) {
		t.Fatalf("duplicate id must fail: %v", err)
	}
}

func TestResolveToolCallMismatch(t *testing.T) {
	r := DefaultRegistry()
	_, err := r.Resolve(ResolveRequest{ModelID: "qwen-lite", Stage: "c", RequireToolCall: true})
	var ce *CapabilityError
	if !errors.As(err, &ce) {
		t.Fatalf("expected capability error, got %v", err)
	}
	if ce.Error() == "" || !strings.Contains(ce.Error(), CodeCapabilityMismatch) {
		t.Fatalf("error must carry code: %v", err)
	}
	suggested := strings.Join(ce.Suggested, ",")
	if !strings.Contains(suggested, "qwen-max") || !strings.Contains(suggested, "qwen-plus") {
		t.Fatalf("suggested = %v", ce.Suggested)
	}
	if strings.Contains(suggested, "qwen-lite") {
		t.Fatalf("non-tool model must not be suggested: %v", ce.Suggested)
	}
}

func TestResolveByStrategy(t *testing.T) {
	r := DefaultRegistry()

	res, err := r.Resolve(ResolveRequest{Stage: "c", RequireToolCall: true, Strategy: StrategyCostFirst})
	if err != nil {
		t.Fatalf("cost first: %v", err)
	}
	if res.Model.ID != "qwen-plus" {
		t.Fatalf("cost first chose %s", res.Model.ID)
	}

	res, err = r.Resolve(ResolveRequest{Stage: "c", RequireToolCall: true, Strategy: StrategyQualityFirst})
	if err != nil {
		t.Fatalf("quality first: %v", err)
	}
	if res.Model.ID != "qwen-max" {
		t.Fatalf("quality first chose %s", res.Model.ID)
	}
}

func TestResolveInheritsTaskModel(t *testing.T) {
	r := DefaultRegistry()
	res, err := r.Resolve(ResolveRequest{ModelID: "qwen-plus", Stage: "b", RequireToolCall: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Model.ID != "qwen-plus" {
		t.Fatalf("inherited model = %s", res.Model.ID)
	}
}

func TestResolveStageOverride(t *testing.T) {
	r := DefaultRegistry()
	req := ResolveRequest{
		ModelID:  "qwen-plus",
		Stage:    "d",
		Strategy: StrategyQualityFirst,
		StageOverrides: map[string]StageModel{
			"d": {Model: "qwen-max", RequireToolCall: true},
		},
	}
	res, err := r.Resolve(req)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Model.ID != "qwen-max" {
		t.Fatalf("stage override not applied: %s", res.Model.ID)
	}

	req.Stage = "c"
	res, err = r.Resolve(req)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Model.ID != "qwen-plus" {
		t.Fatalf("non-overridden stage must inherit: %s", res.Model.ID)
	}
}

func TestResolveWindowRequirement(t *testing.T) {
	r := DefaultRegistry()
	res, err := r.Resolve(ResolveRequest{Stage: "c", RequireToolCall: true, RequiredContextWindow: 100000})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Model.ContextWindow < 100000 {
		t.Fatalf("window requirement ignored: %d", res.Model.ContextWindow)
	}

	if _, err := r.Resolve(ResolveRequest{ModelID: "qwen-max", Stage: "c", RequiredContextWindow: 100000}); err == nil {
		t.Fatal("model with smaller window must be rejected")
	}
}

func TestResolveUnknownModel(t *testing.T) {
	r := DefaultRegistry()
	if _, err := r.Resolve(ResolveRequest{ModelID: "ghost", Stage: "c"}); err == nil {
		t.Fatal("unknown model must fail")
	}
	if _, err := r.Resolve(ResolveRequest{ModelID: "text-embedding-v3", Stage: "c"}); err == nil {
		t.Fatal("embedding model must not be used for chat stages")
	}
}

func TestCheckStageCapability(t *testing.T) {
	r := DefaultRegistry()
	if err := r.CheckStageCapability("qwen-max", "c", true); err != nil {
		t.Fatalf("qwen-max must support tools: %v", err)
	}
	err := r.CheckStageCapability("qwen-lite", "c", true)
	var ce *CapabilityError
	if !errors.As(err, &ce) || len(ce.Suggested) == 0 {
		t.Fatalf("qwen-lite must be rejected with suggestions: %v", err)
	}
	if r.SupportsToolCall("qwen-lite") {
		t.Fatal("qwen-lite must not support tool calls")
	}
}

func TestViewsHaveNoSecret(t *testing.T) {
	r := DefaultRegistry()
	views := r.Views()
	if len(views) == 0 {
		t.Fatal("no views")
	}
	for _, v := range views {
		if !v.HasCredential {
			t.Fatalf("view must report credential presence for %s", v.ID)
		}
	}
	ref, ok := r.ResolveCredential("qwen-max")
	if !ok || !strings.HasPrefix(ref, "env:") {
		t.Fatalf("credential ref = %q", ref)
	}
	if _, ok := r.ResolveCredential("ghost"); ok {
		t.Fatal("unknown model must not resolve credential")
	}
}

func TestNoHardcodedWindow(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.INT {
				return true
			}
			v, err := strconv.Atoi(lit.Value)
			if err != nil {
				return true
			}
			if v > 10000 {
				t.Fatalf("%s contains hardcoded context window literal %s; windows must come from the registry", name, lit.Value)
			}
			return true
		})
	}
}
