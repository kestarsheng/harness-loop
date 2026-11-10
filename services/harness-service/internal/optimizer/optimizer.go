// Package optimizer provides prompt optimization strategies and evaluation hooks.
// It depends on both the prompt and evaluate packages, breaking the import cycle
// that would exist if optimizer lived inside either of them.
package optimizer

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"agent-platform/pkg/llm"
	"agent-platform/services/harness-service/internal/evaluate"
	"agent-platform/services/harness-service/internal/prompt"
)

// ---- Types ----

// Optimizer is the interface for prompt optimization strategies.
type Optimizer interface {
	Optimize(ctx context.Context, req *OptimizeRequest) (*OptimizeResult, error)
}

// OptimizeRequest is the input to an optimizer.
type OptimizeRequest struct {
	PromptKey string // Prompt to optimize
	DatasetID string // EvalDataset to score against
	Metric    string // "overall" / "faithfulness" / "relevancy"
	Strategy  string // "textgrad" / "gepa"
	Config    OptimizerConfig
	Variables map[string]interface{} // Variables for rendering
}

// OptimizerConfig controls the optimization run.
type OptimizerConfig struct {
	MaxRounds          int     // Default 5
	NoImproveLimit     int     // Stop after N rounds without improvement, default 2
	ScoreThreshold     float64 // Only optimize if score below this, default 0.8
	Model              string  // LLM model for optimization
	Temperature        float64 // Default 0.7
	CandidatesPerRound int     // How many candidates per round, default 2
	PopulationSize     int     // For GEPA: population size, default 4
}

// DefaultOptimizerConfig returns sensible defaults.
func DefaultOptimizerConfig() OptimizerConfig {
	return OptimizerConfig{
		MaxRounds:          5,
		NoImproveLimit:     2,
		ScoreThreshold:     0.8,
		Temperature:        0.7,
		CandidatesPerRound: 2,
		PopulationSize:     4,
	}
}

// OptimizeResult is the output of an optimization run.
type OptimizeResult struct {
	BestVersionID string
	BestScore     float64
	OriginalScore float64
	Rounds        int
	History       []OptimizeRound
	Strategy      string
}

// OptimizeRound records one round of optimization.
type OptimizeRound struct {
	Round      int
	PromptText string
	Score      float64
	Improved   bool
	Suggestion string // LLM-generated improvement suggestion
	VersionID  string // Created version ID
}

// ---- TextGrad Optimizer ----

// TextGradOptimizer implements the TextGrad strategy:
// 1. Run current prompt against dataset → find failed cases
// 2. Ask LLM to analyze failures → get improvement suggestion ("text gradient")
// 3. Ask LLM to revise prompt based on suggestion → new candidate
// 4. Score new candidate → adopt if better
// 5. Stop after NoImproveLimit consecutive rounds without improvement
type TextGradOptimizer struct {
	engine      *prompt.Engine
	experiments *evaluate.ExperimentEngine
	datasets    *evaluate.DatasetEngine
	llmClient   llm.Client
}

// NewTextGradOptimizer creates a new TextGrad optimizer.
func NewTextGradOptimizer(engine *prompt.Engine, experiments *evaluate.ExperimentEngine, datasets *evaluate.DatasetEngine, llmClient llm.Client) *TextGradOptimizer {
	return &TextGradOptimizer{
		engine:      engine,
		experiments: experiments,
		datasets:    datasets,
		llmClient:   llmClient,
	}
}

