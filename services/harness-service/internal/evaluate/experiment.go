package evaluate

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"agent-platform/pkg/llm"
)

// Experiment is a single run of "configuration × dataset".
// It records which prompt version / model was tested against which dataset,
// and stores the aggregate score.
type Experiment struct {
	ID          string `gorm:"primaryKey"`
	Name        string
	DatasetID   string `gorm:"index"`
	ConfigType  string // "prompt_version" / "model" / "manual"
	ConfigRef   string // prompt version ID / model name / config JSON
	Model       string
	Status      string // "running" / "completed" / "failed"
	AvgScore    float64
	CaseCount   int
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// ExperimentResult is the result of running a single DatasetCase through the LLM.
type ExperimentResult struct {
	ID           string  `gorm:"primaryKey"`
	ExperimentID string  `gorm:"index"`
	CaseID       string  `gorm:"index"`
	Output       string  // LLM actual output
	Score        float64 // 0-1 overall score
	Passed       bool
	LatencyMs    int64
	Tokens       int64
	Scores       string // JSON: multi-dimension scores (faithfulness/relevancy/...)
	Error        string
}

// ExperimentEngine manages experiments: create, run, compare.
type ExperimentEngine struct {
	db        *gorm.DB
	datasets  *DatasetEngine
	scorer    *Scorer
	llmClient llm.Client
	mu        sync.RWMutex
}

// NewExperimentEngine creates a new experiment engine.
func NewExperimentEngine(db *gorm.DB, datasets *DatasetEngine, scorer *Scorer, llmClient llm.Client) *ExperimentEngine {
	return &ExperimentEngine{
		db:        db,
		datasets:  datasets,
		scorer:    scorer,
		llmClient: llmClient,
	}
}

// NewExperimentEngineMemory creates an in-memory experiment engine (for testing).
func NewExperimentEngineMemory(scorer *Scorer) *ExperimentEngine {
	return &ExperimentEngine{
		datasets: NewDatasetEngine(nil),
		scorer:   scorer,
	}
}

// AutoMigrate creates database tables for experiment models.
func (e *ExperimentEngine) AutoMigrate() error {
	if e.db == nil {
		return nil
	}
	return e.db.AutoMigrate(&Experiment{}, &ExperimentResult{})
}

// CreateExperiment creates a new experiment record.
func (e *ExperimentEngine) CreateExperiment(ctx context.Context, exp *Experiment) error {
	if exp.ID == "" {
		exp.ID = uuid.New().String()
	}
	exp.CreatedAt = time.Now()
	exp.Status = "running"

	if e.db != nil {
		if err := e.db.Create(exp).Error; err != nil {
			return fmt.Errorf("create experiment: %w", err)
		}
	}
	return nil
}

// GetExperiment retrieves an experiment by ID.
func (e *ExperimentEngine) GetExperiment(ctx context.Context, id string) (*Experiment, error) {
	if e.db != nil {
		var exp Experiment
		if err := e.db.First(&exp, "id = ?", id).Error; err != nil {
			return nil, fmt.Errorf("experiment not found: %w", err)
		}
		return &exp, nil
	}
	return nil, fmt.Errorf("experiment not found: %s", id)
}

// ListExperiments lists experiments for a dataset.
func (e *ExperimentEngine) ListExperiments(ctx context.Context, datasetID string) ([]*Experiment, error) {
	if e.db != nil {
		var exps []*Experiment
		query := e.db.Model(&Experiment{})
		if datasetID != "" {
			query = query.Where("dataset_id = ?", datasetID)
		}
		if err := query.Order("created_at DESC").Find(&exps).Error; err != nil {
			return nil, fmt.Errorf("list experiments: %w", err)
		}
		return exps, nil
	}
	return nil, nil
}

// RunExperiment runs all cases in a dataset through the LLM and scores them.
// This is the core evaluation loop: for each case → render prompt → call LLM → score.
func (e *ExperimentEngine) RunExperiment(ctx context.Context, exp *Experiment, promptContent string, variables map[string]interface{}) (*Experiment, error) {
	// Load cases
	cases, err := e.datasets.ListCases(ctx, exp.DatasetID)
	if err != nil {
		return nil, fmt.Errorf("load cases: %w", err)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("dataset %s has no cases", exp.DatasetID)
	}

	// Create experiment record if not persisted
	if exp.ID == "" {
		if err := e.CreateExperiment(ctx, exp); err != nil {
			return nil, err
		}
	}

	var totalScore float64
	var results []*ExperimentResult

	for _, c := range cases {
		result := e.runCase(ctx, exp, c, promptContent, variables)
		results = append(results, result)

		if result.Error == "" {
			totalScore += result.Score
		}

		// Persist result
		if e.db != nil {
			if err := e.db.Create(result).Error; err != nil {
				log.Printf("[harness:experiment] failed to save result for case %s: %v", c.ID, err)
			}
		}
	}

	// Calculate average score
	caseCount := len(cases)
	avgScore := 0.0
	if caseCount > 0 {
		avgScore = totalScore / float64(caseCount)
	}

	// Update experiment
	now := time.Now()
	exp.AvgScore = avgScore
	exp.CaseCount = caseCount
	exp.Status = "completed"
	exp.CompletedAt = &now

	if e.db != nil {
		e.db.Model(exp).Updates(map[string]interface{}{
			"avg_score":    avgScore,
			"case_count":   caseCount,
			"status":       "completed",
			"completed_at": &now,
		})
	}

	return exp, nil
}

// runCase runs a single eval case through the LLM and scores the output.
func (e *ExperimentEngine) runCase(ctx context.Context, exp *Experiment, c *DatasetCase, promptContent string, variables map[string]interface{}) *ExperimentResult {
	start := time.Now()

	result := &ExperimentResult{
		ID:           uuid.New().String(),
		ExperimentID: exp.ID,
		CaseID:       c.ID,
	}

	// Build the user message from the case input
	userMsg := c.Input
	if c.Context != "" {
		userMsg = fmt.Sprintf("Context:\n%s\n\nQuestion:\n%s", c.Context, c.Input)
	}

	// Call LLM
	messages := []llm.Message{
		{Role: "system", Content: promptContent},
		{Role: "user", Content: userMsg},
	}

	resp, err := e.llmClient.Chat(ctx, &llm.ChatRequest{
		Model:    exp.Model,
		Messages: messages,
	})
	if err != nil {
		result.Error = err.Error()
		result.Score = 0
		result.Passed = false
		result.LatencyMs = time.Since(start).Milliseconds()
		return result
	}

	result.Output = resp.Content
	result.Tokens = int64(resp.TotalTokens)
	result.LatencyMs = time.Since(start).Milliseconds()

	// Score the output
	scoreResult := e.scorer.Score(ctx, c.Input, resp.Content, c.Expected)
	result.Score = scoreResult.Overall
	result.Passed = scoreResult.Overall >= 0.6

	// Store multi-dimension scores as JSON
	scoresJSON, _ := json.Marshal(scoreResult)
	result.Scores = string(scoresJSON)

	return result
}

// GetResults retrieves all results for an experiment.
func (e *ExperimentEngine) GetResults(ctx context.Context, experimentID string) ([]*ExperimentResult, error) {
	if e.db != nil {
		var results []*ExperimentResult
		if err := e.db.Where("experiment_id = ?", experimentID).Order("case_id ASC").Find(&results).Error; err != nil {
			return nil, fmt.Errorf("get results: %w", err)
		}
		return results, nil
	}
	return nil, nil
}

// CompareExperiments compares multiple experiments on the same dataset.
// Returns a comparison with each experiment's average score and per-case breakdown.
func (e *ExperimentEngine) CompareExperiments(ctx context.Context, experimentIDs []string) (*ExperimentComparison, error) {
	if len(experimentIDs) < 2 {
		return nil, fmt.Errorf("need at least 2 experiments to compare")
	}

	comparison := &ExperimentComparison{
		Experiments: make([]*ExperimentSummary, 0, len(experimentIDs)),
	}

	var bestScore float64
	var bestID string

	for _, id := range experimentIDs {
		exp, err := e.GetExperiment(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("get experiment %s: %w", id, err)
		}

		results, err := e.GetResults(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("get results for %s: %w", id, err)
		}

		summary := &ExperimentSummary{
			ID:          exp.ID,
			Name:        exp.Name,
			ConfigType:  exp.ConfigType,
			ConfigRef:   exp.ConfigRef,
			AvgScore:    exp.AvgScore,
			CaseCount:   exp.CaseCount,
			Status:      exp.Status,
			PassedCount: 0,
			FailedCount: 0,
		}

		for _, r := range results {
			if r.Passed {
				summary.PassedCount++
			} else {
				summary.FailedCount++
			}
		}

		comparison.Experiments = append(comparison.Experiments, summary)

		if exp.AvgScore > bestScore {
			bestScore = exp.AvgScore
			bestID = exp.ID
		}
	}

	comparison.BestExperimentID = bestID
	return comparison, nil
}

// DeleteExperiment deletes an experiment and its results.
func (e *ExperimentEngine) DeleteExperiment(ctx context.Context, id string) error {
	if e.db != nil {
		if err := e.db.Where("experiment_id = ?", id).Delete(&ExperimentResult{}).Error; err != nil {
			return fmt.Errorf("delete results: %w", err)
		}
		if err := e.db.Delete(&Experiment{}, "id = ?", id).Error; err != nil {
			return fmt.Errorf("delete experiment: %w", err)
		}
	}
	return nil
}

// ExperimentComparison holds the result of comparing multiple experiments.
type ExperimentComparison struct {
	Experiments      []*ExperimentSummary
	BestExperimentID string
}

// ExperimentSummary is a summary of a single experiment for comparison.
type ExperimentSummary struct {
	ID          string
	Name        string
	ConfigType  string
	ConfigRef   string
	AvgScore    float64
	CaseCount   int
	PassedCount int
	FailedCount int
	Status      string
}
