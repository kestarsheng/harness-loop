// Package service provides business logic for Harness service
// This file contains gRPC handler methods for evaluation dataset, experiment, and prompt optimizer.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	commonpb "agent-platform/pkg/pb/common"
	pb "agent-platform/pkg/pb/harness"
	"agent-platform/services/harness-service/internal/evaluate"
	"agent-platform/services/harness-service/internal/optimizer"
)

// ==================== Evaluation Dataset Methods ====================

// CreateDataset creates a new evaluation dataset.
func (s *HarnessService) CreateDataset(ctx context.Context, req *pb.CreateDatasetRequest) (*pb.EvalDatasetEntry, error) {
	ds := &evaluate.EvalDataset{
		Name:        req.Name,
		Description: req.Description,
		TenantID:    req.TenantId,
	}
	if err := s.datasetEngine.CreateDataset(ctx, ds); err != nil {
		return nil, fmt.Errorf("create dataset: %w", err)
	}
	return s.datasetToPB(ds), nil
}

// GetDataset retrieves a dataset by ID.
func (s *HarnessService) GetDataset(ctx context.Context, req *pb.GetDatasetRequest) (*pb.EvalDatasetEntry, error) {
	ds, err := s.datasetEngine.GetDataset(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("get dataset: %w", err)
	}
	return s.datasetToPB(ds), nil
}

// ListDatasets lists datasets for a tenant.
func (s *HarnessService) ListDatasets(ctx context.Context, req *pb.ListDatasetsRequest) (*pb.ListDatasetsResponse, error) {
	datasets, err := s.datasetEngine.ListDatasets(ctx, req.TenantId)
	if err != nil {
		return nil, fmt.Errorf("list datasets: %w", err)
	}
	var pbDatasets []*pb.EvalDatasetEntry
	for _, ds := range datasets {
		pbDatasets = append(pbDatasets, s.datasetToPB(ds))
	}
	return &pb.ListDatasetsResponse{Datasets: pbDatasets}, nil
}

// DeleteDataset deletes a dataset and all its cases.
func (s *HarnessService) DeleteDataset(ctx context.Context, req *pb.GetDatasetRequest) (*commonpb.Empty, error) {
	if err := s.datasetEngine.DeleteDataset(ctx, req.Id); err != nil {
		return nil, fmt.Errorf("delete dataset: %w", err)
	}
	return &commonpb.Empty{}, nil
}

// AddDatasetCase adds a single test case to a dataset.
func (s *HarnessService) AddDatasetCase(ctx context.Context, req *pb.AddDatasetCaseRequest) (*pb.DatasetCaseEntry, error) {
	c := &evaluate.DatasetCase{
		DatasetID: req.DatasetId,
		Input:     req.Input,
		Expected:  req.Expected,
		Context:   req.Context,
		Tags:      req.Tags,
	}
	if err := s.datasetEngine.AddCase(ctx, c); err != nil {
		return nil, fmt.Errorf("add case: %w", err)
	}
	return s.datasetCaseToPB(c), nil
}

// AddDatasetCases adds multiple test cases to a dataset in batch.
func (s *HarnessService) AddDatasetCases(ctx context.Context, req *pb.AddDatasetCasesRequest) (*commonpb.Empty, error) {
	cases := make([]*evaluate.DatasetCase, 0, len(req.Cases))
	for _, pc := range req.Cases {
		cases = append(cases, &evaluate.DatasetCase{
			DatasetID: req.DatasetId,
			Input:     pc.Input,
			Expected:  pc.Expected,
			Context:   pc.Context,
			Tags:      pc.Tags,
		})
	}
	if err := s.datasetEngine.AddCases(ctx, cases); err != nil {
		return nil, fmt.Errorf("add cases: %w", err)
	}
	return &commonpb.Empty{}, nil
}

// ListDatasetCases lists all cases in a dataset.
func (s *HarnessService) ListDatasetCases(ctx context.Context, req *pb.ListDatasetCasesRequest) (*pb.ListDatasetCasesResponse, error) {
	cases, err := s.datasetEngine.ListCases(ctx, req.DatasetId)
	if err != nil {
		return nil, fmt.Errorf("list cases: %w", err)
	}
	var pbCases []*pb.DatasetCaseEntry
	for _, c := range cases {
		pbCases = append(pbCases, s.datasetCaseToPB(c))
	}
	return &pb.ListDatasetCasesResponse{Cases: pbCases}, nil
}

