package optimizer

import (
	"context"
	"fmt"
	"log"
	"sync"

	"agent-platform/pkg/llm"
	"agent-platform/services/harness-service/internal/evaluate"
	"agent-platform/services/harness-service/internal/prompt"
	"gorm.io/gorm"
)

// EvalHook is called when a prompt version is activated, automatically running
// evaluation against the bound dataset and alerting if the score drops.
// It implements prompt.ActivateHook.
type EvalHook struct {
	experiments *evaluate.ExperimentEngine
	promptEng   *prompt.Engine
	mu          sync.Mutex
	bindings    map[string]string // promptKey → datasetID
}

// NewEvalHook creates a new evaluation hook.
func NewEvalHook(experiments *evaluate.ExperimentEngine, promptEng *prompt.Engine) *EvalHook {
	return &EvalHook{
		experiments: experiments,
		promptEng:   promptEng,
		bindings:    make(map[string]string),
	}
}

// BindDataset binds a dataset to a prompt key.
func (h *EvalHook) BindDataset(promptKey, datasetID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bindings[promptKey] = datasetID
}

// UnbindDataset removes the dataset binding for a prompt key.
func (h *EvalHook) UnbindDataset(promptKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.bindings, promptKey)
}

// GetBinding returns the dataset ID bound to a prompt!key, or empty string.
func (h *EvalHook) GetBinding(promptKey string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bindings[promptKey]
}

// OnActivate is called when a prompt version is activated.
// It implements prompt.ActivateHook.
func (h *EvalHook) OnActivate(ctx context.Context, promptKey, versionID string) {
	h.mu.Lock()
	datasetID, bound := h.bindings[promptKey]
	h.mu.Unlock()

	if !bound || datasetID == "" {
		return
	}

	go func() {
		if err := h.runEval(context.Background(), promptKey, versionID, datasetID); err != nil {
			log.Printf("[harness:eval-hook] eval failed for %s version %s: %v", promptKey, versionID, err)
		}
	}()
}

func (h *EvalHook) runEval(ctx context.Context, promptKey, versionID, datasetID string) error {
	version, err := h.promptEng.GetVersion(ctx, versionID)
	if err != nil {
		return fmt.Errorf("get version: %w", err)
	}

	prevScore := h.getPreviousScore(ctx, promptKey, datasetID)

	exp := &evaluate.Experiment{
		Name:       fmt.Sprintf("auto-eval-%s-%s", promptKey, version.Version),
		DatasetID:  datasetID,
		ConfigType: "prompt_version",
		ConfigRef:  versionID,
	}

	if _, err := h.experiments.RunExperiment(ctx, exp, version.Content, nil); err != nil {
		return fmt.Errorf("run experiment: %w", err)
	}

	if prevScore > 0 && exp.AvgScore < prevScore {
		log.Printf("[harness:eval-hook] ⚠️ REGRESSION: %s version %s scored %.2f (previous: %.2f, delta: %.2f)",
			promptKey, version.Version, exp.AvgScore, prevScore, exp.AvgScore-prevScore)
	} else {
		log.Printf("[harness:eval-hook] ✓ %s version %s scored %.2f (previous: %.2f)",
			promptKey, version.Version, exp.AvgScore, prevScore)
	}

	return nil
}

func (h *EvalHook) getPreviousScore(ctx context.Context, promptKey, datasetID string) float64 {
	experiments, err := h.experiments.ListExperiments(ctx, datasetID)
	if err != nil || len(experiments) == 0 {
		return 0
	}

	for _, exp := range experiments {
		if exp.Status == "completed" && exp.ConfigType == "prompt_version" {
			v, err := h.promptEng.GetVersion(ctx, exp.ConfigRef)
			if err != nil {
				continue
			}
			p, err := h.promptEng.GetPrompt(ctx, promptKey)
			if err != nil {
				continue
			}
			if v.PromptID == p.ID {
				return exp.AvgScore
			}
		}
	}
	return 0
}

// NewEvalHookWithDB creates an EvalHook with database-backed engines.
func NewEvalHookWithDB(db *gorm.DB, promptEng *prompt.Engine, llmClient llm.Client, model string) *EvalHook {
	datasets := evaluate.NewDatasetEngine(db)
	scorer := evaluate.NewScorerWithLLM(llmClient, model)
	experiments := evaluate.NewExperimentEngine(db, datasets, scorer, llmClient)
	return NewEvalHook(experiments, promptEng)
}
