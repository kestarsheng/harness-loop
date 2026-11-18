package event

import (
	"errors"
	"sync"
)

var ErrSeqRegression = errors.New("seq regression: watermark is behind the observed allocation")

// SeqWatermark 是「任务内 seq」的只读水位镜像。
//
// T-P2-32 定下的契约：seq 的唯一分配者是事件库（repository.EventRepo 在写入
// 事务里、租户锁内按 task 取 MAX(seq)+1，并把结果回填到 Event.Seq）。发布器
// 这一侧只允许「读取 / 回填」：
//
//   - 落库成功后 Observe(库分配到的 seq)，让内存水位跟上真实进度；
//   - 重启时 Restore(Checkpoint 里的 event_seq_watermark)，用于漂移判定。
//
// 这里**故意没有** Next()：进程内一旦再出现「自己 +1」的分配器，就是第二个
// 写者，会与库分配的 seq 撞 (task_id, seq) 唯一键，撞车的事件被静默丢掉——
// 实测丢的正是 stage b/c 的 tool.denied 与 task.completed。
type SeqWatermark struct {
	mu           sync.Mutex
	last         int64
	persisted    int64
	persistEvery int
}

func NewSeqWatermark(watermark int64) *SeqWatermark {
	if watermark < 0 {
		watermark = 0
	}
	return &SeqWatermark{last: watermark, persisted: watermark, persistEvery: 100}
}

// Observe 记录「库里已经写到的 seq」。它只会把水位往前推，永远不会分配新值。
func (w *SeqWatermark) Observe(seq int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if seq > w.last {
		w.last = seq
	}
}

func (w *SeqWatermark) Last() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

func (w *SeqWatermark) Restore(watermark int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if watermark < w.last {
		return ErrSeqRegression
	}
	w.last = watermark
	return nil
}

func (w *SeqWatermark) NeedsPersist() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last-w.persisted >= int64(w.persistEvery)
}

func (w *SeqWatermark) PersistedTo(seq int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if seq > w.persisted {
		w.persisted = seq
	}
}

func (w *SeqWatermark) Persisted() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.persisted
}

func (w *SeqWatermark) SetPersistEvery(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n > 0 {
		w.persistEvery = n
	}
}
