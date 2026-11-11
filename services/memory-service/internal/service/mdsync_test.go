//go:build integration

package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-platform/pkg/memoryfile"
	"agent-platform/pkg/pgindex"
	mempg "agent-platform/services/memory-service/internal/pgindex"
)

func mdDoc(topics ...memoryfile.Topic) *memoryfile.Document {
	doc := &memoryfile.Document{
		SchemaVersion: memoryfile.SchemaVersion,
		Scope:         memoryfile.ScopeProject,
		TenantID:      "tenant-md",
		ProjectID:     "proj-md",
		UpdatedAt:     time.Now().UTC(),
		OrderVersion:  1,
	}
	for _, t := range topics {
		doc.Topics = append(doc.Topics, t)
		doc.Order = append(doc.Order, t.Key)
	}
	return doc
}

func mdTopic(key, title, body string) memoryfile.Topic {
	return memoryfile.Topic{
		Key: key, Title: title, Body: body,
		Meta: memoryfile.Meta{
			Category: memoryfile.CategoryProjectFact, UpdatedAt: time.Now().UTC(),
			ContentHash: memoryfile.ComputeContentHash(body),
		},
	}
}

func writeDoc(t *testing.T, path string, doc *memoryfile.Document) {
	t.Helper()
	raw, err := doc.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write md: %v", err)
	}
}

func setupMD(t *testing.T) (*mempg.MemoryWriter, *Syncer, string) {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HARNESS_TEST_PG_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	writer := mempg.NewMemoryWriter(db)
	syncer := NewSyncer(writer, MDSyncOptions{TenantID: "tenant-md", ProjectID: "proj-md", PollInterval: 200 * time.Millisecond})
	path := filepath.Join(t.TempDir(), "project.md")
	syncer.Register(path, memoryfile.ScopeProject)
	return writer, syncer, path
}

func TestMDSyncIndexesWithinDeadline(t *testing.T) {
	writer, syncer, path := setupMD(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	writeDoc(t, path, mdDoc(
		mdTopic("login-flow", "登录流程", "阶段 c 已完成登录接口，待补测试"),
		mdTopic("api-constraint", "API 约束", "所有外部接口必须以 /api/v2 开头"),
	))

	go func() { _ = syncer.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	var found []mempg.TopicRecord
	for time.Now().Before(deadline) {
		found, _ = writer.SearchTopics(ctx, "tenant-md", "proj-md", "登录 接口", 10)
		if len(found) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(found) == 0 {
		t.Fatal("markdown change must be searchable within 10s")
	}
	if found[0].TopicKey != "login-flow" {
		t.Fatalf("matched topic = %s", found[0].TopicKey)
	}

	count, err := writer.CountTopics(ctx, "tenant-md", "proj-md")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("indexed topics = %d, want 2", count)
	}
}

func TestMDSyncUpdatesAndDeletes(t *testing.T) {
	writer, syncer, path := setupMD(t)
	ctx := context.Background()

	writeDoc(t, path, mdDoc(
		mdTopic("keep", "保留", "保留主题内容"),
		mdTopic("drop", "删除", "即将删除的主题"),
	))
	if _, err := syncer.SyncFile(ctx, path, memoryfile.ScopeProject); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if count, _ := writer.CountTopics(ctx, "tenant-md", "proj-md"); count != 2 {
		t.Fatalf("count = %d", count)
	}

	writeDoc(t, path, mdDoc(mdTopic("keep", "保留", "保留主题内容已更新为 v2")))
	if _, err := syncer.SyncFile(ctx, path, memoryfile.ScopeProject); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if count, _ := writer.CountTopics(ctx, "tenant-md", "proj-md"); count != 1 {
		t.Fatalf("deleted topic must be removed, count = %d", count)
	}
	found, err := writer.SearchTopics(ctx, "tenant-md", "proj-md", "保留 v2", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) == 0 || found[0].Body != "保留主题内容已更新为 v2" {
		t.Fatalf("updated body not indexed: %+v", found)
	}
	if stale, _ := writer.SearchTopics(ctx, "tenant-md", "proj-md", "即将删除", 10); len(stale) != 0 {
		t.Fatalf("deleted topic must not be searchable: %+v", stale)
	}
}

func TestMDSyncSkipsUnchangedFile(t *testing.T) {
	_, syncer, path := setupMD(t)
	ctx := context.Background()
	writeDoc(t, path, mdDoc(mdTopic("stable", "稳定", "内容不变")))
	first, err := syncer.SyncFile(ctx, path, memoryfile.ScopeProject)
	if err != nil || first.Skipped {
		t.Fatalf("first sync: %v %+v", err, first)
	}
	second, err := syncer.SyncFile(ctx, path, memoryfile.ScopeProject)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if !second.Skipped {
		t.Fatalf("unchanged file must be skipped: %+v", second)
	}
}

func TestMarkdownWritesIndependentOfService(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.md")
	writeDoc(t, path, mdDoc(mdTopic("offline", "离线", "服务未启动也能写 md")))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("md must be writable without memory-service: %v", err)
	}
	if _, err := memoryfile.Parse(raw); err != nil {
		t.Fatalf("written md must be valid: %v", err)
	}
}
