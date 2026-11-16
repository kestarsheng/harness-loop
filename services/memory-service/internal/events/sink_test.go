package events

import (
	"context"
	"testing"
	"time"
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
