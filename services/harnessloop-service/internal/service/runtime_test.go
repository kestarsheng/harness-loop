package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/acceptance"
	"agent-platform/pkg/harnessloop/checkpoint"
	"agent-platform/pkg/harnessloop/compaction"
	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/fsm"
	"agent-platform/pkg/harnessloop/retry"
	"agent-platform/pkg/memoryfile"
	"agent-platform/services/harnessloop-service/internal/repository"
)

type memTasks struct {
	mu        sync.Mutex
	task      *repository.TaskRun
	transfers []fsm.Stage
	statuses  []repository.TaskStatus
}

func newMemTasks() *memTasks {
	return &memTasks{task: &repository.TaskRun{
		TaskID:         "task-1",
		TenantID:       "tenant-a",
		ProjectID:      "proj-a",
		CurrentStage:   fsm.StageRequirement,
		Status:         repository.TaskRunning,
		AttemptNo:      map[string]int{},
		RetryCount:     map[string]int{},
		AcceptanceFail: map[string]int{},
		UpdatedAt:      time.Unix(1700000000, 0),
	}}
}

func (m *memTasks) Get(ctx context.Context, tenantID, taskID string) (*repository.TaskRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if taskID != m.task.TaskID || tenantID != m.task.TenantID {
		return nil, repository.ErrTaskNotFound
	}
	cp := *m.task
	return &cp, nil
}

func (m *memTasks) Transfer(ctx context.Context, tenantID, taskID string, to fsm.Stage, expectedVersion int64, events []repository.EventInsert) (*repository.TaskRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.task.Version != expectedVersion {
		return nil, repository.ErrVersionConflict
	}
	m.task.CurrentStage = to
	m.task.Version++
	m.transfers = append(m.transfers, to)
	cp := *m.task
	return &cp, nil
}

func (m *memTasks) UpdateStatus(ctx context.Context, tenantID, taskID string, status repository.TaskStatus, reason string, expectedVersion int64) (*repository.TaskRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.task.Version != expectedVersion {
		return nil, repository.ErrVersionConflict
	}
	m.task.Status = status
	m.task.TerminalReason = reason
	m.task.Version++
	m.task.UpdatedAt = time.Unix(1700000100, 0)
	m.statuses = append(m.statuses, status)
	cp := *m.task
	return &cp, nil
}

func (m *memTasks) UpdateAttemptNo(ctx context.Context, tenantID, taskID string, stage fsm.Stage, no int, expectedVersion int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.task.Version != expectedVersion {
		return repository.ErrVersionConflict
	}
	m.task.AttemptNo[string(stage)] = no
	m.task.Version++
	return nil
}

type memAttempts struct {
	mu       sync.Mutex
	next     map[string]int
	created  []fsm.Attempt
	finished map[string]bool
}

func newMemAttempts() *memAttempts {
	return &memAttempts{next: map[string]int{}, finished: map[string]bool{}}
}

func (m *memAttempts) NextAttemptNo(ctx context.Context, taskID string, stage fsm.Stage) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := taskID + "/" + string(stage)
	m.next[key]++
	return m.next[key], nil
}

func (m *memAttempts) Create(ctx context.Context, a fsm.Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created = append(m.created, a)
	return nil
}

func (m *memAttempts) Finish(ctx context.Context, attemptID string, endedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finished[attemptID] = true
	return nil
}

func (m *memAttempts) AttachAcceptance(ctx context.Context, attemptID, acceptanceID string) error {
	return nil
}

type stubStages struct {
	mu        sync.Mutex
	calls     int
	lastInput StageRunInput
	inputs    []StageRunInput
	fn        func(call int, ctx context.Context) (*StageRunResult, error)
}

func (s *stubStages) Run(ctx context.Context, in StageRunInput) (*StageRunResult, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.lastInput = in
	s.inputs = append(s.inputs, in)
	s.mu.Unlock()
	if s.fn != nil {
		return s.fn(call, ctx)
	}
	return &StageRunResult{FinishReceived: true, Decision: retry.FinishDecision{Action: retry.ActionDone}}, nil
}

type captureEvents struct {
	mu     sync.Mutex
	events []*event.Event
}

func (c *captureEvents) Publish(ctx context.Context, e *event.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

func (c *captureEvents) count(typ event.EventType) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e.EventType == typ {
			n++
		}
	}
	return n
}

