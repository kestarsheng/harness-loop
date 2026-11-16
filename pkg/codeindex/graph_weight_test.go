package codeindex

import (
	"math"
	"testing"

	"agent-platform/pkg/rrf"
)

func TestSeedDecayWeightLadder(t *testing.T) {
	cases := []struct {
		name string
		rank int
		step float64
		want float64
	}{
		{"rank1 full", 1, 0.1, 1.0},
		{"rank2", 2, 0.1, 0.9},
		{"rank3", 3, 0.1, 0.8},
		{"rank4", 4, 0.1, 0.7},
		{"rank5 keeps 60%", 5, 0.1, 0.6},
		{"no decay", 5, 0, 1.0},
		{"decay floors at zero", 6, 0.5, 0},
		{"rank zero is treated as top", 0, 0.1, 1.0},
	}
	for _, c := range cases {
		if got := seedDecayWeight(c.rank, c.step); math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("%s: seedDecayWeight(%d, %v) = %v, want %v", c.name, c.rank, c.step, got, c.want)
		}
	}
}

// 种子顺序就是检索排名；排序会把它抹掉，截断就成了「取字母表前 N 个」。
func TestCleanSeedsPreservesRankOrder(t *testing.T) {
	got := cleanSeeds([]string{"Zeta", "", "Alpha", "Zeta", "Beta"})
	want := []string{"Zeta", "Alpha", "Beta"}
	if len(got) != len(want) {
		t.Fatalf("seeds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seeds = %v, want %v (order matters)", got, want)
		}
	}
}

// 逐项系数必须真的乘进 RRF 分数，否则「按排名衰减」只是换了个排序。
func TestMergeWeightedAppliesItemWeight(t *testing.T) {
	routes := []rrf.Route{
		{Name: RouteFulltext, Items: []rrf.RankedItem{
			{Entity: rrf.Entity{ID: "a"}},
			{Entity: rrf.Entity{ID: "b"}},
		}},
		{Name: RouteGraph, Items: []rrf.RankedItem{
			{Entity: rrf.Entity{ID: "b"}},
		}},
	}

	flat := mergeWeighted(60, routes, []float64{1, 0.8}, nil)
	decayed := mergeWeighted(60, routes, []float64{1, 0.8}, []map[string]float64{nil, {"b": 0.6}})

	scoreOf := func(hits []rrf.Hit, id string) float64 {
		for _, h := range hits {
			if h.Entity.ID == id {
				return h.RRFScore
			}
		}
		return -1
	}

	// a 只走 fulltext：1/(60+0+1)。
	if got, want := scoreOf(flat, "a"), 1.0/61; math.Abs(got-want) > 1e-12 {
		t.Fatalf("a score = %v, want %v", got, want)
	}
	// b 等权时：1/62 + 0.8/61。
	if got, want := scoreOf(flat, "b"), 1.0/62+0.8/61; math.Abs(got-want) > 1e-12 {
		t.Fatalf("b flat score = %v, want %v", got, want)
	}
	// b 衰减 0.6 时：1/62 + 0.48/61。
	if got, want := scoreOf(decayed, "b"), 1.0/62+0.48/61; math.Abs(got-want) > 1e-12 {
		t.Fatalf("b decayed score = %v, want %v", got, want)
	}
	if scoreOf(decayed, "b") >= scoreOf(flat, "b") {
		t.Fatal("decay must lower the graph contribution")
	}
}
