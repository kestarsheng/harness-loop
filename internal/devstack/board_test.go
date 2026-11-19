package devstack

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-platform/internal/cli/tui"
)

func newTestBoard(t *testing.T) (*Board, *fakeProber, *fakeRunner) {
	t.Helper()
	contract := loadContract(t)
	root := t.TempDir()
	b, err := NewBoard(root, contract)
	if err != nil {
		t.Fatalf("NewBoard: %v", err)
	}
	prober := newFakeProber()
	runner := newFakeRunner(prober, contract)
	b.Prober = prober
	b.Runner = runner
	b.PrepareConfig = func(string, Item) error { return nil }
	b.PollInterval = 2 * time.Millisecond
	b.StartTimeout = 40 * time.Millisecond
	b.KillGrace = 20 * time.Millisecond
	return b, prober, runner
}

func TestBoardItemIsTheSharedFrontend(t *testing.T) {
	c := loadContract(t)
	item, ok := c.BoardItem()
	if !ok {
		t.Fatal("contract must flag exactly one board item")
	}
	if item.Name != "vite" || item.Port != 5174 || item.Kind != KindFrontend {
		t.Fatalf("board item = %+v, want the vite frontend on 5174", item)
	}
	if !strings.HasSuffix(BoardURL("http://localhost:5174/"), MonitorPath) {
		t.Fatalf("board URL = %q", BoardURL("http://localhost:5174/"))
	}
}

// The board URL a terminal prints must be byte-identical to the TUI's, or two
// terminals would advertise two different boards.
func TestBoardURLMatchesTUI(t *testing.T) {
	for _, base := range []string{
		"", "http://localhost:5174", "http://localhost:5174/",
		"http://localhost:5174/harness-loop/monitor",
		"http://localhost:5174?tenant=x", "http://127.0.0.1:5174",
	} {
		if got, want := BoardURL(base), tui.URLFor(base); got != want {
			t.Fatalf("BoardURL(%q) = %q, tui.URLFor = %q", base, got, want)
		}
	}
}

// 两个终端 start：同一 URL、只有一个实例。
func TestBoardStartTwiceRegistersOneInstance(t *testing.T) {
	b, prober, runner := newTestBoard(t)
	ctx := context.Background()

	first, err := b.Start(ctx, "terminal:A")
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	second, err := b.Start(ctx, "terminal:B")
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if !first.Started || second.Started {
		t.Fatalf("first must start, second must reuse: %+v %+v", first, second)
	}
	if !second.Reused {
		t.Fatalf("second Start must report reuse: %+v", second)
	}
	if first.URL != second.URL {
		t.Fatalf("terminals got different URLs: %q vs %q", first.URL, second.URL)
	}
	if second.Refs != 2 {
		t.Fatalf("refs = %d, want 2", second.Refs)
	}
	_, starts, _ := runner.snapshot()
	if len(starts) != 1 {
		t.Fatalf("board started %d times, want 1: %v", len(starts), starts)
	}
	if !prober.get("vite") {
		t.Fatal("board must be healthy after Start")
	}

	// 关闭任一终端（不 stop）→ 引用保留，URL 仍可访问。
	closed, err := b.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !closed.Healthy || closed.Refs != 2 {
		t.Fatalf("after closing one terminal: %+v", closed)
	}

	// The lock is the single source of truth other terminals read.
	raw, err := os.ReadFile(filepath.Join(b.Dirs.Run, BoardLockFile))
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	var state boardFile
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("parse lock: %v", err)
	}
	if len(state.Refs) != 2 || state.Port != 5174 || state.URL != first.URL {
		t.Fatalf("lock = %+v", state)
	}
}