// Optimize runs the TextGrad optimization loop.
func (o *TextGradOptimizer) Optimize(ctx context.Context, req *OptimizeRequest) (*OptimizeResult, error) {
	config := req.Config
	if config.MaxRounds == 0 {
		config = DefaultOptimizerConfig()
	}
	if config.NoImproveLimit == 0 {
		config.NoImproveLimit = 2
	}
	if config.ScoreThreshold == 0 {
		config.ScoreThreshold = 0.8
	}
	if config.Temperature == 0 {
		config.Temperature = 0.7
	}

	// Get current prompt version
	version, err := o.engine.GetActiveVersion(ctx, req.PromptKey)
	if err != nil {
		return nil, fmt.Errorf("get active version: %w", err)
	}

	currentPrompt := version.Content
	currentScore, err := o.scorePrompt(ctx, currentPrompt, req)
	if err != nil {
		return nil, fmt.Errorf("score current prompt: %w", err)
	}

	result := &OptimizeResult{
		OriginalScore: currentScore,
		BestScore:     currentScore,
		BestVersionID: version.ID,
		Strategy:      "textgrad",
	}

	// Skip if already above threshold
	if currentScore >= config.ScoreThreshold {
		log.Printf("[optimizer:textgrad] %s already at %.2f (threshold %.2f), skipping", req.PromptKey, currentScore, config.ScoreThreshold)
		return result, nil
	}

	noImproveCount := 0

	for round := 1; round <= config.MaxRounds; round++ {
		// Step 1: Find failed cases
		failedCases, err := o.findFailedCases(ctx, currentPrompt, req)
		if err != nil {
			return result, fmt.Errorf("round %d: find failures: %w", round, err)
		}

		if len(failedCases) == 0 {
			log.Printf("[optimizer:textgrad] round %d: no failures, prompt is optimal", round)
			break
		}

		// Step 2: Analyze failures → get improvement suggestion (text gradient)
		suggestion, err := o.analyzePrompt(ctx, currentPrompt, failedCases, config)
		if err != nil {
			log.Printf("[optimizer:textgrad] round %d: analyze failed: %v", round, err)
			noImproveCount++
			if noImproveCount >= config.NoImproveLimit {
				break
			}
			continue
		}

		// Step 3: Revise prompt based on suggestion
		newPrompt, err := o.revisePrompt(ctx, currentPrompt, suggestion, config)
		if err != nil {
			log.Printf("[optimizer:textgrad] round %d: revise failed: %v", round, err)
			noImproveCount++
			if noImproveCount >= config.NoImproveLimit {
				break
			}
			continue
		}

		// Step 4: Score the revised prompt
		newScore, err := o.scorePrompt(ctx, newPrompt, req)
		if err != nil {
			log.Printf("[optimizer:textgrad] round %d: score failed: %v", round, err)
			noImproveCount++
			if noImproveCount >= config.NoImproveLimit {
				break
			}
			continue
		}

		improved := newScore > currentScore
		roundRecord := OptimizeRound{
			Round:      round,
			PromptText: newPrompt,
			Score:      newScore,
			Improved:   improved,
			Suggestion: suggestion,
		}

		if improved {
			// Create a new draft version with the improved prompt
			p, _ := o.engine.GetPrompt(ctx, req.PromptKey)
			if p != nil {
				newVersion := &prompt.PromptVersion{
					PromptID: p.ID,
					Version:  fmt.Sprintf("opt-round-%d", round),
					Content:  newPrompt,
					Status:   prompt.VersionStatusDraft,
					Metadata: fmt.Sprintf(`{"optimizer":"textgrad","round":%d,"score":%.4f,"improvement":%.4f}`, round, newScore, newScore-currentScore),
				}
				if err := o.engine.CreateVersion(ctx, newVersion); err != nil {
					log.Printf("[optimizer:textgrad] round %d: create version failed: %v", round, err)
				} else {
					roundRecord.VersionID = newVersion.ID
				}
			}

			currentPrompt = newPrompt
			currentScore = newScore
			result.BestScore = newScore
			if roundRecord.VersionID != "" {
				result.BestVersionID = roundRecord.VersionID
			}
			noImproveCount = 0
			log.Printf("[optimizer:textgrad] round %d: improved to %.4f", round, newScore)
		} else {
			noImproveCount++
			log.Printf("[optimizer:textgrad] round %d: no improvement (%.4f)", round, newScore)
		}

		result.History = append(result.History, roundRecord)
		result.Rounds = round

		if noImproveCount >= config.NoImproveLimit {
			log.Printf("[optimizer:textgrad] stopping: %d rounds without improvement", noImproveCount)
			break
		}
	}

	return result, nil
}

