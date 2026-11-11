package memoryfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedClock() time.Time { return time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC) }

func TestRecoverHealthyFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.md")
	doc := sampleDoc()
	raw, err := doc.Render()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := RecoverFile(path, filepath.Join(dir, "backup"), fixedClock)
	if err != nil {
		t.Fatalf("recover healthy: %v", err)
	}
	if res.Corrupted {
		t.Fatal("healthy file must not be reported corrupted")
	}
	if _, err := os.Stat(filepath.Join(dir, "backup")); err == nil {
		t.Fatal("no backup must be created for a healthy file")
	}
}

func TestRecoverPartialKeepsValidTopics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.md")
	backupDir := filepath.Join(dir, "backup")
	broken := `---
schema_version: 1
scope: project
updated_at: 2026-11-11T00:00:00Z
order_version: 3
---

<!-- order: ["good"] -->

## good
<!-- meta: category=task_state; pinned=false; updated_at=2026-11-11T00:00:00Z; content_hash=sha256:abc -->

### 保留的主题
这部分是合法内容。

## bad
这个块缺少 meta 行，应当被丢弃。
`
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := RecoverFile(path, backupDir, fixedClock)
	if err != nil {
		t.Fatalf("recover partial: %v", err)
	}
	if !res.Corrupted || res.Retained != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if _, err := os.Stat(res.BackupPath); err != nil {
		t.Fatalf("backup must exist: %v", err)
	}
	if !strings.Contains(filepath.Base(res.BackupPath), "project-") {
		t.Fatalf("backup name = %s", res.BackupPath)
	}
	if res.Document == nil || len(res.Document.Topics) != 1 || res.Document.Topics[0].Key != "good" {
		t.Fatalf("recovered document = %+v", res.Document)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	repaired, err := Parse(raw)
	if err != nil {
		t.Fatalf("repaired file must parse: %v", err)
	}
	if len(repaired.Topics) != 1 || repaired.Topics[0].Key != "good" {
		t.Fatalf("repaired topics = %+v", repaired.Topics)
	}
}

func TestRecoverFullyCorruptedReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.md")
	backupDir := filepath.Join(dir, "backup")
	if err := os.WriteFile(path, []byte("total garbage without any topic block"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := RecoverFile(path, backupDir, fixedClock)
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("expected ErrCorrupted, got %v", err)
	}
	if res == nil || !res.Corrupted || res.Retained != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if _, statErr := os.Stat(res.BackupPath); statErr != nil {
		t.Fatalf("backup must exist even when nothing is recoverable: %v", statErr)
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != "total garbage without any topic block" {
		t.Fatal("fully corrupted file must be left untouched for manual recovery")
	}
}

func TestRecoverMissingFileFails(t *testing.T) {
	if _, err := RecoverFile(filepath.Join(t.TempDir(), "missing.md"), t.TempDir(), fixedClock); err == nil {
		t.Fatal("missing file must fail")
	}
}

func TestRecoverIsIdempotentOnRepairedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.md")
	backupDir := filepath.Join(dir, "backup")
	broken := `---
schema_version: 1
scope: project
updated_at: 2026-11-11T00:00:00Z
order_version: 1
---

<!-- order: ["good"] -->

## good
<!-- meta: category=task_state; pinned=false; updated_at=2026-11-11T00:00:00Z; content_hash=sha256:abc -->

### 保留
内容

## bad
无 meta
`
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverFile(path, backupDir, fixedClock); err != nil {
		t.Fatalf("first recover: %v", err)
	}
	second, err := RecoverFile(path, backupDir, fixedClock)
	if err != nil {
		t.Fatalf("second recover: %v", err)
	}
	if second.Corrupted {
		t.Fatal("repaired file must be healthy on the second pass")
	}
}