type passAcceptor struct{}

func (p *passAcceptor) Evaluate(ctx context.Context, in AcceptanceInput) (acceptance.Verdict, error) {
	return acceptance.Verdict{
		AcceptanceID:  "acc-" + in.Stage,
		Verdict:       acceptance.VerdictPass,
		Reason:        "program checks passed",
		Evidence:      []acceptance.Evidence{{Kind: "test", Ref: "go test ./..."}},
		VerifiedBy:    acceptance.VerifiedByProgram,
		PromptVersion: acceptance.PromptVersion,
		InputDigest:   "digest",
	}, nil
}

func runtimeInput() RunInput {
	return RunInput{
		TenantID:         "tenant-a",
		ProjectID:        "proj-a",
		TaskID:           "task-1",
		SessionID:        "sess-1",
		EntryAgent:       "exec-agent",
		PolicySnapshot:   []byte(`{}`),
		PolicyHash:       "sha256:policy",
		StageModel:       "qwen-max",
		ModelWindow:      32768,
		RequiresToolCall: true,
		MaxSteps:         20,
	}
}

func newTestRuntime(stages *stubStages) (*Runtime, *memTasks, *captureEvents, *memAttempts) {
	tasks := newMemTasks()
	attempts := newMemAttempts()
	events := &captureEvents{}
	rt := NewRuntime(tasks, attempts, stages)
	rt.Events = events
	rt.Acceptor = &passAcceptor{}
	rt.Now = func() time.Time { return time.Unix(1700000000, 0) }
	return rt, tasks, events, attempts
}

func TestRuntimeCompleted(t *testing.T) {
	rt, tasks, events, attempts := newTestRuntime(&stubStages{})
	res, err := rt.Run(context.Background(), runtimeInput())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TerminalReason != ReasonCompleted {
		t.Fatalf("reason = %s", res.TerminalReason)
	}
	if res.FinalStage != string(fsm.StageDone) {
		t.Fatalf("final stage = %s", res.FinalStage)
	}
	if res.Steps != 4 {
		t.Fatalf("steps = %d, want 4 (a,b,c,d)", res.Steps)
	}
	tasks.mu.Lock()
	if tasks.task.Status != repository.TaskCompleted {
		t.Fatalf("status = %s", tasks.task.Status)
	}
	if len(tasks.transfers) != 4 {
		t.Fatalf("transfers = %v", tasks.transfers)
	}
	tasks.mu.Unlock()

	if len(attempts.created) != 4 {
		t.Fatalf("attempts = %d", len(attempts.created))
	}
	if events.count(event.TaskCompleted) != 1 {
		t.Fatalf("task.completed events = %d", events.count(event.TaskCompleted))
	}
}

