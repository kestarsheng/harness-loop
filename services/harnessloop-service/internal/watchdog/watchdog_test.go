package watchdog

import (
	"sync"
	"testing"
	"time"

	"agent-platform/pkg/harnessloop/retry"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1700000000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestSilentThresholdAt121Seconds(t *testing.T) {
	clk := newFakeClock()
	tr := NewTracker(120*time.Second, clk.Now)
	tr.Start()

	clk.Advance(120 * time.Second)
	if v := tr.Evaluate(); v.Trigger {
		t.Fatalf("exactly at threshold must not trigger: %+v", v)
	}

	clk.Advance(time.Second)
	v := tr.Evaluate()
	if !v.Trigger {
		t.Fatalf("121s of silence must trigger: %+v", v)
	}
	if v.Category != retry.NoFinish {
		t.Fatalf("category = %s", v.Category)
	}
	if v.Elapsed != 121*time.Second {
		t.Fatalf("elapsed = %v", v.Elapsed)
	}
}

func TestTokensRefreshActivity(t *testing.T) {
	clk := newFakeClock()
	tr := NewTracker(120*time.Second, clk.Now)
	tr.Start()

	for i := 0; i < 5; i++ {
		clk.Advance(60 * time.Second)
		tr.Touch(ActivityToken)
		if v := tr.Evaluate(); v.Trigger {
			t.Fatalf("streaming token at %ds must suppress silence: %+v", (i+1)*60, v)
		}
	}

	clk.Advance(121 * time.Second)
	if v := tr.Evaluate(); !v.Trigger {
		t.Fatalf("after tokens stop, silence must trigger: %+v", v)
	}
}

func TestToolProgressRefreshesActivity(t *testing.T) {
	clk := newFakeClock()
	tr := NewTracker(120*time.Second, clk.Now)
	tr.Start()
	clk.Advance(100 * time.Second)
	tr.Touch(ActivityToolProgress)
	clk.Advance(100 * time.Second)
	if v := tr.Evaluate(); v.Trigger {
		t.Fatalf("tool.progress must refresh activity: %+v", v)
	}
}

func TestHeartbeatDoesNotCountAsProgress(t *testing.T) {
	clk := newFakeClock()
	tr := NewTracker(120*time.Second, clk.Now)
	tr.Start()

	clk.Advance(121 * time.Second)
	tr.Touch(ActivityHeartbeat)
	clk.Advance(121 * time.Second)

	v := tr.Evaluate()
	if !v.Trigger {
		t.Fatalf("agent.alive heartbeat must not reset last_activity: %+v", v)
	}
	if v.Elapsed != 242*time.Second {
		t.Fatalf("elapsed = %v, heartbeat must be ignored", v.Elapsed)
	}
}

func TestFinishStopsSilenceDetection(t *testing.T) {
	clk := newFakeClock()
	tr := NewTracker(120*time.Second, clk.Now)
	tr.Start()
	clk.Advance(200 * time.Second)
	tr.MarkFinish()
	if v := tr.Evaluate(); v.Trigger {
		t.Fatalf("finish received must disable detection: %+v", v)
	}
}

func TestNotRunningNeverTriggers(t *testing.T) {
	clk := newFakeClock()
	tr := NewTracker(120*time.Second, clk.Now)
	clk.Advance(300 * time.Second)
	if v := tr.Evaluate(); v.Trigger {
		t.Fatalf("idle tracker must not trigger: %+v", v)
	}
}

func TestResetClearsState(t *testing.T) {
	clk := newFakeClock()
	tr := NewTracker(120*time.Second, clk.Now)
	tr.Start()
	clk.Advance(200 * time.Second)
	if !tr.Evaluate().Trigger {
		t.Fatal("expected trigger before reset")
	}
	tr.Reset()
	if tr.Active() {
		t.Fatal("reset must stop tracking")
	}
	if v := tr.Evaluate(); v.Trigger {
		t.Fatalf("reset tracker must not trigger: %+v", v)
	}
	if !tr.LastActivity().IsZero() {
		t.Fatal("reset must clear last activity")
	}
}

func TestDefaultThreshold(t *testing.T) {
	tr := NewTracker(0, nil)
	if tr.Threshold() != 120*time.Second {
		t.Fatalf("threshold = %v", tr.Threshold())
	}
}
