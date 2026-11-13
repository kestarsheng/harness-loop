package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agent-platform/pkg/harnessloop/subtask"
)

const (
	ConflictSamePathMultiWriter = "same_path_multi_writer"
	ConflictSymbolMultiWriter   = "symbol_multi_writer"
	ConflictContractMismatch    = "contract_mismatch"
)

// ArtifactCheck is the result of verifying one artifact reference.
type ArtifactCheck struct {
	Exists     bool
	ActualHash string
	HashMatch  bool
}

// ArtifactVerifier checks existence and content hash. FSVerifier is the
// default implementation; tests may inject a fake.
type ArtifactVerifier interface {
	Verify(ctx context.Context, path, wantHash string) (ArtifactCheck, error)
}

// FSVerifier verifies artifacts under a workspace root.
type FSVerifier struct {
	Root string
}

func (v FSVerifier) Verify(_ context.Context, path, wantHash string) (ArtifactCheck, error) {
	if err := validateSandboxPath(v.Root, path); err != nil {
		return ArtifactCheck{}, err
	}
	full := path
	if v.Root != "" {
		full = filepath.Join(v.Root, path)
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return ArtifactCheck{Exists: false}, nil
		}
		return ArtifactCheck{}, err
	}
	sum := sha256.Sum256(raw)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	want := strings.TrimSpace(wantHash)
	if !strings.HasPrefix(strings.ToLower(want), "sha256:") {
		want = "sha256:" + want
	}
	return ArtifactCheck{Exists: true, ActualHash: actual, HashMatch: actual == want}, nil
}

// SymbolConflictDetector is an optional port backed by the code index
// (code_edges, T-P1-14). The aggregator never queries the index directly.
type SymbolConflictDetector interface {
	Detect(ctx context.Context, paths []string) ([]Conflict, error)
}

type AggregatedArtifact struct {
	Path       string `json:"path"`
	Hash       string `json:"hash"`
	Exists     bool   `json:"exists"`
	HashMatch  bool   `json:"hash_match"`
	ActualHash string `json:"actual_hash,omitempty"`
}

type AggregationItem struct {
	SubtaskID      string               `json:"subtask_id"`
	DagNodeID      string               `json:"dag_node_id"`
	Status         subtask.Status       `json:"status"`
	Artifacts      []AggregatedArtifact `json:"artifacts,omitempty"`
	Verdict        string               `json:"verdict"`
	Reason         string               `json:"reason,omitempty"`
	ConsumedTokens int64                `json:"consumed_tokens"`
}

type Conflict struct {
	Kind       string   `json:"kind"`
	Path       string   `json:"path,omitempty"`
	Subtasks   []string `json:"subtasks"`
	Resolution string   `json:"resolution"`
	Evidence   []string `json:"evidence,omitempty"`
}

