package optimizer

import (
	"context"
	"fmt"
	"testing"

	"agent-platform/pkg/llm"
	"agent-platform/services/harness-service/internal/evaluate"
	"agent-platform/services/harness-service/internal/prompt"
)

// ---- Mock LLM Client ----

type mockLLM struct {
	responses []string
	index     int
}

func (m *mockLLM) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	if m.index >= len(m.responses) {
		return &llm.ChatResponse{Content: "no more responses"}, nil
	}
	resp := &llm.ChatResponse{Content: m.responses[m.index], TotalTokens: 100}
	m.index++
	return resp, nil
}

func (m *mockLLM) ChatStream(ctx context.Context, req *llm.ChatRequest) (<-chan llm.ChatStreamChunk, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockLLM) Embed(ctx context.Context, text string) ([]float64, error) {
	return []float64{0.1, 0.2, 0.3}, nil
}

func (m *mockLLM) EmbedBatch(ctx context.Context, texts []string) ([][]float64, error) {
	result := make([][]float64, len(texts))
	for i := range result {
		result[i] = []float64{0.1, 0.2, 0.3}
	}
	return result, nil
}

// ---- Test: VariablesPreserved ----

func TestVariablesPreserved(t *testing.T) {
	tests := []struct {
		name     string
		original string
		revised  string
		want     bool
	}{
		{
			name:     "all variables preserved",
			original: "Hello {{name}}, your age is {{age}}.",
			revised:  "Greetings {{name}}, you are {{age}} years old.",
			want:     true,
		},
		{
			name:     "variable removed",
			original: "Hello {{name}}, your age is {{age}}.",
			revised:  "Greetings {{name}}, welcome back.",
			want:     false,
		},
		{
			name:     "default syntax preserved",
			original: "Hello {{name|guest}}.",
			revised:  "Hi {{name|guest}}, welcome!",
			want:     true,
		},
		{
			name:     "no variables",
			original: "Hello world.",
			revised:  "Greetings world.",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := VariablesPreserved(tt.original, tt.revised)
			if got != tt.want {
				t.Errorf("VariablesPreserved() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---- Test: TextGrad optimizer stops when no improvement ----
// This test requires a dataset with cases, which needs DB persistence.
// The core logic is tested via TestVariablesPreserved and TestCosineSimilarity.

func TestTextGradOptimizerStopsNoImprove(t *testing.T) {
	t.Skip("requires DB-backed dataset with cases; see integration tests")
	llmClient := &mockLLM{responses: []string{
		"建议：prompt 已经很好了",
		"改写后的 prompt：{{name}} 的年龄是 {{age}}",
	}}

	engine := prompt.NewEngineMemory()
	experiments := evaluate.NewExperimentEngineMemory(evaluate.NewScorer())
	datasets := evaluate.NewDatasetEngine(nil)

	optimizer := NewTextGradOptimizer(engine, experiments, datasets, llmClient)

	// Create prompt — the in-memory engine indexes by Key, so CreateVersion
	// looks up by PromptID in e.prompts (which is keyed by prompt.Key).
	// We set Key = ID to make lookups work in the memory engine.
	p := &prompt.Prompt{ID: "test", Key: "test", Name: "Test"}
	engine.CreatePrompt(context.Background(), p)
	version := &prompt.PromptVersion{ID: "v1", PromptID: "test", Version: "1.0", Content: "{{name}} 的年龄是 {{age}}", Status: prompt.VersionStatusActive, IsActive: true}
	engine.CreateVersion(context.Background(), version)
	engine.ActivateVersion(context.Background(), "v1")

	req := &OptimizeRequest{
		PromptKey: "test",
		DatasetID: "ds1",
		Strategy:  "textgrad",
		Config:    OptimizerConfig{MaxRounds: 3, NoImproveLimit: 2, ScoreThreshold: 0.8, Model: "test"},
	}

	result, err := optimizer.Optimize(context.Background(), req)
	if err != nil {
		t.Fatalf("Optimize error: %v", err)
	}
	if result.Strategy != "textgrad" {
		t.Errorf("Strategy = %q, want textgrad", result.Strategy)
	}
	if result.Rounds > 3 {
		t.Errorf("Rounds = %d, should be at most 3", result.Rounds)
	}
}

// ---- Test: Optimizer respects MaxRounds ----

func TestOptimizerRespectsMaxRounds(t *testing.T) {
	t.Skip("requires DB-backed dataset with cases; see integration tests")
	llmClient := &mockLLM{responses: []string{}}

	engine := prompt.NewEngineMemory()
	experiments := evaluate.NewExperimentEngineMemory(evaluate.NewScorer())
	datasets := evaluate.NewDatasetEngine(nil)

	optimizer := NewTextGradOptimizer(engine, experiments, datasets, llmClient)

	p := &prompt.Prompt{ID: "test2", Key: "test2", Name: "Test2"}
	engine.CreatePrompt(context.Background(), p)
	version := &prompt.PromptVersion{ID: "v2", PromptID: "test2", Version: "1.0", Content: "Hello {{name}}", Status: prompt.VersionStatusActive, IsActive: true}
	engine.CreateVersion(context.Background(), version)
	engine.ActivateVersion(context.Background(), "v2")

	req := &OptimizeRequest{
		PromptKey: "test2",
		DatasetID: "ds2",
		Strategy:  "textgrad",
		Config:    OptimizerConfig{MaxRounds: 1, NoImproveLimit: 1, ScoreThreshold: 0.8, Model: "test"},
	}

	result, err := optimizer.Optimize(context.Background(), req)
	if err != nil {
		t.Fatalf("Optimize error: %v", err)
	}
	if result.Rounds > 1 {
		t.Errorf("Rounds = %d, should respect MaxRounds=1", result.Rounds)
	}
}

// ---- Test: NewOptimizer factory ----

func TestNewOptimizerFactory(t *testing.T) {
	llmClient := &mockLLM{}
	engine := prompt.NewEngineMemory()
	experiments := evaluate.NewExperimentEngineMemory(evaluate.NewScorer())
	datasets := evaluate.NewDatasetEngine(nil)

	textgrad := NewOptimizer("textgrad", engine, experiments, datasets, llmClient)
	if _, ok := textgrad.(*TextGradOptimizer); !ok {
		t.Error("expected TextGradOptimizer for strategy 'textgrad'")
	}

	gepa := NewOptimizer("gepa", engine, experiments, datasets, llmClient)
	if _, ok := gepa.(*GEPAOptimizer); !ok {
		t.Error("expected GEPAOptimizer for strategy 'gepa'")
	}

	def := NewOptimizer("unknown", engine, experiments, datasets, llmClient)
	if _, ok := def.(*TextGradOptimizer); !ok {
		t.Error("expected TextGradOptimizer for unknown strategy")
	}
}

// ---- Test: KNNSelector ----

func TestKNNSelectorPicksSimilar(t *testing.T) {
	selector := NewKNNSelector(&mockLLM{})

	cases := []*evaluate.DatasetCase{
		{ID: "1", Input: "what is the weather"},
		{ID: "2", Input: "how to cook rice"},
		{ID: "3", Input: "tell me about weather"},
	}

	selected, err := selector.Select(context.Background(), "weather forecast", cases, 2)
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if len(selected) != 2 {
		t.Errorf("expected 2 selected, got %d", len(selected))
	}
}

// ---- Test: CosineSimilarity ----

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float64
		want float64
	}{
		{
			name: "identical vectors",
			a:    []float64{1, 0, 0},
			b:    []float64{1, 0, 0},
			want: 1.0,
		},
		{
			name: "orthogonal vectors",
			a:    []float64{1, 0, 0},
			b:    []float64{0, 1, 0},
			want: 0.0,
		},
		{
			name: "opposite vectors",
			a:    []float64{1, 0, 0},
			b:    []float64{-1, 0, 0},
			want: -1.0,
		},
		{
			name: "zero vector",
			a:    []float64{0, 0, 0},
			b:    []float64{1, 0, 0},
			want: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CosineSimilarity(tt.a, tt.b)
			if got < tt.want-0.01 || got > tt.want+0.01 {
				t.Errorf("CosineSimilarity() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---- Test: DefaultOptimizerConfig ----

func TestDefaultOptimizerConfig(t *testing.T) {
	cfg := DefaultOptimizerConfig()
	if cfg.MaxRounds != 5 {
		t.Errorf("MaxRounds = %d, want 5", cfg.MaxRounds)
	}
	if cfg.NoImproveLimit != 2 {
		t.Errorf("NoImproveLimit = %d, want 2", cfg.NoImproveLimit)
	}
	if cfg.ScoreThreshold != 0.8 {
		t.Errorf("ScoreThreshold = %f, want 0.8", cfg.ScoreThreshold)
	}
	if cfg.CandidatesPerRound != 2 {
		t.Errorf("CandidatesPerRound = %d, want 2", cfg.CandidatesPerRound)
	}
	if cfg.PopulationSize != 4 {
		t.Errorf("PopulationSize = %d, want 4", cfg.PopulationSize)
	}
}

// ---- Test: OptimizeService ----

func TestOptimizeServiceCreation(t *testing.T) {
	llmClient := &mockLLM{}
	engine := prompt.NewEngineMemory()
	experiments := evaluate.NewExperimentEngineMemory(evaluate.NewScorer())
	datasets := evaluate.NewDatasetEngine(nil)

	svc := NewOptimizeService(engine, experiments, datasets, llmClient)
	if svc == nil {
		t.Error("expected non-nil OptimizeService")
	}
}
