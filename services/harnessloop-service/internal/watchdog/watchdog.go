package watchdog

import (
	"sync"
	"time"

	"agent-platform/pkg/harnessloop/retry"
)

type ActivityKind string

const (
	ActivityToken        ActivityKind = "llm.token"
	ActivityToolResult   ActivityKind = "tool.result"
	ActivityToolProgress ActivityKind = "tool.progress"
	ActivityHeartbeat    ActivityKind = "agent.alive"
)

func (k ActivityKind) CountsAsActivity() bool {
	return k == ActivityToken || k == ActivityToolResult || k == ActivityToolProgress
}

type Clock func() time.Time

type Verdict struct {
	Trigger  bool
	Category retry.Category
	Elapsed  time.Duration
	Reason   string
}

type Tracker struct {
	mu             sync.Mutex
	now            Clock
	threshold      time.Duration
	lastActivity   time.Time
	running        bool
	finishReceived bool
}

func NewTracker(threshold time.Duration, now Clock) *Tracker {
	if threshold <= 0 {
		threshold = 120 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &Tracker{now: now, threshold: threshold}
}

func (t *Tracker) Threshold() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.threshold
}

func (t *Tracker) Start() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running = true
	t.finishReceived = false
	t.lastActivity = t.now()
}

func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running = false
	t.finishReceived = false
	t.lastActivity = time.Time{}
}

func (t *Tracker) Touch(kind ActivityKind) {
	if !kind.CountsAsActivity() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running {
		t.lastActivity = t.now()
	}
}

func (t *Tracker) MarkFinish() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finishReceived = true
}

func (t *Tracker) LastActivity() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastActivity
}

func (t *Tracker) Active() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

func (t *Tracker) Evaluate() Verdict {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.running {
		return Verdict{Reason: "tracker is not running"}
	}
	if t.finishReceived {
		return Verdict{Reason: "finish already received"}
	}
	if t.lastActivity.IsZero() {
		return Verdict{Reason: "no activity recorded"}
	}
	elapsed := t.now().Sub(t.lastActivity)
	if elapsed <= t.threshold {
		return Verdict{Elapsed: elapsed, Reason: "activity within threshold"}
	}
	return Verdict{
		Trigger:  true,
		Category: retry.NoFinish,
		Elapsed:  elapsed,
		Reason:   "no activity beyond silent threshold",
	}
}