// scorePrompt runs the current prompt against the dataset and returns the average score.
func (o *TextGradOptimizer) scorePrompt(ctx context.Context, promptContent string, req *OptimizeRequest) (float64, error) {
	exp := &evaluate.Experiment{
		Name:       fmt.Sprintf("opt-score-%d", time.Now().UnixNano()),
		DatasetID:  req.DatasetID,
		ConfigType: "prompt_version",
		ConfigRef:  "optimizer-draft",
		Model:      req.Config.Model,
	}

	completed, err := o.experiments.RunExperiment(ctx, exp, promptContent, req.Variables)
	if err != nil {
		return 0, err
	}
	return completed.AvgScore, nil
}

// findFailedCases runs the prompt and returns cases that scored below 0.6.
func (o *TextGradOptimizer) findFailedCases(ctx context.Context, promptContent string, req *OptimizeRequest) ([]*evaluate.DatasetCase, error) {
	cases, err := o.datasets.ListCases(ctx, req.DatasetID)
	if err != nil {
		return nil, err
	}

	var failed []*evaluate.DatasetCase
	scorer := evaluate.NewScorer() // Keyword fallback for speed

	for _, c := range cases {
		score := scorer.Score(ctx, c.Input, promptContent, c.Expected)
		if score.Overall < 0.6 {
			failed = append(failed, c)
		}
	}

	return failed, nil
}

// analyzePrompt asks the LLM to analyze failed cases and suggest improvements.
func (o *TextGradOptimizer) analyzePrompt(ctx context.Context, currentPrompt string, failedCases []*evaluate.DatasetCase, config OptimizerConfig) (string, error) {
	var casesText strings.Builder
	for i, c := range failedCases {
		if i >= 5 { // Limit to 5 examples
			break
		}
		casesText.WriteString(fmt.Sprintf("\n--- 案例 %d ---\n输入: %s\n期望: %s\n", i+1, c.Input, c.Expected))
	}

	analysisPrompt := fmt.Sprintf(`你是一个 prompt 工程专家。批判性分析以下 prompt 和失败案例，指出 prompt 的具体不足。

## 当前 Prompt

%s

## 失败案例（得分低于 0.6）

%s

## 要求

1. 指出 prompt 在哪些方面导致失败（如：缺少格式要求、缺少约束、指令模糊等）
2. 给出具体的改进建议（2-3 条）
3. 只输出建议，不要重复 prompt 内容`, currentPrompt, casesText.String())

	resp, err := o.llmClient.Chat(ctx, &llm.ChatRequest{
		Model:    config.Model,
		Messages: []llm.Message{{Role: "user", Content: analysisPrompt}},
	})
	if err != nil {
		return "", fmt.Errorf("LLM analyze: %w", err)
	}

	return resp.Content, nil
}

// revisePrompt asks the LLM to revise the prompt based on the suggestion.
// Crucially: it must preserve all {{var}} and {{var|default}} placeholders.
func (o *TextGradOptimizer) revisePrompt(ctx context.Context, currentPrompt, suggestion string, config OptimizerConfig) (string, error) {
	revisePromptText := fmt.Sprintf(`根据以下改进建议，改写 prompt。

## 当前 Prompt

%s

## 改进建议

%s

## 规则（必须遵守）

1. 保留所有 {{var}} 和 {{var|default}} 占位符，一个都不能删
2. 只改指令、约束、格式要求部分
3. 不要改变 prompt 的核心用途
4. 输出改写后的完整 prompt，不要输出其他内容`, currentPrompt, suggestion)

	resp, err := o.llmClient.Chat(ctx, &llm.ChatRequest{
		Model:    config.Model,
		Messages: []llm.Message{{Role: "user", Content: revisePromptText}},
	})
	if err != nil {
		return "", fmt.Errorf("LLM revise: %w", err)
	}

	revised := resp.Content

	// Validate that all {{var}} placeholders are preserved
	if !VariablesPreserved(currentPrompt, revised) {
		log.Printf("[optimizer:textgrad] WARNING: revised prompt lost variables, keeping original")
		return currentPrompt, nil
	}

	return revised, nil
}

