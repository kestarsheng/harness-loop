package memoryfile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type WriteResult struct {
	Skipped    bool
	Hash       string
	Bytes      int
	Topics     []TopicChange
	BackupPath string
}

type Writer struct {
	mu      sync.Mutex
	store   *Store
	path    string
	scope   string
	sink    EventSink
	timeout time.Duration
	bashash string
	baseVer int
}

func NewWriter(store *Store, path string) *Writer {
	scope := ScopeProject
	if store != nil {
		if store.Document() != nil {
			scope = store.Document().Scope
		}
		store.setPath(path)
	}
	return &Writer{
		store:   store,
		path:    path,
		scope:   scope,
		timeout: 5 * time.Second,
	}
}

// WithSink wires the sink into both the Writer and its Store. This is the
// single event exit: Get/Insert publish promoted/evicted automatically, and
// Writer publishes written/promoted/evicted through the same sink. Manual
// EmitPromotion / EmitEvictions remain available for compatibility but are
// de-duplicated as described on Store.WithSink.
func (w *Writer) WithSink(sink EventSink) *Writer {
	w.sink = sink
	if w.store != nil {
		w.store.WithSink(sink)
	}
	return w
}

func (w *Writer) WithTimeout(d time.Duration) *Writer {
	if d > 0 {
		w.timeout = d
	}
	return w
}

func hashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (w *Writer) Load() (*Document, error) {
	raw, err := os.ReadFile(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Document{SchemaVersion: SchemaVersion, Scope: w.scope}, nil
		}
		return nil, err
	}
	doc, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	w.bashash = hashBytes(raw)
	w.baseVer = doc.OrderVersion
	return doc, nil
}

func (w *Writer) Save() (WriteResult, error) {
	if w.store == nil {
		return WriteResult{}, fmt.Errorf("memoryfile: store is required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return WriteResult{}, fmt.Errorf("memoryfile: ensure dir: %w", err)
	}

	lockPath := w.path + ".lock"
	lock, err := acquireLock(lockPath, w.timeout)
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = lock.release() }()

	var diskDoc *Document
	diskRaw, readErr := os.ReadFile(w.path)
	if readErr == nil {
		diskDoc, err = Parse(diskRaw)
		if err != nil {
			return WriteResult{}, fmt.Errorf("%w: %v", ErrCorrupted, err)
		}
	} else if !os.IsNotExist(readErr) {
		return WriteResult{}, fmt.Errorf("memoryfile: read: %w", readErr)
	}

	if diskDoc != nil && w.bashash != "" && hashBytes(diskRaw) != w.bashash {
		return WriteResult{}, ErrConcurrentModification
	}
	if diskDoc != nil && w.baseVer != 0 && diskDoc.OrderVersion != w.baseVer {
		return WriteResult{}, ErrConcurrentModification
	}

	doc := w.store.Document()
	if doc.UpdatedAt.IsZero() {
		doc.UpdatedAt = time.Now().UTC()
	}
	raw, err := doc.Render()
	if err != nil {
		return WriteResult{}, err
	}

	if diskRaw != nil && hashBytes(diskRaw) == hashBytes(raw) {
		w.bashash = hashBytes(raw)
		w.baseVer = doc.OrderVersion
		return WriteResult{Skipped: true, Hash: w.bashash, Bytes: len(raw)}, nil
	}

	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return WriteResult{}, fmt.Errorf("memoryfile: ensure dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(w.path), ".tmp-"+filepath.Base(w.path)+"-*")
	if err != nil {
		return WriteResult{}, fmt.Errorf("memoryfile: create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		cleanup()
		return WriteResult{}, fmt.Errorf("memoryfile: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return WriteResult{}, fmt.Errorf("memoryfile: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return WriteResult{}, fmt.Errorf("memoryfile: close temp: %w", err)
	}

	if err := renameWithRetry(tmpName, w.path); err != nil {
		cleanup()
		return WriteResult{}, err
	}
	_ = syncDir(filepath.Dir(w.path))

	w.bashash = hashBytes(raw)
	w.baseVer = doc.OrderVersion

	result := WriteResult{Hash: w.bashash, Bytes: len(raw), Topics: diffTopics(diskDoc, doc)}

	// Publish any evictions still pending from Insert before the write event.
	// When Store.WithSink is wired, Insert already published them and the
	// pending queue is empty, so Save cannot double-publish the same key.
	for _, ev := range w.store.DrainEvictions() {
		w.emit(newEvictionEvent(w.path, w.scope, ev, time.Now().UTC()))
	}

	w.emit(ChangeEvent{
		Type:         EventWritten,
		File:         w.path,
		Scope:        w.scope,
		Topics:       result.Topics,
		OrderVersion: doc.OrderVersion,
		Source:       "runtime",
		Timestamp:    time.Now().UTC(),
	})
	return result, nil
}

func renameWithRetry(from, to string) error {
	delays := []time.Duration{0, 100 * time.Millisecond, 300 * time.Millisecond, 900 * time.Millisecond}
	var lastErr error
	for _, d := range delays {
		if d > 0 {
			time.Sleep(d)
		}
		if err := os.Rename(from, to); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("%w: rename %s -> %s: %v", ErrWriteBusy, from, to, lastErr)
}

func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func diffTopics(oldDoc, newDoc *Document) []TopicChange {
	changes := make([]TopicChange, 0, len(newDoc.Topics))
	oldHash := map[string]string{}
	if oldDoc != nil {
		for _, t := range oldDoc.Topics {
			oldHash[t.Key] = t.Meta.ContentHash
		}
	}
	for rank, t := range newDoc.Topics {
		action := string(ActionCreated)
		if prev, ok := oldHash[t.Key]; ok {
			if prev == t.Meta.ContentHash {
				continue
			}
			action = string(ActionUpdated)
		}
		changes = append(changes, TopicChange{Key: t.Key, Action: action, ContentHash: t.Meta.ContentHash, OrderRank: rank})
	}
	if oldDoc != nil {
		newKeys := map[string]bool{}
		for _, t := range newDoc.Topics {
			newKeys[t.Key] = true
		}
		for _, t := range oldDoc.Topics {
			if !newKeys[t.Key] {
				changes = append(changes, TopicChange{Key: t.Key, Action: string(ActionDeleted)})
			}
		}
	}
	return changes
}

func (w *Writer) emit(e ChangeEvent) {
	if w.sink == nil {
		return
	}
	w.sink.Emit(e)
}

// EmitEvictions is kept for callers that still forward DrainEvictions by hand.
// It is not needed once Store.WithSink is wired: Insert publishes evicted
// immediately, DrainEvictions returns nothing for those records, and Save only
// drains records that were never published. The event shape is shared with the
// automatic path through newEvictionEvent.
func (w *Writer) EmitEvictions(evictions []Eviction) {
	for _, ev := range evictions {
		w.emit(newEvictionEvent(w.path, w.scope, ev, time.Now().UTC()))
	}
}

// EmitPromotion is kept for compatibility. When the store exists it goes
// through the same promotion exit as Store.Get, so an already-promoted key is
// not published a second time. The manual event is only emitted for keys the
// store does not know about.
func (w *Writer) EmitPromotion(key string, rank int) {
	if w.store != nil && w.store.promote(key) {
		return
	}
	w.emit(newPromotionEvent(w.path, w.scope, key, rank, time.Now().UTC()))
}

func (w *Writer) Path() string { return w.path }

func (w *Writer) ResetBaseline() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.bashash = ""
	w.baseVer = 0
}

func (w *Writer) BaselineHash() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bashash
}

func sanitizeFileStem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
