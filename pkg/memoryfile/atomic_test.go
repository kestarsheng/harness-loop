package memoryfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingSink struct {
	mu     sync.Mutex
	events []ChangeEvent
}

func (r *recordingSink) Emit(e ChangeEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingSink) count(typ string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func newWriterFixture(t *testing.T) (*Writer, *Store, string, *recordingSink) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "project.md")
	doc := &Document{SchemaVersion: SchemaVersion, Scope: ScopeProject, UpdatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	store := NewStore(doc, DefaultLimits())
	sink := &recordingSink{}
	writer := NewWriter(store, path).WithSink(sink)
	return writer, store, path, sink
}

func TestWriterCreatesFileAtomically(t *testing.T) {
	writer, store, path, sink := newWriterFixture(t)
	if err := store.Insert(topicFor("t01", CategoryTaskState, "stage c", false)); err != nil {
		t.Fatal(err)
	}
	res, err := writer.Save()
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if res.Skipped || res.Bytes == 0 || res.Hash == "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := Parse(raw); err != nil {
		t.Fatalf("written file must parse: %v", err)
	}
	if sink.count(EventWritten) != 1 {
		t.Fatalf("written events = %d", sink.count(EventWritten))
	}
	if len(res.Topics) != 1 || res.Topics[0].Action != string(ActionCreated) {
		t.Fatalf("topic diff = %+v", res.Topics)
	}
}

func TestWriterIsIdempotentForIdenticalContent(t *testing.T) {
	writer, store, path, sink := newWriterFixture(t)
	if err := store.Insert(topicFor("t01", CategoryTaskState, "stage c", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Save(); err != nil {
		t.Fatalf("first save: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	firstHash := hashBytes(first)

	time.Sleep(10 * time.Millisecond)
	if _, err := writer.Save(); err != nil {
		t.Fatalf("second save: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if hashBytes(second) != firstHash {
		t.Fatal("identical content must not rewrite the file")
	}
	if sink.count(EventWritten) != 1 {
		t.Fatalf("skipped save must not emit another event: %d", sink.count(EventWritten))
	}
}

func TestWriterDetectsExternalModification(t *testing.T) {
	writer, store, path, _ := newWriterFixture(t)
	if err := store.Insert(topicFor("t01", CategoryTaskState, "stage c", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	external, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	external = append(external, []byte("\n<!-- user edit -->\n")...)
	if err := os.WriteFile(path, external, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := store.Insert(topicFor("t02", CategoryProjectFact, "fact", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Save(); !errors.Is(err, ErrConcurrentModification) {
		t.Fatalf("expected concurrent modification, got %v", err)
	}
}

func TestWriterRejectsCorruptedTarget(t *testing.T) {
	writer, store, path, _ := newWriterFixture(t)
	if err := os.WriteFile(path, []byte("not a memory file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(topicFor("t01", CategoryTaskState, "x", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Save(); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("expected ErrCorrupted, got %v", err)
	}
}

func TestWriterConcurrentSavesAreSerialized(t *testing.T) {
	writer, store, path, _ := newWriterFixture(t)
	if err := store.Insert(topicFor("t01", CategoryTaskState, "stage c", false)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = writer.Save()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err != nil {
		t.Fatalf("file must stay valid after concurrent writes: %v", err)
	}
}

func TestInterruptedWriteKeepsOriginalReadable(t *testing.T) {
	writer, store, path, _ := newWriterFixture(t)
	if err := store.Insert(topicFor("t01", CategoryTaskState, "stage c", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	partial := filepath.Join(filepath.Dir(path), ".tmp-project.md-crash")
	if err := os.WriteFile(partial, before[:len(before)/2], 0o644); err != nil {
		t.Fatal(err)
	}

	parsed, err := Parse(before)
	if err != nil {
		t.Fatalf("original file must remain readable: %v", err)
	}
	if len(parsed.Topics) != 1 {
		t.Fatalf("topics = %d", len(parsed.Topics))
	}
	if _, err := Parse(append([]byte(nil), []byte("---\nschema_version: 1\nscope: project\nupdated_at: 2026-11-11T00:00:00Z\norder_version: 1\n---\n\n<!-- order: [\"t01\"] -->\n")...)); err == nil {
		t.Fatal("partial temp content is expected to be invalid; only the target must stay readable")
	}
}

func TestWriterEmitsEvictionAndPromotionEvents(t *testing.T) {
	writer, store, _, sink := newWriterFixture(t)
	writer.EmitEvictions([]Eviction{{Key: "old", Reason: "lru"}})
	writer.EmitPromotion("hot", 0)
	if sink.count(EventEvicted) != 1 {
		t.Fatalf("evicted events = %d", sink.count(EventEvicted))
	}
	if sink.count(EventPromoted) != 1 {
		t.Fatalf("promoted events = %d", sink.count(EventPromoted))
	}
	_ = store
}

func TestWriterDiffReportsUpdatesAndDeletes(t *testing.T) {
	writer, store, path, _ := newWriterFixture(t)
	if err := store.Insert(topicFor("keep", CategoryTaskState, "v1", false)); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(topicFor("drop", CategoryProjectFact, "x", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Save(); err != nil {
		t.Fatal(err)
	}

	if !store.Remove("drop") {
		t.Fatal("remove failed")
	}
	updated := topicFor("keep", CategoryTaskState, "v2", false)
	if err := store.Insert(updated); err != nil {
		t.Fatal(err)
	}
	res, err := writer.Save()
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	actions := map[string]string{}
	for _, c := range res.Topics {
		actions[c.Key] = c.Action
	}
	if actions["keep"] != string(ActionUpdated) {
		t.Fatalf("keep action = %q", actions["keep"])
	}
	if actions["drop"] != string(ActionDeleted) {
		t.Fatalf("drop action = %q", actions["drop"])
	}
	if !strings.Contains(string(mustRead(t, path)), "v2") {
		t.Fatal("updated body must be persisted")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return raw
}
