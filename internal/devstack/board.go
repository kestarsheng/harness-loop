package devstack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The shared monitor board is a singleton (REQ-178, A 方案 2026-11-18):
//
//   - one fixed port, one URL for every terminal;
//   - the first Start launches the process and writes the lock file;
//   - later Start calls only register a ref, they never launch a second process;
//   - Stop releases the caller's ref; the process is killed only when no ref
//     remains, and then the lock file (and the URL) goes away;
//   - closing a terminal releases nothing (zombie refs are an accepted tradeoff,
//     there is no TTL).
//
// The board process itself is a contract item (board:true — vite in the shipped
// contract), so the singleton never hardcodes a second list of items.

// MonitorPath is the board route served by the shared web console.
// It must stay identical to internal/cli/tui.MonitorPath.
const MonitorPath = "/harness-loop/monitor"

// BoardLockFile is the singleton lock/ref file inside Dirs.Run.
const BoardLockFile = "board.lock"

// DefaultWebBaseURL is used only when the board item carries no web address.
// It must stay identical to internal/cli/tui.DefaultWebBaseURL.
const DefaultWebBaseURL = "http://localhost:5174"

// boardLockStaleAfter bounds how long a crashed writer can block the lock.
const boardLockStaleAfter = 10 * time.Second

// BoardState is the observable outcome of one board operation.
type BoardState struct {
	URL      string `json:"url"`
	Ref      string `json:"ref"`
	Refs     int    `json:"refs"`
	Port     int    `json:"port"`
	PID      int    `json:"pid,omitempty"`
	Started  bool   `json:"started,omitempty"`
	Reused   bool   `json:"reused,omitempty"`
	Released bool   `json:"released,omitempty"`
	Healthy  bool   `json:"healthy"`
	Detail   string `json:"detail,omitempty"`
}

// boardFile is the on-disk lock: the process that owns the board plus one
// entry per terminal that ran `loop start` and has not run `loop stop` yet.
type boardFile struct {
	PID       int               `json:"pid"`
	Port      int               `json:"port"`
	URL       string            `json:"url"`
	Refs      map[string]string `json:"refs"`
	UpdatedAt string            `json:"updated_at"`
}

// Board manages the singleton monitor board process for one repository.
type Board struct {
	Root     string
	Contract *Contract
	// Item is the contract item flagged board:true.
	Item Item
	Dirs Dirs
	Out  io.Writer

	Prober Prober
	Runner Runner
	// PrepareConfig is only needed when the board item carries a template.
	PrepareConfig func(root string, item Item) error

	PollInterval time.Duration
	StartTimeout time.Duration
	KillGrace    time.Duration
	// LockPath overrides the lock location (tests).
	LockPath string
}

