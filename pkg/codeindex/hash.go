package codeindex

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

func FileHash(src []byte) string {
	sum := sha256.Sum256(src)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func HashFile(path string) (string, time.Time, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}, err
	}
	return FileHash(src), info.ModTime(), nil
}

func LanguageOf(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".py":
		return "python"
	default:
		return "unknown"
	}
}

type Metrics struct {
	indexed int64
	skipped int64
	failed  int64
	chunks  int64
	edges   int64
	lastMs  int64
}

func NewMetrics() *Metrics { return &Metrics{} }

func (m *Metrics) RecordIndexed(chunks, edges int, durationMs int64) {
	atomic.AddInt64(&m.indexed, 1)
	atomic.AddInt64(&m.chunks, int64(chunks))
	atomic.AddInt64(&m.edges, int64(edges))
	atomic.StoreInt64(&m.lastMs, durationMs)
}

func (m *Metrics) RecordSkipped() { atomic.AddInt64(&m.skipped, 1) }

func (m *Metrics) RecordFailed() { atomic.AddInt64(&m.failed, 1) }

type MetricsSnapshot struct {
	Indexed int64
	Skipped int64
	Failed  int64
	Chunks  int64
	Edges   int64
	LastMs  int64
}

func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	return MetricsSnapshot{
		Indexed: atomic.LoadInt64(&m.indexed),
		Skipped: atomic.LoadInt64(&m.skipped),
		Failed:  atomic.LoadInt64(&m.failed),
		Chunks:  atomic.LoadInt64(&m.chunks),
		Edges:   atomic.LoadInt64(&m.edges),
		LastMs:  atomic.LoadInt64(&m.lastMs),
	}
}
