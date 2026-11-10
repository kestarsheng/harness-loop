package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

const SchemaVersion = "1.0.0"

const (
	KindStageEntry     = "stage_entry"
	KindStageExit      = "stage_exit"
	KindPreTransition  = "pre_transition"
	KindToolBatch      = "tool_batch"
	KindPreCompaction  = "pre_compaction"
	KindPostCompaction = "post_compaction"
	KindAbort          = "abort"
	KindInterval       = "interval"
)

var ErrNotFound = errors.New("checkpoint not found")
var ErrCorrupted = errors.New("checkpoint is corrupted")

func ValidKind(kind string) bool {
	switch kind {
	case KindStageEntry, KindStageExit, KindPreTransition, KindToolBatch, KindPreCompaction, KindPostCompaction, KindAbort, KindInterval:
		return true
	}
	return false
}

type FSMSnapshot struct {
	CurrentStage          string         `json:"current_stage"`
	Status                string         `json:"status"`
	TerminalReason        string         `json:"terminal_reason,omitempty"`
	AttemptNoByStage      map[string]int `json:"attempt_no_by_stage"`
	RetryCountByStage     map[string]int `json:"retry_count_by_stage"`
	AcceptanceFailByStage map[string]int `json:"acceptance_fail_count_by_stage"`
	PolicySnapshotHash    string         `json:"policy_snapshot_hash,omitempty"`
}

type MessagesRef struct {
	Store      string `json:"store"`
	Collection string `json:"collection"`
	DocID      string `json:"doc_id"`
	Count      int    `json:"count"`
	Digest     string `json:"digest"`
}

type PlanRef struct {
	PlanID  string `json:"plan_id"`
	Version int    `json:"version"`
}

type MemoryRef struct {
	File        string `json:"file"`
	TopicKey    string `json:"topic_key"`
	ContentHash string `json:"content_hash"`
	OrderRank   int    `json:"order_rank"`
}

type Artifact struct {
	Path  string `json:"path"`
	Hash  string `json:"hash"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime,omitempty"`
}

type Consumed struct {
	Tokens           int64   `json:"tokens"`
	Cost             float64 `json:"cost"`
	CompactionTokens int64   `json:"compaction_tokens"`
}

type Checkpoint struct {
	CheckpointID      string      `json:"checkpoint_id"`
	TaskID            string      `json:"task_id"`
	TenantID          string      `json:"tenant_id"`
	SessionID         string      `json:"session_id,omitempty"`
	TraceID           string      `json:"trace_id,omitempty"`
	Stage             string      `json:"stage"`
	AttemptNo         int         `json:"attempt_no"`
	AttemptID         string      `json:"attempt_id,omitempty"`
	Step              int         `json:"step"`
	Kind              string      `json:"kind"`
	FSM               FSMSnapshot `json:"fsm_snapshot"`
	MessagesRef       MessagesRef `json:"messages_ref,omitempty"`
	PlanRef           *PlanRef    `json:"plan_ref,omitempty"`
	MemoryRefs        []MemoryRef `json:"memory_refs,omitempty"`
	Artifacts         []Artifact  `json:"artifacts,omitempty"`
	Consumed          Consumed    `json:"consumed"`
	EventSeqWatermark int64       `json:"event_seq_watermark"`
	SchemaVersion     string      `json:"schema_version"`
	Checksum          string      `json:"checksum"`
	CreatedAt         time.Time   `json:"created_at"`
}

func (c *Checkpoint) Validate() error {
	if c.CheckpointID == "" {
		return fmt.Errorf("%w: checkpoint_id is required", ErrCorrupted)
	}
	if c.TaskID == "" {
		return fmt.Errorf("%w: task_id is required", ErrCorrupted)
	}
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schema_version %q is not supported", ErrCorrupted, c.SchemaVersion)
	}
	if !ValidKind(c.Kind) {
		return fmt.Errorf("%w: kind %q is invalid", ErrCorrupted, c.Kind)
	}
	if c.Stage == "" || c.FSM.CurrentStage == "" {
		return fmt.Errorf("%w: stage and fsm snapshot are required", ErrCorrupted)
	}
	if c.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at is required", ErrCorrupted)
	}
	if c.Checksum != "" && c.Checksum != c.ComputeChecksum() {
		return fmt.Errorf("%w: checksum mismatch", ErrCorrupted)
	}
	return nil
}

func (c *Checkpoint) ComputeChecksum() string {
	clone := *c
	clone.Checksum = ""
	clone.Artifacts = append([]Artifact(nil), c.Artifacts...)
	sort.Slice(clone.Artifacts, func(i, j int) bool { return clone.Artifacts[i].Path < clone.Artifacts[j].Path })
	raw, err := json.Marshal(clone)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (c *Checkpoint) Seal() {
	c.SchemaVersion = SchemaVersion
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	c.Checksum = c.ComputeChecksum()
}

type Store interface {
	Save(ctx context.Context, cp *Checkpoint) error
	Load(ctx context.Context, checkpointID string) (*Checkpoint, error)
	Latest(ctx context.Context, taskID string) (*Checkpoint, error)
	List(ctx context.Context, taskID string, limit int) ([]*Checkpoint, error)
	Delete(ctx context.Context, checkpointID string) error
}

type MemoryStore struct {
	mu     sync.RWMutex
	byID   map[string]*Checkpoint
	byTask map[string][]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: make(map[string]*Checkpoint), byTask: make(map[string][]string)}
}

func (s *MemoryStore) Save(ctx context.Context, cp *Checkpoint) error {
	if cp == nil {
		return errors.New("checkpoint: nil checkpoint")
	}
	if cp.Checksum == "" {
		cp.Seal()
	}
	if err := cp.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[cp.CheckpointID]; !exists {
		s.byTask[cp.TaskID] = append(s.byTask[cp.TaskID], cp.CheckpointID)
	}
	cpCopy := *cp
	s.byID[cp.CheckpointID] = &cpCopy
	return nil
}

func (s *MemoryStore) Load(ctx context.Context, checkpointID string) (*Checkpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp, ok := s.byID[checkpointID]
	if !ok {
		return nil, ErrNotFound
	}
	cpCopy := *cp
	return &cpCopy, nil
}

func (s *MemoryStore) Latest(ctx context.Context, taskID string) (*Checkpoint, error) {
	list, err := s.List(ctx, taskID, 0)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return list[0], nil
}

func (s *MemoryStore) List(ctx context.Context, taskID string, limit int) ([]*Checkpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.byTask[taskID]
	out := make([]*Checkpoint, 0, len(ids))
	for _, id := range ids {
		cpCopy := *s.byID[id]
		out = append(out, &cpCopy)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) Delete(ctx context.Context, checkpointID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, ok := s.byID[checkpointID]
	if !ok {
		return nil
	}
	delete(s.byID, checkpointID)
	ids := s.byTask[cp.TaskID]
	kept := ids[:0]
	for _, id := range ids {
		if id != checkpointID {
			kept = append(kept, id)
		}
	}
	s.byTask[cp.TaskID] = kept
	return nil
}