// NewBoard resolves the singleton board for root.
func NewBoard(root string, contract *Contract) (*Board, error) {
	if contract == nil {
		return nil, errors.New("devstack: contract not loaded")
	}
	item, ok := contract.BoardItem()
	if !ok {
		return nil, errors.New("devstack: contract has no item flagged board:true")
	}
	if root == "" {
		root = "."
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Board{
		Root:          root,
		Contract:      contract,
		Item:          item,
		Dirs:          contract.Dirs(root),
		Out:           io.Discard,
		Prober:        NewSystemProber(),
		Runner:        NewSystemRunner(),
		PrepareConfig: prepareConfigFor(contract),
		PollInterval:  500 * time.Millisecond,
		KillGrace:     10 * time.Second,
	}, nil
}

// URL is the terminal-independent board URL every terminal shares.
func (b *Board) URL() string { return BoardURL(b.webBase()) }

// BoardURL mirrors internal/cli/tui.URLFor: the base is stripped of any
// trailing path, query or fragment, then MonitorPath is appended once. Two
// terminals sharing a web base therefore resolve to the exact same string.
func BoardURL(webBase string) string {
	base := strings.TrimSpace(webBase)
	if base == "" {
		base = DefaultWebBaseURL
	}
	if idx := strings.IndexAny(base, "?#"); idx >= 0 {
		base = base[:idx]
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, MonitorPath) {
		return base
	}
	return base + MonitorPath
}

// Start registers ref as an attached terminal. It launches the board only when
// no healthy instance exists, so a second terminal reuses the first one's
// process and URL. Registering the same ref twice is idempotent.
func (b *Board) Start(ctx context.Context, ref string) (*BoardState, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("devstack: board ref is required")
	}
	if err := b.ensureDirs(); err != nil {
		return nil, err
	}

	var st *BoardState
	err := b.withLock(ctx, func() error {
		state, err := b.readState()
		if err != nil {
			return err
		}
		healthy, detail := b.probe(ctx)

		if healthy {
			reused := state != nil
			if state == nil {
				// Started outside the singleton (scripts/dev-up.sh, dev up).
				state = &boardFile{Port: b.Item.Port, URL: b.URL(), Refs: map[string]string{}}
				state.PID, _ = b.Runner.LookupListenerPID(b.Item.Port)
			}
			if state.URL == "" {
				state.URL = b.URL()
			}
			if state.Port == 0 {
				state.Port = b.Item.Port
			}
			if state.Refs == nil {
				state.Refs = map[string]string{}
			}
			state.Refs[ref] = nowStamp()
			if err := b.writeState(state); err != nil {
				return err
			}
			note := "registered on the running board"
			if reused {
				note = fmt.Sprintf("reused the running board (%s)", detail)
			}
			st = &BoardState{
				URL: state.URL, Ref: ref, Refs: len(state.Refs),
				Port: state.Port, PID: state.PID, Reused: true, Healthy: true,
				Detail: note,
			}
			return nil
		}

		// No healthy board: the lock, if any, belongs to a dead process. Its
		// refs cannot be honoured (there is nothing to keep alive), so the
		// singleton restarts from this ref alone.
		dropped := 0
		if state != nil {
			dropped = len(state.Refs)
		}
		pid, err := b.launch(ctx)
		if err != nil {
			return fmt.Errorf("devstack: start board %q: %w", b.Item.Name, err)
		}
		if ok, d := b.waitHealthy(ctx); !ok {
			b.killStarted(ctx, pid)
			return &StepError{Item: b.Item.Name, Err: fmt.Errorf(
				"board health check did not pass within %s: %s", b.timeout(), d)}
		}
		if pid == 0 {
			pid, _ = b.Runner.LookupListenerPID(b.Item.Port)
		}
		if pid > 0 {
			_ = writePIDFile(filepath.Join(b.Dirs.Run, b.Item.PIDFileStem()+".pid"), pid)
		}
		state = &boardFile{
			PID: pid, Port: b.Item.Port, URL: b.URL(),
			Refs: map[string]string{ref: nowStamp()},
		}
		if err := b.writeState(state); err != nil {
			return err
		}
		note := "started the board"
		if dropped > 0 {
			note = fmt.Sprintf("started the board; cleared %d stale ref(s)", dropped)
		}
		st = &BoardState{
			URL: state.URL, Ref: ref, Refs: 1, Port: state.Port, PID: pid,
			Started: true, Healthy: true, Detail: note,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// Stop releases ref. While any other ref remains the board keeps running and
// only the remaining count is reported; the last release kills the process and
// deletes the lock, so the URL stops answering. Stopping an unknown ref is a
// no-op, which makes repeated stops idempotent.
// waitPortFree blocks until nothing is LISTENING on the board port. A stop
// must not report success while the URL still answers: on Windows the listener
// can be an IPv6 socket whose PID resolution failed, in which case Kill(0) does
// nothing and the lock would otherwise be deleted while vite keeps serving.
func (b *Board) waitPortFree(ctx context.Context) error {
	deadline := time.Now().Add(b.killGrace())
	for {
		if pid, _ := b.Runner.LookupListenerPID(b.Item.Port); pid == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("devstack: board %q still listening on port %d after stop", b.Item.Name, b.Item.Port)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (b *Board) Stop(ctx context.Context, ref string) (*BoardState, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("devstack: board ref is required")
	}

	var st *BoardState
	err := b.withLock(ctx, func() error {
		state, err := b.readState()
		if err != nil {
			return err
		}
		if state == nil {
			st = &BoardState{URL: b.URL(), Ref: ref, Port: b.Item.Port, Detail: "no board lock; nothing to stop"}
			return nil
		}
		delete(state.Refs, ref)
		if len(state.Refs) > 0 {
			if err := b.writeState(state); err != nil {
				return err
			}
			st = &BoardState{
				URL: state.URL, Ref: ref, Refs: len(state.Refs), Port: state.Port,
				PID: state.PID, Reused: true, Released: true, Healthy: true,
				Detail: fmt.Sprintf("released; %d ref(s) still attached, board stays up", len(state.Refs)),
			}
			return nil
		}

		pid := state.PID
		if pid == 0 {
			pid, _ = b.Runner.LookupListenerPID(b.Item.Port)
		}
		if pid > 0 {
			if err := b.Runner.Kill(ctx, pid, b.killGrace()); err != nil {
				return fmt.Errorf("devstack: stop board %q: %w", b.Item.Name, err)
			}
		}
		if err := b.waitPortFree(ctx); err != nil {
			return err
		}
		_ = os.Remove(filepath.Join(b.Dirs.Run, b.Item.PIDFileStem()+".pid"))
		if err := os.Remove(b.lockPath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("devstack: remove board lock: %w", err)
		}
		st = &BoardState{
			URL: state.URL, Ref: ref, Refs: 0, Port: state.Port, PID: pid,
			Detail: "last ref released; board stopped",
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// Status reports the lock and the live probe without changing anything.
func (b *Board) Status(ctx context.Context) (*BoardState, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	state, err := b.readState()
	if err != nil {
		return nil, err
	}
	healthy, detail := b.probe(ctx)
	st := &BoardState{URL: b.URL(), Port: b.Item.Port, Healthy: healthy, Detail: detail}
	if state != nil {
		st.URL, st.Port, st.PID = state.URL, state.Port, state.PID
		st.Refs = len(state.Refs)
		if st.URL == "" {
			st.URL = b.URL()
		}
	}
	return st, nil
}

// Refs returns the registered refs in stable order (diagnostics).
func (b *Board) Refs() ([]string, error) {
	state, err := b.readState()
	if err != nil || state == nil {
		return nil, err
	}
	out := make([]string, 0, len(state.Refs))
	for ref := range state.Refs {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out, nil
}

func (b *Board) check() error {
	if b == nil || b.Contract == nil {
		return errors.New("devstack: contract not loaded")
	}
	if b.Prober == nil {
		b.Prober = NewSystemProber()
	}
	if b.Runner == nil {
		b.Runner = NewSystemRunner()
	}
	if b.PrepareConfig == nil {
		b.PrepareConfig = prepareConfigFor(b.Contract)
	}
	if b.Dirs.Run == "" {
		b.Dirs = b.Contract.Dirs(b.Root)
	}
	return nil
}

func (b *Board) ensureDirs() error {
	for _, dir := range []string{b.Dirs.Bin, b.Dirs.Conf, b.Dirs.Logs, b.Dirs.Run} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("devstack: create %s: %w", dir, err)
		}
	}
	return nil
}

func (b *Board) lockPath() string {
	if b.LockPath != "" {
		return b.LockPath
	}
	return filepath.Join(b.Dirs.Run, BoardLockFile)
}

func (b *Board) probe(ctx context.Context) (bool, string) {
	return b.Prober.Healthy(ctx, b.Item)
}

func (b *Board) launch(ctx context.Context) (int, error) {
	if err := b.PrepareConfig(b.Root, b.Item); err != nil {
		return 0, err
	}
	env := os.Environ()
	keys := make([]string, 0, len(b.Item.Env))
	for k := range b.Item.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+b.Item.Env[k])
	}
	return b.Runner.Start(ctx, LaunchSpec{
		Name:    b.Item.Name,
		Dir:     b.Root,
		Command: b.Item.Command,
		Env:     env,
		LogPath: filepath.Join(b.Dirs.Logs, b.Item.Name+".log"),
	})
}

func (b *Board) waitHealthy(ctx context.Context) (bool, string) {
	deadline := time.Now().Add(b.timeout())
	detail := ""
	for {
		healthy, d := b.probe(ctx)
		if healthy {
			return true, d
		}
		detail = d
		if !time.Now().Before(deadline) {
			return false, detail
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err().Error()
		case <-time.After(b.pollInterval()):
		}
	}
}

func (b *Board) killStarted(ctx context.Context, pid int) {
	if pid == 0 {
		pid, _ = b.Runner.LookupListenerPID(b.Item.Port)
	}
	if pid > 0 {
		if err := b.Runner.Kill(ctx, pid, b.killGrace()); err != nil {
			fmt.Fprintf(b.Out, "devstack: rollback board %s (pid %d): %v\n", b.Item.Name, pid, err)
		}
	}
}

func (b *Board) readState() (*boardFile, error) {
	raw, err := os.ReadFile(b.lockPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("devstack: read board lock: %w", err)
	}
	var state boardFile
	if err := json.Unmarshal(raw, &state); err != nil {
		// A truncated lock (crash mid-write) is stale, not fatal.
		fmt.Fprintf(b.Out, "devstack: ignoring unreadable board lock %s: %v\n", b.lockPath(), err)
		return nil, nil
	}
	if state.Refs == nil {
		state.Refs = map[string]string{}
	}
	if state.URL == "" {
		state.URL = b.URL()
	}
	if state.Port == 0 {
		state.Port = b.Item.Port
	}
	return &state, nil
}

func (b *Board) writeState(state *boardFile) error {
	state.UpdatedAt = nowStamp()
	state.Port = b.Item.Port
	if state.URL == "" {
		state.URL = b.URL()
	}
	path := b.lockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("devstack: create board lock dir: %w", err)
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("devstack: encode board lock: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("devstack: write board lock: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("devstack: commit board lock: %w", err)
	}
	return nil
}

// withLock serializes read-modify-write of the ref set across processes with a
// create-exclusive mutex file; the design deliberately avoids flock so the same
// code path works on Windows and Unix.
func (b *Board) withLock(ctx context.Context, fn func() error) error {
	path := b.lockPath() + ".mutex"
	deadline := time.Now().Add(b.lockWait())

	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			defer os.Remove(path)
			return fn()
		}
		if !os.IsExist(err) {
			return fmt.Errorf("devstack: board lock %s: %w", path, err)
		}
		if fi, statErr := os.Stat(path); statErr == nil && time.Since(fi.ModTime()) > boardLockStaleAfter {
			_ = os.Remove(path)
			continue
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("devstack: board lock %s is held by another process", path)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (b *Board) webBase() string {
	if b.Item.Health.Type == HealthHTTP && b.Item.Health.URL != "" {
		if u, err := url.Parse(b.Item.Health.URL); err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
		return b.Item.Health.URL
	}
	switch {
	case b.Item.HTTPPort > 0:
		return "http://localhost:" + strconv.Itoa(b.Item.HTTPPort)
	case b.Item.Port > 0:
		return "http://localhost:" + strconv.Itoa(b.Item.Port)
	}
	return ""
}

func (b *Board) pollInterval() time.Duration {
	if b.PollInterval > 0 {
		return b.PollInterval
	}
	return 500 * time.Millisecond
}

func (b *Board) timeout() time.Duration {
	if b.StartTimeout > 0 {
		return b.StartTimeout
	}
	if b.Item.Kind == KindFrontend {
		return 120 * time.Second
	}
	return 60 * time.Second
}

func (b *Board) lockWait() time.Duration {
	if b.PollInterval > 0 && b.PollInterval < 100*time.Millisecond {
		return 2 * time.Second
	}
	return 15 * time.Second
}

func (b *Board) killGrace() time.Duration {
	if b.KillGrace > 0 {
		return b.KillGrace
	}
	return 10 * time.Second
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
