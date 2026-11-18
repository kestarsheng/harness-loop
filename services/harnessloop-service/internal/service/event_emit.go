package service

// T-P2-32 事件可见性出口。
//
// 背景：event.Publisher 与 runtime 状态转移曾经是两条独立的 seq 写者，撞
// (task_id, seq) 唯一键后 emit 的错误又被调用方写成 `_ = publish(...)` 吞掉，
// 结果是 stage b/c 的 tool.denied 与 task.completed 静默消失。
//
// 现在 seq 只由事件库分配（见 repository.EventRepo.Append），失败错误一律经由
// 本文件上报：结构化日志负责「丢了哪条、为什么丢」，指标负责「丢了多少」。

import (
	"log"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/services/harnessloop-service/internal/metrics"
)

// eventObserverSetter 由 *event.Publisher 实现。发布器在 cmd/main.go 里构造，
// 生产装配中只有 Runtime 同时握着发布器与指标面，所以用可选接口挂载，
// 而不是把指标依赖塞进 event 包。
type eventObserverSetter interface {
	SetObserver(event.Observer)
}

// harnessEventObserver 把发布失败计数接到 harness 指标面。
type harnessEventObserver struct{ h *metrics.Harness }

func (o harnessEventObserver) EventPublishFailed(eventType, reason string) {
	o.h.EventPublishFailed(eventType, reason)
}

// attachEventObserver 幂等挂载指标观测；events / h 任一为空则跳过。
func attachEventObserver(events EventPublisher, h *metrics.Harness) {
	if events == nil || h == nil {
		return
	}
	if setter, ok := events.(eventObserverSetter); ok {
		setter.SetObserver(harnessEventObserver{h: h})
	}
}

// reportEmitFailure 是 emit/publish 失败的唯一上报出口：落一条结构化日志并
// 把错误交给调用方决定（缓冲重投 / 记 warn / 中断）。指标由发布器的 Observer
// 计数，这里不重复计，避免同一个失败被算两次。
func reportEmitFailure(taskID string, typ event.EventType, err error) error {
	if err == nil {
		return nil
	}
	log.Printf("[event] emit failed: task_id=%s type=%s reason=%s err=%v",
		taskID, typ, event.FailureReason(err), err)
	return err
}
