package event

import (
	"errors"
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

// SeqWatermark 只做「读取 / 回填」，没有 Next()——进程内一旦再出现分配器，
// 就是第二个写者，会与库分配的 seq 撞 (task_id, seq) 唯一键（T-P2-32）。
func TestSeqWatermarkObserveKeepsMax(t *testing.T) {
	w := NewSeqWatermark(0)
	for _, n := range []int64{3, 1, 7, 5} {
		w.Observe(n)
	}
	if w.Last() != 7 {
		t.Fatalf("watermark = %d, want 7 (must never move backwards)", w.Last())
	}
}

func TestSeqWatermarkRestoreRejectsRegression(t *testing.T) {
	w := NewSeqWatermark(5)
	w.Observe(7)
	if err := w.Restore(7); err != nil {
		t.Fatalf("forward restore: %v", err)
	}
	if err := w.Restore(3); !errors.Is(err, ErrSeqRegression) {
		t.Fatalf("regression must be rejected, got %v", err)
	}
	if w.Last() != 7 {
		t.Fatalf("last must stay at 7, got %d", w.Last())
	}
}

func TestSeqWatermarkPersistWatermark(t *testing.T) {
	w := NewSeqWatermark(0)
	w.SetPersistEvery(3)
	if w.NeedsPersist() {
		t.Fatal("fresh watermark must not need persisting")
	}
	w.Observe(1)
	w.Observe(2)
	w.Observe(3)
	if !w.NeedsPersist() {
		t.Fatal("must need persisting after threshold")
	}
	w.PersistedTo(3)
	if w.NeedsPersist() || w.Persisted() != 3 {
		t.Fatalf("watermark not recorded: %d", w.Persisted())
	}
	w.PersistedTo(1)
	if w.Persisted() != 3 {
		t.Fatal("watermark must not move backwards")
	}
}

func TestSeqWatermarkConcurrentObserve(t *testing.T) {
	w := NewSeqWatermark(0)
	var wg sync.WaitGroup
	for i := 1; i <= 200; i++ {
		wg.Add(1)
		go func(n int64) {
			defer wg.Done()
			w.Observe(n)
		}(int64(i))
	}
	wg.Wait()
	if w.Last() != 200 {
		t.Fatalf("last = %d, want 200", w.Last())
	}
}

// 发布前 seq 还没有分配（0 表示「待库分配」），所以 Publish 路径必须放行 0；
// 已落库事件仍然要求 seq > 0。
func TestValidateForPublishAllowsUnassignedSeq(t *testing.T) {
	e := minimalEvent()
	e.Seq = 0
	if err := e.ValidateForPublish(); err != nil {
		t.Fatalf("unassigned seq must pass publish validation: %v", err)
	}
	if err := e.Validate(); err == nil {
		t.Fatal("strict validation must still reject seq 0")
	}
	e.Seq = -1
	if err := e.ValidateForPublish(); err == nil {
		t.Fatal("negative seq must be rejected everywhere")
	}
}
