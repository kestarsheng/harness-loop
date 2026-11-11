package pgindex

import (
	"context"
	"os"
	"testing"
	"time"

	"agent-platform/pkg/pgindex"
)

const (
	recallTenant  = "tenant-recall"
	recallProject = "proj-recall"
)

func setupRecall(t *testing.T) *MemoryWriter {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HARNESS_TEST_PG_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	w := NewMemoryWriter(db)
	if _, err := w.DeleteTopics(ctx, recallTenant, recallProject, "project", []string{"login-flow", "api-constraint", "task-state"}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	records := []TopicRecord{
		{TenantID: recallTenant, ProjectID: recallProject, Scope: "project", TopicKey: "login-flow", Category: "project_fact",
			Title: "登录流程", Body: "实现 login 接口并补充测试", OrderRank: 0, ContentHash: "sha256:a", SourceFile: "project.md", UpdatedAt: time.Now().UTC()},
		{TenantID: recallTenant, ProjectID: recallProject, Scope: "project", TopicKey: "api-constraint", Category: "long_term_constraint",
			Title: "API 约束", Body: "所有外部接口必须以 /api/v2 开头", OrderRank: 1, ContentHash: "sha256:b", SourceFile: "project.md", UpdatedAt: time.Now().UTC()},
		{TenantID: recallTenant, ProjectID: recallProject, Scope: "project", TopicKey: "task-state", Category: "task_state",
			Title: "当前任务", Body: "阶段 c 进行中，待补集成测试", OrderRank: 2, ContentHash: "sha256:c", SourceFile: "project.md", UpdatedAt: time.Now().UTC()},
	}
	if _, err := w.UpsertTopics(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return w
}

func fakeVector(seed int) []float32 {
	vec := make([]float32, 1024)
	vec[seed] = 1
	return vec
}

func TestRecallFulltextSatisfiedSkipsEmbedding(t *testing.T) {
	w := setupRecall(t)
	ctx := context.Background()
	calls := 0
	hits, stats, err := w.Recall(ctx, RecallOptions{
		TenantID: recallTenant, ProjectID: recallProject, Query: "接口", TopK: 2,
		Embedder: func(ctx context.Context, text string) ([]float32, error) {
			calls++
			return fakeVector(0), nil
		},
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if calls != 0 || stats.VectorCalled {
		t.Fatalf("embedding must not be called when fulltext is enough: calls=%d stats=%+v", calls, stats)
	}
	if len(hits) == 0 || len(hits) > 2 {
		t.Fatalf("hits = %d", len(hits))
	}
}

func TestRecallTriggersVectorForSemanticQuery(t *testing.T) {
	w := setupRecall(t)
	ctx := context.Background()
	if err := w.SetEmbedding(ctx, recallTenant, recallProject, "project", "login-flow", fakeVector(0)); err != nil {
		t.Fatalf("set embedding: %v", err)
	}
	if err := w.SetEmbedding(ctx, recallTenant, recallProject, "project", "api-constraint", fakeVector(1)); err != nil {
		t.Fatalf("set embedding: %v", err)
	}
	calls := 0
	hits, stats, err := w.Recall(ctx, RecallOptions{
		TenantID: recallTenant, ProjectID: recallProject, Query: "how to finish login", TopK: 3,
		Embedder: func(ctx context.Context, text string) ([]float32, error) {
			calls++
			return fakeVector(0), nil
		},
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if calls != 1 || !stats.VectorCalled {
		t.Fatalf("semantic query must trigger embedding: calls=%d stats=%+v", calls, stats)
	}
	if stats.Trigger != TriggerSemantic {
		t.Fatalf("trigger = %s", stats.Trigger)
	}
	if len(hits) == 0 || hits[0].TopicKey != "login-flow" {
		t.Fatalf("vector route must rank login-flow first: %+v", hits)
	}
}

func TestRecallMergesRoutesWithoutDuplicates(t *testing.T) {
	w := setupRecall(t)
	ctx := context.Background()
	for i, key := range []string{"login-flow", "api-constraint", "task-state"} {
		if err := w.SetEmbedding(ctx, recallTenant, recallProject, "project", key, fakeVector(i)); err != nil {
			t.Fatalf("set embedding: %v", err)
		}
	}
	hits, stats, err := w.Recall(ctx, RecallOptions{
		TenantID: recallTenant, ProjectID: recallProject, Query: "how 登录 接口", TopK: 5,
		Embedder: func(ctx context.Context, text string) ([]float32, error) {
			return fakeVector(0), nil
		},
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	seen := map[string]bool{}
	for _, h := range hits {
		if seen[h.TopicKey] {
			t.Fatalf("duplicate topic in merged result: %s", h.TopicKey)
		}
		seen[h.TopicKey] = true
		if h.Sources == nil {
			t.Fatalf("hit must carry sources: %+v", h)
		}
	}
	if stats.FullTextHits == 0 || stats.VectorHits == 0 {
		t.Fatalf("expected both routes to contribute: %+v", stats)
	}
}

func TestRecallRespectsFiltersAndTopK(t *testing.T) {
	w := setupRecall(t)
	ctx := context.Background()
	hits, _, err := w.Recall(ctx, RecallOptions{
		TenantID: recallTenant, ProjectID: recallProject, Query: "接口", TopK: 1,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) > 1 {
		t.Fatalf("topK not respected: %d", len(hits))
	}

	hits, _, err = w.Recall(ctx, RecallOptions{
		TenantID: recallTenant, ProjectID: recallProject, Query: "接口", TopK: 5,
		Categories: []string{"long_term_constraint"},
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, h := range hits {
		if h.Category != "long_term_constraint" {
			t.Fatalf("category filter ignored: %+v", h)
		}
	}

	other, _, err := w.Recall(ctx, RecallOptions{TenantID: "tenant-other", ProjectID: recallProject, Query: "接口", TopK: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("cross-tenant recall must be empty: %+v", other)
	}
}

func TestRecallLatencyBudget(t *testing.T) {
	w := setupRecall(t)
	ctx := context.Background()
	start := time.Now()
	_, stats, err := w.Recall(ctx, RecallOptions{
		TenantID: recallTenant, ProjectID: recallProject, Query: "接口 登录", TopK: 3,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	elapsed := time.Since(start)
	if stats.LatencyMs <= 0 {
		t.Fatalf("latency not recorded: %+v", stats)
	}
	if elapsed > time.Second {
		t.Fatalf("recall took %v, budget is 300ms P95", elapsed)
	}
}
