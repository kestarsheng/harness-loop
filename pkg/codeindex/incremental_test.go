package codeindex_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
	"agent-platform/pkg/pgindex"
)

const (
	tenantID  = "tenant-code"
	projectID = "proj-code"
)

func setupWriter(t *testing.T) *codeindex.CodeWriter {
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
	registry := parser.NewRegistry()
	return codeindex.NewCodeWriter(db, func(path string, src []byte) (codeindex.ParseResult, error) {
		return registry.Parse(path, src), nil
	}, nil, nil)
}

func sourceA() string {
	return `package sample

import "fmt"

func Alpha() { fmt.Println() }
func Beta() {}
`
}

func sourceB() string {
	return `package sample

import (
	"fmt"
	"os"
)

func Alpha() {
	fmt.Println()
	os.Getenv("X")
}
func Beta() {}
func Gamma() {}
`
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func chunkIDs(t *testing.T, w *codeindex.CodeWriter, path string) []string {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(ctx, `SELECT chunk_id::text FROM hlp_code.code_chunks WHERE tenant_id=$1 AND project_id=$2 AND file_path=$3 ORDER BY chunk_id`, tenantID, projectID, path)
	if err != nil {
		t.Fatalf("query chunks: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func TestIncrementalFirstIndexAndSkip(t *testing.T) {
	w := setupWriter(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "sample.go", sourceA())
	ctx := context.Background()

	res, err := w.IndexFile(ctx, tenantID, projectID, path, []byte(sourceA()), time.Now().UTC(), false)
	if err != nil {
		t.Fatalf("first index: %v", err)
	}
	if res.Skipped || res.Chunks < 3 {
		t.Fatalf("unexpected first result: %+v", res)
	}
	if res.Edges < 1 {
		t.Fatalf("expected import edge: %+v", res)
	}
	state, err := w.GetFileState(ctx, tenantID, projectID, path)
	if err != nil || state == nil {
		t.Fatalf("state: %v %v", state, err)
	}
	if state.Status != codeindex.StatusIndexed || state.FailureCount != 0 {
		t.Fatalf("state = %+v", state)
	}

	before := chunkIDs(t, w, path)
	again, err := w.IndexFile(ctx, tenantID, projectID, path, []byte(sourceA()), time.Now().UTC(), false)
	if err != nil {
		t.Fatalf("second index: %v", err)
	}
	if !again.Skipped {
		t.Fatalf("identical content must be skipped: %+v", again)
	}
	after := chunkIDs(t, w, path)
	if len(before) != len(after) {
		t.Fatalf("skipped index must not rewrite chunks: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatal("skipped index changed chunk ids")
		}
	}
}

func TestIncrementalChangeReplacesOnlyThatFile(t *testing.T) {
	w := setupWriter(t)
	dir := t.TempDir()
	pathA := writeFile(t, dir, "a.go", sourceA())
	pathB := writeFile(t, dir, "b.go", "package sample\n\nfunc Other() {}\n")
	ctx := context.Background()

	if _, err := w.IndexFile(ctx, tenantID, projectID, pathA, []byte(sourceA()), time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.IndexFile(ctx, tenantID, projectID, pathB, []byte("package sample\n\nfunc Other() {}\n"), time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	oldB := chunkIDs(t, w, pathB)
	oldA := chunkIDs(t, w, pathA)

	res, err := w.IndexFile(ctx, tenantID, projectID, pathA, []byte(sourceB()), time.Now().UTC(), true)
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if res.Chunks < 4 {
		t.Fatalf("changed file must include new chunks: %+v", res)
	}
	newA := chunkIDs(t, w, pathA)
	newB := chunkIDs(t, w, pathB)
	if len(newA) == 0 {
		t.Fatal("changed file lost its chunks")
	}
	for _, id := range oldA {
		for _, keep := range newA {
			if id == keep {
				t.Fatalf("stale chunk %s must be replaced", id)
			}
		}
	}
	if len(oldB) != len(newB) {
		t.Fatalf("unrelated file changed: %d -> %d", len(oldB), len(newB))
	}
}

func TestIncrementalParseFailureKeepsOldIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.go")
	if err := os.WriteFile(path, []byte("package broken\n\nfunc broken( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HARNESS_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(dsn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	registry := parser.NewRegistry()
	goodWriter := codeindex.NewCodeWriter(db, func(p string, src []byte) (codeindex.ParseResult, error) {
		return registry.Parse(p, src), nil
	}, nil, nil)
	if _, err := goodWriter.IndexFile(ctx, tenantID, projectID, path, []byte(sourceA()), time.Now().UTC(), false); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	before := chunkIDs(t, goodWriter, path)

	failing := codeindex.NewCodeWriter(db, func(p string, src []byte) (codeindex.ParseResult, error) {
		return codeindex.ParseResult{}, errors.New("injected parse failure")
	}, nil, goodWriter.Metrics())
	_, err = failing.IndexFile(ctx, tenantID, projectID, path, []byte("package broken\n"), time.Now().UTC(), true)
	if !errors.Is(err, codeindex.ErrParseFailed) {
		t.Fatalf("expected parse failure, got %v", err)
	}
	state, err := failing.GetFileState(ctx, tenantID, projectID, path)
	if err != nil || state == nil {
		t.Fatalf("state: %v %v", state, err)
	}
	if state.Status != codeindex.StatusFailed || state.FailureCount < 1 {
		t.Fatalf("failed state = %+v", state)
	}
	after := chunkIDs(t, failing, path)
	if len(after) != len(before) {
		t.Fatalf("old index must survive failed reindex: %d -> %d", len(before), len(after))
	}
}

func TestIncrementalDeleteFile(t *testing.T) {
	w := setupWriter(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "del.go", sourceA())
	ctx := context.Background()
	if _, err := w.IndexFile(ctx, tenantID, projectID, path, []byte(sourceA()), time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	if err := w.DeleteFile(ctx, tenantID, projectID, path); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, err := w.CountChunks(ctx, tenantID, projectID, path); err != nil || n != 0 {
		t.Fatalf("chunks after delete = %d err=%v", n, err)
	}
	if n, err := w.CountEdges(ctx, tenantID, projectID, path); err != nil || n != 0 {
		t.Fatalf("edges after delete = %d err=%v", n, err)
	}
	if state, err := w.GetFileState(ctx, tenantID, projectID, path); err != nil || state != nil {
		t.Fatalf("state after delete = %+v err=%v", state, err)
	}
}

func TestIncrementalMetrics(t *testing.T) {
	w := setupWriter(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "m.go", sourceA())
	ctx := context.Background()

	if _, err := w.IndexFile(ctx, tenantID, projectID, path, []byte(sourceA()), time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.IndexFile(ctx, tenantID, projectID, path, []byte(sourceA()), time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	snapshot := w.Metrics().Snapshot()
	if snapshot.Indexed < 1 || snapshot.Skipped < 1 {
		t.Fatalf("metrics = %+v", snapshot)
	}
	if snapshot.Chunks == 0 {
		t.Fatalf("chunk counter not recorded: %+v", snapshot)
	}
}

func TestIncrementalWriterWithoutParser(t *testing.T) {
	w := codeindex.NewCodeWriter(nil, nil, nil, nil)
	if _, err := w.IndexFile(context.Background(), tenantID, projectID, "x.go", []byte("package x\n"), time.Now(), false); !errors.Is(err, codeindex.ErrNoParser) {
		t.Fatalf("missing parser must fail: %v", err)
	}
}
