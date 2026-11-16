// Package indexing 提供知识文档的异步索引 worker。
//
// 状态机（design 4.10.2）：
//
//	pending --(worker pick)--> processing --(success)--> ready
//	                              |                        |
//	                              +--(error, retry<n)------+
//	                              +--(error, retry>=n)--> failed --(manual retry)--> pending
//
// 只有 ready 文档可检索；failed 文档保留失败原因并可从列表重试。
package indexing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"agent-platform/services/knowledge-service/internal/repository"
)

var ErrWorkerStopped = errors.New("indexing: worker is stopped")

// Job 是一次索引任务。RetryCount 由 worker 维护，调用方不需要设置。
type Job struct {
	DocumentID   string
	TenantID     string
	KBID         string
	Filename     string
	Content      []byte
	ChunkSize    int
	ChunkOverlap int
	RetryCount   int
}

// Store 是 worker 需要的状态读写能力，由 repository.DocumentRepository 实现。
type Store interface {
	UpdateDocumentStatus(ctx context.Context, docID, status, reason string, retryCount int) error
	GetDocument(ctx context.Context, id, tenantID string) (*repository.Document, error)
}

// Indexer 执行真正的解析/切分/向量化/落库，不负责状态流转。
type Indexer interface {
	Index(ctx context.Context, job Job) error
}

// Config 是 worker 的可配参数；零值会被补成默认值。
type Config struct {
	Concurrency int
	MaxRetries  int
	QueueSize   int
	// BaseBackoff / MaxBackoff 定义指数退避：base*5^(n-1) 且不超过 max，
	// 默认 1m / 5m / 15m 三档，与 design 4.10.2 一致。
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// Sleep 允许测试注入等待实现，默认按 ctx 等待。
	Sleep func(ctx context.Context, d time.Duration) error
}

func DefaultConfig() Config {
	return Config{
		Concurrency: 2,
		MaxRetries:  3,
		QueueSize:   256,
		BaseBackoff: time.Minute,
		MaxBackoff:  15 * time.Minute,
	}
}

func (c Config) normalized() Config {
	if c.Concurrency <= 0 {
		c.Concurrency = 2
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if c.QueueSize <= 0 {
		c.QueueSize = 256
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = time.Minute
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 15 * time.Minute
	}
	if c.MaxBackoff < c.BaseBackoff {
		c.MaxBackoff = c.BaseBackoff
	}
	if c.Sleep == nil {
		c.Sleep = SleepWithContext
	}
	return c
}

// Backoff 返回第 retry 次失败后的等待时长（retry 从 1 开始）。
func (c Config) Backoff(retry int) time.Duration {
	if retry <= 0 {
		retry = 1
	}
	delay := c.BaseBackoff
	for i := 1; i < retry; i++ {
		delay *= 5
		if delay >= c.MaxBackoff {
			return c.MaxBackoff
		}
	}
	if delay > c.MaxBackoff {
		delay = c.MaxBackoff
	}
	return delay
}

// SleepWithContext 是默认等待实现，worker 停止时立即返回。
func SleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats 是 worker 的计数快照，用于 /metrics 与运维排查。
type Stats struct {
	Enqueued   int64
	Processing int64
	Ready      int64
	Failed     int64
	Retried    int64
	Canceled   int64
}

type Worker struct {
	cfg     Config
	store   Store
	indexer Indexer

	queue  chan Job
	mu     sync.Mutex
	queued map[string]Job    // docID -> 尚未被 worker 取走的任务
	state  map[string]string // docID -> 最近一次状态
	stats  Stats

	wg        sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	stopped   bool
}

func New(cfg Config, store Store, indexer Indexer) *Worker {
	cfg = cfg.normalized()
	return &Worker{
		cfg:     cfg,
		store:   store,
		indexer: indexer,
		queue:   make(chan Job, cfg.QueueSize),
		queued:  map[string]Job{},
		state:   map[string]string{},
	}
}

func (w *Worker) Config() Config { return w.cfg }

// Start 拉起 Concurrency 个消费协程，重复调用无副作用。
func (w *Worker) Start(ctx context.Context) {
	w.startOnce.Do(func() {
		runCtx, cancel := context.WithCancel(ctx)
		w.cancel = cancel
		for i := 0; i < w.cfg.Concurrency; i++ {
			w.wg.Add(1)
			go w.loop(runCtx)
		}
	})
}

// Stop 停止消费并等待在途任务结束。
func (w *Worker) Stop(ctx context.Context) {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopped = true
		w.mu.Unlock()
		if w.cancel != nil {
			w.cancel()
		}
	})
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Enqueue 把任务放入队列，同时把文档置为 pending（幂等）。
func (w *Worker) Enqueue(ctx context.Context, job Job) error {
	if job.DocumentID == "" {
		return fmt.Errorf("indexing: document_id is required")
	}
	if err := w.setStatus(ctx, job.DocumentID, repository.StatusPending, "", job.RetryCount); err != nil {
		return err
	}
	return w.push(ctx, job)
}

