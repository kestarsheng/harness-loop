package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
)

// ErrSeqConflict 表示落库端在多轮重试后仍然撞上 (task_id, seq) 唯一键。
// 它只会来自「另一条写路径抢先写了同一个 seq」——正常路径下 seq 由事件库
// 单一分配，不会出现。
var ErrSeqConflict = errors.New("event seq conflict")

// Sink 是事件落库端。seq 的分配权属于实现方（见 repository.EventRepo.Append）：
// 实现方应在写入事务内分配 seq 并把它回填到入参 Event 上，发布器只读取回填值
// 维护水位镜像。实现方若静默丢掉冲突事件，事件就没了——所以冲突必须返回错误，
// 由发布器缓冲并上报。
type Sink interface {
	Append(ctx context.Context, events []*Event) error
}

// Observer 是发布失败的观测钩子（指标面）。服务装配时把它接到 harness 指标上，
// 保证「失败必有计数」；日志由发布器自己打，不依赖 Observer 是否存在。
type Observer interface {
	// EventPublishFailed 在一条事件彻底投递失败（已进缓冲队列，等待重投）时调用。
	EventPublishFailed(eventType, reason string)
}

// PublisherStats 是发布器的累计计数快照（进程内）。
type PublisherStats struct {
	// Published 成功落库的事件数。
	Published int64
	// Failed 落库失败的事件数（失败的都已进 buffer，等待 Flush 重投）。
	Failed int64
	// Buffered 失败后成功进入缓冲队列的事件数。
	Buffered int64
	// BufferedLost 失败且连缓冲都写不进去的事件数——这类事件是真丢了，
	// 只能靠日志与指标告警，所以单独计。
	BufferedLost int64
	// SeqBackfilled 落库后由存储端回填 seq 的次数（seq 的唯一分配者是库）。
	SeqBackfilled int64
}

type Buffer interface {
	Add(e *Event) error
	Drain() ([]*Event, error)
	Len() int
}

type MemoryBuffer struct {
	mu     sync.Mutex
	events []*Event
}

func NewMemoryBuffer() *MemoryBuffer { return &MemoryBuffer{} }

func (b *MemoryBuffer) Add(e *Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e.Clone())
	return nil
}

func (b *MemoryBuffer) Drain() ([]*Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.events
	b.events = nil
	return out, nil
}

func (b *MemoryBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

type JSONLBuffer struct {
	mu   sync.Mutex
	path string
}

func NewJSONLBuffer(path string) *JSONLBuffer { return &JSONLBuffer{path: path} }

func (b *JSONLBuffer) Add(e *Event) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("event buffer: marshal: %w", err)
	}
	raw = append(raw, '\n')
	b.mu.Lock()
	defer b.mu.Unlock()
	f, err := os.OpenFile(b.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("event buffer: open: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("event buffer: write: %w", err)
	}
	return nil
}

func (b *JSONLBuffer) Drain() ([]*Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, err := os.ReadFile(b.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("event buffer: read: %w", err)
	}
	var out []*Event
	for len(raw) > 0 {
		idx := 0
		for idx < len(raw) && raw[idx] != '\n' {
			idx++
		}
		if idx == len(raw) {
			break
		}
		line := raw[:idx]
		raw = raw[idx+1:]
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, &e)
	}
	if err := os.Truncate(b.path, 0); err != nil {
		return nil, fmt.Errorf("event buffer: truncate: %w", err)
	}
	return out, nil
}

func (b *JSONLBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, err := os.ReadFile(b.path)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range raw {
		if c == '\n' {
			n++
		}
	}
	return n
}

type Publisher struct {
	mu      sync.Mutex
	sink    Sink
	idem    Idempotency
	buffer  Buffer
	offsets map[string]*SeqWatermark // 任务内 seq 的水位镜像（只读，不分配）
	obs     atomic.Pointer[observerBox]

	published    atomic.Int64
	failed       atomic.Int64
	buffered     atomic.Int64
	bufferedLost atomic.Int64
	backfilled   atomic.Int64
}

// observerBox 让 Observer 可以原子替换（Runtime 在 Run 入口挂载指标面）。
type observerBox struct{ o Observer }

func NewPublisher(sink Sink, idem Idempotency, buffer Buffer) *Publisher {
	return &Publisher{sink: sink, idem: idem, buffer: buffer, offsets: make(map[string]*SeqWatermark)}
}

// SetObserver 挂载/替换失败观测钩子（幂等）。传 nil 表示卸载。
func (p *Publisher) SetObserver(o Observer) {
	if p == nil {
		return
	}
	if o == nil {
		p.obs.Store(nil)
		return
	}
	p.obs.Store(&observerBox{o: o})
}

func (p *Publisher) observer() Observer {
	if box := p.obs.Load(); box != nil {
		return box.o
	}
	return nil
}

// RestoreSeq 用 Checkpoint 里的 event_seq_watermark 回填水位镜像。
// 注意：这只是「读取 / 回填」，不会让发布器拿到分配权；下一个 seq 仍然由库给。
func (p *Publisher) RestoreSeq(taskID string, watermark int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.offsets[taskID] = NewSeqWatermark(watermark)
}

