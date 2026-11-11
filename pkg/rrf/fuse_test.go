package rrf

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestMergeMatchesHandComputedRRF(t *testing.T) {
	f := New(60)
	hits := f.MergeKeys([]string{"a", "b"}, []string{"b", "c"})
	if len(hits) != 3 {
		t.Fatalf("hits = %d, want 3", len(hits))
	}
	want := map[string]float64{
		"a": 1.0 / 61.0,
		"b": 1.0/62.0 + 1.0/61.0,
		"c": 1.0 / 62.0,
	}
	for _, h := range hits {
		if math.Abs(h.RRFScore-want[h.ID]) > 1e-12 {
			t.Fatalf("%s score = %v, want %v", h.ID, h.RRFScore, want[h.ID])
		}
	}
	if hits[0].ID != "b" {
		t.Fatalf("top hit = %s, want b", hits[0].ID)
	}
	if hits[1].ID != "a" || hits[2].ID != "c" {
		t.Fatalf("order = %s,%s", hits[1].ID, hits[2].ID)
	}
}

func TestOverlappingItemsAppearOnce(t *testing.T) {
	left := make([]RankedItem, 0, 10)
	right := make([]RankedItem, 0, 10)
	for i := 0; i < 10; i++ {
		left = append(left, RankedItem{Entity: Entity{ID: fmt.Sprintf("shared-%d", i), FilePath: "a.go", StartLine: i}})
	}
	for i := 0; i < 10; i++ {
		right = append(right, RankedItem{Entity: Entity{ID: fmt.Sprintf("shared-%d", i), FilePath: "a.go", StartLine: i}})
	}
	right = append(right, RankedItem{Entity: Entity{ID: "only-right", FilePath: "b.go"}})

	hits := New(0).MergeRoutes(Route{Name: "fulltext", Items: left}, Route{Name: "vector", Items: right})
	if len(hits) != 11 {
		t.Fatalf("hits = %d, want 11 (10 shared + 1 unique)", len(hits))
	}
	seen := map[string]bool{}
	for _, h := range hits {
		if seen[h.ID] {
			t.Fatalf("duplicate hit %s", h.ID)
		}
		seen[h.ID] = true
	}
	for _, h := range hits {
		if strings.HasPrefix(h.ID, "shared-") && len(h.Sources) != 2 {
			t.Fatalf("%s sources = %v", h.ID, h.Sources)
		}
	}
}

func TestStableOrderAcrossRepeatedRuns(t *testing.T) {
	routes := func() []Route {
		left := []RankedItem{
			{Entity: Entity{ID: "x", FilePath: "b.go", StartLine: 10}},
			{Entity: Entity{ID: "y", FilePath: "a.go", StartLine: 20}},
		}
		right := []RankedItem{
			{Entity: Entity{ID: "y", FilePath: "a.go", StartLine: 20}},
			{Entity: Entity{ID: "x", FilePath: "b.go", StartLine: 10}},
		}
		return []Route{{Name: "fulltext", Items: left}, {Name: "vector", Items: right}}
	}
	first := New(60).MergeRoutes(routes()...)
	var firstIDs []string
	for _, h := range first {
		firstIDs = append(firstIDs, h.ID)
	}
	for i := 0; i < 10; i++ {
		got := New(60).MergeRoutes(routes()...)
		var ids []string
		for _, h := range got {
			ids = append(ids, h.ID)
		}
		if strings.Join(ids, ",") != strings.Join(firstIDs, ",") {
			t.Fatalf("run %d order = %v, want %v", i, ids, firstIDs)
		}
	}
}

func TestTieBreakOrder(t *testing.T) {
	same := 0.5
	hits := []Hit{
		{Entity: Entity{ID: "z", FilePath: "b.go", StartLine: 5}, RRFScore: same},
		{Entity: Entity{ID: "a", FilePath: "a.go", StartLine: 9}, RRFScore: same},
		{Entity: Entity{ID: "b", FilePath: "a.go", StartLine: 3}, RRFScore: same},
	}
	StableSort(hits)
	if hits[0].ID != "b" || hits[1].ID != "a" || hits[2].ID != "z" {
		t.Fatalf("tie-break order = %s,%s,%s", hits[0].ID, hits[1].ID, hits[2].ID)
	}
}

func TestConfigurableK(t *testing.T) {
	low := New(10).MergeKeys([]string{"a"})
	high := New(100).MergeKeys([]string{"a"})
	if low[0].RRFScore <= high[0].RRFScore {
		t.Fatalf("smaller k must yield larger score: %v vs %v", low[0].RRFScore, high[0].RRFScore)
	}
	if New(0).K != DefaultK {
		t.Fatalf("k=0 must default to %d", DefaultK)
	}
	if New(-5).K != DefaultK {
		t.Fatal("negative k must default")
	}
}

func TestEmptyAndUnknownRanks(t *testing.T) {
	if hits := New(60).MergeRoutes(); len(hits) != 0 {
		t.Fatalf("empty routes must return no hits: %v", hits)
	}
	if hits := New(60).MergeKeys(nil, []string{}); len(hits) != 0 {
		t.Fatalf("empty lists must return no hits: %v", hits)
	}
	f := New(60)
	if score := f.Score([]int{-1, -1}); score != 0 {
		t.Fatalf("missing ranks must score 0, got %v", score)
	}
}

func TestDedupOnlyKeepsAllSources(t *testing.T) {
	left := []RankedItem{{Entity: Entity{ID: "k"}}}
	right := []RankedItem{{Entity: Entity{ID: "k"}}}
	hits := DedupOnly(Route{Name: "fulltext", Items: left}, Route{Name: "vector", Items: right})
	if len(hits) != 1 || len(hits[0].Sources) != 2 {
		t.Fatalf("unexpected dedup result: %+v", hits)
	}
}
