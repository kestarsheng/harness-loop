package memoryfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type RecoveryResult struct {
	Corrupted  bool
	Retained   int
	BackupPath string
	Document   *Document
	Detail     string
}

func RecoverFile(path, backupDir string, now func() time.Time) (*RecoveryResult, error) {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("memoryfile: read %s: %w", path, err)
	}

	doc, parseErr := Parse(raw)
	if parseErr == nil {
		return &RecoveryResult{Corrupted: false, Retained: len(doc.Topics), Document: doc}, nil
	}

	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return nil, fmt.Errorf("memoryfile: ensure backup dir: %w", err)
	}
	stamp := now().UTC().Format("20060102T150405")
	backupPath := filepath.Join(backupDir, fmt.Sprintf("%s-%s.md", sanitizeFileStem(path), stamp))
	if err := os.WriteFile(backupPath, raw, 0o644); err != nil {
		return nil, fmt.Errorf("memoryfile: write backup: %w", err)
	}

	partial, retained := partialParse(raw)
	result := &RecoveryResult{
		Corrupted:  true,
		Retained:   retained,
		BackupPath: backupPath,
		Detail:     parseErr.Error(),
	}
	if retained == 0 {
		return result, fmt.Errorf("%w: %v", ErrCorrupted, parseErr)
	}

	partial.UpdatedAt = now().UTC()
	partial.SchemaVersion = SchemaVersion
	if partial.Scope != ScopeGlobal && partial.Scope != ScopeProject {
		partial.Scope = ScopeProject
	}
	partial.Order = make([]string, 0, len(partial.Topics))
	for _, t := range partial.Topics {
		partial.Order = append(partial.Order, t.Key)
	}

	rendered, err := partial.Render()
	if err != nil {
		return result, fmt.Errorf("memoryfile: render recovered document: %w", err)
	}
	if err := writeBytesAtomic(path, rendered); err != nil {
		return result, err
	}
	result.Document = partial
	return result, nil
}

func partialParse(raw []byte) (*Document, int) {
	doc := &Document{
		SchemaVersion: SchemaVersion,
		Scope:         ScopeProject,
		OrderVersion:  1,
	}
	blocks := splitTopicBlocks(string(raw))
	seen := map[string]bool{}
	for _, block := range blocks {
		topic, err := parseTopicBlock(block)
		if err != nil {
			continue
		}
		if seen[topic.Key] {
			continue
		}
		seen[topic.Key] = true
		doc.Topics = append(doc.Topics, topic)
	}
	return doc, len(doc.Topics)
}

func writeBytesAtomic(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("memoryfile: ensure dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-recover-*")
	if err != nil {
		return fmt.Errorf("memoryfile: create temp: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("memoryfile: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("memoryfile: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("memoryfile: close temp: %w", err)
	}
	if err := renameWithRetry(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	_ = syncDir(filepath.Dir(path))
	return nil
}

func IsCorrupted(err error) bool { return errors.Is(err, ErrCorrupted) }