// ImportDatasetCases imports cases from JSON or CSV content.
func (s *HarnessService) ImportDatasetCases(ctx context.Context, req *pb.ImportDatasetCasesRequest) (*pb.ImportDatasetCasesResponse, error) {
	var imported int
	var err error

	switch req.Format {
	case "json":
		imported, err = s.datasetEngine.ImportJSON(ctx, req.DatasetId, []byte(req.Content))
	case "csv":
		imported, err = s.datasetEngine.ImportCSV(ctx, req.DatasetId, strings.NewReader(req.Content))
	default:
		return nil, fmt.Errorf("unsupported import format: %s (use 'json' or 'csv')", req.Format)
	}

	if err != nil {
		return nil, fmt.Errorf("import cases: %w", err)
	}
	return &pb.ImportDatasetCasesResponse{Imported: int32(imported)}, nil
}

// ==================== Experiment Methods ====================

// RunExperiment creates and runs an experiment against a dataset.
func (s *HarnessService) RunExperiment(ctx context.Context, req *pb.RunExperimentRequest) (*pb.ExperimentEntry, error) {
	var variables map[string]interface{}
	if req.Variables != "" {
		if err := json.Unmarshal([]byte(req.Variables), &variables); err != nil {
			variables = nil
		}
	}

	exp := &evaluate.Experiment{
		Name:       req.Name,
		DatasetID:  req.DatasetId,
		ConfigType: req.ConfigType,
		ConfigRef:  req.ConfigRef,
		Model:      req.Model,
	}

	completed, err := s.experimentEngine.RunExperiment(ctx, exp, req.PromptContent, variables)
	if err != nil {
		return nil, fmt.Errorf("run experiment: %w", err)
	}
	return s.experimentToPB(completed), nil
}

// GetExperiment retrieves an experiment by ID.
func (s *HarnessService) GetExperiment(ctx context.Context, req *pb.GetExperimentRequest) (*pb.ExperimentEntry, error) {
	exp, err := s.experimentEngine.GetExperiment(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("get experiment: %w", err)
	}
	return s.experimentToPB(exp), nil
}

// ListExperiments lists experiments for a dataset.
func (s *HarnessService) ListExperiments(ctx context.Context, req *pb.ListExperimentsRequest) (*pb.ListExperimentsResponse, error) {
	experiments, err := s.experimentEngine.ListExperiments(ctx, req.DatasetId)
	if err != nil {
		return nil, fmt.Errorf("list experiments: %w", err)
	}
	var pbExps []*pb.ExperimentEntry
	for _, exp := range experiments {
		pbExps = append(pbExps, s.experimentToPB(exp))
	}
	return &pb.ListExperimentsResponse{Experiments: pbExps}, nil
}

// GetExperimentResults retrieves all results for an experiment.
func (s *HarnessService) GetExperimentResults(ctx context.Context, req *pb.GetExperimentResultsRequest) (*pb.GetExperimentResultsResponse, error) {
	results, err := s.experimentEngine.GetResults(ctx, req.ExperimentId)
	if err != nil {
		return nil, fmt.Errorf("get experiment results: %w", err)
	}
	var pbResults []*pb.ExperimentResultEntry
	for _, r := range results {
		pbResults = append(pbResults, s.experimentResultToPB(r))
	}
	return &pb.GetExperimentResultsResponse{Results: pbResults}, nil
}

// CompareExperiments compares multiple experiments on the same dataset.
func (s *HarnessService) CompareExperiments(ctx context.Context, req *pb.CompareExperimentsRequest) (*pb.CompareExperimentsResponse, error) {
	comparison, err := s.experimentEngine.CompareExperiments(ctx, req.ExperimentIds)
	if err != nil {
		return nil, fmt.Errorf("compare experiments: %w", err)
	}
	var pbSummaries []*pb.ExperimentSummaryEntry
	for _, summary := range comparison.Experiments {
		pbSummaries = append(pbSummaries, &pb.ExperimentSummaryEntry{
			Id:          summary.ID,
			Name:        summary.Name,
			ConfigType:  summary.ConfigType,
			ConfigRef:   summary.ConfigRef,
			AvgScore:    summary.AvgScore,
			CaseCount:   int32(summary.CaseCount),
			PassedCount: int32(summary.PassedCount),
			FailedCount: int32(summary.FailedCount),
			Status:      summary.Status,
		})
	}
	return &pb.CompareExperimentsResponse{
		Experiments:      pbSummaries,
		BestExperimentId: comparison.BestExperimentID,
	}, nil
}

// ==================== Prompt Optimizer Methods ====================

