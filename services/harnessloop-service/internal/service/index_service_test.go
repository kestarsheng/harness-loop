package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
	"agent-platform/pkg/pgindex"
)

const (
	idxTenant  = "tenant-idx"
	idxProject = "proj-idx"
)

const idxSource = `package sample

import "fmt"

// LoginFlow handles login.
func LoginFlow() {
	fmt.Println("login")
}

func Caller() {
	LoginFlow()
}
`

type searchFixture struct {
	writer *codeindex.CodeWriter
	db     *pgindex.DB
	path   string
}

func setupSearch(t *testing.T) searchFixture {
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
	writer := codeindex.NewCodeWriter(db, func(path string, src []byte) (codeindex.ParseResult, error) {
		return registry.Parse(path, src), nil
	}, nil, nil)

	path := filepath.Join(t.TempDir(), "sample.go")
	if err := os.WriteFile(path, []byte(idxSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.IndexFile(ctx, idxTenant, idxProject, path, []byte(idxSource), time.Now().UTC(), true); err != nil {
		t.Fatalf("index: %v", err)
	}
	return searchFixture{writer: writer, db: db, path: path}
}

func TestSearchCodeKeyword(t *testing.T) {
	f := setupSearch(t)
	svc := NewIndexService(f.writer)
	ctx := context.Background()

	res, err := svc.Search(ctx, idxTenant, idxProject, "LoginFlow login", 5, nil, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("keyword search must hit LoginFlow")
	}
	hit := res.Hits[0]
	if hit.FilePath == "" || hit.StartLine <= 0 || hit.EndLine < hit.StartLine {
		t.Fatalf("hit must carry file/line: %+v", hit)
	}
	if hit.Snippet == "" || hit.ChunkID == "" || hit.Scores <= 0 {
		t.Fatalf("hit must carry snippet/score: %+v", hit)
	}
	if len(hit.Sources) == 0 || hit.Sources[0] != "keyword" {
		t.Fatalf("sources = %v", hit.Sources)
	}
	if res.Stats.KeywordHits == 0 || res.Stats.LatencyMs < 0 {
		t.Fatalf("stats = %+v", res.Stats)
	}
}

func TestSearchCodeVectorRoute(t *testing.T) {
	f := setupSearch(t)
	ctx := context.Background()
	vec := make([]float32, 1024)
	vec[0] = 1
	if _, err := f.db.Exec(ctx, `
		UPDATE hlp_code.code_chunks SET embedding = $1::vector
		WHERE tenant_id = $2 AND project_id = $3 AND symbol = 'LoginFlow'`,
		"[1"+strings.Repeat(",0", 1023)+"]", idxTenant, idxProject); err != nil {
		t.Fatalf("set embedding: %v", err)
	}

	svc := NewIndexService(f.writer)
	embedder := func(ctx context.Context, texts []string) ([][]float32, error) {
		return [][]float32{vec}, nil
	}
	res, err := svc.Search(ctx, idxTenant, idxProject, "unrelated words", 5, embedder, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Stats.VectorHits == 0 {
		t.Fatalf("vector route must contribute: %+v", res.Stats)
	}
	found := false
	for _, hit := range res.Hits {
		for _, source := range hit.Sources {
			if source == "vector" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("hit must report vector source: %+v", res.Hits)
	}
}

func TestSearchCodeGraphExpansion(t *testing.T) {
	f := setupSearch(t)
	ctx := context.Background()
	svc := NewIndexService(f.writer)

	withoutGraph, err := svc.Search(ctx, idxTenant, idxProject, "LoginFlow", 5, nil, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	withGraph, err := svc.Search(ctx, idxTenant, idxProject, "LoginFlow", 5, nil, true)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if withGraph.Stats.GraphHits == 0 {
		t.Fatalf("graph route must expand callers: %+v", withGraph.Stats)
	}
	if len(withGraph.Hits) < len(withoutGraph.Hits) {
		t.Fatalf("graph expansion must not drop hits: %d -> %d", len(withoutGraph.Hits), len(withGraph.Hits))
	}
}

func TestSearchCodeIsolation(t *testing.T) {
	f := setupSearch(t)
	svc := NewIndexService(f.writer)
	ctx := context.Background()

	other, err := svc.Search(ctx, idxTenant, "another-project", "LoginFlow", 5, nil, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(other.Hits) != 0 {
		t.Fatalf("cross-project search must be empty: %+v", other.Hits)
	}
	otherTenant, err := svc.Search(ctx, "another-tenant", idxProject, "LoginFlow", 5, nil, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(otherTenant.Hits) != 0 {
		t.Fatalf("cross-tenant search must be empty: %+v", otherTenant.Hits)
	}

	if _, err := svc.Search(ctx, "", idxProject, "x", 5, nil, false); !errors.Is(err, ErrTenantRequired) {
		t.Fatalf("missing tenant must fail: %v", err)
	}
	if _, err := NewIndexService(nil).Search(ctx, "t", "p", "x", 5, nil, false); !errors.Is(err, ErrIndexDisabled) {
		t.Fatalf("nil writer must fail: %v", err)
	}
}

func TestSearchCodeDeterministicOrder(t *testing.T) {
	f := setupSearch(t)
	svc := NewIndexService(f.writer)
	ctx := context.Background()

	first, err := svc.Search(ctx, idxTenant, idxProject, "LoginFlow login sample", 5, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var firstIDs []string
	for _, hit := range first.Hits {
		firstIDs = append(firstIDs, hit.ChunkID)
	}
	for i := 0; i < 5; i++ {
		next, err := svc.Search(ctx, idxTenant, idxProject, "LoginFlow login sample", 5, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, hit := range next.Hits {
			ids = append(ids, hit.ChunkID)
		}
		if strings.Join(ids, ",") != strings.Join(firstIDs, ",") {
			t.Fatalf("order changed on run %d: %v vs %v", i, ids, firstIDs)
		}
	}
}

func TestSearchCodeEmptyQuery(t *testing.T) {
	f := setupSearch(t)
	svc := NewIndexService(f.writer)
	res, err := svc.Search(context.Background(), idxTenant, idxProject, "   ", 5, nil, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("empty query must return no hits: %+v", res.Hits)
	}
}
