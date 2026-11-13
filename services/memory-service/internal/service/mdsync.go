package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"agent-platform/pkg/memoryfile"
	mempg "agent-platform/services/memory-service/internal/pgindex"
)

type MDSyncOptions struct {
	TenantID     string
	ProjectID    string
	PollInterval time.Duration
	Debounce     time.Duration
}

func DefaultMDSyncOptions() MDSyncOptions {
	return MDSyncOptions{PollInterval: 2 * time.Second, Debounce: 100 * time.Millisecond}
}

type SyncResult struct {
	Path     string
	Scope    string
	Indexed  int
	Deleted  int
	Skipped  bool
	Topics   int
	SyncedAt time.Time
}

type Syncer struct {
	writer *mempg.MemoryWriter
	opts   MDSyncOptions

	mu    sync.Mutex
	files map[string]string
	hash  map[string]string
}

func NewSyncer(writer *mempg.MemoryWriter, opts MDSyncOptions) *Syncer {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 100 * time.Millisecond
	}
	return &Syncer{writer: writer, opts: opts, files: map[string]string{}, hash: map[string]string{}}
}

func (s *Syncer) Register(path, scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = scope
}

func (s *Syncer) SyncFile(ctx context.Context, path, scope string) (SyncResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SyncResult{}, fmt.Errorf("mdsync: read %s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])

	s.mu.Lock()
	if s.hash[path] == digest {
		s.mu.Unlock()
		return SyncResult{Path: path, Scope: scope, Skipped: true}, nil
	}
	s.mu.Unlock()

	doc, err := memoryfile.Parse(raw)
	if err != nil {
		return SyncResult{}, fmt.Errorf("mdsync: parse %s: %w", path, err)
	}

	records := make([]mempg.TopicRecord, 0, len(doc.Topics))
	keys := make([]string, 0, len(doc.Topics))
	for rank, topic := range doc.Topics {
		keys = append(keys, topic.Key)
		records = append(records, mempg.TopicRecord{
			TenantID:    s.opts.TenantID,
			ProjectID:   s.opts.ProjectID,
			Scope:       scope,
			TopicKey:    topic.Key,
			Category:    string(topic.Meta.Category),
			Title:       topic.Title,
			Body:        topic.Body,
			Pinned:      topic.Meta.Pinned,
			OrderRank:   rank,
			ContentHash: topic.Meta.ContentHash,
			SourceFile:  path,
			UpdatedAt:   topic.Meta.UpdatedAt,
		})
	}

	existing, err := s.writer.ListTopicKeys(ctx, s.opts.TenantID, s.opts.ProjectID, scope)
	if err != nil {
		return SyncResult{}, err
	}
	keep := map[string]bool{}
	for _, key := range keys {
		keep[key] = true
	}
	var stale []string
	for _, key := range existing {
		if !keep[key] {
			stale = append(stale, key)
		}
	}

	indexed, err := s.writer.UpsertTopics(ctx, records)
	if err != nil {
		return SyncResult{}, err
	}
	deleted := 0
	if len(stale) > 0 {
		deleted, err = s.writer.DeleteTopics(ctx, s.opts.TenantID, s.opts.ProjectID, scope, stale)
		if err != nil {
			return SyncResult{}, err
		}
	}

	s.mu.Lock()
	s.hash[path] = digest
	s.mu.Unlock()

	return SyncResult{
		Path: path, Scope: scope, Indexed: indexed, Deleted: deleted,
		Topics: len(doc.Topics), SyncedAt: time.Now().UTC(),
	}, nil
}

func (s *Syncer) SyncOnce(ctx context.Context) ([]SyncResult, error) {
	s.mu.Lock()
	type entry struct{ path, scope string }
	entries := make([]entry, 0, len(s.files))
	for path, scope := range s.files {
		entries = append(entries, entry{path, scope})
	}
	s.mu.Unlock()

	results := make([]SyncResult, 0, len(entries))
	for _, e := range entries {
		res, err := s.SyncFile(ctx, e.path, e.scope)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

func (s *Syncer) Run(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return s.pollOnly(ctx)
	}
	defer watcher.Close()

	s.mu.Lock()
	for path := range s.files {
		_ = watcher.Add(filepath.Dir(path))
	}
	s.mu.Unlock()

	ticker := time.NewTicker(s.opts.PollInterval)
	defer ticker.Stop()

	trigger := make(chan struct{}, 1)
	var debounce *time.Timer
	schedule := func() {
		if debounce != nil {
			debounce.Stop()
		}
		debounce = time.AfterFunc(s.opts.Debounce, func() {
			select {
			case trigger <- struct{}{}:
			default:
			}
		})
	}

	for {
		select {
		case <-ctx.Done():
			if debounce != nil {
				debounce.Stop()
			}
			return nil
		case ev, ok := <-watcher.Events:
			if !ok {
				return s.pollOnly(ctx)
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0 {
				schedule()
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return s.pollOnly(ctx)
			}
		case <-trigger:
			if _, err := s.SyncOnce(ctx); err != nil {
				continue
			}
		case <-ticker.C:
			if _, err := s.SyncOnce(ctx); err != nil {
				continue
			}
		}
	}
}

func (s *Syncer) pollOnly(ctx context.Context) error {
	ticker := time.NewTicker(s.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_, _ = s.SyncOnce(ctx)
		}
	}
}

func (s *Syncer) WaitIndexed(ctx context.Context, path string, scope string, timeout time.Duration) (SyncResult, error) {
	deadline := time.Now().Add(timeout)
	for {
		res, err := s.SyncFile(ctx, path, scope)
		if err == nil && !res.Skipped {
			return res, nil
		}
		if time.Now().After(deadline) {
			return res, fmt.Errorf("mdsync: timeout waiting for change in %s", path)
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
