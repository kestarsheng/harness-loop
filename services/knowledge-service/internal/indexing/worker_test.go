package indexing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-platform/services/knowledge-service/internal/repository"
)

type fakeStore struct {
	mu      sync.Mutex
	status  map[string]string
	reason  map[string]string
	retry   map[string]int
	history []string
	docs    map[string]*repository.Document
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		status: map[string]string{},
		reason: map[string]string{},
		retry:  map[string]int{},
		docs:   map[string]*repository.Document{},
	}
}

func (f *fakeStore) UpdateDocumentStatus(_ context.Context, docID, status, reason string, retryCount int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status[docID] != status {
		f.history = append(f.history, status)
	}
	f.status[docID] = status
	f.reason[docID] = reason
	f.retry[docID] = retryCount
	return nil
}

func (f *fakeStore) GetDocument(_ context.Context, id, tenantID string) (*repository.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, ok := f.docs[id]
	if !ok {
		return nil, errors.New("not found")
	}
	copied := *doc
	copied.Status = f.status[id]
	copied.RetryCount = f.retry[id]
	copied.FailureReason = f.reason[id]
	return &copied, nil
}

func (f *fakeStore) Status(docID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[docID]
}

func (f *fakeStore) Reason(docID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reason[docID]
}

func (f *fakeStore) RetryCount(docID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.retry[docID]
}

func (f *fakeStore) History() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.history...)
}

type fakeIndexer struct {
	mu       sync.Mutex
	failures int
	calls    int
	err      error
	block    chan struct{}
	started  chan struct{}
	once     sync.Once
}

func (f *fakeIndexer) Index(_ context.Context, job Job) error {
	f.mu.Lock()
	f.calls++
	failures := f.failures
	if f.failures > 0 {
		f.failures--
	}
	err := f.err
	block := f.block
	f.mu.Unlock()

	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if block != nil {
		<-block
	}
	if failures > 0 {
		if err == nil {
			err = errors.New("index boom")
		}
		return err
	}
	return nil
}

func (f *fakeIndexer) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.Concurrency = 1
	cfg.MaxRetries = 3
	cfg.BaseBackoff = time.Millisecond
	cfg.MaxBackoff = 2 * time.Millisecond
	return cfg
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", msg)
}

func TestIndexWorkerTransitionsToReady(t *testing.T) {
	store := newFakeStore()
	indexer := &fakeIndexer{}
	worker := New(testConfig(), store, indexer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Stop(context.Background())

	if err := worker.Enqueue(ctx, Job{DocumentID: "d1", TenantID: "t1", Filename: "a.md", Content: []byte("hello")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got := store.Status("d1"); got != repository.StatusPending {
		t.Fatalf("status right after upload = %s, want pending", got)
	}
	waitFor(t, 2*time.Second, func() bool { return store.Status("d1") == repository.StatusReady }, "ready")
	if indexer.Calls() != 1 {
		t.Fatalf("index calls = %d", indexer.Calls())
	}
	if got := store.History(); len(got) != 3 || got[0] != repository.StatusPending ||
		got[1] != repository.StatusProcessing || got[2] != repository.StatusReady {
		t.Fatalf("state flow = %v", got)
	}
	stats := worker.Stats()
	if stats.Ready != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestIndexWorkerRetriesThenFails(t *testing.T) {
	store := newFakeStore()
	indexer := &fakeIndexer{failures: 100}
	cfg := testConfig()
	var mu sync.Mutex
	var delays []time.Duration
	cfg.Sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		return nil
	}
	worker := New(cfg, store, indexer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Stop(context.Background())

	if err := worker.Enqueue(ctx, Job{DocumentID: "d2", TenantID: "t1", Filename: "b.md", Content: []byte("x")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return store.Status("d2") == repository.StatusFailed }, "failed")

	if indexer.Calls() != 3 {
		t.Fatalf("attempts = %d, want 3 (MaxRetries)", indexer.Calls())
	}
	if store.RetryCount("d2") != 3 {
		t.Fatalf("retry count = %d", store.RetryCount("d2"))
	}
	if store.Reason("d2") == "" {
		t.Fatal("failure reason must be retained")
	}
	history := store.History()
	if history[len(history)-1] != repository.StatusFailed {
		t.Fatalf("history = %v", history)
	}
	pendingCount := 0
	for _, status := range history {
		if status == repository.StatusPending {
			pendingCount++
		}
	}
	if pendingCount < 2 {
		t.Fatalf("retries must go back through pending: %v", history)
	}
	mu.Lock()
	got := append([]time.Duration(nil), delays...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("backoff sleeps = %d, want 2 (before attempt 2 and 3)", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("backoff must not shrink: %v", got)
		}
	}
	stats := worker.Stats()
	if stats.Failed != 1 || stats.Retried != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestIndexWorkerManualRetryAfterFailure(t *testing.T) {
	store := newFakeStore()
	store.docs["d3"] = &repository.Document{
		ID: "d3", Filename: "c.md", Content: "content", Status: repository.StatusFailed, TenantID: "t1",
	}
	indexer := &fakeIndexer{failures: 2}
	cfg := testConfig()
	cfg.MaxRetries = 2
	cfg.Sleep = func(context.Context, time.Duration) error { return nil }
	worker := New(cfg, store, indexer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Stop(context.Background())

	if err := worker.Enqueue(ctx, Job{DocumentID: "d3", TenantID: "t1", Filename: "c.md", Content: []byte("content")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return store.Status("d3") == repository.StatusFailed }, "failed")

	// 修复外部依赖后从列表重试：failed -> pending -> processing -> ready。
	if err := worker.Retry(ctx, "d3", "t1"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return store.Status("d3") == repository.StatusReady }, "ready after retry")

	if err := worker.Retry(ctx, "d3", "t1"); err == nil {
		t.Fatal("retry on ready document must be rejected")
	}
}

func TestIndexWorkerCancelDropsQueuedJob(t *testing.T) {
	store := newFakeStore()
	indexer := &fakeIndexer{block: make(chan struct{}), started: make(chan struct{})}
	worker := New(testConfig(), store, indexer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Stop(context.Background())

	if err := worker.Enqueue(ctx, Job{DocumentID: "d4", TenantID: "t1", Filename: "d.md"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	select {
	case <-indexer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never picked the first job")
	}
	if err := worker.Enqueue(ctx, Job{DocumentID: "d5", TenantID: "t1", Filename: "e.md"}); err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	if !worker.Cancel("d5") {
		t.Fatal("cancel must report the queued job")
	}
	close(indexer.block)
	waitFor(t, 2*time.Second, func() bool { return store.Status("d4") == repository.StatusReady }, "first job ready")

	time.Sleep(30 * time.Millisecond)
	if got := store.Status("d5"); got == repository.StatusReady {
		t.Fatalf("canceled job must not be indexed, status = %s", got)
	}
	if worker.Cancel("missing") {
		t.Fatal("cancel on unknown document must be false")
	}
}

func TestIndexWorkerRejectsEmptyDocumentID(t *testing.T) {
	worker := New(testConfig(), newFakeStore(), &fakeIndexer{})
	if err := worker.Enqueue(context.Background(), Job{}); err == nil {
		t.Fatal("empty document id must be rejected")
	}
}

func TestIndexWorkerBackoffSchedule(t *testing.T) {
	cfg := DefaultConfig()
	want := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, expected := range want {
		if got := cfg.Backoff(i + 1); got != expected {
			t.Fatalf("backoff(%d) = %v, want %v", i+1, got, expected)
		}
	}
}