func TestRuntimeFailed(t *testing.T) {
	stages := &stubStages{fn: func(call int, ctx context.Context) (*StageRunResult, error) {
		cls := retry.Classification{Category: retry.Network, Retryable: true, Reason: "connection reset"}
		return &StageRunResult{Classification: &cls, Decision: retry.FinishDecision{Action: retry.ActionRetry, Category: retry.Network}}, errors.New("connection reset")
	}}
	rt, tasks, events, _ := newTestRuntime(stages)

	res, err := rt.Run(context.Background(), runtimeInput())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TerminalReason != ReasonFailed || res.Category != retry.Network {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.FailedStage != "a" {
		t.Fatalf("failed stage = %s", res.FailedStage)
	}
	if res.RetryCount != 4 {
		t.Fatalf("retry count = %d", res.RetryCount)
	}
	tasks.mu.Lock()
	if tasks.task.Status != repository.TaskFailed {
		t.Fatalf("status = %s", tasks.task.Status)
	}
	tasks.mu.Unlock()
	if events.count(event.TaskFailed) != 1 {
		t.Fatalf("task.failed events = %d", events.count(event.TaskFailed))
	}
	if events.count(event.Error) != 4 {
		t.Fatalf("error events = %d", events.count(event.Error))
	}
}

func TestRuntimeEmptyOutputDoesNotComplete(t *testing.T) {
	stages := &stubStages{fn: func(call int, ctx context.Context) (*StageRunResult, error) {
		cls := retry.Classification{Category: retry.NoFinish, Retryable: true, Reason: "empty response"}
		return &StageRunResult{Decision: retry.FinishDecision{Action: retry.ActionRetry, Category: retry.NoFinish}, Classification: &cls}, nil
	}}
	rt, tasks, _, _ := newTestRuntime(stages)

	res, err := rt.Run(context.Background(), runtimeInput())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TerminalReason != ReasonFailed || res.Category != retry.NoFinish {
		t.Fatalf("empty output must not complete: %+v", res)
	}
	if res.Steps != 3 {
		t.Fatalf("no_finish retries = %d steps, want 3", res.Steps)
	}
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	if tasks.task.CurrentStage != fsm.StageRequirement {
		t.Fatalf("stage advanced on empty output: %s", tasks.task.CurrentStage)
	}
	if len(tasks.transfers) != 0 {
		t.Fatalf("transfers = %v", tasks.transfers)
	}
}

func TestRuntimeAbortedMidRun(t *testing.T) {
	var rt *Runtime
	var events *captureEvents
	stages := &stubStages{fn: func(call int, ctx context.Context) (*StageRunResult, error) {
		if call == 1 {
			if _, err := rt.Abort(ctx, "tenant-a", "task-1", 1); err != nil {
				t.Errorf("abort: %v", err)
			}
			cls := retry.Classification{Category: retry.NoFinish, Retryable: true, Reason: "interrupted"}
			return &StageRunResult{Classification: &cls, Decision: retry.FinishDecision{Action: retry.ActionRetry, Category: retry.NoFinish}}, errors.New("interrupted")
		}
		return &StageRunResult{FinishReceived: true, Decision: retry.FinishDecision{Action: retry.ActionDone}}, nil
	}}
	rt, _, events, _ = newTestRuntime(stages)

	res, err := rt.Run(context.Background(), runtimeInput())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TerminalReason != ReasonAborted {
		t.Fatalf("reason = %s", res.TerminalReason)
	}
	if events.count(event.TaskAborted) != 1 {
		t.Fatalf("task.aborted events = %d", events.count(event.TaskAborted))
	}
}

func TestRuntimeAbortIdempotent(t *testing.T) {
	rt, tasks, events, _ := newTestRuntime(&stubStages{})

	first, err := rt.Abort(context.Background(), "tenant-a", "task-1", 7)
	if err != nil {
		t.Fatalf("first abort: %v", err)
	}
	if first.Duplicate {
		t.Fatal("first abort must not be duplicate")
	}
	for i := 0; i < 2; i++ {
		again, err := rt.Abort(context.Background(), "tenant-a", "task-1", 7)
		if err != nil {
			t.Fatalf("repeat abort: %v", err)
		}
		if !again.Duplicate {
			t.Fatal("repeat abort must be marked duplicate")
		}
		if again.Timestamp != first.Timestamp {
			t.Fatalf("timestamp changed: %v vs %v", again.Timestamp, first.Timestamp)
		}
		if again.TerminalReason != first.TerminalReason {
			t.Fatalf("reason changed: %s vs %s", again.TerminalReason, first.TerminalReason)
		}
	}
	if events.count(event.TaskAborted) != 1 {
		t.Fatalf("task.aborted events = %d, want 1", events.count(event.TaskAborted))
	}
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	if tasks.task.Status != repository.TaskAborted {
		t.Fatalf("status = %s", tasks.task.Status)
	}
	if len(tasks.statuses) != 1 {
		t.Fatalf("status writes = %v", tasks.statuses)
	}
}

func TestRuntimeRejectsNonRunningTask(t *testing.T) {
	rt, tasks, _, _ := newTestRuntime(&stubStages{})
	tasks.mu.Lock()
	tasks.task.Status = repository.TaskPaused
	tasks.mu.Unlock()
	if _, err := rt.Run(context.Background(), runtimeInput()); !errors.Is(err, ErrRuntimeState) {
		t.Fatalf("paused task must be rejected: %v", err)
	}
}

func TestRuntimeInputValidation(t *testing.T) {
	rt, _, _, _ := newTestRuntime(&stubStages{})
	cases := []func(*RunInput){
		func(in *RunInput) { in.TenantID = "" },
		func(in *RunInput) { in.TaskID = "" },
		func(in *RunInput) { in.EntryAgent = "" },
		func(in *RunInput) { in.PolicyHash = "" },
		func(in *RunInput) { in.StageModel = "" },
		func(in *RunInput) { in.ModelWindow = 0 },
	}
	for _, mut := range cases {
		in := runtimeInput()
		mut(&in)
		if _, err := rt.Run(context.Background(), in); !errors.Is(err, ErrRuntimeInput) {
			t.Fatalf("invalid input must be rejected: %+v -> %v", in, err)
		}
	}
	if _, err := NewRuntime(nil, nil, nil).Run(context.Background(), runtimeInput()); !errors.Is(err, ErrRuntimeInput) {
		t.Fatalf("nil dependencies must be rejected: %v", err)
	}
}

func TestRuntimeSavesCheckpoints(t *testing.T) {
	rt, _, _, _ := newTestRuntime(&stubStages{})
	saver := &captureCheckpoints{}
	rt.Checkpoints = saver
	if _, err := rt.Run(context.Background(), runtimeInput()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(saver.saved) != 4 {
		t.Fatalf("checkpoints = %d, want 4 stage entries", len(saver.saved))
	}
	for _, cp := range saver.saved {
		if cp.Kind != checkpoint.KindStageEntry {
			t.Fatalf("kind = %s", cp.Kind)
		}
		if cp.Checksum == "" {
			t.Fatal("checkpoint must be sealed")
		}
	}
}

type captureCheckpoints struct {
	mu    sync.Mutex
	saved []*checkpoint.Checkpoint
}

func (c *captureCheckpoints) Save(ctx context.Context, cp *checkpoint.Checkpoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saved = append(c.saved, cp)
	return nil
}

func TestRuntimeMaxStepsFailsAsSystem(t *testing.T) {
	stages := &stubStages{fn: func(call int, ctx context.Context) (*StageRunResult, error) {
		cls := retry.Classification{Category: retry.Runtime, Retryable: true, Reason: "keeps failing"}
		return &StageRunResult{Classification: &cls, Decision: retry.FinishDecision{Action: retry.ActionRetry, Category: retry.Runtime}}, errors.New("keeps failing")
	}}
	rt, _, _, _ := newTestRuntime(stages)
	in := runtimeInput()
	in.MaxSteps = 2
	res, err := rt.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TerminalReason != ReasonFailed {
		t.Fatalf("max steps must fail, got %+v", res)
	}
	if res.LastError != "max steps reached" {
		t.Fatalf("last error = %q", res.LastError)
	}
}

func TestRuntimeRewindsWithFailedAcceptance(t *testing.T) {
	var rt *Runtime
	var tasks *memTasks
	acceptCalls := 0
	failAcceptor := &countingAcceptor{fn: func(in AcceptanceInput) (acceptance.Verdict, error) {
		acceptCalls++
		if acceptCalls == 1 {
			return acceptance.Verdict{
				AcceptanceID: "acc-1", Verdict: acceptance.VerdictFail, Reason: "tests failed",
				Evidence:       []acceptance.Evidence{{Kind: "test", Ref: "go test ./..."}},
				FailedCriteria: []acceptance.FailedCriterion{{CriterionID: "c-1", Category: "impl_gap"}},
				RollbackTarget: "a",
				VerifiedBy:     acceptance.VerifiedByProgram, PromptVersion: acceptance.PromptVersion, InputDigest: "d",
			}, nil
		}
		return acceptance.Verdict{
			AcceptanceID: "acc-2", Verdict: acceptance.VerdictPass, Reason: "ok",
			Evidence:   []acceptance.Evidence{{Kind: "test", Ref: "go test ./..."}},
			VerifiedBy: acceptance.VerifiedByProgram, PromptVersion: acceptance.PromptVersion, InputDigest: "d",
		}, nil
	}}
	var attempts *memAttempts
	rt, tasks, _, attempts = newTestRuntime(&stubStages{})
	rt.Acceptor = failAcceptor

	res, err := rt.Run(context.Background(), runtimeInput())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TerminalReason != ReasonCompleted {
		t.Fatalf("acceptance failure must rewind and then complete: %+v", res)
	}
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	if len(tasks.transfers) == 0 {
		t.Fatal("expected transfers")
	}
	if tasks.transfers[len(tasks.transfers)-1] != fsm.StageDone {
		t.Fatalf("must complete after rewind, transfers=%v", tasks.transfers)
	}
	if acceptCalls < 2 {
		t.Fatalf("acceptance calls = %d", acceptCalls)
	}
	attempts.mu.Lock()
	defer attempts.mu.Unlock()
	var sawAcceptanceFailed bool
	for _, a := range attempts.created {
		if a.Stage == fsm.StageRequirement && a.EntryReason == fsm.EntryAcceptanceFailed {
			sawAcceptanceFailed = true
		}
	}
	if !sawAcceptanceFailed {
		t.Fatalf("rewound attempt must record acceptance_failed: %+v", attempts.created)
	}
}

type countingAcceptor struct {
	fn func(in AcceptanceInput) (acceptance.Verdict, error)
}

func (c *countingAcceptor) Evaluate(ctx context.Context, in AcceptanceInput) (acceptance.Verdict, error) {
	if c.fn != nil {
		return c.fn(in)
	}
	return acceptance.Verdict{Verdict: acceptance.VerdictPass}, nil
}

func TestRuntimeAcceptanceBeyondLimitFails(t *testing.T) {
	rt, _, _, _ := newTestRuntime(&stubStages{})
	n := 0
	rt.Acceptor = &countingAcceptor{fn: func(in AcceptanceInput) (acceptance.Verdict, error) {
		n++
		return acceptance.Verdict{
			AcceptanceID: fmt.Sprintf("acc-%d", n), Verdict: acceptance.VerdictFail, Reason: "always fails",
			Evidence:       []acceptance.Evidence{{Kind: "test", Ref: "go test ./..."}},
			FailedCriteria: []acceptance.FailedCriterion{{CriterionID: "c-1", Category: "impl_gap"}},
			RollbackTarget: "a",
			VerifiedBy:     acceptance.VerifiedByProgram, PromptVersion: acceptance.PromptVersion, InputDigest: "d",
		}, nil
	}}
	res, err := rt.Run(context.Background(), runtimeInput())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TerminalReason != ReasonFailed {
		t.Fatalf("acceptance limit must fail: %+v", res)
	}
	if n < 5 {
		t.Fatalf("expected at least 5 acceptance failures, got %d", n)
	}
}

func TestMemoryInjectRuntimePrefix(t *testing.T) {
	stages := &stubStages{}
	rt, _, _, _ := newTestRuntime(stages)
	recaller := &stubRecaller{hits: []MemoryHit{hit("constraint", "long_term_constraint", "禁止使用 mysql", 0.9, true)}}
	rt.Injector = NewMemoryInjector(recaller, nil)
	rt.HarnessHome = t.TempDir()

	if _, err := rt.Run(context.Background(), runtimeInput()); err != nil {
		t.Fatalf("run: %v", err)
	}
	stages.mu.Lock()
	inputs := append([]StageRunInput(nil), stages.inputs...)
	stages.mu.Unlock()
	var injected string
	for _, input := range inputs {
		if strings.Contains(input.Message, "[injected memory]") {
			injected = input.Message
			break
		}
	}
	if injected == "" {
		t.Fatalf("runtime must prefix injected memory in at least one stage: %+v", inputs)
	}
	for _, meta := range []string{"topic=constraint", "file=project.md", "updated=", "score="} {
		if !strings.Contains(injected, meta) {
			t.Fatalf("injected metadata %q missing: %q", meta, injected)
		}
	}
	if !strings.Contains(injected, "禁止使用 mysql") {
		t.Fatalf("injected body missing: %q", injected)
	}
}

func TestMemoryInjectRuntimeDegradesWithoutRecaller(t *testing.T) {
	workspace := t.TempDir()
	projectDir := filepath.Join(workspace, ".harness", "memory")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := &memoryfile.Document{
		SchemaVersion: memoryfile.SchemaVersion, Scope: memoryfile.ScopeProject,
		UpdatedAt: time.Now().UTC(), OrderVersion: 1,
		Order: []string{"fact"},
		Topics: []memoryfile.Topic{{Key: "fact", Title: "事实", Body: "项目使用 PostgreSQL 存储",
			Meta: memoryfile.Meta{Category: memoryfile.CategoryProjectFact, UpdatedAt: time.Now().UTC()}}},
	}
	raw, err := doc.Render()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "project.md"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	stages := &stubStages{}
	rt, _, events, _ := newTestRuntime(stages)
	rt.Injector = NewMemoryInjector(nil, nil).WithEvents(events)
	rt.HarnessHome = t.TempDir()

	input := runtimeInput()
	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("run: %v", err)
	}
	if events.count(event.MemoryIndexDegraded) == 0 {
		t.Fatal("degrade event must be emitted when recaller is missing")
	}
}

type stubSummarizer struct {
	err error
}

func (s *stubSummarizer) Summarize(ctx context.Context, h compaction.History, tmpl compaction.TemplateVersion) (compaction.Summary, error) {
	if s.err != nil {
		return compaction.Summary{}, s.err
	}
	return compaction.Summary{
		Goal: h.Goal, Done: "done", Todo: "todo",
		Constraints: "constraint", Background: "background",
	}, nil
}

func compactionRuntime(t *testing.T, summarizer compaction.Summarizer) (*Runtime, *stubStages, *captureEvents, *captureCheckpoints) {
	t.Helper()
	stages := &stubStages{}
	rt, _, events, _ := newTestRuntime(stages)
	checkpoints := &captureCheckpoints{}
	rt.Checkpoints = checkpoints
	rt.CompactionTrigger = compaction.NewTrigger(compaction.Config{
		ThresholdRatio: 0.8, FallbackWindow: 100, SafetyMargin: 0,
	}, nil)
	rt.CompactionSummarizer = summarizer
	rt.CompactionLimits = compaction.DefaultDegradeLimits()
	rt.CompactionDir = filepath.Join(t.TempDir(), "tmp")
	rt.Injector = NewMemoryInjector(&stubRecaller{hits: []MemoryHit{
		hit("long", "project_fact", strings.Repeat("长上下文内容", 300), 1.0, false),
	}}, nil).WithEvents(events)
	rt.HarnessHome = t.TempDir()
	return rt, stages, events, checkpoints
}

func TestRuntimeCompactionCompleted(t *testing.T) {
	rt, stages, events, checkpoints := compactionRuntime(t, &stubSummarizer{})
	input := runtimeInput()
	input.ModelWindow = 1000

	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("run: %v", err)
	}

	stages.mu.Lock()
	inputs := append([]StageRunInput(nil), stages.inputs...)
	stages.mu.Unlock()
	found := false
	for _, stageInput := range inputs {
		if strings.Contains(stageInput.Message, "[compacted context]") {
			found = true
		}
	}
	if !found {
		t.Fatalf("at least one stage must receive compacted context: %+v", inputs)
	}

	checkpoints.mu.Lock()
	kinds := map[string]bool{}
	for _, cp := range checkpoints.saved {
		kinds[cp.Kind] = true
	}
	checkpoints.mu.Unlock()
	if !kinds[checkpoint.KindPreCompaction] {
		t.Fatalf("pre-compaction checkpoint missing: %v", kinds)
	}
	if events.count(event.CompactionTriggered) == 0 || events.count(event.CompactionCompleted) == 0 {
		t.Fatalf("compaction events missing: triggered=%d completed=%d",
			events.count(event.CompactionTriggered), events.count(event.CompactionCompleted))
	}
}

