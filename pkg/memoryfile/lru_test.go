package memoryfile

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func topicFor(key string, cat Category, body string, pinned bool) Topic {
	return Topic{
		Key: key, Title: key, Body: body,
		Meta: Meta{Category: cat, Pinned: pinned, UpdatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), ContentHash: ComputeContentHash(body)},
	}
}

func newProjectStore() *Store {
	doc := &Document{SchemaVersion: SchemaVersion, Scope: ScopeProject, UpdatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	return NewStore(doc, DefaultLimits())
}

func TestLRUEvictsOldestAtLimit(t *testing.T) {
	s := newProjectStore()
	for i := 1; i <= 31; i++ {
		key := fmt.Sprintf("t%02d", i)
		if err := s.Insert(topicFor(key, CategoryProjectFact, "body", false)); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}
	doc := s.Document()
	if len(doc.Topics) != 30 {
		t.Fatalf("topics = %d, want 30", len(doc.Topics))
	}
	if _, ok := doc.TopicByKey("t01"); ok {
		t.Fatal("oldest topic t01 must be evicted")
	}
	if doc.Order[0] != "t31" {
		t.Fatalf("newest topic must be at head, got %s", doc.Order[0])
	}
	evictions := s.DrainEvictions()
	if len(evictions) != 1 || evictions[0].Key != "t01" {
		t.Fatalf("evictions = %+v", evictions)
	}
}

func TestReadPromotesToFrontAndPersists(t *testing.T) {
	s := newProjectStore()
	for i := 1; i <= 5; i++ {
		key := fmt.Sprintf("t%02d", i)
		if err := s.Insert(topicFor(key, CategoryProjectFact, "body", false)); err != nil {
			t.Fatal(err)
		}
	}
	if s.Document().Order[0] != "t05" {
		t.Fatalf("head = %s", s.Document().Order[0])
	}
	if _, ok := s.Get("t01"); !ok {
		t.Fatal("t01 must exist")
	}
	if s.Document().Order[0] != "t01" {
		t.Fatalf("read must promote to head, got %s", s.Document().Order[0])
	}

	raw, err := s.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Order[0] != "t01" {
		t.Fatalf("persisted order head = %s", parsed.Order[0])
	}
}

func TestReadDoesNotChangeUpdatedAtOrHash(t *testing.T) {
	s := newProjectStore()
	if err := s.Insert(topicFor("t01", CategoryUserPreference, "prefer go", false)); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get("t01")
	for i := 0; i < 10; i++ {
		got, ok := s.Get("t01")
		if !ok {
			t.Fatal("topic disappeared")
		}
		if got.Meta.ContentHash != before.Meta.ContentHash {
			t.Fatalf("content hash changed on read: %s", got.Meta.ContentHash)
		}
		if !got.Meta.UpdatedAt.Equal(before.Meta.UpdatedAt) {
			t.Fatalf("updated_at changed on read: %v", got.Meta.UpdatedAt)
		}
	}
}

func TestPinnedTopicsAreNotEvicted(t *testing.T) {
	s := newProjectStore()
	if err := s.Insert(topicFor("pinned", CategoryLongTermConstraint, "constraint", true)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 29; i++ {
		key := fmt.Sprintf("t%02d", i)
		if err := s.Insert(topicFor(key, CategoryProjectFact, "body", false)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Insert(topicFor("t30", CategoryProjectFact, "body", false)); err != nil {
		t.Fatalf("insert at limit: %v", err)
	}
	doc := s.Document()
	if _, ok := doc.TopicByKey("pinned"); !ok {
		t.Fatal("pinned topic must survive eviction")
	}
	if _, ok := doc.TopicByKey("t01"); ok {
		t.Fatal("oldest unpinned topic must be evicted")
	}
	if len(doc.Topics) != 30 {
		t.Fatalf("topics = %d", len(doc.Topics))
	}
}

func TestAllPinnedRejectsInsert(t *testing.T) {
	s := newProjectStore()
	for i := 1; i <= 30; i++ {
		key := fmt.Sprintf("t%02d", i)
		if err := s.Insert(topicFor(key, CategoryLongTermConstraint, "c", true)); err != nil {
			t.Fatal(err)
		}
	}
	err := s.Insert(topicFor("t31", CategoryProjectFact, "body", false))
	if !errors.Is(err, ErrMemoryFullPinned) {
		t.Fatalf("expected ErrMemoryFullPinned, got %v", err)
	}
	if len(s.Document().Topics) != 30 {
		t.Fatalf("rejected insert must not change topic count: %d", len(s.Document().Topics))
	}
	if _, ok := s.Document().TopicByKey("t31"); ok {
		t.Fatal("rejected topic must not be stored")
	}
}

func TestGlobalSoftLimitOnlyWarns(t *testing.T) {
	doc := &Document{SchemaVersion: SchemaVersion, Scope: ScopeGlobal, UpdatedAt: time.Now().UTC()}
	s := NewStore(doc, DefaultLimits())
	for i := 1; i <= 51; i++ {
		key := fmt.Sprintf("g%02d", i)
		if err := s.Insert(topicFor(key, CategoryUserPreference, "pref", false)); err != nil {
			t.Fatalf("insert global %d: %v", i, err)
		}
	}
	if len(s.Document().Topics) != 51 {
		t.Fatalf("global topics = %d, want no eviction", len(s.Document().Topics))
	}
	warnings := s.DrainWarnings()
	if len(warnings) == 0 || warnings[0].Type != "memory.capacity_warning" {
		t.Fatalf("warnings = %+v", warnings)
	}
	if len(s.DrainEvictions()) != 0 {
		t.Fatal("global memory must not evict")
	}
}

func TestMergeSemantics(t *testing.T) {
	s := newProjectStore()

	if err := s.Insert(topicFor("pref", CategoryUserPreference, "old preference", false)); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(topicFor("pref", CategoryUserPreference, "new preference", false)); err != nil {
		t.Fatal(err)
	}
	pref, _ := s.Document().TopicByKey("pref")
	if pref.Body != "new preference" {
		t.Fatalf("preference must be replaced, got %q", pref.Body)
	}

	if err := s.Insert(topicFor("cons", CategoryLongTermConstraint, "- no mysql\n- use go", true)); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(topicFor("cons", CategoryLongTermConstraint, "- use go\n- no redis", true)); err != nil {
		t.Fatal(err)
	}
	cons, _ := s.Document().TopicByKey("cons")
	if !strings.Contains(cons.Body, "no mysql") || !strings.Contains(cons.Body, "no redis") {
		t.Fatalf("constraints must be appended: %q", cons.Body)
	}
	if strings.Count(cons.Body, "use go") != 1 {
		t.Fatalf("constraints must be deduplicated: %q", cons.Body)
	}

	if err := s.Insert(topicFor("fact", CategoryProjectFact, "old architecture note", false)); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(topicFor("fact", CategoryProjectFact, "new architecture note", false)); err != nil {
		t.Fatal(err)
	}
	fact, _ := s.Document().TopicByKey("fact")
	if !strings.Contains(fact.Body, "new architecture note") || !strings.Contains(fact.Body, "历史要点") {
		t.Fatalf("fact must merge with history: %q", fact.Body)
	}
}

func TestConstraintItemLimit(t *testing.T) {
	s := newProjectStore()
	for i := 1; i <= 12; i++ {
		body := fmt.Sprintf("- constraint %02d", i)
		if err := s.Insert(topicFor("cons", CategoryLongTermConstraint, body, true)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	cons, _ := s.Document().TopicByKey("cons")
	items := splitItems(cons.Body)
	if len(items) != 10 {
		t.Fatalf("constraint items = %d, want 10", len(items))
	}
	if !strings.Contains(cons.Body, "constraint 12") {
		t.Fatalf("newest constraints must be kept: %q", cons.Body)
	}
	if strings.Contains(cons.Body, "constraint 01") || strings.Contains(cons.Body, "constraint 02") {
		t.Fatalf("oldest constraints must be dropped: %q", cons.Body)
	}
}

func TestUnchangedContentKeepsTimestamp(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	s := newProjectStore()
	s.SetClock(func() time.Time { return now })
	if err := s.Insert(topicFor("t01", CategoryTaskState, "stage c", false)); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Document().TopicByKey("t01")

	now = now.Add(time.Hour)
	same := first
	if err := s.Insert(same); err != nil {
		t.Fatal(err)
	}
	second, _ := s.Document().TopicByKey("t01")
	if !second.Meta.UpdatedAt.Equal(first.Meta.UpdatedAt) {
		t.Fatalf("unchanged content must not refresh updated_at: %v -> %v", first.Meta.UpdatedAt, second.Meta.UpdatedAt)
	}
	if second.Meta.ContentHash != first.Meta.ContentHash {
		t.Fatal("unchanged content must keep hash")
	}
}

func TestInsertRejectsInvalid(t *testing.T) {
	s := newProjectStore()
	if err := s.Insert(topicFor("Bad_Key", CategoryTaskState, "x", false)); !errors.Is(err, ErrInvalidTopicKey) {
		t.Fatalf("bad key: %v", err)
	}
	if err := s.Insert(Topic{Key: "a", Title: "t", Body: "b", Meta: Meta{Category: Category("nope"), UpdatedAt: docTime()}}); !errors.Is(err, ErrInvalidMeta) {
		t.Fatalf("bad category: %v", err)
	}
	if err := s.Insert(Topic{Key: "a", Title: "", Body: "b", Meta: Meta{Category: CategoryTaskState, UpdatedAt: docTime()}}); !errors.Is(err, ErrInvalidMeta) {
		t.Fatalf("missing title: %v", err)
	}
	large := strings.Repeat("甲", DefaultLimits().MaxBodyChars+1)
	if err := s.Insert(topicFor("big", CategoryProjectFact, large, false)); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("oversized body: %v", err)
	}
}

func TestRemoveAndTouch(t *testing.T) {
	s := newProjectStore()
	if err := s.Insert(topicFor("a", CategoryTaskState, "x", false)); err != nil {
		t.Fatal(err)
	}
	if !s.Touch("a") {
		t.Fatal("touch existing topic must succeed")
	}
	if s.Touch("missing") {
		t.Fatal("touch missing topic must fail")
	}
	if !s.Remove("a") {
		t.Fatal("remove existing topic must succeed")
	}
	if s.Remove("a") {
		t.Fatal("second remove must fail")
	}
}
