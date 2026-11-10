package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func ruleIDs(vs []violation) map[string]bool {
	ids := make(map[string]bool, len(vs))
	for _, v := range vs {
		ids[v.Rule] = true
	}
	return ids
}

func TestRepositoryIsClean(t *testing.T) {
	vs, err := check("../..")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(vs) != 0 {
		for _, v := range vs {
			t.Errorf("unexpected violation: %s", v.String())
		}
	}
}

func TestDetectsViolations(t *testing.T) {
	cases := []struct {
		name string
		rel  string
		src  string
		want string
	}{
		{
			name: "harnessloop importing service",
			rel:  "pkg/harnessloop/fsm/a.go",
			src:  "package fsm\n\nimport _ \"agent-platform/services/chat-service/internal/service\"\n",
			want: "R1",
		},
		{
			name: "memoryfile importing llm",
			rel:  "pkg/memoryfile/a.go",
			src:  "package memoryfile\n\nimport _ \"agent-platform/pkg/llm\"\n",
			want: "R2",
		},
		{
			name: "rrf importing database",
			rel:  "pkg/rrf/a.go",
			src:  "package rrf\n\nimport _ \"agent-platform/pkg/qdrant\"\n",
			want: "R3",
		},
		{
			name: "service importing another service internal",
			rel:  "services/chat-service/internal/service/a.go",
			src:  "package service\n\nimport _ \"agent-platform/services/memory-service/internal/repository\"\n",
			want: "R4",
		},
		{
			name: "event type declared outside event package",
			rel:  "pkg/foo/a.go",
			src:  "package foo\n\ntype HarnessEvent struct{}\n",
			want: "R5",
		},
		{
			name: "orchestrator importing agent-service",
			rel:  "pkg/harnessloop/orchestrator/a.go",
			src:  "package orchestrator\n\nimport _ \"agent-platform/services/agent-service/internal/service\"\n",
			want: "R6",
		},
		{
			name: "policy importing llm",
			rel:  "pkg/harnessloop/policy/a.go",
			src:  "package policy\n\nimport _ \"agent-platform/pkg/llm\"\n",
			want: "R7",
		},
		{
			name: "sandbox importing database",
			rel:  "pkg/harnessloop/policy/sandbox/a.go",
			src:  "package sandbox\n\nimport _ \"github.com/jackc/pgx/v5\"\n",
			want: "R7",
		},
		{
			name: "modelregistry importing service",
			rel:  "pkg/harnessloop/modelregistry/a.go",
			src:  "package modelregistry\n\nimport _ \"agent-platform/services/gateway/internal/router\"\n",
			want: "R8",
		},
		{
			name: "llm importing modelregistry",
			rel:  "pkg/llm/a.go",
			src:  "package llm\n\nimport _ \"agent-platform/pkg/harnessloop/modelregistry\"\n",
			want: "R8",
		},
		{
			name: "compaction importing external engine",
			rel:  "pkg/harnessloop/compaction/a.go",
			src:  "package compaction\n\nimport _ \"github.com/Context-Engine-AI/Context-Engine/pkg/compress\"\n",
			want: "R9",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, tc.rel, tc.src)
			vs, err := check(root)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if !ruleIDs(vs)[tc.want] {
				t.Fatalf("expected rule %s, got %v", tc.want, vs)
			}
		})
	}
}

func TestGeneratedProtobufIsExempt(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "pkg/pb/harnessloop/harnessloop.pb.go",
		"package harnessloop\n\ntype HarnessEvent struct{}\n")
	writeFile(t, root, "pkg/pb/other/x_grpc.pb.go",
		"package other\n\nimport _ \"agent-platform/services/chat-service/internal/service\"\n")
	vs, err := check(root)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(vs) != 0 {
		t.Fatalf("generated .pb.go must be exempt, got %v", vs)
	}
}

func TestNoFalsePositiveOnStdlib(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "pkg/harnessloop/fsm/a.go", "package fsm\n\nimport \"context\"\n\nvar _ context.Context\n")
	writeFile(t, root, "pkg/rrf/a.go", "package rrf\n\nimport \"compress/gzip\"\n\nvar _ gzip.Header\n")
	vs, err := check(root)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(vs) != 0 {
		t.Fatalf("expected no violations, got %v", vs)
	}
}
