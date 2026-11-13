package memoryfile

import (
	"path/filepath"
	"testing"
	"time"
)

func newBoundedStoreSync(sink EventSink, maxTopics int) *Store {
	doc := &Document{SchemaVersion: SchemaVersion, Scope: ScopeProject, UpdatedAt: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)}
	limits := DefaultLimits()
	limits.MaxTopicsProject = maxTopics
	store := NewStore(doc, limits)
	if sink != nil {
		store.WithSink(sink)
	}
	return store
}

func eventKeys(events []ChangeEvent, typ string) map[string]int {
	out := map[string]int{}
	for _, e := range events {
		if e.Type != typ {
			continue
		}
		for _, tc := range e.Topics {
			out[tc.Key]++
		}
	}
	return out
}

// Store.WithSink must publish memory.lru_promoted when Get actually reorders a
// topic, and must not publish again when the topic is already at the front.
func TestStoreGetAutoPublishesPromotedOncePerReorder(t *testing.T) {
	_, store, _, sink := newWriterFixture(t)
	for _, key := range []string{"a", "b", "c"} {
		if err := store.Insert(topicFor(key, CategoryUserPreference, "pref-"+key, false)); err != nil {
			t.Fatal(err)
		}
	}

	if _, ok := store.Get("a"); !ok {
		t.Fatal("a must exist")
	}
	promoted := eventKeys(sink.events, EventPromoted)
	if promoted["a"] != 1 {
		t.Fatalf("promoted[a] = %d, want 1", promoted["a"])
	}
	if got := sink.count(EventPromoted); got != 1 {
		t.Fatalf("promoted events = %d, want 1", got)
	}
	for _, e := range sink.events {
		if e.Type == EventPromoted && e.File == "" {
			t.Fatalf("promoted event must carry the writer path: %+v", e)
		}
	}

	// Reading the same key again must not re-publish: it is already at rank 0.
	if _, ok := store.Get("a"); !ok {
		t.Fatal("a must exist")
	}
	if got := sink.count(EventPromoted); got != 1 {
		t.Fatalf("promoted events after second read = %d, want 1", got)
	}
}

// Insert must publish memory.evicted immediately when a sink is wired, and the
// key must not stay in the pending queue (otherwise Save or a manual drain
// would publish the same key twice in one eviction cycle).
func TestStoreInsertAutoPublishesEvictedOncePerCycle(t *testing.T) {
	dir := t.TempDir()
	sink := &recordingSink{}
	store := newBoundedStoreSync(sink, 2)
	writer := NewWriter(store, filepath.Join(dir, "project.md")).WithSink(sink)

	for _, key := range []string{"a", "b"} {
		if err := store.Insert(topicFor(key, CategoryTaskState, "v1", false)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Insert(topicFor("c", CategoryTaskState, "v1", false)); err != nil {
		t.Fatal(err)
	}

	evicted := eventKeys(sink.events, EventEvicted)
	if evicted["a"] != 1 {
		t.Fatalf("evicted[a] = %d, want 1", evicted["a"])
	}
	if pending := store.DrainEvictions(); len(pending) != 0 {
		t.Fatalf("pending evictions = %d, want 0 when a sink is wired", len(pending))
	}

	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}
	if got := sink.count(EventEvicted); got != 1 {
		t.Fatalf("evicted events after Save = %d, want 1", got)
	}

	// The legacy manual path must stay idempotent in the same cycle.
	writer.EmitEvictions(store.DrainEvictions())
	if got := sink.count(EventEvicted); got != 1 {
		t.Fatalf("evicted events after manual drain = %d, want 1", got)
	}
}

// deleted and evicted are different semantics and must both be kept. Consumers
// de-duplicate on (topic_key, event_type); Save must not drop the deleted diff
// just because Insert already published evicted for the same key.
func TestEvictedAndDeletedAreBothKept(t *testing.T) {
	dir := t.TempDir()
	sink := &recordingSink{}
	store := newBoundedStoreSync(sink, 2)
	writer := NewWriter(store, filepath.Join(dir, "project.md")).WithSink(sink)

	for _, key := range []string{"a", "b"} {
		if err := store.Insert(topicFor(key, CategoryTaskState, "v1", false)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(topicFor("c", CategoryTaskState, "v1", false)); err != nil {
		t.Fatal(err)
	}
	res, err := writer.Save()
	if err != nil {
		t.Fatal(err)
	}

	deleted := 0
	for _, tc := range res.Topics {
		if tc.Key == "a" && tc.Action == string(ActionDeleted) {
			deleted++
		}
	}
	if deleted != 1 {
		t.Fatalf("deleted diff for a = %d, want 1 (got %+v)", deleted, res.Topics)
	}
	if eventKeys(sink.events, EventEvicted)["a"] != 1 {
		t.Fatalf("evicted[a] = %d, want 1", eventKeys(sink.events, EventEvicted)["a"])
	}
}

// Without a sink, the library must keep the old behavior: no panic, evictions
// stay pending for DrainEvictions, and Save still works.
func TestStoreWithoutSinkKeepsLegacyBehavior(t *testing.T) {
	dir := t.TempDir()
	store := newBoundedStoreSync(nil, 2)
	writer := NewWriter(store, filepath.Join(dir, "project.md"))

	for _, key := range []string{"a", "b"} {
		if err := store.Insert(topicFor(key, CategoryTaskState, "v1", false)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Insert(topicFor("c", CategoryTaskState, "v1", false)); err != nil {
		t.Fatal(err)
	}
	pending := store.DrainEvictions()
	if len(pending) != 1 || pending[0].Key != "a" {
		t.Fatalf("pending evictions = %+v, want one for a", pending)
	}
	if _, ok := store.Get("a"); ok {
		t.Fatal("a should have been evicted")
	}
	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}
}

// The legacy manual promotion must not double publish after Get already
// promoted the key through the store sink.
func TestManualPromotionDoesNotDoublePublish(t *testing.T) {
	_, store, _, sink := newWriterFixture(t)
	for _, key := range []string{"a", "b", "c"} {
		if err := store.Insert(topicFor(key, CategoryUserPreference, "pref-"+key, false)); err != nil {
			t.Fatal(err)
		}
	}
	writer := NewWriter(store, filepath.Join(t.TempDir(), "project.md")).WithSink(sink)

	if _, ok := store.Get("a"); !ok {
		t.Fatal("a must exist")
	}
	if got := sink.count(EventPromoted); got != 1 {
		t.Fatalf("promoted events after Get = %d, want 1", got)
	}
	writer.EmitPromotion("a", 0)
	if got := sink.count(EventPromoted); got != 1 {
		t.Fatalf("promoted events after manual call = %d, want 1", got)
	}
}
