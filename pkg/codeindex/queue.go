package codeindex

import "sync"

type Queue struct {
	mu            sync.Mutex
	maxConcurrent int
	sem           chan struct{}
	pending       map[string]bool
	inFlight      map[string]bool
	handler       func(path string)
	wg            sync.WaitGroup
	processed     int
	failed        int
}

func NewQueue(maxConcurrent int, handler func(path string)) *Queue {
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}
	return &Queue{
		maxConcurrent: maxConcurrent,
		sem:           make(chan struct{}, maxConcurrent),
		pending:       map[string]bool{},
		inFlight:      map[string]bool{},
		handler:       handler,
	}
}

func (q *Queue) Submit(paths []string) int {
	q.mu.Lock()
	toRun := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" || q.pending[path] || q.inFlight[path] {
			continue
		}
		q.pending[path] = true
		toRun = append(toRun, path)
	}
	q.mu.Unlock()

	for _, path := range toRun {
		q.wg.Add(1)
		go q.run(path)
	}
	return len(toRun)
}

func (q *Queue) run(path string) {
	defer q.wg.Done()
	q.sem <- struct{}{}
	defer func() { <-q.sem }()

	q.mu.Lock()
	delete(q.pending, path)
	q.inFlight[path] = true
	q.mu.Unlock()

	if q.handler != nil {
		q.handler(path)
	}

	q.mu.Lock()
	delete(q.inFlight, path)
	q.processed++
	q.mu.Unlock()
}

func (q *Queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending) + len(q.inFlight)
}

func (q *Queue) State(path string) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending[path] {
		return "pending"
	}
	if q.inFlight[path] {
		return "running"
	}
	return "idle"
}

func (q *Queue) Processed() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.processed
}

func (q *Queue) Failed() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.failed
}

func (q *Queue) MaxConcurrent() int { return q.maxConcurrent }

func (q *Queue) Wait() { q.wg.Wait() }
