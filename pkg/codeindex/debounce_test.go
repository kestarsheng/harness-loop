package codeindex

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDebounceCollapsesBurst(t *testing.T) {
	var (
		mu      sync.Mutex
		batches [][]string
	)
	deb := NewDebouncer(60*time.Millisecond, func(paths []string) {
		mu.Lock()
		batches = append(batches, paths)
		mu.Unlock()
	})

	for i := 0; i < 5; i++ {
		deb.Add("a.go")
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 1 {
		t.Fatalf("5 saves within window must flush once, got %d batches", len(batches))
	}
	if len(batches[0]) != 1 || batches[0][0] != "a.go" {
		t.Fatalf("batch = %v", batches[0])
	}
}

func TestDebounceSeparatesUnrelatedFiles(t *testing.T) {
	var (
		mu      sync.Mutex
		batches [][]string
	)
	deb := NewDebouncer(50*time.Millisecond, func(paths []string) {
		mu.Lock()
		batches = append(batches, paths)
		mu.Unlock()
	})
	deb.AddAll([]string{"a.go", "b.go", "c.go"})
	time.Sleep(120 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 1 || len(batches[0]) != 3 {
		t.Fatalf("batches = %v", batches)
	}
}

func TestDebounceInitialWindow(t *testing.T) {
	deb := NewDebouncer(0, nil)
	if deb.Window() != 5*time.Second {
		t.Fatalf("default window = %v", deb.Window())
	}
}

func TestDebounceFlushDrains(t *testing.T) {
	deb := NewDebouncer(5*time.Second, nil)
	deb.Add("x.go")
	if deb.Pending() != 1 {
		t.Fatalf("pending = %d", deb.Pending())
	}
	paths := deb.Flush()
	if len(paths) != 1 || deb.Pending() != 0 {
		t.Fatalf("flush = %v pending=%d", paths, deb.Pending())
	}
	if got := deb.Flush(); len(got) != 0 {
		t.Fatalf("second flush must be empty: %v", got)
	}
}

func TestDebounceStopPreventsFlush(t *testing.T) {
	var calls int32
	deb := NewDebouncer(30*time.Millisecond, func(paths []string) {
		atomic.AddInt32(&calls, 1)
	})
	deb.Add("y.go")
	deb.Stop()
	time.Sleep(80 * time.Millisecond)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("stopped debouncer must not flush")
	}
}

func TestQueueDeduplicatesSameFile(t *testing.T) {
	var calls int32
	q := NewQueue(4, func(path string) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond)
	})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.Submit([]string{"same.go"})
		}()
	}
	wg.Wait()
	q.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("same file must be indexed once, got %d", got)
	}
	if q.Processed() != 1 {
		t.Fatalf("processed = %d", q.Processed())
	}
}

func TestQueueConcurrencyLimit(t *testing.T) {
	var (
		current int32
		peak    int32
	)
	q := NewQueue(2, func(path string) {
		n := atomic.AddInt32(&current, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&current, -1)
	})

	paths := []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"}
	for i := 0; i < 3; i++ {
		q.Submit(paths)
	}
	q.Wait()

	if got := atomic.LoadInt32(&peak); got > 2 {
		t.Fatalf("concurrency limit exceeded: peak=%d", got)
	}
	if q.Processed() != len(paths) {
		t.Fatalf("processed = %d, want %d", q.Processed(), len(paths))
	}
}

func TestQueueDepthAndState(t *testing.T) {
	q := NewQueue(1, nil)
	q.Submit([]string{"a.go"})
	q.Wait()
	if q.Depth() != 0 {
		t.Fatalf("depth = %d", q.Depth())
	}
	if q.State("a.go") != "idle" {
		t.Fatalf("state = %s", q.State("a.go"))
	}
	if q.MaxConcurrent() != 1 {
		t.Fatalf("max = %d", q.MaxConcurrent())
	}
}

func TestIgnoreRules(t *testing.T) {
	ig := DefaultIgnore()
	ignored := []string{
		".git/config", "node_modules/pkg/index.js", "vendor/lib/a.go",
		"dist/bundle.js", "build/out", "bin/tool", ".harness/tmp/x",
		"app.min.js", "go.sum.lock", "logo.png", "archive.zip",
	}
	for _, path := range ignored {
		if !ig.ShouldIgnore(path) {
			t.Fatalf("%q must be ignored", path)
		}
	}
	allowed := []string{"pkg/agent/engine.go", "frontend/src/App.tsx", "docs/readme.md"}
	for _, path := range allowed {
		if ig.ShouldIgnore(path) {
			t.Fatalf("%q must not be ignored", path)
		}
	}
	if !ig.TooLarge(3 << 20) {
		t.Fatal("oversized file must be rejected")
	}
	if ig.TooLarge(1024) {
		t.Fatal("small file must pass")
	}
	if ig.ShouldWatchDir("src/node_modules") {
		t.Fatal("node_modules dir must not be watched")
	}
	if !ig.ShouldWatchDir("src/pkg") {
		t.Fatal("normal dir must be watched")
	}
}

func TestWatcherHandleEvent(t *testing.T) {
	q := NewQueue(2, nil)
	w := NewWatcher(".", q, WatcherOptions{DebounceWindow: 40 * time.Millisecond})
	defer w.Stop()

	w.HandleEvent("pkg/a.go")
	w.HandleEvent(".git/config")
	w.HandleEvent("node_modules/pkg/index.js")
	if w.Debouncer().Pending() != 1 {
		t.Fatalf("pending = %d, want 1 (ignored paths must not queue)", w.Debouncer().Pending())
	}
}

func TestWatcherScanMTime(t *testing.T) {
	dir := t.TempDir()
	if err := writeTempFile(dir, "a.go", "package a\n"); err != nil {
		t.Fatal(err)
	}
	w := NewWatcher(dir, nil, WatcherOptions{})
	defer w.Stop()

	first := w.ScanMTime()
	if len(first) != 1 {
		t.Fatalf("first scan = %v", first)
	}
	second := w.ScanMTime()
	if len(second) != 0 {
		t.Fatalf("unchanged scan = %v", second)
	}
	time.Sleep(10 * time.Millisecond)
	if err := writeTempFile(dir, "a.go", "package a\n\nvar X = 1\n"); err != nil {
		t.Fatal(err)
	}
	third := w.ScanMTime()
	if len(third) != 1 {
		t.Fatalf("changed scan = %v", third)
	}
}

func TestWatcherStartOnRealDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := writeTempFile(dir, "a.go", "package a\n"); err != nil {
		t.Fatal(err)
	}
	q := NewQueue(2, nil)
	w := NewWatcher(dir, q, WatcherOptions{DebounceWindow: 50 * time.Millisecond, PollInterval: 0})
	if err := w.Start(nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !w.Running() {
		t.Fatal("watcher must report running")
	}
	w.Stop()
	if w.Running() {
		t.Fatal("watcher must stop")
	}
}

func writeTempFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}
