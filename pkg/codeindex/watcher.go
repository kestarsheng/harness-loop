package codeindex

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type WatcherOptions struct {
	DebounceWindow time.Duration
	PollInterval   time.Duration
	MaxFileBytes   int64
	MaxWatchDirs   int
}

func DefaultWatcherOptions() WatcherOptions {
	return WatcherOptions{
		DebounceWindow: 5 * time.Second,
		PollInterval:   30 * time.Second,
		MaxFileBytes:   2 << 20,
		MaxWatchDirs:   2048,
	}
}

type Watcher struct {
	root   string
	opts   WatcherOptions
	ignore Ignore
	deb    *Debouncer
	queue  *Queue

	mu       sync.Mutex
	mtimes   map[string]time.Time
	fs       *fsnotify.Watcher
	watching bool
	stopOnce sync.Once
	stop     chan struct{}
	onPoll   func([]string)
}

func NewWatcher(root string, queue *Queue, opts WatcherOptions) *Watcher {
	if opts.DebounceWindow <= 0 {
		opts = DefaultWatcherOptions()
	}
	ignore := DefaultIgnore()
	if opts.MaxFileBytes > 0 {
		ignore.MaxFileBytes = opts.MaxFileBytes
	}
	w := &Watcher{
		root:   root,
		opts:   opts,
		ignore: ignore,
		queue:  queue,
		mtimes: map[string]time.Time{},
		stop:   make(chan struct{}),
	}
	w.deb = NewDebouncer(opts.DebounceWindow, func(paths []string) {
		if queue != nil {
			queue.Submit(paths)
		}
	})
	return w
}

func (w *Watcher) Debouncer() *Debouncer { return w.deb }

func (w *Watcher) rel(path string) string {
	rel, err := filepath.Rel(w.root, path)
	if err != nil || rel == ".." || len(rel) > 1 && rel[:2] == ".." {
		return path
	}
	return rel
}

func (w *Watcher) HandleEvent(path string) {
	if w.ignore.ShouldIgnore(w.rel(path)) {
		return
	}
	w.deb.Add(path)
}

func (w *Watcher) HandleRename(oldPath, newPath string) {
	w.HandleEvent(oldPath)
	w.HandleEvent(newPath)
}

func (w *Watcher) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	fs, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.fs = fs
	w.watching = true
	w.mu.Unlock()

	if err := w.addTree(w.root); err != nil {
		_ = fs.Close()
		return err
	}

	go w.loop(ctx, fs)
	if w.opts.PollInterval > 0 {
		go w.pollLoop(ctx)
	}
	return nil
}

func (w *Watcher) addTree(root string) error {
	added := 0
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if w.ignore.ShouldWatchDir(w.rel(path)) == false && path != root {
			return filepath.SkipDir
		}
		if w.opts.MaxWatchDirs > 0 && added >= w.opts.MaxWatchDirs {
			return filepath.SkipAll
		}
		if err := w.fs.Add(path); err != nil {
			return nil
		}
		added++
		return nil
	})
}

func (w *Watcher) loop(ctx context.Context, fs *fsnotify.Watcher) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case event, ok := <-fs.Events:
			if !ok {
				return
			}
			switch {
			case event.Op&(fsnotify.Create|fsnotify.Write) != 0:
				w.HandleEvent(event.Name)
			case event.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
				w.HandleEvent(event.Name)
			}
		case <-fs.Errors:
		}
	}
}

func (w *Watcher) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(w.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case <-ticker.C:
			changed := w.ScanMTime()
			if len(changed) > 0 {
				w.deb.AddAll(changed)
			}
		}
	}
}

func (w *Watcher) ScanMTime() []string {
	var changed []string
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = filepath.WalkDir(w.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if w.ignore.ShouldIgnore(w.rel(path)) {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if w.ignore.TooLarge(info.Size()) {
			return nil
		}
		prev, seen := w.mtimes[path]
		if !seen || !prev.Equal(info.ModTime()) {
			w.mtimes[path] = info.ModTime()
			changed = append(changed, path)
		}
		return nil
	})
	return changed
}

func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stop)
		w.mu.Lock()
		if w.fs != nil {
			_ = w.fs.Close()
			w.fs = nil
		}
		w.watching = false
		w.mu.Unlock()
		w.deb.Stop()
	})
}

func (w *Watcher) Running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.watching
}
