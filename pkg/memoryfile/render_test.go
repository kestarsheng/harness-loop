package memoryfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func docTime() time.Time {
	return time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
}

func TestRenderParseRoundTripIsStable(t *testing.T) {
	doc := sampleDoc()
	first, err := doc.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	parsed, err := Parse(first)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	second, err := parsed.Render()
	if err != nil {
		t.Fatalf("re-render: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("round trip changed bytes:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestRenderFormat(t *testing.T) {
	doc := sampleDoc()
	raw, err := doc.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, "---\nschema_version: 1\nscope: project\n") {
		t.Fatalf("frontmatter order is wrong:\n%s", text)
	}
	if !strings.Contains(text, `<!-- order: ["constraints-api","task-login-flow","pref-commit-style"] -->`) {
		t.Fatalf("order block missing or malformed:\n%s", text)
	}
	if !strings.Contains(text, "## constraints-api\n<!-- meta: category=long_term_constraint; pinned=true;") {
		t.Fatalf("topic block format is wrong:\n%s", text)
	}
	if !strings.Contains(text, "### API 约束\n") {
		t.Fatalf("title line missing:\n%s", text)
	}
}

func TestRenderValidatesDocument(t *testing.T) {
	doc := sampleDoc()
	doc.Scope = "session"
	if _, err := doc.Render(); err == nil {
		t.Fatal("invalid scope must fail render")
	}
}

func TestLayoutCreatesGlobalAndProjectFiles(t *testing.T) {
	home := t.TempDir()
	workspaces := []string{t.TempDir(), t.TempDir(), t.TempDir()}

	global := &Layout{HarnessHome: home, WorkspaceRoot: workspaces[0]}
	globalDoc := sampleDoc()
	globalDoc.Scope = ScopeGlobal
	globalDoc.ProjectID = ""
	if err := global.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	raw, err := globalDoc.Render()
	if err != nil {
		t.Fatalf("render global: %v", err)
	}
	if err := os.WriteFile(global.GlobalFile(), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	for i, ws := range workspaces {
		layout, err := NewLayout(home, ws)
		if err != nil {
			t.Fatalf("layout: %v", err)
		}
		if err := layout.EnsureDirs(); err != nil {
			t.Fatalf("ensure dirs: %v", err)
		}
		doc := sampleDoc()
		doc.ProjectID = "repo-" + string(rune('a'+i))
		raw, err := doc.Render()
		if err != nil {
			t.Fatalf("render project: %v", err)
		}
		if err := os.WriteFile(layout.ProjectFile(), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	globalPath := filepath.Join(home, "memory", "global.md")
	if _, err := os.Stat(globalPath); err != nil {
		t.Fatalf("global file missing: %v", err)
	}
	for _, ws := range workspaces {
		p := filepath.Join(ws, ".harness", "memory", "project.md")
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("project file missing: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "memory", "backup")); err != nil {
		t.Fatalf("global backup dir missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspaces[0], ".harness", "memory", "backup")); err != nil {
		t.Fatalf("project backup dir missing: %v", err)
	}
}

func TestSingleTopicUpdateHasSmallDiff(t *testing.T) {
	doc := sampleDoc()
	before, err := doc.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	topic := doc.Topics[1]
	topic.Body = "阶段 c 第 2 次尝试：登录接口与测试均已完成。"
	topic.Meta.UpdatedAt = topic.Meta.UpdatedAt.Add(5 * time.Minute)
	topic.Meta.ContentHash = ComputeContentHash(topic.Body)
	doc.UpsertTopic(topic, true)

	after, err := doc.Render()
	if err != nil {
		t.Fatalf("re-render: %v", err)
	}
	removed, added := changedLineCount(string(before), string(after))
	if removed != added {
		t.Fatalf("unbalanced diff: -%d/+%d", removed, added)
	}
	// order line + meta line + body line: three replacements at most.
	if removed > 3 {
		t.Fatalf("single topic update rewrote %d lines, want <= 3", removed)
	}
	if !strings.HasPrefix(string(after), "---\nschema_version: 1\n") {
		t.Fatal("frontmatter must stay stable")
	}
}

func changedLineCount(a, b string) (removed, added int) {
	removed = multisetDiff(a, b)
	added = multisetDiff(b, a)
	return removed, added
}

func multisetDiff(a, b string) int {
	counts := map[string]int{}
	for _, line := range strings.Split(a, "\n") {
		counts[line]++
	}
	for _, line := range strings.Split(b, "\n") {
		if counts[line] > 0 {
			counts[line]--
		}
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}

func TestTopicOperations(t *testing.T) {
	doc := &Document{
		SchemaVersion: SchemaVersion, Scope: ScopeProject,
		UpdatedAt: docTime(), OrderVersion: 1,
	}
	for _, key := range []string{"a", "b", "c"} {
		doc.UpsertTopic(Topic{
			Key: key, Title: key, Body: "body",
			Meta: Meta{Category: CategoryProjectFact, UpdatedAt: docTime()},
		}, false)
	}
	if got := doc.TopicKeys(); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("keys = %v", got)
	}

	doc.MoveToFront("c")
	if got := doc.TopicKeys(); strings.Join(got, ",") != "c,a,b" {
		t.Fatalf("after move keys = %v", got)
	}
	if strings.Join(doc.Order, ",") != "c,a,b" {
		t.Fatalf("order = %v", doc.Order)
	}

	doc.UpsertTopic(Topic{
		Key: "d", Title: "d", Body: "body",
		Meta: Meta{Category: CategoryTaskState, UpdatedAt: docTime()},
	}, true)
	if got := doc.TopicKeys(); strings.Join(got, ",") != "d,c,a,b" {
		t.Fatalf("after front insert keys = %v", got)
	}

	if !doc.RemoveTopic("a") {
		t.Fatal("remove existing topic must succeed")
	}
	if doc.RemoveTopic("nope") {
		t.Fatal("removing unknown topic must fail")
	}
	if got := doc.TopicKeys(); strings.Join(got, ",") != "d,c,b" {
		t.Fatalf("after remove keys = %v", got)
	}

	if err := doc.Reorder([]string{"b", "c", "d"}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if got := doc.TopicKeys(); strings.Join(got, ",") != "b,c,d" {
		t.Fatalf("after reorder keys = %v", got)
	}
	if err := doc.Reorder([]string{"b"}); err == nil {
		t.Fatal("short order must fail")
	}
	if err := doc.Reorder([]string{"b", "c", "zzz"}); err == nil {
		t.Fatal("unknown key must fail")
	}
}

func TestLayoutRejectsMissingRoots(t *testing.T) {
	if _, err := NewLayout("", "/ws"); err == nil {
		t.Fatal("missing harness home must fail")
	}
	if _, err := NewLayout("/home", ""); err == nil {
		t.Fatal("missing workspace must fail")
	}
}

func TestLayoutPaths(t *testing.T) {
	l, err := NewLayout("/home/.harness", "/ws")
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if l.GlobalFile() != filepath.FromSlash("/home/.harness/memory/global.md") &&
		!strings.HasSuffix(filepath.ToSlash(l.GlobalFile()), "/home/.harness/memory/global.md") {
		t.Fatalf("global file = %s", l.GlobalFile())
	}
	if !strings.HasSuffix(filepath.ToSlash(l.ProjectFile()), "/ws/.harness/memory/project.md") {
		t.Fatalf("project file = %s", l.ProjectFile())
	}
	if !strings.HasSuffix(filepath.ToSlash(l.GlobalLock()), "/memory/.lock") {
		t.Fatalf("global lock = %s", l.GlobalLock())
	}
	if !strings.HasSuffix(filepath.ToSlash(l.ProjectBackupDir()), "/memory/backup") {
		t.Fatalf("project backup = %s", l.ProjectBackupDir())
	}
}
