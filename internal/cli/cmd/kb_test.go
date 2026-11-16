package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"

	knowledgepb "agent-platform/pkg/pb/knowledge"
)

type fakeKnowledgeServer struct {
	knowledgepb.UnimplementedKnowledgeServiceServer
	getCalls   atomic.Int32
	uploadMode atomic.Bool
}

func (s *fakeKnowledgeServer) Search(context.Context, *knowledgepb.SearchRequest) (*knowledgepb.SearchResponse, error) {
	return &knowledgepb.SearchResponse{
		Results: []*knowledgepb.SearchResult{{
			ChunkId: "ch-1", DocumentId: "doc-1", Content: "harness loop design", Score: 0.75,
			Sources: []*knowledgepb.SourceRef{{
				ChunkId: "ch-1", DocumentId: "doc-1", DocumentName: "design.md",
				Uri: "/api/v2/knowledge/documents/doc-1", Snippet: "harness loop design", Score: 0.75,
			}},
		}},
		Total: 1, LatencyMs: 7,
	}, nil
}

func (s *fakeKnowledgeServer) Upload(context.Context, *knowledgepb.UploadRequest) (*knowledgepb.UploadResponse, error) {
	return &knowledgepb.UploadResponse{
		DocumentId: "doc-1", Filename: "a.txt", ChunkCount: 2, Status: "processing",
	}, nil
}

func (s *fakeKnowledgeServer) GetDocument(context.Context, *knowledgepb.GetDocumentRequest) (*knowledgepb.Document, error) {
	status := "ready"
	if s.uploadMode.Load() && s.getCalls.Add(1) == 1 {
		status = "processing"
	}
	return &knowledgepb.Document{
		Id: "doc-1", Filename: "a.txt", Title: "Doc One", Status: status, ChunkCount: 2,
		Content: "hello harness", CreatedAt: 1700000000, UpdatedAt: 1700000001,
	}, nil
}

func startFakeKnowledge(t *testing.T) (*fakeKnowledgeServer, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	stub := &fakeKnowledgeServer{}
	knowledgepb.RegisterKnowledgeServiceServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return stub, lis.Addr().String()
}

func runKBCommand(t *testing.T, addr string, args ...string) (string, error) {
	t.Helper()
	resetOpts()
	t.Cleanup(resetOpts)
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"--kb-server", addr}, args...))
	err := root.Execute()
	return buf.String(), err
}

func TestKBSearchCommand(t *testing.T) {
	_, addr := startFakeKnowledge(t)
	out, err := runKBCommand(t, addr, "--json", "kb", "search", "--query", "harness", "--top-k", "3")
	if err != nil {
		t.Fatalf("kb search: %v\n%s", err, out)
	}
	var payload kbSearchOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out)
	}
	if !payload.OK || payload.Total != 1 || len(payload.Results) != 1 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	got := payload.Results[0]
	if got.Title != "design.md" || got.Score != 0.75 || !strings.Contains(got.Source, "doc-1") {
		t.Fatalf("unexpected result: %+v", got)
	}
	if got.Snippet != "harness loop design" || got.DocumentID != "doc-1" || got.ChunkID != "ch-1" {
		t.Fatalf("result missing title/snippet/source/score fields: %+v", got)
	}
}

func TestKBSearchCommandHumanOutput(t *testing.T) {
	_, addr := startFakeKnowledge(t)
	out, err := runKBCommand(t, addr, "kb", "search", "--query", "harness")
	if err != nil {
		t.Fatalf("kb search: %v\n%s", err, out)
	}
	for _, want := range []string{"results=1", "score=0.7500", "title=design.md", "source=/api/v2/knowledge/documents/doc-1", "snippet=harness loop design"} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output missing %q:\n%s", want, out)
		}
	}
}

func TestKBUploadFollowsDocumentStates(t *testing.T) {
	stub, addr := startFakeKnowledge(t)
	stub.uploadMode.Store(true)
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("hello harness"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runKBCommand(t, addr, "--json", "kb", "upload", "--file", path, "--timeout", "3s")
	if err != nil {
		t.Fatalf("kb upload: %v\n%s", err, out)
	}
	var payload kbUploadOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out)
	}
	if !payload.OK || payload.DocumentID != "doc-1" || payload.Status != "ready" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	var states []string
	for _, state := range payload.States {
		states = append(states, state.Status)
	}
	if strings.Join(states, "→") != "pending→processing→ready" {
		t.Fatalf("state machine = %v, want pending→processing→ready", states)
	}
}

func TestKBShowCommand(t *testing.T) {
	_, addr := startFakeKnowledge(t)
	out, err := runKBCommand(t, addr, "--json", "kb", "show", "--id", "doc-1")
	if err != nil {
		t.Fatalf("kb show: %v\n%s", err, out)
	}
	var payload kbShowOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out)
	}
	if !payload.OK || payload.Document.ID != "doc-1" || payload.Document.Title != "Doc One" ||
		payload.Document.Status != "ready" || payload.Document.ChunkCount != 2 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if payload.Document.Content != "hello harness" {
		t.Fatalf("content = %q", payload.Document.Content)
	}
}

func TestKBCommandValidation(t *testing.T) {
	_, addr := startFakeKnowledge(t)
	if _, err := runKBCommand(t, addr, "kb", "search"); err == nil || !strings.Contains(err.Error(), "--query is required") {
		t.Fatalf("search must require --query, got %v", err)
	}
	if _, err := runKBCommand(t, addr, "kb", "upload"); err == nil || !strings.Contains(err.Error(), "--file is required") {
		t.Fatalf("upload must require --file, got %v", err)
	}
	if _, err := runKBCommand(t, addr, "kb", "show"); err == nil || !strings.Contains(err.Error(), "--id is required") {
		t.Fatalf("show must require --id, got %v", err)
	}
}

func TestResolveKBServerPrecedence(t *testing.T) {
	resetOpts()
	t.Cleanup(resetOpts)
	t.Setenv("HARNESS_KB_SERVER", "")

	if got := resolveKBServer(fileConfig{}); got != DefaultKBServerAddr {
		t.Fatalf("default kb server = %s", got)
	}
	if DefaultKBServerAddr != "127.0.0.1:50002" {
		t.Fatalf("default kb server must target knowledge-service: %s", DefaultKBServerAddr)
	}
	if got := resolveKBServer(fileConfig{KBServer: "file:50002"}); got != "file:50002" {
		t.Fatalf("file kb server = %s", got)
	}
	t.Setenv("HARNESS_KB_SERVER", "env:50002")
	if got := resolveKBServer(fileConfig{KBServer: "file:50002"}); got != "env:50002" {
		t.Fatalf("env kb server = %s", got)
	}
	opts.KBServer = "flag:50002"
	if got := resolveKBServer(fileConfig{KBServer: "file:50002"}); got != "flag:50002" {
		t.Fatalf("flag kb server = %s", got)
	}
}
