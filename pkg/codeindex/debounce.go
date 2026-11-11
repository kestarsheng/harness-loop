package codeindex

import (
	"sync"
	"time"
)

type Debouncer struct {
	mu      sync.Mutex
	window  time.Duration
	pending map[string]struct{}
	timer   *time.Timer
	flush   func([]string)
	stopped bool
}

func NewDebouncer(window time.Duration, flush func([]string)) *Debouncer {
	if window <= 0 {
		window = 5 * time.Second
	}
	return &Debouncer{window: window, pending: map[string]struct{}{}, flush: flush}
}

func (d *Debouncer) Add(path string) {
	if path == "" {
		return
	}
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.pending[path] = struct{}{}
	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer = time.AfterFunc(d.window, d.fire)
	d.mu.Unlock()
}

func (d *Debouncer) AddAll(paths []string) {
	for _, path := range paths {
		d.Add(path)
	}
}

func (d *Debouncer) fire() {
	d.mu.Lock()
	paths := d.drainLocked()
	d.mu.Unlock()
	if len(paths) == 0 {
		return
	}
	if d.flush != nil {
		d.flush(paths)
	}
}

func (d *Debouncer) drainLocked() []string {
	if len(d.pending) == 0 {
		return nil
	}
	paths := make([]string, 0, len(d.pending))
	for path := range d.pending {
		paths = append(paths, path)
	}
	d.pending = map[string]struct{}{}
	return paths
}

func (d *Debouncer) Flush() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	return d.drainLocked()
}

func (d *Debouncer) Pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending)
}

func (d *Debouncer) Window() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.window
}

func (d *Debouncer) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}
