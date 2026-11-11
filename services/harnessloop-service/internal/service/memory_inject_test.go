package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/memoryfile"
)

type stubRecaller struct {
	hits           []MemoryHit
	err            error
	calls          int
	lastCategories []string
}

func (s *stubRecaller) Recall(ctx context.Context, tenantID, projectID, query string, categories []string, topK int) ([]MemoryHit, error) {
	s.calls++
	s.lastCategories = categories
	return s.hits, s.err
}

func hit(key, category, body string, score float64, pinned bool) MemoryHit {
	return MemoryHit{
		TopicKey: key, Title: key, Body: body, Category: category,
		SourceFile: "project.md", UpdatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC),
		Score: score, Pinned: pinned,
	}
}

func TestMemoryInjectPlanStageUsesConstraintsAndFacts(t *testing.T) {
	recaller := &stubRecaller{hits: []MemoryHit{
		hit("constraint", "long_term_constraint", "禁止使用 mysql", 0.9, true),
		hit("fact", "project_fact", "服务端口 50008", 0.8, false),
		hit("state", "task_state", "阶段 c 进行中", 0.7, false),
	}}
	injector := NewMemoryInjector(recaller, nil)
	res, err := injector.Inject(context.Background(), InjectOptions{
		TenantID: "t1", ProjectID: "p1", Stage: "b", Query: "计划",
		ContextBudget: 10000,
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if recaller.calls != 1 {
		t.Fatalf("recaller calls = %d", recaller.calls)
	}
	if strings.Join(recaller.lastCategories, ",") != "long_term_constraint,project_fact" {
		t.Fatalf("categories = %v", recaller.lastCategories)
	}
	for _, item := range res.Items {
		if item.Category == "task_state" {
			t.Fatalf("b stage must not inject task_state: %+v", item)
		}
		if item.TopicKey == "" || item.SourceFile == "" || item.UpdatedAt.IsZero() || item.Score == 0 {
			t.Fatalf("injected item must carry 4 metadata fields: %+v", item)
		}
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(res.Items))
	}
	if res.Ratio > DefaultInjectRatio+1e-9 {
		t.Fatalf("ratio = %f exceeds 15%%", res.Ratio)
	}
}

func TestMemoryInjectBudgetTruncates(t *testing.T) {
	body := strings.Repeat("很长的记忆内容", 200)
	recaller := &stubRecaller{hits: []MemoryHit{
		hit("a", "project_fact", body, 0.9, false),
		hit("b", "project_fact", body, 0.8, false),
		hit("c", "long_term_constraint", body, 0.7, true),
	}}
	injector := NewMemoryInjector(recaller, nil)
	res, err := injector.Inject(context.Background(), InjectOptions{
		TenantID: "t1", ProjectID: "p1", Stage: "b", Query: "x",
		ContextBudget: 1000, Ratio: 0.15,
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if res.TokensUsed > 150 {
		t.Fatalf("tokens used %d exceeds 15%% of 1000", res.TokensUsed)
	}
	if len(res.Items) == 0 {
		t.Fatal("at least one item must be injected")
	}
	truncated := false
	for _, item := range res.Items {
		if strings.HasSuffix(item.Body, "…[truncated]") {
			truncated = true
		}
	}
	if !truncated {
		t.Fatalf("long body must be truncated: %+v", res.Items)
	}
}

func TestMemoryInjectDegradesToMarkdown(t *testing.T) {
	dir := t.TempDir()
	projectPath := filepath.Join(dir, "project.md")
	doc := &memoryfile.Document{
		SchemaVersion: memoryfile.SchemaVersion, Scope: memoryfile.ScopeProject,
		UpdatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), OrderVersion: 1,
		Order: []string{"plan-fact", "state"},
		Topics: []memoryfile.Topic{
			{Key: "plan-fact", Title: "计划事实", Body: "计划必须遵守端口 50008 约束",
				Meta: memoryfile.Meta{Category: memoryfile.CategoryProjectFact, Pinned: true, UpdatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}},
			{Key: "state", Title: "任务状态", Body: "阶段 c 进行中",
				Meta: memoryfile.Meta{Category: memoryfile.CategoryTaskState, UpdatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}},
		},
	}
	raw, err := doc.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := os.WriteFile(projectPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	events := &captureEvents{}
	recaller := &stubRecaller{err: errors.New("pg unavailable")}
	injector := NewMemoryInjector(recaller, &MDFallback{ProjectFile: projectPath}).WithEvents(events)
	res, err := injector.Inject(context.Background(), InjectOptions{
		TenantID: "t1", ProjectID: "p1", Stage: "b", Query: "计划 端口", ContextBudget: 10000,
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !res.Degraded {
		t.Fatal("degraded flag must be set")
	}
	if len(res.Items) == 0 {
		t.Fatal("fallback must inject at least one item")
	}
	if res.Items[0].TopicKey != "plan-fact" {
		t.Fatalf("pinned relevant topic must rank first: %+v", res.Items)
	}
	if events.count(event.MemoryIndexDegraded) != 1 {
		t.Fatalf("degraded events = %d", events.count(event.MemoryIndexDegraded))
	}
}

func TestMemoryInjectEmptyForTerminalStage(t *testing.T) {
	recaller := &stubRecaller{hits: []MemoryHit{hit("x", "task_state", "状态", 1, false)}}
	injector := NewMemoryInjector(recaller, nil)
	res, err := injector.Inject(context.Background(), InjectOptions{TenantID: "t1", ProjectID: "p1", Stage: "e", ContextBudget: 1000})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if len(res.Items) != 0 || recaller.calls != 0 {
		t.Fatalf("e stage must inject nothing: %+v calls=%d", res, recaller.calls)
	}
}

func TestMemoryInjectNoRecallerDegrades(t *testing.T) {
	dir := t.TempDir()
	projectPath := filepath.Join(dir, "project.md")
	doc := &memoryfile.Document{
		SchemaVersion: memoryfile.SchemaVersion, Scope: memoryfile.ScopeProject,
		UpdatedAt: time.Now().UTC(), OrderVersion: 1,
		Order: []string{"fact"},
		Topics: []memoryfile.Topic{{Key: "fact", Title: "事实", Body: "项目使用 PostgreSQL",
			Meta: memoryfile.Meta{Category: memoryfile.CategoryProjectFact, UpdatedAt: time.Now().UTC()}}},
	}
	raw, _ := doc.Render()
	os.WriteFile(projectPath, raw, 0o644)

	events := &captureEvents{}
	injector := NewMemoryInjector(nil, &MDFallback{ProjectFile: projectPath}).WithEvents(events)
	res, err := injector.Inject(context.Background(), InjectOptions{TenantID: "t1", ProjectID: "p1", Stage: "c", Query: "PostgreSQL", ContextBudget: 10000})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !res.Degraded || len(res.Items) == 0 {
		t.Fatalf("missing recaller must degrade to markdown: %+v", res)
	}
	if events.count(event.MemoryIndexDegraded) != 1 {
		t.Fatalf("degraded events = %d", events.count(event.MemoryIndexDegraded))
	}
}

func TestEstimateTokensAndTruncation(t *testing.T) {
	if EstimateTokens("") != 0 {
		t.Fatal("empty text must estimate 0 tokens")
	}
	if EstimateTokens("abcd") != 2 {
		t.Fatalf("estimate = %d", EstimateTokens("abcd"))
	}
	body := strings.Repeat("字", 1000)
	got, tokens := truncateToTokens(body, 10)
	if tokens > 10 || !strings.HasSuffix(got, "…[truncated]") {
		t.Fatalf("truncation failed: tokens=%d body=%q", tokens, got[:20])
	}
}
