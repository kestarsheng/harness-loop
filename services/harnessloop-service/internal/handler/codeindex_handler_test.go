package handler

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
	codeindexpb "agent-platform/pkg/pb/codeindex"
	"agent-platform/pkg/pgindex"
	"agent-platform/services/harnessloop-service/internal/service"
)

const handlerSource = `package sample

import "fmt"

func LoginHandler() {
	fmt.Println("login")
}

func Caller() {
	LoginHandler()
}
`

func setupCodeIndex(t *testing.T) *CodeIndexHandler {
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
	return NewCodeIndexHandler(service.NewIndexService(writer))
}

func TestCodeIndexRPCIndexAndSearch(t *testing.T) {
	h := setupCodeIndex(t)
	ctx := context.Background()

	indexed, err := h.IndexFile(ctx, &codeindexpb.IndexFileRequest{
		TenantId: "tenant-rpc", ProjectId: "proj-rpc", FilePath: "sample.go",
		Content: []byte(handlerSource), Force: true,
	})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if indexed.GetChunks() < 3 || indexed.GetStatus() != codeindex.StatusIndexed {
		t.Fatalf("unexpected index response: %+v", indexed)
	}

	search, err := h.SearchCode(ctx, &codeindexpb.SearchCodeRequest{
		TenantId: "tenant-rpc", ProjectId: "proj-rpc", Query: "LoginHandler login", TopK: 5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(search.Hits) == 0 {
		t.Fatal("search must hit indexed chunk")
	}
	hit := search.Hits[0]
	if hit.FilePath != "sample.go" || hit.StartLine <= 0 || hit.Snippet == "" || hit.Score <= 0 {
		t.Fatalf("hit fields incomplete: %+v", hit)
	}
	if len(hit.Sources) == 0 {
		t.Fatalf("sources missing: %+v", hit)
	}
	if search.KeywordHits == 0 {
		t.Fatalf("stats = %+v", search)
	}
}

func TestCodeIndexRPCStrictTenant(t *testing.T) {
	h := setupCodeIndex(t)
	ctx := context.Background()

	if _, err := h.IndexFile(ctx, &codeindexpb.IndexFileRequest{TenantId: "tenant-rpc", ProjectId: "proj-rpc", FilePath: "sample.go", Content: []byte(handlerSource)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	other, err := h.SearchCode(ctx, &codeindexpb.SearchCodeRequest{TenantId: "other-tenant", ProjectId: "proj-rpc", Query: "LoginHandler", TopK: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(other.Hits) != 0 {
		t.Fatalf("cross-tenant must be empty: %+v", other.Hits)
	}
	if _, err := h.SearchCode(ctx, &codeindexpb.SearchCodeRequest{ProjectId: "proj-rpc", Query: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing tenant must be InvalidArgument: %v", err)
	}
}

func TestCodeIndexRPCDisabled(t *testing.T) {
	h := NewCodeIndexHandler(nil)
	_, err := h.SearchCode(context.Background(), &codeindexpb.SearchCodeRequest{TenantId: "t", ProjectId: "p", Query: "x"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("nil service must be Unavailable: %v", err)
	}
}