// 其中一个 stop → URL 仍 200；全部 stop → URL 不可访问。
func TestBoardStopReleasesOwnRefOnly(t *testing.T) {
	b, prober, runner := newTestBoard(t)
	ctx := context.Background()
	if _, err := b.Start(ctx, "terminal:A"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(ctx, "terminal:B"); err != nil {
		t.Fatal(err)
	}

	released, err := b.Stop(ctx, "terminal:A")
	if err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if released.Refs != 1 {
		t.Fatalf("remaining refs = %d, want 1", released.Refs)
	}
	if _, _, kills := runner.snapshot(); len(kills) != 0 {
		t.Fatalf("board must survive while a ref remains: kills=%v", kills)
	}
	if released.Detail == "" || !strings.Contains(released.Detail, "still attached") {
		t.Fatalf("stop must print the remaining refs: %q", released.Detail)
	}

	last, err := b.Stop(ctx, "terminal:B")
	if err != nil {
		t.Fatalf("last Stop: %v", err)
	}
	if last.Refs != 0 {
		t.Fatalf("refs = %d, want 0", last.Refs)
	}
	if _, _, kills := runner.snapshot(); len(kills) != 1 {
		t.Fatalf("last Stop must kill the board once: kills=%v", kills)
	}
	if _, err := os.Stat(filepath.Join(b.Dirs.Run, BoardLockFile)); !os.IsNotExist(err) {
		t.Fatalf("lock must be removed with the board: %v", err)
	}
	// The process is gone, so the fixed URL stops answering.
	prober.set("vite", false)
	down, err := b.Status(ctx)
	if err != nil {
		t.Fatalf("Status after shutdown: %v", err)
	}
	if down.Healthy || down.Refs != 0 {
		t.Fatalf("URL must be unreachable after the last stop: %+v", down)
	}
}

// 重复 start/stop 不重复计数。
func TestBoardStartAndStopAreIdempotent(t *testing.T) {
	b, _, runner := newTestBoard(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		st, err := b.Start(ctx, "terminal:A")
		if err != nil {
			t.Fatalf("Start #%d: %v", i+1, err)
		}
		if st.Refs != 1 {
			t.Fatalf("Start #%d refs = %d, want 1", i+1, st.Refs)
		}
	}
	if _, starts, _ := runner.snapshot(); len(starts) != 1 {
		t.Fatalf("repeated start launched %d processes", len(starts))
	}

	for i := 0; i < 3; i++ {
		st, err := b.Stop(ctx, "terminal:A")
		if err != nil {
			t.Fatalf("Stop #%d: %v", i+1, err)
		}
		if st.Refs != 0 {
			t.Fatalf("Stop #%d refs = %d, want 0", i+1, st.Refs)
		}
	}
	if _, _, kills := runner.snapshot(); len(kills) != 1 {
		t.Fatalf("repeated stop killed %d times, want 1: %v", len(kills), kills)
	}

	st, err := b.Stop(ctx, "terminal:unknown")
	if err != nil {
		t.Fatalf("stop without a board must not fail: %v", err)
	}
	if st.Refs != 0 || !strings.Contains(st.Detail, "nothing to stop") {
		t.Fatalf("stop after shutdown = %+v", st)
	}
}

// A board started outside the singleton (dev up / dev-up.sh) is adopted, not
// duplicated: the first loop start only registers a ref.
func TestBoardStartAdoptsRunningBoard(t *testing.T) {
	b, prober, runner := newTestBoard(t)
	prober.set("vite", true)

	st, err := b.Start(context.Background(), "terminal:A")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !st.Reused || st.Started {
		t.Fatalf("an already running board must be reused: %+v", st)
	}
	if _, starts, _ := runner.snapshot(); len(starts) != 0 {
		t.Fatalf("adopting a running board must not launch: %v", starts)
	}
	if st.Refs != 1 {
		t.Fatalf("refs = %d", st.Refs)
	}
}

// A dead board leaves a stale lock; the next start must not resurrect refs for
// a process that no longer exists (documented zombie-ref cleanup).
func TestBoardStartClearsStaleRefsAfterCrash(t *testing.T) {
	b, prober, runner := newTestBoard(t)
	ctx := context.Background()
	if _, err := b.Start(ctx, "terminal:A"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(ctx, "terminal:B"); err != nil {
		t.Fatal(err)
	}

	prober.set("vite", false) // the board died with two refs attached
	st, err := b.Start(ctx, "terminal:C")
	if err != nil {
		t.Fatalf("restart after crash: %v", err)
	}
	if !st.Started {
		t.Fatalf("crashed board must be started again: %+v", st)
	}
	if st.Refs != 1 {
		t.Fatalf("refs = %d, want only the restarting terminal", st.Refs)
	}
	if !strings.Contains(st.Detail, "cleared 2 stale ref") {
		t.Fatalf("detail must report the cleared refs: %q", st.Detail)
	}
	if _, starts, _ := runner.snapshot(); len(starts) != 2 {
		t.Fatalf("starts = %d, want 2 (initial + restart)", len(starts))
	}
}

func TestBoardStartFailsWhenHealthNeverPasses(t *testing.T) {
	b, _, runner := newTestBoard(t)
	runner.noHealth["vite"] = true

	_, err := b.Start(context.Background(), "terminal:A")
	if err == nil {
		t.Fatal("Start must fail when the board never becomes healthy")
	}
	var step *StepError
	if !errors.As(err, &step) || step.Item != "vite" {
		t.Fatalf("error = %v, want StepError(vite)", err)
	}
	if _, _, kills := runner.snapshot(); len(kills) != 1 {
		t.Fatalf("a failed start must roll the process back: kills=%v", kills)
	}
	if _, statErr := os.Stat(filepath.Join(b.Dirs.Run, BoardLockFile)); !os.IsNotExist(statErr) {
		t.Fatalf("a failed start must not leave a lock: %v", statErr)
	}
}

// dev down is an explicit stop: the board lock goes with it.
func TestDownRemovesBoardLock(t *testing.T) {
	contract := loadContract(t)
	root := t.TempDir()
	b, err := NewBoard(root, contract)
	if err != nil {
		t.Fatal(err)
	}
	prober := newFakeProber()
	runner := newFakeRunner(prober, contract)
	b.Prober, b.Runner = prober, runner
	b.PrepareConfig = func(string, Item) error { return nil }
	b.PollInterval = 2 * time.Millisecond
	b.StartTimeout = 40 * time.Millisecond
	if _, err := b.Start(context.Background(), "terminal:A"); err != nil {
		t.Fatal(err)
	}

	m := New(root, contract)
	m.Prober = prober
	m.Runner = runner
	m.PrepareConfig = func(string, Item) error { return nil }
	m.PollInterval = 2 * time.Millisecond
	m.StartTimeout = 40 * time.Millisecond
	m.KillGrace = 20 * time.Millisecond
	if _, err := m.Down(context.Background(), false); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.Dirs.Run, BoardLockFile)); !os.IsNotExist(err) {
		t.Fatalf("dev down must clear the board lock: %v", err)
	}
}

func TestValidateRejectsTwoBoardItems(t *testing.T) {
	base := Item{Kind: KindProcess, Port: 1, Command: "true",
		Health: Health{Type: HealthPort, Address: "127.0.0.1:1"}, Board: true}
	c := &Contract{Version: SupportedVersion, Items: []Item{
		base,
		func() Item { it := base; it.Name, it.Port = "second", 2; return it }(),
	}}
	c.Items[0].Name = "first"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "singleton") {
		t.Fatalf("two board items must be rejected, got %v", err)
	}

	c.Items = []Item{base}
	c.Items[0].Kind = KindContainer
	if err := c.Validate(); err == nil {
		t.Fatal("a container cannot be the board item")
	}
}