// ---- GEPA Optimizer ----

// GEPAOptimizer implements the GEPA (Reflective Evolution) strategy.
type GEPAOptimizer struct {
	engine      *prompt.Engine
	experiments *evaluate.ExperimentEngine
	datasets    *evaluate.DatasetEngine
	llmClient   llm.Client
}

// NewGEPAOptimizer creates a new GEPA optimizer.
func NewGEPAOptimizer(engine *prompt.Engine, experiments *evaluate.ExperimentEngine, datasets *evaluate.DatasetEngine, llmClient llm.Client) *GEPAOptimizer {
	return &GEPAOptimizer{
		engine:      engine,
		experiments: experiments,
		datasets:    datasets,
		llmClient:   llmClient,
	}
}

// candidate represents a prompt candidate in the population.
type candidate struct {
	prompt string
	score  float64
}

// Optimize runs the GEPA optimization loop.
func (o *GEPAOptimizer) Optimize(ctx context.Context, req *OptimizeRequest) (*OptimizeResult, error) {
	config := req.Config
	if config.MaxRounds == 0 {
		config = DefaultOptimizerConfig()
	}
	if config.NoImproveLimit == 0 {
		config.NoImproveLimit = 2
	}
	if config.PopulationSize == 0 {
		config.PopulationSize = 4
	}

	version, err := o.engine.GetActiveVersion(ctx, req.PromptKey)
	if err != nil {
		return nil, fmt.Errorf("get active version: %w", err)
	}

	population := []candidate{{prompt: version.Content}}
	originalScore, err := o.scorePrompt(ctx, version.Content, req)
	if err != nil {
		return nil, fmt.Errorf("score original: %w", err)
	}
	population[0].score = originalScore

	// Generate initial variations
	for i := 1; i < config.PopulationSize; i++ {
		variation, err := o.generateVariation(ctx, version.Content, config)
		if err != nil {
			continue
		}
		score, err := o.scorePrompt(ctx, variation, req)
		if err != nil {
			continue
		}
		population = append(population, candidate{prompt: variation, score: score})
	}

	result := &OptimizeResult{
		OriginalScore: originalScore,
		BestScore:     originalScore,
		BestVersionID: version.ID,
		Strategy:      "gepa",
	}

	noImproveCount := 0

	for round := 1; round <= config.MaxRounds; round++ {
		topK := selectTopK(population, 2)

		reflection, err := o.reflect(ctx, topK, config)
		if err != nil {
			noImproveCount++
			if noImproveCount >= config.NoImproveLimit {
				break
			}
			continue
		}

		for i := 0; i < config.CandidatesPerRound; i++ {
			newPrompt, err := o.mutate(ctx, topK[0].prompt, reflection, config)
			if err != nil {
				continue
			}
			score, err := o.scorePrompt(ctx, newPrompt, req)
			if err != nil {
				continue
			}
			population = append(population, candidate{prompt: newPrompt, score: score})
		}

		if len(population) > config.PopulationSize*2 {
			population = selectTopK(population, config.PopulationSize)
		}

		best := selectTopK(population, 1)[0]
		improved := best.score > result.BestScore

		roundRecord := OptimizeRound{
			Round:      round,
			PromptText: best.prompt,
			Score:      best.score,
			Improved:   improved,
			Suggestion: reflection,
		}

		if improved {
			p, _ := o.engine.GetPrompt(ctx, req.PromptKey)
			if p != nil {
				newVersion := &prompt.PromptVersion{
					PromptID: p.ID,
					Version:  fmt.Sprintf("gepa-round-%d", round),
					Content:  best.prompt,
					Status:   prompt.VersionStatusDraft,
					Metadata: fmt.Sprintf(`{"optimizer":"gepa","round":%d,"score":%.4f}`, round, best.score),
				}
				if err := o.engine.CreateVersion(ctx, newVersion); err == nil {
					roundRecord.VersionID = newVersion.ID
					result.BestVersionID = newVersion.ID
				}
			}
			result.BestScore = best.score
			noImproveCount = 0
		} else {
			noImproveCount++
		}

		result.History = append(result.History, roundRecord)
		result.Rounds = round

		if noImproveCount >= config.NoImproveLimit {
			break
		}
	}

	return result, nil
}