func (w *Worker) push(ctx context.Context, job Job) error {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return ErrWorkerStopped
	}
	w.queued[job.DocumentID] = job
	w.state[job.DocumentID] = repository.StatusPending
	w.stats.Enqueued++
	w.mu.Unlock()

	select {
	case w.queue <- job:
		return nil
	case <-ctx.Done():
		w.forget(job.DocumentID)
		return ctx.Err()
	}
}

// Retry 把失败文档重新置为 pending 并入队（列表页「重试」按钮）。
func (w *Worker) Retry(ctx context.Context, docID, tenantID string) error {
	if w.store == nil {
		return fmt.Errorf("indexing: store is not configured")
	}
	doc, err := w.store.GetDocument(ctx, docID, tenantID)
	if err != nil {
		return err
	}
	if doc == nil {
		return fmt.Errorf("indexing: document %s not found", docID)
	}
	if !repository.Retryable(doc.Status) {
		return fmt.Errorf("indexing: document %s is %s, not retryable", docID, doc.Status)
	}
	return w.Enqueue(ctx, Job{
		DocumentID: doc.ID,
		TenantID:   tenantID,
		KBID:       doc.KBID,
		Filename:   doc.Filename,
		Content:    []byte(doc.Content),
	})
}

// Cancel 丢弃尚未被取走的任务（文档更新/删除时调用）。
func (w *Worker) Cancel(docID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.queued[docID]; !ok {
		return false
	}
	delete(w.queued, docID)
	w.stats.Canceled++
	return true
}

func (w *Worker) State(docID string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state[docID]
}

func (w *Worker) Stats() Stats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

func (w *Worker) loop(ctx context.Context) {
	defer w.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-w.queue:
			w.process(ctx, job)
		}
	}
}

func (w *Worker) process(ctx context.Context, job Job) {
	if !w.take(job.DocumentID) {
		return // 已被取消
	}
	w.mu.Lock()
	w.state[job.DocumentID] = repository.StatusProcessing
	w.stats.Processing++
	w.mu.Unlock()

	if err := w.setStatus(ctx, job.DocumentID, repository.StatusProcessing, "", job.RetryCount); err != nil {
		w.fail(ctx, job, err)
		return
	}

	var indexErr error
	if w.indexer == nil {
		indexErr = fmt.Errorf("indexing: indexer is not configured")
	} else {
		indexErr = w.indexer.Index(ctx, job)
	}
	if indexErr == nil {
		if err := w.setStatus(ctx, job.DocumentID, repository.StatusReady, "", 0); err != nil {
			// 索引已成功但状态写不进去：交给重试逻辑，避免文档卡在 processing。
			w.fail(ctx, job, err)
			return
		}
		w.mu.Lock()
		w.state[job.DocumentID] = repository.StatusReady
		w.stats.Ready++
		w.mu.Unlock()
		return
	}
	w.fail(ctx, job, indexErr)
}

// fail 按重试次数决定回到 pending（退避重排）还是落到 failed。
func (w *Worker) fail(ctx context.Context, job Job, cause error) {
	retry := job.RetryCount + 1
	reason := cause.Error()
	if retry >= w.cfg.MaxRetries {
		_ = w.setStatus(ctx, job.DocumentID, repository.StatusFailed, reason, retry)
		w.mu.Lock()
		w.state[job.DocumentID] = repository.StatusFailed
		w.stats.Failed++
		w.mu.Unlock()
		return
	}

	if err := w.setStatus(ctx, job.DocumentID, repository.StatusPending, reason, retry); err != nil {
		// 状态都写不进去，只能落到 failed，避免静默丢任务。
		_ = w.setStatus(ctx, job.DocumentID, repository.StatusFailed, err.Error(), retry)
		w.mu.Lock()
		w.state[job.DocumentID] = repository.StatusFailed
		w.stats.Failed++
		w.mu.Unlock()
		return
	}
	w.mu.Lock()
	w.state[job.DocumentID] = repository.StatusPending
	w.stats.Retried++
	w.mu.Unlock()

	next := job
	next.RetryCount = retry
	delay := w.cfg.Backoff(retry)
	go func() {
		if err := w.cfg.Sleep(ctx, delay); err != nil {
			return
		}
		_ = w.push(ctx, next)
	}()
}

func (w *Worker) setStatus(ctx context.Context, docID, status, reason string, retry int) error {
	if w.store == nil {
		return fmt.Errorf("indexing: store is not configured")
	}
	if err := w.store.UpdateDocumentStatus(ctx, docID, status, reason, retry); err != nil {
		return fmt.Errorf("indexing: update status %s: %w", status, err)
	}
	return nil
}

// take 取出任务所有权；返回 false 表示该任务已被 Cancel。
func (w *Worker) take(docID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.queued[docID]; !ok {
		return false
	}
	delete(w.queued, docID)
	return true
}

func (w *Worker) forget(docID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.queued, docID)
}