// SeqWatermark 返回某任务已观察到（已落库）的最大 seq，0 表示还没有观测到。
func (p *Publisher) SeqWatermark(taskID string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if w := p.offsets[taskID]; w != nil {
		return w.Last()
	}
	return 0
}

// observeSeq 只用库回填的 seq 更新水位镜像：发布器不再自行编号。
func (p *Publisher) observeSeq(e *Event) {
	if e == nil || e.Seq <= 0 {
		return
	}
	p.mu.Lock()
	w := p.offsets[e.TaskID]
	if w == nil {
		w = NewSeqWatermark(0)
		p.offsets[e.TaskID] = w
	}
	p.mu.Unlock()
	w.Observe(e.Seq)
	p.backfilled.Add(1)
}

// Stats 返回累计计数快照。
func (p *Publisher) Stats() PublisherStats {
	return PublisherStats{
		Published:     p.published.Load(),
		Failed:        p.failed.Load(),
		Buffered:      p.buffered.Load(),
		BufferedLost:  p.bufferedLost.Load(),
		SeqBackfilled: p.backfilled.Load(),
	}
}

func (p *Publisher) Publish(ctx context.Context, e *Event) error {
	if e == nil {
		return errors.New("event is nil")
	}
	if p.idem != nil {
		seen, err := p.idem.Seen(ctx, e.EventID)
		if err != nil {
			return fmt.Errorf("event publisher: idempotency check: %w", err)
		}
		if seen {
			return nil
		}
	}
	// Seq 允许为 0：0 表示「尚未分配」，由存储端在写入事务里分配并回填。
	// 这里绝不再调用任何进程内分配器——那正是 T-P2-32 的双写者缺陷。
	if err := e.ValidateForPublish(); err != nil {
		return err
	}
	if err := p.sink.Append(ctx, []*Event{e}); err != nil {
		p.failed.Add(1)
		p.bufferFailed(e, err)
		p.reportFailure(e, err)
		return err
	}
	p.published.Add(1)
	p.observeSeq(e)
	if p.idem != nil {
		if err := p.idem.Mark(ctx, e.EventID); err != nil {
			// 事件已经落库，Mark 失败只影响后续去重，不影响本次投递；
			// 但它必须可见——否则重复事件会被当成「已去重」继续写入。
			logf("[event] publisher: idempotency mark failed: event_id=%s err=%v", e.EventID, err)
		}
	}
	return nil
}

// bufferFailed 把投递失败的事件放进缓冲队列，保证「不丢」。
func (p *Publisher) bufferFailed(e *Event, cause error) {
	if p.buffer == nil {
		p.bufferedLost.Add(1)
		return
	}
	if berr := p.buffer.Add(e); berr != nil {
		p.bufferedLost.Add(1)
		logf("[event] publisher: buffer write failed, event lost: event_id=%s task_id=%s type=%s err=%v (cause=%v)",
			e.EventID, e.TaskID, e.EventType, berr, cause)
		return
	}
	p.buffered.Add(1)
}

// PublishBatch 逐条发布；第一条失败即返回，已成功的不回滚（事件是追加式）。
func (p *Publisher) PublishBatch(ctx context.Context, events []*Event) error {
	for _, e := range events {
		if err := p.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *Publisher) Flush(ctx context.Context) error {
	if p.buffer == nil {
		return nil
	}
	pending, err := p.buffer.Drain()
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	if err := p.sink.Append(ctx, pending); err != nil {
		for _, e := range pending {
			if berr := p.buffer.Add(e); berr != nil {
				p.bufferedLost.Add(1)
				logf("[event] publisher: buffer re-add failed, event lost: event_id=%s err=%v", e.EventID, berr)
			}
		}
		return err
	}
	for _, e := range pending {
		p.observeSeq(e)
	}
	return nil
}

func (p *Publisher) Pending() int {
	if p.buffer == nil {
		return 0
	}
	return p.buffer.Len()
}

// reportFailure 让每一次投递失败都留下痕迹：结构化日志 + 观测钩子。
// 早前这些错误被调用方的 `_ = Publish(...)` 吞掉，事后只能从「事件缺失」反推，
// 既不知道丢了哪条，也不知道为什么丢。
func (p *Publisher) reportFailure(e *Event, err error) {
	reason := FailureReason(err)
	logf("[event] publish failed: event_id=%s task_id=%s tenant_id=%s type=%s reason=%s pending=%d err=%v",
		e.EventID, e.TaskID, e.TenantID, e.EventType, reason, p.Pending(), err)
	if o := p.observer(); o != nil {
		o.EventPublishFailed(string(e.EventType), reason)
	}
}

// FailureReason 把发布错误归一成低基数标签，供指标 label 使用。
func FailureReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrSeqConflict):
		return "seq_conflict"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "sink_error"
}

func logf(format string, args ...any) {
	log.Printf(format, args...)
}
