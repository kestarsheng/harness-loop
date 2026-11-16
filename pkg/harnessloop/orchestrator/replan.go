package orchestrator

// T-P2-10 · 重规划（design 4.13.3 / REQ-133 关联）与多 DAG 版本管理。
//
// 规则（任务书 T-P2-10 实施要点 1 与 5）：
//  1. 重规划只生成**新 dag_id**，旧 DAG 置 `superseded` 并保留完整快照可查；
//  2. 旧 DAG 里未终态的子任务一律 `cancelled`（原因 superseded），不允许
//     新旧两个 DAG 同时往下跑；
//  3. 重规划事件同时携带 `old_dag_id` 与 `new_dag_id`（复用已注册的
//     `subtask.plan_created` 事件类型，不新增平台事件字典条目）；
//  4. 重规划成功即复位该类熔断（design 4.13.3），复位写审计 source=dag_replan。

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"agent-platform/pkg/harnessloop/subtask"
)

// DAGStatus is the lifecycle of one DAG version of a parent task.
type DAGStatus string

const (
	DAGStatusActive     DAGStatus = "active"
	DAGStatusSuperseded DAGStatus = "superseded"
)

// DAGRecord is the persisted, queryable snapshot of one DAG version. The plan
// and the expanded subtasks are kept so a superseded DAG can still be inspected
// (GET /harness-loop/tasks/:id?dag_id=...) after the replan.
type DAGRecord struct {
	DagID        string             `json:"dag_id"`
	ParentTaskID string             `json:"parent_task_id"`
	TenantID     string             `json:"tenant_id"`
	ProjectID    string             `json:"project_id"`
	Revision     int                `json:"revision"`
	Status       DAGStatus          `json:"status"`
	SupersededBy string             `json:"superseded_by,omitempty"`
	SupersededAt time.Time          `json:"superseded_at,omitempty"`
	Reason       string             `json:"superseded_reason,omitempty"`
	Plan         *Plan              `json:"plan,omitempty"`
	Subtasks     []*subtask.Subtask `json:"subtasks,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
	UpdatedAt    time.Time          `json:"updated_at"`
}

// DAGStore persists DAG versions. Every read is tenant/project scoped: a
// mismatch returns "not found" instead of leaking existence (REQ-137, 4.13.8).
type DAGStore interface {
	Save(ctx context.Context, record *DAGRecord) error
	Get(ctx context.Context, dagID, tenantID, projectID string) (*DAGRecord, error)
	ListByParent(ctx context.Context, parentTaskID, tenantID, projectID string) ([]*DAGRecord, error)
}

// MemoryDAGStore is the in-memory store used by tests and as a default before
// the PostgreSQL implementation lands.
type MemoryDAGStore struct {
	mu      sync.Mutex
	records map[string]*DAGRecord
	order   []string
}

func NewMemoryDAGStore() *MemoryDAGStore {
	return &MemoryDAGStore{records: make(map[string]*DAGRecord)}
}

func (s *MemoryDAGStore) Save(_ context.Context, record *DAGRecord) error {
	if record == nil || strings.TrimSpace(record.DagID) == "" {
		return newError(CodeDAGIncomplete, "dag record requires dag_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[record.DagID]; !ok {
		s.order = append(s.order, record.DagID)
	}
	s.records[record.DagID] = cloneDAGRecord(record)
	return nil
}

func (s *MemoryDAGStore) Get(_ context.Context, dagID, tenantID, projectID string) (*DAGRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[dagID]
	if !ok {
		return nil, newError(CodeDAGNotFound, "dag %s not found", dagID)
	}
	if tenantID != "" && record.TenantID != tenantID {
		return nil, newError(CodeDAGNotFound, "dag %s not found", dagID)
	}
	if projectID != "" && record.ProjectID != projectID {
		return nil, newError(CodeDAGNotFound, "dag %s not found", dagID)
	}
	return cloneDAGRecord(record), nil
}

func (s *MemoryDAGStore) ListByParent(_ context.Context, parentTaskID, tenantID, projectID string) ([]*DAGRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*DAGRecord
	for _, id := range s.order {
		record := s.records[id]
		if record.ParentTaskID != parentTaskID {
			continue
		}
		if tenantID != "" && record.TenantID != tenantID {
			continue
		}
		if projectID != "" && record.ProjectID != projectID {
			continue
		}
		out = append(out, cloneDAGRecord(record))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Revision < out[j].Revision })
	return out, nil
}

func cloneDAGRecord(record *DAGRecord) *DAGRecord {
	copied := *record
	if record.Subtasks != nil {
		copied.Subtasks = make([]*subtask.Subtask, len(record.Subtasks))
		for i, st := range record.Subtasks {
			if st == nil {
				continue
			}
			row := *st
			copied.Subtasks[i] = &row
		}
	}
	return &copied
}

// CircuitResetter is the slice of Scheduler the replanner needs: a successful
// replan is one of the two legal circuit recovery paths (design 4.13.3).
type CircuitResetter interface {
	ResetCircuit(ctx context.Context, req CircuitResetRequest) ([]string, error)
}

// ReplanRequest is one replanning request. Plan/RawPlan describe the new DAG.
type ReplanRequest struct {
	ParentTaskID string
	TenantID     string
	ProjectID    string
	OldDagID     string
	Stage        string
	Plan         *Plan
	RawPlan      []byte
	Reason       string
	Actor        string
	// Bucket is the business rollback bucket the replan belongs to, forwarded
	// to the plan_created payload for the dashboard.
	Bucket string
}

// ReplanResult is what the runtime needs to continue: the new DAG, its
// subtasks, the superseded snapshot and the old subtasks that were cancelled.
type ReplanResult struct {
	Old          *DAGRecord
	New          *DAGRecord
	Subtasks     []*subtask.Subtask
	Levels       map[string]int
	Cancelled    []string
	ResetClasses []string
	OldDagID     string
	NewDagID     string
}

// Replanner creates a new DAG version and retires the previous one.
type Replanner struct {
	planner  *Planner
	store    DAGStore
	emitter  Emitter
	circuit  CircuitResetter
	now      func() time.Time
	newDagID func() (string, error)
}

func NewReplanner(planner *Planner, store DAGStore, emitter Emitter) *Replanner {
	if store == nil {
		store = NewMemoryDAGStore()
	}
	return &Replanner{planner: planner, store: store, emitter: emitter, now: time.Now, newDagID: newDagIdentifier}
}

// WithCircuitResetter wires the parent scheduler so a successful replan also
// clears the class circuit breaker (audited with source=dag_replan).
func (r *Replanner) WithCircuitResetter(resetter CircuitResetter) *Replanner {
	r.circuit = resetter
	return r
}

func (r *Replanner) SetClock(now func() time.Time) {
	if now != nil {
		r.now = now
	}
}

func (r *Replanner) Store() DAGStore { return r.store }

// RecordInitial stores the first DAG version of a parent task (b stage).
func (r *Replanner) RecordInitial(ctx context.Context, req PlanRequest, result *PlanResult) (*DAGRecord, error) {
	if result == nil || result.Plan == nil {
		return nil, newError(CodeDAGIncomplete, "plan result is required")
	}
	now := r.now().UTC()
	record := &DAGRecord{
		DagID:        result.Plan.DagID,
		ParentTaskID: req.ParentTaskID,
		TenantID:     req.TenantID,
		ProjectID:    req.ProjectID,
		Revision:     1,
		Status:       DAGStatusActive,
		Plan:         result.Plan,
		Subtasks:     result.Subtasks,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := r.store.Save(ctx, record); err != nil {
		return nil, err
	}
	return record, nil
}

// Replan validates the new plan, supersedes the old DAG and returns the new
// pending subtasks. A superseded DAG stays readable, and its non-terminal
// subtasks are cancelled exactly once.
func (r *Replanner) Replan(ctx context.Context, req ReplanRequest) (*ReplanResult, error) {
	if strings.TrimSpace(req.ParentTaskID) == "" || strings.TrimSpace(req.OldDagID) == "" {
		return nil, newError(CodeDAGIncomplete, "parent_task_id and old dag_id are required for a replan")
	}
	if strings.TrimSpace(req.Reason) == "" {
		return nil, newError(CodeDAGIncomplete, "a replan requires a reason")
	}
	old, err := r.store.Get(ctx, req.OldDagID, req.TenantID, req.ProjectID)
	if err != nil {
		return nil, err
	}
	if old.ParentTaskID != req.ParentTaskID {
		// Treat a mismatched parent as "not found" rather than confirming that
		// another task owns this dag_id.
		return nil, newError(CodeDAGNotFound, "dag %s does not belong to task %s", req.OldDagID, req.ParentTaskID)
	}
	if old.Status != DAGStatusActive {
		return nil, newError(CodeDAGNotActive, "dag %s is already %s (superseded_by=%s)",
			old.DagID, old.Status, old.SupersededBy)
	}

	plan := req.Plan
	if plan == nil {
		parsed, err := ParsePlan(req.RawPlan)
		if err != nil {
			return nil, err
		}
		plan = parsed
	}
	newDagID := strings.TrimSpace(plan.DagID)
	if newDagID == "" || newDagID == old.DagID {
		if newDagID == old.DagID {
			return nil, newError(CodeReplanSameDag, "replan must produce a new dag_id, got %s", newDagID)
		}
		generated, err := r.newDagID()
		if err != nil {
			return nil, err
		}
		newDagID = generated
	}
	if _, err := r.store.Get(ctx, newDagID, req.TenantID, req.ProjectID); err == nil {
		return nil, newError(CodeReplanSameDag, "dag %s already exists", newDagID)
	}
	plan.DagID = newDagID

	planned, err := r.planner.Plan(ctx, PlanRequest{
		ParentTaskID: req.ParentTaskID,
		TenantID:     req.TenantID,
		ProjectID:    req.ProjectID,
		Stage:        req.Stage,
		DagID:        newDagID,
		Plan:         plan,
	})
	if err != nil {
		return nil, err
	}

	now := r.now().UTC()
	cancelled, err := r.cancelSuperseded(ctx, old, req, newDagID, now)
	if err != nil {
		return nil, err
	}
	newRecord := &DAGRecord{
		DagID:        newDagID,
		ParentTaskID: req.ParentTaskID,
		TenantID:     req.TenantID,
		ProjectID:    req.ProjectID,
		Revision:     old.Revision + 1,
		Status:       DAGStatusActive,
		Plan:         planned.Plan,
		Subtasks:     planned.Subtasks,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	// Old is written before New: if the process dies in between, the parent has
	// no active DAG (an error the runtime must surface), never two active DAGs.
	if err := r.store.Save(ctx, old); err != nil {
		return nil, err
	}
	if err := r.store.Save(ctx, newRecord); err != nil {
		return nil, err
	}

	result := &ReplanResult{
		Old:       old,
		New:       newRecord,
		Subtasks:  planned.Subtasks,
		Levels:    planned.Levels,
		Cancelled: cancelled,
		OldDagID:  old.DagID,
		NewDagID:  newDagID,
	}

	if r.circuit != nil {
		reset, err := r.circuit.ResetCircuit(ctx, CircuitResetRequest{
			Actor:        firstNonEmptyString(req.Actor, "orchestrator"),
			Reason:       "replan:" + req.Reason,
			Source:       CircuitResetSourceReplan,
			TenantID:     req.TenantID,
			ProjectID:    req.ProjectID,
			ParentTaskID: req.ParentTaskID,
			DagID:        newDagID,
		})
		if err != nil {
			return result, err
		}
		result.ResetClasses = reset
	}

	if err := emit(ctx, r.emitter, Event{
		Type:         EventSubtaskPlanCreated,
		ParentTaskID: req.ParentTaskID,
		DagID:        newDagID,
		OldDagID:     old.DagID,
		NewDagID:     newDagID,
		Payload: map[string]any{
			"dag_id":             newDagID,
			"old_dag_id":         old.DagID,
			"new_dag_id":         newDagID,
			"revision":           newRecord.Revision,
			"replanned":          true,
			"reason":             req.Reason,
			"actor":              req.Actor,
			"bucket":             req.Bucket,
			"node_count":         len(planned.Plan.Nodes),
			"subtask_count":      len(planned.Subtasks),
			"cancelled_subtasks": cancelled,
			"reset_classes":      result.ResetClasses,
			"levels":             planned.Levels,
		},
	}); err != nil {
		return result, err
	}
	return result, nil
}

// cancelSuperseded retires the old version: mark it superseded, cancel every
// non-terminal subtask and emit one subtask.cancelled per affected child.
func (r *Replanner) cancelSuperseded(ctx context.Context, old *DAGRecord, req ReplanRequest, newDagID string, now time.Time) ([]string, error) {
	old.Status = DAGStatusSuperseded
	old.SupersededBy = newDagID
	old.SupersededAt = now
	old.Reason = req.Reason
	old.UpdatedAt = now
	var cancelled []string
	for _, st := range old.Subtasks {
		if st == nil || st.Status.Terminal() {
			continue
		}
		st.Status = subtask.StatusCancelled
		st.UpdatedAt = now
		cancelled = append(cancelled, st.SubtaskID)
		if err := emit(ctx, r.emitter, Event{
			Type:         EventSubtaskCancelled,
			Severity:     SeverityWarn,
			ParentTaskID: old.ParentTaskID,
			SubtaskID:    st.SubtaskID,
			DagID:        old.DagID,
			DagNodeID:    st.DagNodeID,
			OldDagID:     old.DagID,
			Payload: map[string]any{
				"reason":      "superseded",
				"replaced_by": newDagID,
			},
		}); err != nil {
			return cancelled, err
		}
	}
	return cancelled, nil
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func newDagIdentifier() (string, error) {
	id, err := newSubtaskID()
	if err != nil {
		return "", err
	}
	return "dag-" + id, nil
}
