package events

import (
	"context"
	"testing"
	"time"

	eventpkg "agent-platform/pkg/harnessloop/event"
)

func TestDefaultIsNoopAndAcceptsEveryEvent(t *testing.T) {
	sink := Default()
	if _, ok := sink.(NoopSink); !ok {
		t.Fatalf("Default() = %T, want NoopSink: 当前没有生产方，默认出口必须是 no-op", sink)
	}
	// 取消的 ctx 也不能让 no-op 出口 panic 或阻塞：它出现在 memoryfile 锁内链路上。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink.Emit(ctx, Event{Type: TypePromoted, TenantID: "t1", ProjectID: "p1", TopicKey: "k"})
	sink.Emit(nil, Event{})
}

func TestEventValidateAcceptsWhitelistedTypes(t *testing.T) {
	for _, typ := range []Type{TypePromoted, TypeEvicted} {
		ev := Event{Type: typ, TenantID: "t1", ProjectID: "p1", TopicKey: "topic", Timestamp: time.Now()}
		if err := ev.Validate(); err != nil {
			t.Fatalf("Validate(%s) = %v, want nil", typ, err)
		}
	}
}

// 事件名必须与平台白名单逐字一致：写错一个点号，发布边界就会静默丢事件。
func TestTypeNamesMatchPlatformWhitelist(t *testing.T) {
	cases := map[Type]string{
		TypePromoted: "memory.promoted",
		TypeEvicted:  "memory.evicted",
	}
	for typ, want := range cases {
		if string(typ) != want {
			t.Fatalf("event type = %q, want %q", typ, want)
		}
	}
}

// 事件名必须真的登记在平台白名单里，而不只是「字符串长得对」：
// 发布边界按 pkg/harnessloop/event.IsKnownType 判定，白名单里没有的名字会被
// 静默丢弃。因此这里直接问白名单，而不是跟字面量比。
func TestTypesAreRegisteredInPlatformWhitelist(t *testing.T) {
	for _, typ := range []Type{TypePromoted, TypeEvicted} {
		if !eventpkg.IsKnownType(eventpkg.EventType(typ)) {
			t.Fatalf("event type %q is not registered in pkg/harnessloop/event whitelist: 发布边界会静默丢弃", typ)
		}
	}
	// 同时要求逐字一致：本包常量与白名单常量必须是同一个字符串，
	// 否则 IsKnownType 通过、下游按 EventType 分支却匹配不上。
	pairs := []struct {
		local    Type
		platform eventpkg.EventType
	}{
		{TypePromoted, eventpkg.MemoryPromoted},
		{TypeEvicted, eventpkg.MemoryEvicted},
	}
	for _, p := range pairs {
		if string(p.local) != string(p.platform) {
			t.Fatalf("local type %q != platform type %q", p.local, p.platform)
		}
	}
}

// 容量告警刻意不是平台事件类型：它没有发送方，也不该进白名单（那会牵动前端
// 73 项、TUI、alert 三处）。这条断言把「为什么删除 EventCapacityWarn」的结论
// 固化成可执行的守卫——如果哪天有人把它加进白名单，这里会先红。
func TestCapacityWarningIsNotAPlatformEventType(t *testing.T) {
	if eventpkg.IsKnownType(eventpkg.EventType("memory.capacity_warning")) {
		t.Fatalf("memory.capacity_warning must not be a platform event type: 它没有发送方，属于内部告警（见 pkg/memoryfile/lru.go capacityWarningType）")
	}
}

func TestEventValidateRejectsNonWhitelistedAndIncompleteEvents(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
	}{
		{"non-whitelisted type", Event{Type: "memory.capacity_warning", TenantID: "t1", ProjectID: "p1", TopicKey: "k"}},
		{"empty type", Event{TenantID: "t1", ProjectID: "p1", TopicKey: "k"}},
		{"missing tenant", Event{Type: TypePromoted, ProjectID: "p1", TopicKey: "k"}},
		{"missing project", Event{Type: TypePromoted, TenantID: "t1", TopicKey: "k"}},
		{"missing topic", Event{Type: TypePromoted, TenantID: "t1", ProjectID: "p1"}},
	}
	for _, c := range cases {
		if err := c.ev.Validate(); err == nil {
			t.Fatalf("%s: Validate() = nil, want error", c.name)
		}
	}
}
