package memoryfile

import (
	"strings"
	"testing"
	"time"
)

func newGlobalStore() *Store {
	doc := &Document{SchemaVersion: SchemaVersion, Scope: ScopeGlobal, UpdatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	return NewStore(doc, DefaultLimits())
}

func TestGlobalMemoryKeepsWriteOrder(t *testing.T) {
	s := newGlobalStore()
	for _, key := range []string{"g01", "g02", "g03"} {
		if err := s.Insert(topicFor(key, CategoryUserPreference, "pref", false)); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(s.Document().Order, ","); got != "g01,g02,g03" {
		t.Fatalf("global order must be write order, got %s", got)
	}
}

func TestGlobalMemoryReadDoesNotReorder(t *testing.T) {
	s := newGlobalStore()
	for _, key := range []string{"g01", "g02", "g03"} {
		if err := s.Insert(topicFor(key, CategoryUserPreference, "pref-"+key, false)); err != nil {
			t.Fatal(err)
		}
	}
	before := strings.Join(s.Document().Order, ",")
	if _, ok := s.Get("g01"); !ok {
		t.Fatal("topic missing")
	}
	if got := strings.Join(s.Document().Order, ","); got != before {
		t.Fatalf("global read must not reorder: %s -> %s", before, got)
	}
}

func TestGlobalMemoryReadsKeepMetaStable(t *testing.T) {
	s := newGlobalStore()
	if err := s.Insert(topicFor("g01", CategoryLongTermConstraint, "constraint", true)); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Document().TopicByKey("g01")
	for i := 0; i < 10; i++ {
		if _, ok := s.Get("g01"); !ok {
			t.Fatal("topic missing")
		}
	}
	second, _ := s.Document().TopicByKey("g01")
	if second.Meta.ContentHash != first.Meta.ContentHash {
		t.Fatal("global read must not change content hash")
	}
	if !second.Meta.UpdatedAt.Equal(first.Meta.UpdatedAt) {
		t.Fatal("global read must not change updated_at")
	}
	if strings.Join(s.Document().Order, ",") != "g01" {
		t.Fatal("global read must not change order")
	}
}

func TestGlobalMemoryOverSoftLimitOnlyWarns(t *testing.T) {
	s := newGlobalStore()
	for i := 1; i <= 51; i++ {
		if err := s.Insert(topicFor("g"+strings.Repeat("0", 0)+string(rune('a'+i%26))+string(rune('0'+i%10)), CategoryUserPreference, "v", false)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if len(s.Document().Topics) <= 50 {
		t.Fatalf("global soft limit must not delete topics, got %d", len(s.Document().Topics))
	}
	if len(s.DrainEvictions()) != 0 {
		t.Fatal("global memory must never evict")
	}
}

func TestProjectMemoryStillLRU(t *testing.T) {
	doc := &Document{SchemaVersion: SchemaVersion, Scope: ScopeProject, UpdatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	s := NewStore(doc, DefaultLimits())
	for _, key := range []string{"p01", "p02", "p03"} {
		if err := s.Insert(topicFor(key, CategoryProjectFact, "v", false)); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(s.Document().Order, ","); got != "p03,p02,p01" {
		t.Fatalf("project order must be LRU (newest first), got %s", got)
	}
	s.Get("p01")
	if got := strings.Join(s.Document().Order, ","); got != "p01,p03,p02" {
		t.Fatalf("project read must promote, got %s", got)
	}
}
