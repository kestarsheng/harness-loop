package event

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func minimalEvent() *Event {
	return &Event{
		EventID:       "01J9Z6K3N5Q8V2M4T7R1X0B6C8",
		EventType:     AcceptanceFailed,
		SchemaVersion: CurrentSchemaVersion,
		TaskID:        "task-1",
		Seq:           1,
		Timestamp:     time.Unix(1700000000, 0),
		Severity:      SeverityWarn,
		TenantID:      "tenant-a",
		ProjectID:     "proj-a",
		Payload:       []byte(`{"verdict":"fail"}`),
	}
}

func TestValidateAcceptsMinimalEvent(t *testing.T) {
	if err := minimalEvent().Validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
}

func TestValidateRejectsMissingFields(t *testing.T) {
	cases := []struct {
		field string
		mut   func(e *Event)
		code  string
	}{
		{"event_id", func(e *Event) { e.EventID = "" }, CodeMissingField},
		{"event_type", func(e *Event) { e.EventType = "" }, CodeMissingField},
		{"schema_version", func(e *Event) { e.SchemaVersion = "" }, CodeMissingField},
		{"task_id", func(e *Event) { e.TaskID = "" }, CodeMissingField},
		{"timestamp", func(e *Event) { e.Timestamp = time.Time{} }, CodeMissingField},
		{"severity", func(e *Event) { e.Severity = "" }, CodeMissingField},
		{"tenant_id", func(e *Event) { e.TenantID = "" }, CodeMissingField},
		{"project_id", func(e *Event) { e.ProjectID = "" }, CodeMissingField},
		{"payload", func(e *Event) { e.Payload = nil }, CodePayloadEmpty},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			e := minimalEvent()
			c.mut(e)
			err := e.Validate()
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("expected ValidationError, got %v", err)
			}
			if ve.Code != c.code {
				t.Fatalf("code = %s, want %s", ve.Code, c.code)
			}
			if ve.Field != c.field {
				t.Fatalf("field = %s, want %s", ve.Field, c.field)
			}
		})
	}
}

