package devstack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ItemStatus is the per-item result of Ensure/Up/Down/Status.
type ItemStatus struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Port    int    `json:"port"`
	Healthy bool   `json:"healthy"`
	Started bool   `json:"started,omitempty"`
	Reused  bool   `json:"reused,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Report is the outcome of one stack operation.
type Report struct {
	Reused bool         `json:"reused"`
	Items  []ItemStatus `json:"items"`
}

// healthy reports whether every item probe passed.
func (r *Report) healthy() bool {
	if r == nil || len(r.Items) == 0 {
		return false
	}
	for _, it := range r.Items {
		if !it.Healthy {
			return false
		}
	}
	return true
}

// StepError names the contract item that failed.
type StepError struct {
	Item string
	Err  error
}

func (e *StepError) Error() string { return fmt.Sprintf("devstack: %s: %v", e.Item, e.Err) }
func (e *StepError) Unwrap() error { return e.Err }

// Manager owns one repository's dev stack.
//
// Every dependency that touches the OS is a field so unit tests can exercise
// ordering, health waiting, idempotency and rollback without starting the real
// stack (the live 50013 stack is shared with other tasks).
type Manager struct {
	Root     string
	Contract *Contract
	Dirs     Dirs
	// Out receives warnings; never nil after New.
	Out io.Writer

	Prober Prober
	Runner Runner

	// PrepareConfig materializes config_template -> config_output.
	PrepareConfig func(root string, item Item) error

	PollInterval time.Duration
	// StartTimeout overrides the per-kind health timeout when non-zero.
	StartTimeout time.Duration
	KillGrace    time.Duration
}

// New builds a Manager for root using contract.
func New(root string, contract *Contract) *Manager {
	if root == "" {
		root = "."
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	m := &Manager{
		Root:          root,
		Contract:      contract,
		Out:           io.Discard,
		Prober:        NewSystemProber(),
		Runner:        NewSystemRunner(),
		PrepareConfig: prepareConfigFor(contract),
		PollInterval:  500 * time.Millisecond,
		KillGrace:     10 * time.Second,
	}
	if contract != nil {
		m.Dirs = contract.Dirs(root)
	}
	return m
}

// Ensure probes harnessloop (127.0.0.1:50013) and reuses the stack when it is
// already healthy; otherwise it brings the whole contract up.
func (m *Manager) Ensure(ctx context.Context) (*Report, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	loop, ok := m.Contract.HarnessLoop()
	if !ok {
		return nil, fmt.Errorf("devstack: contract has no %q item", "harnessloop")
	}
	if healthy, _ := m.Prober.Healthy(ctx, loop); healthy {
		rep, err := m.Status(ctx)
		if err != nil {
			return nil, err
		}
		rep.Reused = true
		return rep, nil
	}
	return m.Up(ctx)
}

// Up starts every contract item that is not already healthy, in contract order.
// Items that probe healthy are reused and never restarted, which makes a second
// call a no-op. On failure, processes started by this call are rolled back.
func (m *Manager) Up(ctx context.Context) (*Report, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	if err := m.ensureDirs(); err != nil {
		return nil, err
	}

	rep := &Report{Items: make([]ItemStatus, 0, len(m.Contract.Items))}
	var started []launched

	for _, item := range m.Contract.Items {
		st := ItemStatus{Name: item.Name, Kind: item.Kind, Port: item.Port}

		if healthy, detail := m.Prober.Healthy(ctx, item); healthy {
			st.Healthy, st.Reused, st.Detail = true, true, detail
			rep.Items = append(rep.Items, st)
			continue
		}

		if err := m.PrepareConfig(m.Root, item); err != nil {
			m.rollback(ctx, started)
			return nil, &StepError{Item: item.Name, Err: err}
		}

		pid, err := m.launch(ctx, item)
		if err != nil {
			m.rollback(ctx, started)
			return nil, &StepError{Item: item.Name, Err: err}
		}
		if item.IsProcess() {
			started = append(started, launched{
				name:    item.Name,
				port:    item.Port,
				pidFile: m.pidPath(item),
			})
		}

		if ok, detail := m.waitHealthy(ctx, item); !ok {
			m.rollback(ctx, started)
			return nil, &StepError{Item: item.Name, Err: fmt.Errorf(
				"health check did not pass within %s: %s", m.timeoutFor(item), detail)}
		}

		if item.IsProcess() {
			st.PID = m.recordPID(item, pid)
		}
		st.Healthy, st.Started, st.Detail = true, true, "started"
		rep.Items = append(rep.Items, st)
	}
	return rep, nil
}

// Down stops business processes and vite in reverse contract order, then
// containers. keepData keeps compose data containers (postgres/mongo/qdrant)
// running, mirroring scripts/dev-down.sh --keep-db.
func (m *Manager) Down(ctx context.Context, keepData bool) (*Report, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	rep := &Report{Items: make([]ItemStatus, 0, len(m.Contract.Items))}
	var errs []error

	for i := len(m.Contract.Items) - 1; i >= 0; i-- {
		item := m.Contract.Items[i]
		st := ItemStatus{Name: item.Name, Kind: item.Kind, Port: item.Port}

		if item.Kind == KindContainer {
			if keepData && isComposeCommand(item.Command) {
				st.Detail = "kept (--keep-db)"
				rep.Items = append(rep.Items, st)
				continue
			}
			cmd, ok := dockerStopCommand(item.Command)
			if !ok {
				st.Detail = "no container stop command"
				rep.Items = append(rep.Items, st)
				continue
			}
			spec := LaunchSpec{
				Name:    item.Name + "-stop",
				Dir:     m.Root,
				Command: cmd,
				Env:     m.env(item),
				LogPath: m.logPath(item),
			}
			if err := m.Runner.Run(ctx, spec); err != nil {
				st.Detail = err.Error()
				errs = append(errs, &StepError{Item: item.Name, Err: err})
			} else {
				st.Detail = "stopped"
			}
			rep.Items = append(rep.Items, st)
			continue
		}

		pid, _ := readPIDFile(m.pidPath(item))
		if pid == 0 && item.Port > 0 {
			pid, _ = m.Runner.LookupListenerPID(item.Port)
		}
		if pid > 0 {
			if err := m.Runner.Kill(ctx, pid, m.killGrace()); err != nil {
				st.Detail = err.Error()
				errs = append(errs, &StepError{Item: item.Name, Err: err})
			} else {
				st.Detail = "stopped"
			}
		} else {
			st.Detail = "not running"
		}
		_ = os.Remove(m.pidPath(item))
		rep.Items = append(rep.Items, st)
	}

	// dev down is an explicit operator stop. The singleton board lock and its
	// refs belong to that same decision, so the next `loop start` begins clean
	// instead of inheriting refs for a board this call just killed.
	if _, ok := m.Contract.BoardItem(); ok {
		if err := os.Remove(filepath.Join(m.Dirs.Run, BoardLockFile)); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(m.Out, "devstack: remove board lock: %v\n", err)
		}
	}

	if len(errs) > 0 {
		return rep, errors.Join(errs...)
	}
	return rep, nil
}

// Status probes all contract items without changing anything.
func (m *Manager) Status(ctx context.Context) (*Report, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	rep := &Report{Items: make([]ItemStatus, 0, len(m.Contract.Items))}
	for _, item := range m.Contract.Items {
		st := ItemStatus{Name: item.Name, Kind: item.Kind, Port: item.Port}
		st.Healthy, st.Detail = m.Prober.Healthy(ctx, item)
		if item.IsProcess() {
			st.PID, _ = readPIDFile(m.pidPath(item))
		}
		rep.Items = append(rep.Items, st)
	}
	return rep, nil
}

// Healthy reports whether the last report has every item healthy.
func (r *Report) Healthy() bool { return r.healthy() }

// ErrNotHealthy is returned by the CLI when a probe pass fails.
var ErrNotHealthy = errors.New("devstack: stack is not healthy")

func (m *Manager) check() error {
	if m == nil || m.Contract == nil {
		return errors.New("devstack: contract not loaded")
	}
	if m.Prober == nil {
		return errors.New("devstack: prober not configured")
	}
	if m.Runner == nil {
		return errors.New("devstack: runner not configured")
	}
	if m.PrepareConfig == nil {
		m.PrepareConfig = prepareConfigFor(m.Contract)
	}
	if m.Dirs.Run == "" {
		m.Dirs = m.Contract.Dirs(m.Root)
	}
	return nil
}

func (m *Manager) ensureDirs() error {
	for _, dir := range []string{m.Dirs.Bin, m.Dirs.Conf, m.Dirs.Logs, m.Dirs.Run} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("devstack: create %s: %w", dir, err)
		}
	}
	return nil
}

func (m *Manager) launch(ctx context.Context, item Item) (int, error) {
	spec := LaunchSpec{
		Name:    item.Name,
		Dir:     m.Root,
		Command: item.Command,
		Env:     m.env(item),
		LogPath: m.logPath(item),
	}
	if item.Kind == KindContainer {
		cctx, cancel := context.WithTimeout(ctx, m.timeoutFor(item))
		defer cancel()
		return 0, m.Runner.Run(cctx, spec)
	}
	return m.Runner.Start(ctx, spec)
}

func (m *Manager) waitHealthy(ctx context.Context, item Item) (bool, string) {
	deadline := time.Now().Add(m.timeoutFor(item))
	detail := ""
	for {
		if healthy, d := m.Prober.Healthy(ctx, item); healthy {
			return true, d
		} else {
			detail = d
		}
		if !time.Now().Before(deadline) {
			return false, detail
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err().Error()
		case <-time.After(m.pollInterval()):
		}
	}
}

type launched struct {
	name    string
	port    int
	pidFile string
}

// rollback stops processes started by the current Up call, newest first.
// Containers are intentionally left running: data volumes are shared with
// other in-flight tasks, and scripts/dev-up.sh rolls back processes only.
func (m *Manager) rollback(ctx context.Context, started []launched) {
	if len(started) == 0 {
		return
	}
	for i := len(started) - 1; i >= 0; i-- {
		l := started[i]
		pid, _ := readPIDFile(l.pidFile)
		if pid == 0 && l.port > 0 {
			pid, _ = m.Runner.LookupListenerPID(l.port)
		}
		if pid > 0 {
			if err := m.Runner.Kill(ctx, pid, m.killGrace()); err != nil {
				fmt.Fprintf(m.Out, "devstack: rollback %s (pid %d): %v\n", l.name, pid, err)
			}
		}
		_ = os.Remove(l.pidFile)
	}
	fmt.Fprintf(m.Out, "devstack: rolled back %d process(es) started by this run\n", len(started))
}

func (m *Manager) recordPID(item Item, pid int) int {
	if pid == 0 && item.Port > 0 {
		if resolved, err := m.Runner.LookupListenerPID(item.Port); err == nil {
			pid = resolved
		}
	}
	if pid == 0 {
		fmt.Fprintf(m.Out, "devstack: cannot resolve %s PID by port %d; dev down will fall back to the port\n", item.Name, item.Port)
		return 0
	}
	if err := writePIDFile(m.pidPath(item), pid); err != nil {
		fmt.Fprintf(m.Out, "devstack: write pid file for %s: %v\n", item.Name, err)
	}
	return pid
}

func (m *Manager) env(item Item) []string {
	env := os.Environ()
	keys := make([]string, 0, len(item.Env))
	for k := range item.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+item.Env[k])
	}
	return env
}

func (m *Manager) logPath(item Item) string {
	return filepath.Join(m.Dirs.Logs, item.Name+".log")
}

func (m *Manager) pidPath(item Item) string {
	return filepath.Join(m.Dirs.Run, item.PIDFileStem()+".pid")
}

func (m *Manager) pollInterval() time.Duration {
	if m.PollInterval > 0 {
		return m.PollInterval
	}
	return 500 * time.Millisecond
}

func (m *Manager) killGrace() time.Duration {
	if m.KillGrace > 0 {
		return m.KillGrace
	}
	return 10 * time.Second
}

func (m *Manager) timeoutFor(item Item) time.Duration {
	if m.StartTimeout > 0 {
		return m.StartTimeout
	}
	switch item.Kind {
	case KindContainer:
		return 90 * time.Second
	case KindFrontend:
		return 120 * time.Second
	default:
		return 60 * time.Second
	}
}

// prepareConfigFor binds RenderConfig to the contract in use.
func prepareConfigFor(contract *Contract) func(root string, item Item) error {
	return func(root string, item Item) error {
		return RenderConfig(root, contract, item)
	}
}

func isComposeCommand(command string) bool {
	fields := strings.Fields(command)
	return len(fields) > 1 && fields[0] == "docker" && fields[1] == "compose"
}

// dockerStopCommand derives the stop command from the contract start command.
func dockerStopCommand(command string) (string, bool) {
	fields := strings.Fields(command)
	if len(fields) < 2 || fields[0] != "docker" {
		return "", false
	}
	switch fields[1] {
	case "compose":
		file := ""
		for i := 2; i+1 < len(fields); i++ {
			if fields[i] == "-f" {
				file = fields[i+1]
			}
		}
		svc := fields[len(fields)-1]
		if file == "" || svc == "" {
			return "", false
		}
		return fmt.Sprintf("docker compose -f %s stop %s", file, svc), true
	case "run":
		for i := 2; i+1 < len(fields); i++ {
			if fields[i] == "--name" {
				return "docker stop " + fields[i+1], true
			}
		}
	}
	return "", false
}

func readPIDFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	text := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, string(raw))
	if text == "" {
		return 0, fmt.Errorf("devstack: empty pid file %s", path)
	}
	pid, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("devstack: parse pid file %s: %w", path, err)
	}
	return pid, nil
}

func writePIDFile(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// MarshalReport renders a report as indented JSON.
func MarshalReport(rep *Report) ([]byte, error) {
	return json.MarshalIndent(rep, "", "  ")
}