func (o *GEPAOptimizer) scorePrompt(ctx context.Context, promptContent string, req *OptimizeRequest) (float64, error) {
	exp := &evaluate.Experiment{
		Name:       fmt.Sprintf("gepa-score-%d", time.Now().UnixNano()),
		DatasetID:  req.DatasetID,
		ConfigType: "prompt_version",
		ConfigRef:  "gepa-draft",
		Model:      req.Config.Model,
	}
	completed, err := o.experiments.RunExperiment(ctx, exp, promptContent, req.Variables)
	if err != nil {
		return 0, err
	}
	return completed.AvgScore, nil
}

func selectTopK(pop []candidate, k int) []candidate {
	sorted := make([]candidate, len(pop))
	copy(sorted, pop)
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].score > sorted[i].score {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	if k > len(sorted) {
		k = len(sorted)
	}
	return sorted[:k]
}

func (o *GEPAOptimizer) generateVariation(ctx context.Context, original string, config OptimizerConfig) (string, error) {
	genPrompt := fmt.Sprintf(`请改写以下 prompt，使其在保持核心用途不变的前提下更有效。

## 原始 Prompt

%s

## 规则
1. 保留所有 {{var}} 和 {{var|default}} 占位符
2. 只输出改写后的完整 prompt`, original)

	resp, err := o.llmClient.Chat(ctx, &llm.ChatRequest{
		Model:    config.Model,
		Messages: []llm.Message{{Role: "user", Content: genPrompt}},
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func (o *GEPAOptimizer) reflect(ctx context.Context, topK []candidate, config OptimizerConfig) (string, error) {
	var topTexts strings.Builder
	for i, c := range topK {
		topTexts.WriteString(fmt.Sprintf("\n--- 候选 %d (得分 %.2f) ---\n%s", i+1, c.score, c.prompt))
	}

	reflectPrompt := fmt.Sprintf(`分析以下高分 prompt 候选，总结它们为什么好，以及还可以怎么改进。

%s

请给出 2-3 条具体的改进方向：`, topTexts.String())

	resp, err := o.llmClient.Chat(ctx, &llm.ChatRequest{
		Model:    config.Model,
		Messages: []llm.Message{{Role: "user", Content: reflectPrompt}},
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func (o *GEPAOptimizer) mutate(ctx context.Context, parent, reflection string, config OptimizerConfig) (string, error) {
	mutatePrompt := fmt.Sprintf(`根据反思建议，改写以下 prompt。

## 当前 Prompt

%s

## 改进方向

%s

## 规则
1. 保留所有 {{var}} 和 {{var|default}} 占位符
2. 只输出改写后的完整 prompt`, parent, reflection)

	resp, err := o.llmClient.Chat(ctx, &llm.ChatRequest{
		Model:    config.Model,
		Messages: []llm.Message{{Role: "user", Content: mutatePrompt}},
	})
	if err != nil {
		return "", err
	}

	revised := resp.Content
	if !VariablesPreserved(parent, revised) {
		return parent, nil
	}
	return revised, nil
}

// ---- Few-shot Selector ----

// FewShotSelector picks the best few-shot examples for a prompt.
type FewShotSelector interface {
	Select(ctx context.Context, query string, candidates []*evaluate.DatasetCase, k int) ([]*evaluate.DatasetCase, error)
}

// KNNSelector picks examples by embedding similarity.
type KNNSelector struct {
	embedder llm.Client
}

// NewKNNSelector creates a KNN-based few-shot selector.
func NewKNNSelector(embedder llm.Client) *KNNSelector {
	return &KNNSelector{embedder: embedder}
}

// Select picks the k most similar cases to the query by embedding cosine similarity.
func (s *KNNSelector) Select(ctx context.Context, query string, candidates []*evaluate.DatasetCase, k int) ([]*evaluate.DatasetCase, error) {
	if len(candidates) <= k {
		return candidates, nil
	}

	queryEmb, err := s.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	type scoredCase struct {
		c     *evaluate.DatasetCase
		score float64
	}

	var scored []scoredCase
	for _, c := range candidates {
		emb, err := s.embedder.Embed(ctx, c.Input)
		if err != nil {
			continue
		}
		sim := CosineSimilarity(queryEmb, emb)
		scored = append(scored, scoredCase{c: c, score: sim})
	}

	for i := 0; i < len(scored)-1; i++ {
		for j := i + 1; j < len(scored); j++ {
			if scored[j].score > scored[i].score {
				scored[i], scored[j] = scored[j], scored[i]
			}
		}
	}

	var result []*evaluate.DatasetCase
	for i := 0; i < k && i < len(scored); i++ {
		result = append(result, scored[i].c)
	}
	return result, nil
}

// ---- Utility functions ----

// VariablesPreserved checks that all {{var}} placeholders in the original
// are still present in the revised version.
func VariablesPreserved(original, revised string) bool {
	re := regexp.MustCompile(`\{\{\s*(\w+)(?:\s*\|\s*.*?\s*)?\s*\}\}`)
	originalVars := re.FindAllString(original, -1)

	for _, v := range originalVars {
		varName := regexp.MustCompile(`\{\{\s*(\w+)`).FindStringSubmatch(v)
		if len(varName) < 2 {
			continue
		}
		if !strings.Contains(revised, varName[1]) {
			return false
		}
	}
	return true
}

// CosineSimilarity computes the cosine similarity between two vectors.
func CosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (sqrt(normA) * sqrt(normB))
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 20; i++ {
		z = (z + x/z) / 2
	}
	return z
}

// ---- Factory ----

// NewOptimizer creates an optimizer by strategy name.
func NewOptimizer(strategy string, engine *prompt.Engine, experiments *evaluate.ExperimentEngine, datasets *evaluate.DatasetEngine, llmClient llm.Client) Optimizer {
	switch strategy {
	case "gepa":
		return NewGEPAOptimizer(engine, experiments, datasets, llmClient)
	default:
		return NewTextGradOptimizer(engine, experiments, datasets, llmClient)
	}
}

// ---- OptimizeService ----

// OptimizeService orchestrates optimization runs.
type OptimizeService struct {
	engine      *prompt.Engine
	experiments *evaluate.ExperimentEngine
	datasets    *evaluate.DatasetEngine
	llmClient   llm.Client
}

// NewOptimizeService creates a new optimization service.
func NewOptimizeService(engine *prompt.Engine, experiments *evaluate.ExperimentEngine, datasets *evaluate.DatasetEngine, llmClient llm.Client) *OptimizeService {
	return &OptimizeService{
		engine:      engine,
		experiments: experiments,
		datasets:    datasets,
		llmClient:   llmClient,
	}
}

// RunOptimization runs a prompt optimization and returns the result.
func (s *OptimizeService) RunOptimization(ctx context.Context, req *OptimizeRequest) (*OptimizeResult, error) {
	optimizer := NewOptimizer(req.Strategy, s.engine, s.experiments, s.datasets, s.llmClient)
	return optimizer.Optimize(ctx, req)
}

// AdoptVersion activates the best version found by optimization.
func (s *OptimizeService) AdoptVersion(ctx context.Context, versionID string) error {
	return s.engine.ActivateVersion(ctx, versionID)
}