func TestValidateRejectsUnknownType(t *testing.T) {
	e := minimalEvent()
	e.EventType = "nope.happened"
	var ve *ValidationError
	if err := e.Validate(); !errors.As(err, &ve) || ve.Code != CodeUnknownType {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidateRejectsSchemaVersionMismatch(t *testing.T) {
	e := minimalEvent()
	e.SchemaVersion = "2.0.0"
	var ve *ValidationError
	if err := e.Validate(); !errors.As(err, &ve) || ve.Code != CodeInvalidSchemaVer {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidateRejectsInvalidSeverityAndSeq(t *testing.T) {
	e := minimalEvent()
	e.Severity = Severity("fatal")
	var ve *ValidationError
	if err := e.Validate(); !errors.As(err, &ve) || ve.Code != CodeInvalidSeverity {
		t.Fatalf("severity: %v", err)
	}

	e = minimalEvent()
	e.Seq = 0
	if err := e.Validate(); !errors.As(err, &ve) || ve.Code != CodeInvalidSeq {
		t.Fatalf("seq: %v", err)
	}
}

func TestValidateRejectsOversizedPayload(t *testing.T) {
	e := minimalEvent()
	e.Payload = []byte(strings.Repeat("a", MaxPayloadBytes+1))
	var ve *ValidationError
	if err := e.Validate(); !errors.As(err, &ve) || ve.Code != CodePayloadTooLarge {
		t.Fatalf("unexpected: %v", err)
	}

	e.Payload = []byte(strings.Repeat("a", MaxPayloadBytes))
	if err := e.Validate(); err != nil {
		t.Fatalf("payload at limit must pass: %v", err)
	}
}

func TestValidateRejectsSecrets(t *testing.T) {
	payloads := [][]byte{
		[]byte(`{"api_key":"sk-abcdefghijklmnopqrstuvwxyz123456"}`),
		[]byte(`{"password":"supersecret12345"}`),
		[]byte(`Authorization: Bearer abcdefghijklmnop`),
		[]byte(`-----BEGIN RSA PRIVATE KEY-----`),
		[]byte(`{"token":"abcdef1234567890xyz"}`),
	}
	for _, p := range payloads {
		e := minimalEvent()
		e.Payload = p
		var ve *ValidationError
		if err := e.Validate(); !errors.As(err, &ve) || ve.Code != CodeSecretInPayload {
			t.Fatalf("payload %s not rejected: %v", p, err)
		}
	}
}

func TestKnownTypesCoverage(t *testing.T) {
	families := map[string][]EventType{
		"task":       {TaskStarted, TaskCompleted, TaskAborted, TaskFailed, TaskPaused, TaskResumed},
		"stage":      {StageEntered, StageExited, StageTransition, StageIllegalTransition},
		"acceptance": {AcceptanceStarted, AcceptancePassed, AcceptanceFailed, AcceptanceInvalid, AcceptanceRollbackOverridden},
		"retry":      {RetryScheduled, RetryExhausted},
		"checkpoint": {CheckpointSaved, CheckpointRestored, CheckpointDrift, CheckpointCorrupted},
		"compaction": {CompactionTriggered, CompactionCompleted, CompactionDegraded, CompactionRejected},
		"memory":     {MemoryWritten, MemoryMerged, MemoryPromoted, MemoryEvicted, MemoryDecayed, MemoryCorrupted, MemoryResync, MemoryIndexDegraded},
		"index":      {IndexQueued, IndexUpdated, IndexFailed, IndexRebuilt},
		"tool":       {ToolStarted, ToolResult, ToolProgress},
		"error":      {Error},
		"security":   {SecurityViolation},
	}
	for family, types := range families {
		for _, tp := range types {
			if !IsKnownType(tp) {
				t.Fatalf("%s type %q missing from dictionary", family, tp)
			}
		}
	}
	if len(KnownTypes()) != len(knownTypes) {
		t.Fatal("KnownTypes must return the full dictionary")
	}
}

func TestSeqAllocatorMonotonic(t *testing.T) {
	a := NewSeqAllocator(0)
	var last int64
	for i := 0; i < 100; i++ {
		next := a.Next()
		if next != last+1 {
			t.Fatalf("seq jumped from %d to %d", last, next)
		}
		last = next
	}
	if a.Last() != 100 {
		t.Fatalf("last = %d", a.Last())
	}
}

func TestSeqAllocatorRestoreRejectsRegression(t *testing.T) {
	a := NewSeqAllocator(5)
	a.Next()
	a.Next()
	if err := a.Restore(7); err != nil {
		t.Fatalf("forward restore: %v", err)
	}
	if err := a.Restore(3); !errors.Is(err, ErrSeqRegression) {
		t.Fatalf("regression must be rejected, got %v", err)
	}
	if a.Last() != 7 {
		t.Fatalf("last must stay at 7, got %d", a.Last())
	}
}

func TestSeqAllocatorPersistWatermark(t *testing.T) {
	a := NewSeqAllocator(0)
	a.SetPersistEvery(3)
	if a.NeedsPersist() {
		t.Fatal("fresh allocator must not need persisting")
	}
	a.Next()
	a.Next()
	a.Next()
	if !a.NeedsPersist() {
		t.Fatal("must need persisting after threshold")
	}
	a.PersistedTo(3)
	if a.NeedsPersist() || a.Persisted() != 3 {
		t.Fatalf("watermark not recorded: %d", a.Persisted())
	}
	a.PersistedTo(1)
	if a.Persisted() != 3 {
		t.Fatal("watermark must not move backwards")
	}
}

func TestSeqAllocatorConcurrent(t *testing.T) {
	a := NewSeqAllocator(0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make([]int64, 0, 200)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := a.Next()
			mu.Lock()
			seen = append(seen, n)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(seen, func(i, j int) bool { return seen[i] < seen[j] })
	for i, n := range seen {
		if n != int64(i+1) {
			t.Fatalf("seq not contiguous at %d: %v", i, seen[:10])
		}
	}
}

func TestIdempotencyDeduplicates(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryIdempotency(100)
	for i := 0; i < 3; i++ {
		if err := store.Mark(ctx, "evt-1"); err != nil {
			if !errors.Is(err, ErrDuplicateEvent) {
				t.Fatalf("unexpected: %v", err)
			}
		}
	}
	if store.Count() != 1 {
		t.Fatalf("count = %d, want 1", store.Count())
	}
	ok, err := store.Seen(ctx, "evt-1")
	if err != nil || !ok {
		t.Fatalf("seen = %v err=%v", ok, err)
	}
	if err := store.Mark(ctx, ""); err == nil {
		t.Fatal("empty event id must fail")
	}
}

func TestIdempotencyEvictsOldest(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryIdempotency(2)
	for _, id := range []string{"a", "b", "c"} {
		if err := store.Mark(ctx, id); err != nil {
			t.Fatalf("mark %s: %v", id, err)
		}
		time.Sleep(time.Millisecond)
	}
	if store.Count() != 2 {
		t.Fatalf("count = %d, want 2", store.Count())
	}
	if ok, _ := store.Seen(ctx, "a"); ok {
		t.Fatal("oldest event should have been evicted")
	}
}

func TestEventCloneIsDeep(t *testing.T) {
	e := minimalEvent()
	cp := e.Clone()
	cp.Payload[0] = 'X'
	if e.Payload[0] == 'X' {
		t.Fatal("clone must not share payload buffer")
	}
}
