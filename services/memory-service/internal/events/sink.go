// Package events 定义 memory-service 的记忆事件出口（EventSink）。
//
// # 为什么这里只有抽象和 no-op
//
// T-P2「记忆事件落库」评估结论（2026-11-17）：
//
//  1. tenant/project 语义成立：mdsync 的 MDSyncOptions 带 TenantID/ProjectID
//     （HARNESS_MEM_TENANT / HARNESS_MEM_PROJECT，默认 default），
//     hlp_mem.memory_topics 也是按 (tenant_id, project_id, scope, topic_key) 建键。
//  2. 事件表可写但口径受限：memory-service 用 HARNESS_MEM_PG_DSN 打开
//     agent-platform/pkg/pgindex 并执行 Migrate，hlp_event.events 就在同一个库、
//     同一条 DSN 下；global_seq 没有 sequence 对象，靠「租户级
//     pg_advisory_xact_lock(hashtext(tenant)) + MAX(global_seq)+1」分配，与
//     harnessloop-service 的 event_repo.Append 同口径，可以直接复用。
//     但 hlp_event.events 的 task_id / seq 都是 NOT NULL 且有 UNIQUE(task_id, seq)，
//     记忆提升/淘汰没有任务上下文，硬写只能借用空 task_id，语义上说不清。
//  3. 真正的拦路条件是没有生产方：memory-service 全链路只调用
//     memoryfile.Parse（md → 派生索引），从不构造 memoryfile.Store/Writer，
//     因此没有任何 LRU 提升（Get 重排）或淘汰（Insert 超限驱逐）发生。
//     pkg/memoryfile 的 WithSink 事件链路目前只有测试在用。
//
// 所以本包只交付出口契约与默认 NoopSink：不接一条假链路，也不写一张没有
// 生产方的表。等 memory-service 真的持有 memoryfile.Store（或等价 LRU）之后，
// 再在这里补 PGSink —— 写 hlp_event.events，事件名只能取本包 Type 里的两个
// （已在平台白名单 pkg/harnessloop/event/types.go），global_seq 按上面第 2 条
// 的 advisory 锁 + MAX+1 分配，并把 Default() 的返回值换成它。
package events

import (
	"context"
	"fmt"
	"time"
)

// Type 是记忆事件类型。取值必须是平台白名单里已有的名字：不在白名单的事件名
// 会在发布边界被静默丢弃（见 pkg/harnessloop/event/types.go 与 G23）。
type Type string

const (
	// TypePromoted 对应白名单 memory.promoted：一个主题因被读取而真实前移。
	TypePromoted Type = "memory.promoted"
	// TypeEvicted 对应白名单 memory.evicted：一个主题因超出容量而被真实驱逐。
	TypeEvicted Type = "memory.evicted"
)

// Event 是一条待出口的记忆事件。字段与 memoryfile.ChangeEvent 对齐到「谁、
// 哪个租户、哪个主题、发生了什么」这一层，足够拼出 hlp_event.events 的一行；
// 需要更细的内容再按需扩展，不要提前把整份 ChangeEvent 搬进来。
type Event struct {
	Type      Type
	TenantID  string
	ProjectID string
	TopicKey  string
	Scope     string
	Detail    string
	Timestamp time.Time
}

// Validate 拒绝无法安全落库或不在白名单里的事件。Sink 实现应在写入前调用它，
// 这样「白名单对齐」就在编译期之外多一道运行时闸门。
func (e Event) Validate() error {
	switch e.Type {
	case TypePromoted, TypeEvicted:
	default:
		return fmt.Errorf("events: %q is not a whitelisted memory event", e.Type)
	}
	if e.TenantID == "" {
		return fmt.Errorf("events: tenant id is required")
	}
	if e.ProjectID == "" {
		return fmt.Errorf("events: project id is required")
	}
	if e.TopicKey == "" {
		return fmt.Errorf("events: topic key is required")
	}
	return nil
}

// Sink 是记忆事件的唯一出口。实现必须自己保证并发安全，且不得阻塞调用方的
// 读/写路径 —— Emit 出现在 memoryfile 的锁内调用链上。
type Sink interface {
	Emit(ctx context.Context, ev Event)
}

// NoopSink 丢弃所有事件。它是 Default 的返回值：当前没有任何生产方，写出去
// 只能是伪造的。
type NoopSink struct{}

func (NoopSink) Emit(context.Context, Event) {}

// Default 返回当前应当使用的 Sink。目前恒为 NoopSink，见包注释。
func Default() Sink { return NoopSink{} }
