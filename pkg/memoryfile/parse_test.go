package memoryfile

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func sampleDoc() *Document {
	ts := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	return &Document{
		SchemaVersion: SchemaVersion,
		Scope:         ScopeProject,
		TenantID:      "t-001",
		ProjectID:     "repo-a",
		UpdatedAt:     ts,
		OrderVersion:  42,
		Order:         []string{"constraints-api", "task-login-flow", "pref-commit-style"},
		Topics: []Topic{
			{
				Key: "constraints-api", Title: "API 约束",
				Body: "所有外部接口必须以 `/api/v2` 开头，错误码使用 `E_*` 前缀。",
				Meta: Meta{Category: CategoryLongTermConstraint, Pinned: true, UpdatedAt: ts, ContentHash: ComputeContentHash("所有外部接口必须以 `/api/v2` 开头，错误码使用 `E_*` 前缀。")},
			},
			{
				Key: "task-login-flow", Title: "当前阶段",
				Body: "阶段 c 第 2 次尝试：已完成登录接口，待补测试。",
				Meta: Meta{Category: CategoryTaskState, UpdatedAt: ts},
			},
			{
				Key: "pref-commit-style", Title: "提交规范",
				Body: "使用 conventional commits，英文描述。",
				Meta: Meta{Category: CategoryUserPreference, UpdatedAt: ts},
			},
		},
	}
}

func TestParseSampleDocument(t *testing.T) {
	doc := sampleDoc()
	raw, err := doc.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Scope != ScopeProject || parsed.ProjectID != "repo-a" || parsed.OrderVersion != 42 {
		t.Fatalf("frontmatter = %+v", parsed)
	}
	if len(parsed.Topics) != 3 || len(parsed.Order) != 3 {
		t.Fatalf("topics=%d order=%d", len(parsed.Topics), len(parsed.Order))
	}
	if parsed.Topics[0].Key != "constraints-api" || !parsed.Topics[0].Meta.Pinned {
		t.Fatalf("first topic = %+v", parsed.Topics[0])
	}
	if parsed.Topics[0].Title != "API 约束" {
		t.Fatalf("title = %q", parsed.Topics[0].Title)
	}
	if !strings.Contains(parsed.Topics[0].Body, "/api/v2") {
		t.Fatalf("body = %q", parsed.Topics[0].Body)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"no frontmatter", "## a\n", ErrInvalidFrontmatter},
		{"unclosed frontmatter", "---\nschema_version: 1\n", ErrInvalidFrontmatter},
		{"unknown frontmatter key", "---\nschema_version: 1\nscope: project\nfoo: bar\n---\n\n<!-- order: [] -->\n", ErrInvalidFrontmatter},
		{"bad scope", "---\nschema_version: 1\nscope: session\nupdated_at: 2026-11-10T10:00:00Z\n---\n\n<!-- order: [] -->\n", ErrInvalidFrontmatter},
		{"bad schema", "---\nschema_version: 9\nscope: project\nupdated_at: 2026-11-10T10:00:00Z\n---\n\n<!-- order: [] -->\n", ErrInvalidFrontmatter},
		{"bad order json", "---\nschema_version: 1\nscope: project\nupdated_at: 2026-11-10T10:00:00Z\n---\n\n<!-- order: nope -->\n", ErrInvalidFrontmatter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.raw)); !errors.Is(err, c.want) {
				t.Fatalf("want %v, got %v", c.want, err)
			}
		})
	}
}

func TestParseRejectsInvalidTopicMeta(t *testing.T) {
	base := func(topicBlock string) string {
		return "---\nschema_version: 1\nscope: project\nupdated_at: 2026-11-10T10:00:00Z\norder_version: 1\n---\n\n<!-- order: [\"a\"] -->\n\n" + topicBlock
	}
	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"bad key", base("## Bad_Key\n<!-- meta: category=task_state; pinned=false; updated_at=2026-11-10T10:00:00Z; content_hash=x -->\n\n### t\nb\n"), ErrInvalidTopicKey},
		{"missing meta", base("## a\n\n### t\nb\n"), ErrInvalidMeta},
		{"unknown category", base("## a\n<!-- meta: category=nope; pinned=false; updated_at=2026-11-10T10:00:00Z; content_hash=x -->\n\n### t\nb\n"), ErrInvalidMeta},
		{"missing title", base("## a\n<!-- meta: category=task_state; pinned=false; updated_at=2026-11-10T10:00:00Z; content_hash=x -->\n\nb\n"), ErrInvalidMeta},
		{"bad meta time", base("## a\n<!-- meta: category=task_state; pinned=false; updated_at=bad; content_hash=x -->\n\n### t\nb\n"), ErrInvalidMeta},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.raw)); !errors.Is(err, c.want) {
				t.Fatalf("want %v, got %v", c.want, err)
			}
		})
	}
}

func TestParseRejectsDuplicateAndOrderMismatch(t *testing.T) {
	dup := `---
schema_version: 1
scope: project
updated_at: 2026-11-10T10:00:00Z
order_version: 1
---

<!-- order: ["a","a"] -->

## a
<!-- meta: category=task_state; pinned=false; updated_at=2026-11-10T10:00:00Z; content_hash=x -->

### t
b

## a
<!-- meta: category=task_state; pinned=false; updated_at=2026-11-10T10:00:00Z; content_hash=x -->

### t
b
`
	if _, err := Parse([]byte(dup)); !errors.Is(err, ErrDuplicateTopic) {
		t.Fatalf("duplicate key must fail: %v", err)
	}

	mismatch := `---
schema_version: 1
scope: project
updated_at: 2026-11-10T10:00:00Z
order_version: 1
---

<!-- order: ["a","b"] -->

## a
<!-- meta: category=task_state; pinned=false; updated_at=2026-11-10T10:00:00Z; content_hash=x -->

### t
b
`
	if _, err := Parse([]byte(mismatch)); !errors.Is(err, ErrOrderMismatch) {
		t.Fatalf("order mismatch must fail: %v", err)
	}
}

func TestValidateRejectsOversizedBody(t *testing.T) {
	doc := sampleDoc()
	doc.Topics[0].Body = strings.Repeat("甲", MaxBodyChars+1)
	if err := doc.Validate(); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("oversized body must fail: %v", err)
	}
}

func TestComputeContentHashStable(t *testing.T) {
	a := ComputeContentHash("hello")
	b := ComputeContentHash("  hello  ")
	if a != b {
		t.Fatalf("hash must ignore surrounding whitespace: %s != %s", a, b)
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Fatalf("hash format = %s", a)
	}
	if ComputeContentHash("hello") == ComputeContentHash("Hello") {
		t.Fatal("hash must be case-sensitive")
	}
}