// RunPromptOptimization runs a prompt optimization loop (TextGrad or GEPA).
func (s *HarnessService) RunPromptOptimization(ctx context.Context, req *pb.RunPromptOptimizationRequest) (*pb.RunPromptOptimizationResponse, error) {
	var variables map[string]interface{}
	if req.Variables != "" {
		if err := json.Unmarshal([]byte(req.Variables), &variables); err != nil {
			variables = nil
		}
	}

	strategy := req.Strategy
	if strategy == "" {
		strategy = "textgrad"
	}

	optReq := &optimizer.OptimizeRequest{
		PromptKey: req.PromptKey,
		DatasetID: req.DatasetId,
		Metric:    req.Metric,
		Strategy:  strategy,
		Config: optimizer.OptimizerConfig{
			MaxRounds:          int(req.MaxRounds),
			NoImproveLimit:     int(req.NoImproveLimit),
			ScoreThreshold:     req.ScoreThreshold,
			Model:              req.Model,
			CandidatesPerRound: int(req.CandidatesPerRound),
			PopulationSize:     int(req.PopulationSize),
		},
		Variables: variables,
	}

	// Fill defaults if no config provided
	if optReq.Config.MaxRounds == 0 {
		optReq.Config = optimizer.DefaultOptimizerConfig()
	}

	result, err := s.optimizeService.RunOptimization(ctx, optReq)
	if err != nil {
		return nil, fmt.Errorf("run optimization: %w", err)
	}

	var pbHistory []*pb.OptimizeRoundEntry
	for _, round := range result.History {
		pbHistory = append(pbHistory, &pb.OptimizeRoundEntry{
			Round:      int32(round.Round),
			PromptText: round.PromptText,
			Score:      round.Score,
			Improved:   round.Improved,
			Suggestion: round.Suggestion,
			VersionId:  round.VersionID,
		})
	}

	return &pb.RunPromptOptimizationResponse{
		BestVersionId: result.BestVersionID,
		BestScore:     result.BestScore,
		OriginalScore: result.OriginalScore,
		Rounds:        int32(result.Rounds),
		Strategy:      result.Strategy,
		History:       pbHistory,
	}, nil
}

// AdoptPromptVersion activates the best prompt version found by optimization.
func (s *HarnessService) AdoptPromptVersion(ctx context.Context, req *pb.AdoptPromptVersionRequest) (*commonpb.Empty, error) {
	if err := s.optimizeService.AdoptVersion(ctx, req.VersionId); err != nil {
		return nil, fmt.Errorf("adopt version: %w", err)
	}
	return &commonpb.Empty{}, nil
}

// ==================== Proto Conversion Helpers ====================

func (s *HarnessService) datasetToPB(ds *evaluate.EvalDataset) *pb.EvalDatasetEntry {
	return &pb.EvalDatasetEntry{
		Id:          ds.ID,
		Name:        ds.Name,
		Description: ds.Description,
		TenantId:    ds.TenantID,
		CaseCount:   int32(ds.CaseCount),
		CreatedAt:   ds.CreatedAt.Unix(),
		UpdatedAt:   ds.UpdatedAt.Unix(),
	}
}

func (s *HarnessService) datasetCaseToPB(c *evaluate.DatasetCase) *pb.DatasetCaseEntry {
	return &pb.DatasetCaseEntry{
		Id:        c.ID,
		DatasetId: c.DatasetID,
		Input:     c.Input,
		Expected:  c.Expected,
		Context:   c.Context,
		Tags:      c.Tags,
		CreatedAt: c.CreatedAt.Unix(),
	}
}

func (s *HarnessService) experimentToPB(exp *evaluate.Experiment) *pb.ExperimentEntry {
	var completedAt int64
	if exp.CompletedAt != nil {
		completedAt = exp.CompletedAt.Unix()
	}
	return &pb.ExperimentEntry{
		Id:          exp.ID,
		Name:        exp.Name,
		DatasetId:   exp.DatasetID,
		ConfigType:  exp.ConfigType,
		ConfigRef:   exp.ConfigRef,
		Model:       exp.Model,
		Status:      exp.Status,
		AvgScore:    exp.AvgScore,
		CaseCount:   int32(exp.CaseCount),
		CreatedAt:   exp.CreatedAt.Unix(),
		CompletedAt: completedAt,
	}
}

func (s *HarnessService) experimentResultToPB(r *evaluate.ExperimentResult) *pb.ExperimentResultEntry {
	return &pb.ExperimentResultEntry{
		Id:           r.ID,
		ExperimentId: r.ExperimentID,
		CaseId:       r.CaseID,
		Output:       r.Output,
		Score:        r.Score,
		Passed:       r.Passed,
		LatencyMs:    r.LatencyMs,
		Tokens:       r.Tokens,
		Scores:       r.Scores,
		Error:        r.Error,
	}
}