type AggregationSummary struct {
	Total     int `json:"total"`
	Accepted  int `json:"accepted"`
	Rejected  int `json:"rejected"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

// AggregationReport is AggregationReport/v1 and is a required d-stage
// acceptance input (REQ-133).
type AggregationReport struct {
	TaskID      string             `json:"task_id"`
	DagID       string             `json:"dag_id"`
	Items       []AggregationItem  `json:"items"`
	Conflicts   []Conflict         `json:"conflicts"`
	Summary     AggregationSummary `json:"summary"`
	Complete    bool               `json:"complete"`
	CompletedAt time.Time          `json:"completed_at"`
}

type AggregationRequest struct {
	ParentTaskID string
	DagID        string
	TenantID     string
	ProjectID    string
	Subtasks     []*subtask.Subtask
}

// Aggregator collects every subtask verdict and verifies accepted artifacts.
type Aggregator struct {
	emitter        Emitter
	verifier       ArtifactVerifier
	symbolDetector SymbolConflictDetector
}

func NewAggregator(verifier ArtifactVerifier, emitter Emitter) *Aggregator {
	if verifier == nil {
		verifier = FSVerifier{}
	}
	return &Aggregator{emitter: emitter, verifier: verifier}
}

// WithSymbolDetector wires the optional code-index based conflict detector.
func (a *Aggregator) WithSymbolDetector(detector SymbolConflictDetector) *Aggregator {
	a.symbolDetector = detector
	return a
}

// Aggregate verifies every accepted artifact, lists every non-accepted
// verdict, and detects conflicts. It returns a report even when the report is
// incomplete, together with the coded error.
func (a *Aggregator) Aggregate(ctx context.Context, req AggregationRequest) (*AggregationReport, error) {
	if len(req.Subtasks) == 0 {
		return nil, newError(CodeAggregationMissing, "no subtasks supplied for aggregation")
	}
	report := &AggregationReport{
		TaskID:      req.ParentTaskID,
		DagID:       req.DagID,
		Items:       make([]AggregationItem, 0, len(req.Subtasks)),
		CompletedAt: time.Now().UTC(),
		Complete:    true,
	}
	var securityViolation bool
	for _, st := range req.Subtasks {
		if st == nil {
			continue
		}
		if st.TenantID != req.TenantID || st.ProjectID != req.ProjectID || st.ParentTaskID != req.ParentTaskID {
			securityViolation = true
			report.Complete = false
			continue
		}
		item := AggregationItem{
			SubtaskID:      st.SubtaskID,
			DagNodeID:      st.DagNodeID,
			Status:         st.Status,
			ConsumedTokens: st.ConsumedTokens,
		}
		report.Summary.Total++
		switch st.Status {
		case subtask.StatusAccepted:
			report.Summary.Accepted++
			item.Verdict = "pass"
			for _, ref := range st.ArtifactRefs {
				check, err := a.verifier.Verify(ctx, ref.Path, ref.Hash)
				if err != nil {
					report.Complete = false
					item.Verdict = "fail"
					item.Reason = err.Error()
					break
				}
				artifact := AggregatedArtifact{
					Path:       ref.Path,
					Hash:       ref.Hash,
					Exists:     check.Exists,
					HashMatch:  check.HashMatch,
					ActualHash: check.ActualHash,
				}
				if !check.Exists || !check.HashMatch {
					report.Complete = false
					item.Verdict = "fail"
					if !check.Exists {
						item.Reason = "artifact_missing"
					} else {
						item.Reason = "artifact_drift"
					}
				}
				item.Artifacts = append(item.Artifacts, artifact)
			}
		case subtask.StatusRejected:
			report.Summary.Rejected++
			item.Verdict = "fail"
			item.Reason = string(st.Status)
		case subtask.StatusFailed:
			report.Summary.Failed++
			item.Verdict = "fail"
			item.Reason = string(st.Status)
		case subtask.StatusCancelled:
			report.Summary.Cancelled++
			item.Verdict = "cancelled"
			item.Reason = string(st.Status)
		default:
			// pending/running/waiting_handoff must never contribute artifacts.
			securityViolation = true
			report.Complete = false
			item.Verdict = "rejected"
			item.Reason = "subtask is not terminal"
		}
		report.Items = append(report.Items, item)
	}

	if securityViolation {
		if a.emitter != nil {
			if err := emitSecurityViolation(ctx, a.emitter, req.ParentTaskID, "", "",
				CodeSubtaskNotAccepted, "non-accepted subtask was supplied to aggregation"); err != nil {
				return report, err
			}
		}
		return report, newError(CodeSubtaskNotAccepted,
			"aggregation only accepts accepted subtasks; reject/fail/cancel rows are listed without artifacts")
	}

	a.detectConflicts(ctx, req, report)
	if len(report.Conflicts) > 0 {
		report.Complete = false
		for _, conflict := range report.Conflicts {
			if err := emit(ctx, a.emitter, Event{
				Type:         EventAggregationConflict,
				Severity:     SeverityWarn,
				ParentTaskID: req.ParentTaskID,
				DagID:        req.DagID,
				Payload: map[string]any{
					"kind":       conflict.Kind,
					"path":       conflict.Path,
					"subtasks":   conflict.Subtasks,
					"resolution": conflict.Resolution,
				},
			}); err != nil {
				return report, err
			}
		}
	}

	severity := SeverityInfo
	if !report.Complete {
		severity = SeverityWarn
	}
	if err := emit(ctx, a.emitter, Event{
		Type:         EventAggregationCompleted,
		Severity:     severity,
		ParentTaskID: req.ParentTaskID,
		DagID:        req.DagID,
		Payload: map[string]any{
			"total":          report.Summary.Total,
			"accepted":       report.Summary.Accepted,
			"rejected":       report.Summary.Rejected,
			"failed":         report.Summary.Failed,
			"cancelled":      report.Summary.Cancelled,
			"conflict_count": len(report.Conflicts),
			"complete":       report.Complete,
		},
	}); err != nil {
		return report, err
	}
	if !report.Complete {
		return report, newError(CodeAggregationIncomplete,
			"aggregation report is incomplete: %d conflicts, artifact verification failed", len(report.Conflicts))
	}
	return report, nil
}

func (a *Aggregator) detectConflicts(ctx context.Context, req AggregationRequest, report *AggregationReport) {
	byPath := make(map[string][]string)
	acceptedFiles := make(map[string][]string)
	byNode := make(map[string]*subtask.Subtask, len(req.Subtasks))
	for _, st := range req.Subtasks {
		if st != nil {
			byNode[st.DagNodeID] = st
		}
	}
	for _, item := range report.Items {
		if item.Status != subtask.StatusAccepted {
			continue
		}
		for _, artifact := range item.Artifacts {
			byPath[artifact.Path] = append(byPath[artifact.Path], item.SubtaskID)
			acceptedFiles[artifact.Path] = append(acceptedFiles[artifact.Path], item.SubtaskID)
		}
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		owners := byPath[path]
		if len(owners) < 2 {
			continue
		}
		sort.Strings(owners)
		report.Conflicts = append(report.Conflicts, Conflict{
			Kind:       ConflictSamePathMultiWriter,
			Path:       path,
			Subtasks:   owners,
			Resolution: "reject",
		})
	}

	for _, st := range req.Subtasks {
		if st == nil || st.Status != subtask.StatusAccepted {
			continue
		}
		for _, depID := range st.DependsOn {
			dep := byNode[depID]
			if dep == nil || dep.Status != subtask.StatusAccepted {
				continue
			}
			upstream := contractType(dep.OutputContract)
			downstream := contractType(st.InputContract)
			if upstream == "" || downstream == "" || upstream == downstream {
				continue
			}
			report.Conflicts = append(report.Conflicts, Conflict{
				Kind:       ConflictContractMismatch,
				Subtasks:   []string{dep.SubtaskID, st.SubtaskID},
				Resolution: "reject",
				Evidence:   []string{"upstream=" + upstream, "downstream=" + downstream},
			})
		}
	}

	if a.symbolDetector != nil {
		var allPaths []string
		for path := range acceptedFiles {
			allPaths = append(allPaths, path)
		}
		sort.Strings(allPaths)
		detected, err := a.symbolDetector.Detect(ctx, allPaths)
		if err == nil {
			report.Conflicts = append(report.Conflicts, detected...)
		}
	}
}

func contractType(contract map[string]any) string {
	if contract == nil {
		return ""
	}
	if v, ok := contract["type"].(string); ok {
		return v
	}
	return ""
}

// RequireForAcceptance is the d-stage gate: without a complete report the
// acceptance must not start (REQ-133).
func (a *Aggregator) RequireForAcceptance(report *AggregationReport) error {
	if report == nil || report.TaskID == "" || len(report.Items) == 0 {
		return newError(CodeAggregationMissing, "aggregation report is required before d-stage acceptance")
	}
	if len(report.Conflicts) > 0 || !report.Complete {
		return newError(CodeAggregationIncomplete, "aggregation report is incomplete or has conflicts")
	}
	return nil
}

// AcceptanceInput returns the report as the extra d-stage acceptance injection
// (design 4.13.5: artifact list + aggregation report).
func AcceptanceInput(report *AggregationReport) map[string]any {
	if report == nil {
		return nil
	}
	return map[string]any{
		"aggregation_report": report,
		"items":              report.Items,
		"conflicts":          report.Conflicts,
		"summary":            report.Summary,
	}
}