func TestRuntimeCompactionDegradesOnSummarizerFailure(t *testing.T) {
	rt, stages, events, _ := compactionRuntime(t, &stubSummarizer{err: errors.New("llm timeout")})
	input := runtimeInput()
	input.ModelWindow = 1000

	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("degradation must not block the run: %v", err)
	}

	stages.mu.Lock()
	inputs := append([]StageRunInput(nil), stages.inputs...)
	stages.mu.Unlock()
	hasPlaceholder := false
	for _, stageInput := range inputs {
		if strings.Contains(stageInput.Message, "[compacted context]") && strings.Contains(stageInput.Message, "goal:") {
			hasPlaceholder = true
		}
	}
	if !hasPlaceholder {
		t.Fatalf("degraded compaction must keep readable five-element placeholder: %+v", inputs)
	}
	if events.count(event.CompactionDegraded) == 0 {
		t.Fatal("compaction.degraded event missing")
	}
}

func TestRuntimeCompactionSkippedWhenNoTrigger(t *testing.T) {
	stages := &stubStages{}
	rt, _, events, _ := newTestRuntime(stages)
	input := runtimeInput()
	input.ModelWindow = 1000
	if _, err := rt.Run(context.Background(), input); err != nil {
		t.Fatalf("run: %v", err)
	}
	if events.count(event.CompactionTriggered) != 0 {
		t.Fatal("no trigger must not emit compaction events")
	}
}
