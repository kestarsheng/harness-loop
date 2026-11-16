package orchestrator

import (
	"context"
	"sort"
	"sync"
	"time"

	"agent-platform/pkg/harnessloop/subtask"
)

type HandoffStatus string

const (
	HandoffCreated  HandoffStatus = "created"
	HandoffAccepted HandoffStatus = "accepted"
	HandoffRejected HandoffStatus = "rejected"
)

// HandoffRecord is the persisted view of a handoff (handoff_id + outcome).
type HandoffRecord struct {
	HandoffID    string        `json:"handoff_id"`
	ParentTaskID string        `json:"parent_task_id"`
	SubtaskID    string        `json:"subtask_id"`
	TenantID     string        `json:"tenant_id"`
	ProjectID    string        `json:"project_id"`
	FromAgentID  string        `json:"from_agent_id"`
	FromRole     string        `json:"from_role"`
	ToAgentID    string        `json:"to_agent_id"`
	ToRole       string        `json:"to_role"`
	FromStage    string        `json:"from_stage,omitempty"`
	ToStage      string        `json:"to_stage,omitempty"`
	Status       HandoffStatus `json:"status"`
	InputDigest  string        `json:"input_digest"`
	Reason       string        `json:"reason,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// HandoffStore persists handoff records. Queries are always tenant/project
// scoped (REQ-137).
type HandoffStore interface {
	Save(ctx context.Context, record *HandoffRecord) error
	Get(ctx context.Context, handoffID string) (*HandoffRecord, error)
	ListByParent(ctx context.Context, parentTaskID, tenantID, projectID string) ([]*HandoffRecord, error)
}

// MemoryHandoffStore is the in-memory store used by tests and as a default
// before a PostgreSQL implementation is wired.
type MemoryHandoffStore struct {
	mu      sync.Mutex
	records map[string]*HandoffRecord
}

func NewMemoryHandoffStore() *MemoryHandoffStore {
	return &MemoryHandoffStore{records: make(map[string]*HandoffRecord)}
}

func (s *MemoryHandoffStore) Save(_ context.Context, record *HandoffRecord) error {
	if record == nil || record.HandoffID == "" {
		return newError(CodeHandoffIncomplete, "handoff record requires handoff_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := *record
	s.records[record.HandoffID] = &copied
	return nil
}

func (s *MemoryHandoffStore) Get(_ context.Context, handoffID string) (*HandoffRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[handoffID]
	if !ok {
		return nil, newError(CodeHandoffIncomplete, "handoff %s not found", handoffID)
	}
	copied := *record
	return &copied, nil
}

func (s *MemoryHandoffStore) ListByParent(_ context.Context, parentTaskID, tenantID, projectID string) ([]*HandoffRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*HandoffRecord
	for _, record := range s.records {
		if record.ParentTaskID != parentTaskID || record.TenantID != tenantID || record.ProjectID != projectID {
			continue
		}
		copied := *record
		out = append(out, &copied)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].HandoffID < out[j].HandoffID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// HandoffObserver receives every persisted handoff transition. It keeps this
// package free of metrics/tracing dependencies: the service layer supplies an
// adapter that records hlp_handoff_total and the handoff child span.
type HandoffObserver interface {
	HandoffTransition(ctx context.Context, record *HandoffRecord)
}

// HandoffService coordinates validation, context construction, transport and
// persistence. Transport is optional so contract-only tests do not need a2a.
type HandoffService struct {
	validator *HandoffValidator
	transport HandoffTransport
	store     HandoffStore
	emitter   Emitter
	observer  HandoffObserver
	now       func() time.Time
}

// WithObserver wires the optional observability adapter.
func (s *HandoffService) WithObserver(o HandoffObserver) *HandoffService {
	s.observer = o
	return s
}

func NewHandoffService(roles *subtask.Registry, transport HandoffTransport, store HandoffStore, emitter Emitter) *HandoffService {
	return &HandoffService{
		validator: NewHandoffValidator(roles, emitter),
		transport: transport,
		store:     store,
		emitter:   emitter,
		now:       time.Now,
	}
}

// Create validates the contract, freezes the receiver context, hands it to the
// transport (a2a-service adapter) and records the handoff.
func (s *HandoffService) Create(ctx context.Context, h *HandoffContract, req HandoffRequest, injected []InjectedItem) (*HandoffRecord, *HandoffContext, error) {
	if err := s.validator.Validate(ctx, h, req); err != nil {
		return nil, nil, err
	}
	receiverCtx, err := BuildHandoffContext(ctx, h, injected, s.emitter)
	if err != nil {
		return nil, nil, err
	}
	if s.transport != nil {
		envelope := HandoffEnvelope{
			HandoffID:    h.HandoffID,
			ParentTaskID: h.ParentTaskID,
			SubtaskID:    h.SubtaskID,
			FromAgentID:  h.From.AgentID,
			ToAgentID:    h.To.AgentID,
			TenantID:     h.TenantID,
			ProjectID:    h.ProjectID,
			Metadata: map[string]string{
				"handoff_id":     h.HandoffID,
				"parent_task_id": h.ParentTaskID,
				"subtask_id":     h.SubtaskID,
			},
		}
		envelope = BuildCrossStageEnvelope(envelope, req.Stage, req.ToStage)
		if err := s.transport.SendHandoff(ctx, envelope); err != nil {
			return nil, nil, err
		}
	}
	now := s.now().UTC()
	record := &HandoffRecord{
		HandoffID:    h.HandoffID,
		ParentTaskID: h.ParentTaskID,
		SubtaskID:    h.SubtaskID,
		TenantID:     h.TenantID,
		ProjectID:    h.ProjectID,
		FromAgentID:  h.From.AgentID,
		FromRole:     h.From.Role,
		ToAgentID:    h.To.AgentID,
		ToRole:       h.To.Role,
		FromStage:    req.Stage,
		ToStage:      req.ToStage,
		Status:       HandoffCreated,
		InputDigest:  receiverCtx.InputDigest,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if s.store != nil {
		if err := s.store.Save(ctx, record); err != nil {
			return nil, nil, err
		}
	}
	if err := emit(ctx, s.emitter, Event{
		Type:         EventHandoffCreated,
		ParentTaskID: h.ParentTaskID,
		SubtaskID:    h.SubtaskID,
		HandoffID:    h.HandoffID,
		Payload: map[string]any{
			"from_agent_id":  h.From.AgentID,
			"to_agent_id":    h.To.AgentID,
			"from_role":      h.From.Role,
			"to_role":        h.To.Role,
			"from_stage":     req.Stage,
			"to_stage":       req.ToStage,
			"cross_stage":    req.ToStage != "" && req.ToStage != req.Stage,
			"artifact_count": len(h.Artifacts),
			"input_digest":   receiverCtx.InputDigest,
		},
	}); err != nil {
		return nil, nil, err
	}
	if s.observer != nil {
		s.observer.HandoffTransition(ctx, record)
	}
	return record, receiverCtx, nil
}

func (s *HandoffService) Accept(ctx context.Context, handoffID string) error {
	return s.transition(ctx, handoffID, HandoffAccepted, "", EventHandoffAccepted)
}

func (s *HandoffService) Reject(ctx context.Context, handoffID, reason string) error {
	return s.transition(ctx, handoffID, HandoffRejected, reason, EventHandoffRejected)
}

func (s *HandoffService) transition(ctx context.Context, handoffID string, to HandoffStatus, reason, eventType string) error {
	if s.store == nil {
		return newError(CodeHandoffIncomplete, "handoff store is not configured")
	}
	record, err := s.store.Get(ctx, handoffID)
	if err != nil {
		return err
	}
	if record.Status != HandoffCreated {
		return newError(CodeHandoffIncomplete, "handoff %s is already %s", handoffID, record.Status)
	}
	record.Status = to
	record.Reason = reason
	record.UpdatedAt = s.now().UTC()
	if err := s.store.Save(ctx, record); err != nil {
		return err
	}
	if err := emit(ctx, s.emitter, Event{
		Type:         eventType,
		ParentTaskID: record.ParentTaskID,
		SubtaskID:    record.SubtaskID,
		HandoffID:    record.HandoffID,
		Payload:      map[string]any{"reason": reason},
	}); err != nil {
		return err
	}
	if s.observer != nil {
		s.observer.HandoffTransition(ctx, record)
	}
	return nil
}

func (s *HandoffService) ListByParent(ctx context.Context, parentTaskID, tenantID, projectID string) ([]*HandoffRecord, error) {
	if s.store == nil {
		return nil, nil
	}
	return s.store.ListByParent(ctx, parentTaskID, tenantID, projectID)
}
