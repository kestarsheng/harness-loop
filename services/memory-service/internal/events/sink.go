// Package events 定义 memory-service 的记忆事件出口（EventSink）。
//
// # 为什么这里只有抽象和 no-op
//
// T-P2「记忆事件落库」评估结论（2026-11-17，二次独立复核）：
//
//  1. tenant/project 语义成立：mdsync 的 MDSyncOptions 带 TenantID/ProjectID
//     （HARNESS_MEM_TENANT / HARNESS_MEM_PROJECT，默认 default），
//     hlp_mem.memory_topics 也是按 (tenant_id, project_id, scope, topic_key) 建键。
//  2. 事件表同库同 DSN，但写入口径不安全：memory-service 用 HARNESS_MEM_PG_DSN
//     打开 pkg/pgindex 并执行 Migrate，而 ddl/003_event.sql 就在这条迁移里，
//     所以 hlp_event.events 与本服务的表同库同连接池；global_seq 没有 sequence
//     对象，靠「租户级 pg_advisory_xact_lock(hashtext(tenant)) + MAX+1」分配，
//     与 harnessloop-service 的 event_repo.Append 同口径，理论上可复用。
//     但记忆提升/淘汰没有任务上下文，两道约束都拒绝这种事件：
//     a) 平台校验器 pkg/harnessloop/event.(*Event).Validate 明确要求
//     task_id 非空（"task_id is required"），复用 Append 口径必然先被它挡下；
//     b) hlp_event.events.task_id 是 NOT NULL，且唯一约束是
//     UNIQUE (task_id, seq)——不含 tenant_id。若所有记忆事件共用同一个占位
//     task_id，则 seq 必须在全库范围内唯一，各租户自增的 global_seq 会互相撞号；
//     要绕开就得再引入一把跨租户全局锁，等于伪造一层任务语义。
//     结论：没有真实的 task 上下文时，不应写这张表。
//  3. 更根本的是没有生产方：memory-service 对 pkg/memoryfile 的唯一生产用途是
//     memoryfile.Parse（mdsync.go 的 md → 派生索引），全服务从不构造
//     memoryfile.Store/Writer，也就不存在 Get 重排带来的提升、Insert 超限带来的
//     淘汰。order_rank 只由 md 文件顺序写入（mdsync），recall 侧纯读
//     （ORDER BY ts_rank_cd DESC, order_rank ASC），读取不会回写重排；唯一的
//     写回是 decayed 标记，属 memory.decayed 语义而非 promoted/evicted。
//     pkg/memoryfile 的 WithSink 事件链路目前只有测试在用。
//
// 所以本包只交付出口契约与默认 NoopSink：不接一条假链路，也不写一张没有
// 生产方的表——那样产出的只会是伪造事件。
//
// # 何时才该补 PGSink
//
// 三个条件全部满足时，PGSink 才有意义，缺一不可：
//  1. memory-service 真的持有 memoryfile.Store（或等价的 LRU），并且读取路径
//     会触发重排、写入路径会触发超限驱逐；
//  2. 这些提升/淘汰挂上了真实的 task 上下文，或者事件表的 task_id 语义被
//     正式放宽（ALTER TABLE 由运维侧统一执行，不在本服务里偷偷绕）；
//  3. 事件名取自本包 Type（memory.promoted / memory.evicted，已在白名单），
//     global_seq 用下面的口径分配，且只在该租户持锁事务内生效。
//
// 在那之前，Default() 恒返回 NoopSink 是唯一诚实的实现。
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

// Default 返回当前应当使用的 Sink，恒定是 NoopSink。
//
// 这里是唯一的替换点：等包注释「何时才该补 PGSink」的三个条件全部满足后，
// 把返回值换成 PGSink 即可，调用方无需改动。在此之前不要把它换成任何会
// 写库的实现——那会写出没有生产方、也没有任务上下文的伪造事件。
func Default() Sink { return NoopSink{} }
