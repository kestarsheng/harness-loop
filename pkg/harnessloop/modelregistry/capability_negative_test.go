package modelregistry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNegativeIncompleteMetadata(t *testing.T) {
	bodies := map[string]string{
		"missing window": `version: 1
models:
  - id: m
    provider: p
    kind: chat
    max_output_tokens: 10
    price_in_per_1k: 0
    price_out_per_1k: 0
`,
		"missing price": `version: 1
models:
  - id: m
    provider: p
    kind: chat
    context_window: 100
    max_output_tokens: 10
`,
		"window inverted": `version: 1
models:
  - id: m
    provider: p
    kind: chat
    context_window: 100
    max_output_tokens: 500
    price_in_per_1k: 0
    price_out_per_1k: 0
`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "models.yaml")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("incomplete metadata must be rejected")
			}
			var me *MetaError
			if !errors.As(err, &me) || me.Code != CodeMetaIncomplete {
				t.Fatalf("error must be %s: %v", CodeMetaIncomplete, err)
			}
		})
	}
}

func TestNegativeNonToolModelRejected(t *testing.T) {
	r := DefaultRegistry()
	_, err := r.Resolve(ResolveRequest{ModelID: "qwen-lite", Stage: "c", RequireToolCall: true})
	var ce *CapabilityError
	if !errors.As(err, &ce) {
		t.Fatalf("non-tool model must be rejected: %v", err)
	}
	if len(ce.Suggested) == 0 {
		t.Fatal("suggested models must not be empty")
	}
	for _, id := range ce.Suggested {
		m, ok := r.Get(id)
		if !ok {
			t.Fatalf("suggested model %s does not exist", id)
		}
		if !m.ToolCall {
			t.Fatalf("suggested model %s does not support tool calls", id)
		}
	}
}

func TestNegativeWindowTooSmallRejected(t *testing.T) {
	r := DefaultRegistry()
	_, err := r.Resolve(ResolveRequest{ModelID: "qwen-lite", Stage: "c", RequiredContextWindow: 100000})
	var ce *CapabilityError
	if !errors.As(err, &ce) {
		t.Fatalf("small window must be rejected: %v", err)
	}
	for _, id := range ce.Suggested {
		m, _ := r.Get(id)
		if m.ContextWindow < 100000 {
			t.Fatalf("suggested model %s window %d < required", id, m.ContextWindow)
		}
	}
}

func TestNegativeUnknownAndEmbeddingModels(t *testing.T) {
	r := DefaultRegistry()
	if _, err := r.Resolve(ResolveRequest{ModelID: "ghost-model", Stage: "c"}); err == nil {
		t.Fatal("unknown model must be rejected")
	}
	if _, err := r.Resolve(ResolveRequest{ModelID: "text-embedding-v3", Stage: "c"}); err == nil {
		t.Fatal("embedding model must not be selected for chat stages")
	}
	if err := r.CheckStageCapability("qwen-lite", "c", true); err == nil {
		t.Fatal("stage capability check must reject non-tool model")
	}
}

func TestNegativeCredentialNeverExposed(t *testing.T) {
	r := DefaultRegistry()
	for _, v := range r.Views() {
		if !v.HasCredential {
			t.Fatalf("view must report credential presence for %s", v.ID)
		}
	}
	ref, ok := r.ResolveCredential("qwen-max")
	if !ok || ref == "" {
		t.Fatal("credential ref must resolve internally")
	}
	if ref != "env:DASHSCOPE_API_KEY" {
		t.Fatalf("unexpected ref format: %s", ref)
	}
}

func TestNegativeNoSuggestedModelOutsideConstraints(t *testing.T) {
	r := DefaultRegistry()
	res, err := r.Resolve(ResolveRequest{Stage: "c", RequireToolCall: true, RequiredContextWindow: 100000, Strategy: StrategyCostFirst})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Model.ToolCall || res.Model.ContextWindow < 100000 {
		t.Fatalf("chosen model violates constraints: %+v", res.Model)
	}
	for _, id := range res.Candidates {
		m, _ := r.Get(id)
		if !m.ToolCall || m.ContextWindow < 100000 {
			t.Fatalf("candidate %s violates constraints", id)
		}
	}
}
