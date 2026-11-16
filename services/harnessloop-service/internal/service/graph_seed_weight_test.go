package service

import (
	"context"
	"math"
	"testing"

	"agent-platform/pkg/codeindex"
)

// 截断必须按「检索排名」而不是字母序：A 组（全量）与 C 组（top-5）的差别全靠这里。
func TestGraphSeedLimitKeepsRankOrder(t *testing.T) {
	f := graphFixture(t)
	ctx := context.Background()

	cases := []struct {
		name      string
		seeds     []string
		wantSeed  string
		wantOther string
	}{
		{"LoginFlow first", []string{"LoginFlow", "Caller"}, "LoginFlow", "Caller"},
		{"Caller first", []string{"Caller", "LoginFlow"}, "Caller", "LoginFlow"},
	}
	for _, c := range cases {
		neighbors, err := f.writer.GraphExpand(ctx, codeindex.GraphExpandOptions{
			TenantID:  idxTenant,
			ProjectID: idxProject,
			Seeds:     c.seeds,
			SeedLimit: 1,
		})
		if err != nil {
			t.Fatalf("%s: expand: %v", c.name, err)
		}
		if len(neighbors) == 0 {
			t.Fatalf("%s: truncation dropped every neighbor", c.name)
		}
		for _, n := range neighbors {
			if n.Seed != c.wantSeed {
				t.Fatalf("%s: neighbor from seed %q, want only %q (rank order lost)", c.name, n.Seed, c.wantSeed)
			}
			if n.SeedRank != 1 {
				t.Fatalf("%s: seed rank = %d, want 1", c.name, n.SeedRank)
			}
		}
	}
}

// 权重必须跟着种子排名走：rank1 满权，每降一位减 DecayStep。
func TestGraphSeedDecayWeightsFollowRank(t *testing.T) {
	f := graphFixture(t)
	ctx := context.Background()

	neighbors, err := f.writer.GraphExpand(ctx, codeindex.GraphExpandOptions{
		TenantID:  idxTenant,
		ProjectID: idxProject,
		Seeds:     []string{"LoginFlow", "Caller"},
		DecayStep: codeindex.DefaultGraphSeedDecay,
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(neighbors) == 0 {
		t.Fatal("expansion is empty")
	}

	ranks := map[int]bool{}
	for _, n := range neighbors {
		ranks[n.SeedRank] = true
		want := 1 - float64(n.SeedRank-1)*codeindex.DefaultGraphSeedDecay
		if math.Abs(n.Weight-want) > 1e-9 {
			t.Fatalf("seed %q rank %d: weight = %v, want %v", n.Seed, n.SeedRank, n.Weight, want)
		}
	}
	if len(ranks) < 2 {
		t.Fatalf("decay is untested with a single rank: %v", ranks)
	}
}

// 等权模式（DecayStep<=0）下所有邻居必须是满权，保证 A/B 组的对照有效。
func TestGraphSeedDecayDisabledKeepsFullWeight(t *testing.T) {
	f := graphFixture(t)
	ctx := context.Background()

	neighbors, err := f.writer.GraphExpand(ctx, codeindex.GraphExpandOptions{
		TenantID:  idxTenant,
		ProjectID: idxProject,
		Seeds:     []string{"LoginFlow", "Caller"},
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(neighbors) == 0 {
		t.Fatal("expansion is empty")
	}
	for _, n := range neighbors {
		if n.Weight != 1 {
			t.Fatalf("seed %q rank %d: weight = %v, want 1 (no decay configured)", n.Seed, n.SeedRank, n.Weight)
		}
	}
}

// 策略默认值必须可预测：默认 top-5 + 0.1 衰减，负值才是显式「全量 / 等权」。
func TestGraphPolicySeedDefaultsAndOverrides(t *testing.T) {
	def := DefaultGraphPolicy()
	if def.SeedLimit != 5 || math.Abs(def.SeedDecay-0.1) > 1e-9 {
		t.Fatalf("defaults = %+v", def)
	}
	if got := (GraphPolicy{}).Normalized(); got.SeedLimit != 5 || math.Abs(got.SeedDecay-0.1) > 1e-9 {
		t.Fatalf("zero-value normalization = %+v", got)
	}

	full := GraphPolicy{SeedLimit: -1, SeedDecay: -1}.Normalized()
	if full.SeedLimit != -1 || full.SeedDecay != -1 {
		t.Fatalf("negative overrides must survive normalization: %+v", full)
	}

	expander := NewGraphExpander(nil)
	expander.SetPolicy(GraphPolicy{Enabled: true, SeedLimit: -1, SeedDecay: -1})
	opts := expander.Apply(codeindex.SearchOptions{}, true)
	if opts.GraphSeedLimit != 0 || opts.GraphSeedDecay != 0 {
		t.Fatalf("'-1' must map to 0 (unlimited/equal weight) in SearchOptions: %+v", opts)
	}
}
