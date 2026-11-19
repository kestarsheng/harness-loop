package devstack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProber reports contract items as healthy on demand.
type fakeProber struct {
	mu      sync.Mutex
	healthy map[string]bool
	detail  string
}

func newFakeProber() *fakeProber {
	return &fakeProber{healthy: map[string]bool{}, detail: "probe"}
}

func (p *fakeProber) set(name string, healthy bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.healthy[name] = healthy
}

func (p *fakeProber) Healthy(_ context.Context, item Item) (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.healthy[item.Name], p.detail
}

func (p *fakeProber) get(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.healthy[name]
}

// fakeRunner records launches and marks items healthy like a real service would.
type fakeRunner struct {
	mu        sync.Mutex
	prober    *fakeProber
	runs      []string
	commands  []string
	starts    []string
	kills     []int
	listeners map[int]int
	portOf    map[string]int
	failRun   map[string]error
	failStart map[string]error
	noHealth  map[string]bool
	existing  map[string]bool
	nextPID   int
}

func newFakeRunner(prober *fakeProber, contract *Contract) *fakeRunner {
	r := &fakeRunner{
		prober:    prober,
		listeners: map[int]int{},
		portOf:    map[string]int{},
		failRun:   map[string]error{},
		failStart: map[string]error{},
		noHealth:  map[string]bool{},
		existing:  map[string]bool{},
		nextPID:   1000,
	}
	for _, it := range contract.Items {
		r.portOf[it.Name] = it.Port
	}
	return r
}

func (r *fakeRunner) setExisting(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.existing[name] = true
}

func (r *fakeRunner) Run(_ context.Context, spec LaunchSpec) error {
	if strings.HasPrefix(spec.Command, "docker start ") {
		name := strings.TrimPrefix(spec.Command, "docker start ")
		r.mu.Lock()
		exists := r.existing[name]
		r.mu.Unlock()
		if !exists {
			return errors.New("No such container: " + name)
		}
	}
	r.mu.Lock()
	r.runs = append(r.runs, spec.Name)
	r.commands = append(r.commands, spec.Command)
	err := r.failRun[spec.Name]
	noHealth := r.noHealth[spec.Name]
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if !noHealth {
		r.prober.set(spec.Name, true)
	}
	return nil
}

func (r *fakeRunner) Start(_ context.Context, spec LaunchSpec) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failStart[spec.Name]; err != nil {
		return 0, err
	}
	r.starts = append(r.starts, spec.Name)
	pid := r.nextPID
	r.nextPID++
	r.listeners[r.portOf[spec.Name]] = pid
	if !r.noHealth[spec.Name] {
		r.prober.set(spec.Name, true)
	}
	return pid, nil
}

func (r *fakeRunner) Kill(_ context.Context, pid int, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kills = append(r.kills, pid)
	for port, listening := range r.listeners {
		if listening == pid {
			delete(r.listeners, port)
		}
	}
	return nil
}

func (r *fakeRunner) LookupListenerPID(port int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pid, ok := r.listeners[port]; ok {
		return pid, nil
	}
	return 0, errors.New("not found")
}

func (r *fakeRunner) snapshot() (runs, starts []string, kills []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.runs...), append([]string(nil), r.starts...), append([]int(nil), r.kills...)
}

func (r *fakeRunner) commandSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.commands...)
}

func newTestManager(t *testing.T) (*Manager, *fakeProber, *fakeRunner) {
	t.Helper()
	contract := loadContract(t)
	root := t.TempDir()
	m := New(root, contract)
	prober := newFakeProber()
	runner := newFakeRunner(prober, contract)
	m.Prober = prober
	m.Runner = runner
	m.PrepareConfig = func(string, Item) error { return nil }
	m.PollInterval = 2 * time.Millisecond
	m.StartTimeout = 40 * time.Millisecond
	m.KillGrace = 20 * time.Millisecond
	return m, prober, runner
}

func startOrder(contract *Contract) (containers, processes []string) {
	for _, it := range contract.Items {
		if it.Kind == KindContainer {
			containers = append(containers, it.Name)
		} else {
			processes = append(processes, it.Name)
		}
	}
	return containers, processes
}

func TestUpStartsAll14AndWritesPIDs(t *testing.T) {
	m, _, runner := newTestManager(t)
	rep, err := m.Up(context.Background())
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(rep.Items) != 14 {
		t.Fatalf("items = %d", len(rep.Items))
	}
	for _, it := range rep.Items {
		if !it.Healthy || !it.Started {
			t.Fatalf("%s status = %+v", it.Name, it)
		}
	}
	runs, starts, _ := runner.snapshot()
	containers, processes := startOrder(m.Contract)
	if strings.Join(runs, ",") != strings.Join(containers, ",") {
		t.Fatalf("container order = %v, want %v", runs, containers)
	}
	if strings.Join(starts, ",") != strings.Join(processes, ",") {
		t.Fatalf("process order = %v, want %v", starts, processes)
	}
	for _, name := range processes {
		item, _ := m.Contract.Item(name)
		path := m.pidPath(item)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("pid file for %s missing: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(m.Dirs.Run, "harness-svc.pid")); err != nil {
		t.Fatalf("harness-service pid file must use pid_name: %v", err)
	}
}

func TestUpRestartsExistingStoppedContainer(t *testing.T) {
	m, prober, runner := newTestManager(t)
	for _, it := range m.Contract.Items {
		if it.Name != "redis" {
			prober.set(it.Name, true)
		}
	}
	// redis 的容器仍存在但已停止（dev-down --keep-db / loop stop 的常见状态）。
	runner.setExisting("hlp-dev-redis")

	rep, err := m.Up(context.Background())
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !prober.get("redis") {
		t.Fatalf("redis not healthy after restart: %+v", rep.Items)
	}
	var started, ran bool
	for _, cmd := range runner.commandSnapshot() {
		if cmd == "docker start hlp-dev-redis" {
			started = true
		}
		if strings.HasPrefix(cmd, "docker run ") && strings.Contains(cmd, "--name hlp-dev-redis") {
			ran = true
		}
	}
	if !started {
		t.Fatal("existing stopped container must be started with docker start")
	}
	if ran {
		t.Fatal("existing container must not be re-created with docker run (exit 125)")
	}
}

func TestUpIsIdempotent(t *testing.T) {
	m, prober, runner := newTestManager(t)
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	_, startsAfterFirst, _ := runner.snapshot()

	rep, err := m.Up(context.Background())
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if len(rep.Items) != 14 {
		t.Fatalf("items = %d", len(rep.Items))
	}
	for _, it := range rep.Items {
		if !it.Reused || it.Started {
			t.Fatalf("second Up must reuse %s: %+v", it.Name, it)
		}
		if !prober.get(it.Name) {
			t.Fatalf("%s probe state lost", it.Name)
		}
	}
	_, startsAfterSecond, kills := runner.snapshot()
	if len(startsAfterSecond) != len(startsAfterFirst) {
		t.Fatalf("second Up restarted services: %v -> %v", startsAfterFirst, startsAfterSecond)
	}
	if len(kills) != 0 {
		t.Fatalf("second Up killed processes: %v", kills)
	}
}

func TestEnsureReusesHealthyStack(t *testing.T) {
	m, prober, runner := newTestManager(t)
	prober.set("harnessloop", true)
	// Other items report unhealthy; Ensure must still reuse the running stack.
	rep, err := m.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !rep.Reused {
		t.Fatal("Ensure must report reuse when 50013 is healthy")
	}
	runs, starts, _ := runner.snapshot()
	if len(runs)+len(starts) != 0 {
		t.Fatalf("Ensure started items on a healthy stack: runs=%v starts=%v", runs, starts)
	}
	if len(rep.Items) != 14 {
		t.Fatalf("status items = %d", len(rep.Items))
	}
}

func TestEnsureStartsWhenHarnessLoopDown(t *testing.T) {
	m, _, runner := newTestManager(t)
	rep, err := m.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if rep.Reused {
		t.Fatal("Ensure must not report reuse on a cold start")
	}
	_, starts, _ := runner.snapshot()
	if len(starts) != 10 {
		t.Fatalf("starts = %d, want 10", len(starts))
	}
}

// TestEnsureGroupsStartsInfraAndAgentOnly is the T-P2-40 core: a cold `loop`
// brings up 13 items and never touches vite.
func TestEnsureGroupsStartsInfraAndAgentOnly(t *testing.T) {
	m, _, runner := newTestManager(t)
	rep, err := m.EnsureGroups(context.Background(), GroupInfra, GroupAgent)
	if err != nil {
		t.Fatalf("EnsureGroups: %v", err)
	}
	if rep.Reused {
		t.Fatal("EnsureGroups must not report reuse on a cold start")
	}
	if len(rep.Items) != 13 {
		t.Fatalf("items = %d, want 13 (14 minus the board)", len(rep.Items))
	}
	for _, it := range rep.Items {
		if it.Name == "vite" {
			t.Fatalf("EnsureGroups(infra,agent) must not include vite: %+v", rep.Items)
		}
		if !it.Healthy || !it.Started {
			t.Fatalf("%s status = %+v", it.Name, it)
		}
	}
	_, starts, _ := runner.snapshot()
	if indexOf(starts, "vite") >= 0 {
		t.Fatalf("vite started by EnsureGroups(infra,agent): %v", starts)
	}
	if len(starts) != 9 {
		t.Fatalf("process starts = %v, want the 9 services", starts)
	}
	for _, cmd := range runner.commandSnapshot() {
		if strings.Contains(cmd, "vite") {
			t.Fatalf("vite command issued by EnsureGroups(infra,agent): %q", cmd)
		}
	}
}

// TestUpGroupsStartsOnlySelectedGroup checks the filter itself: board alone
// launches exactly the board item.
func TestUpGroupsStartsOnlySelectedGroup(t *testing.T) {
	m, prober, runner := newTestManager(t)
	rep, err := m.UpGroups(context.Background(), GroupBoard)
	if err != nil {
		t.Fatalf("UpGroups(board): %v", err)
	}
	if len(rep.Items) != 1 || rep.Items[0].Name != "vite" {
		t.Fatalf("board group items = %+v, want only vite", rep.Items)
	}
	runs, starts, _ := runner.snapshot()
	if len(runs) != 0 {
		t.Fatalf("no container must start for the board group: %v", runs)
	}
	if strings.Join(starts, ",") != "vite" {
		t.Fatalf("starts = %v, want [vite]", starts)
	}
	if prober.get("postgres") {
		t.Fatal("infra must stay untouched when only the board group is selected")
	}
}

// TestUpGroupsIsIdempotent mirrors TestUpIsIdempotent for a subset.
func TestUpGroupsIsIdempotent(t *testing.T) {
	m, _, runner := newTestManager(t)
	if _, err := m.UpGroups(context.Background(), GroupInfra, GroupAgent); err != nil {
		t.Fatalf("first UpGroups: %v", err)
	}
	_, startsAfterFirst, _ := runner.snapshot()
	rep, err := m.UpGroups(context.Background(), GroupInfra, GroupAgent)
	if err != nil {
		t.Fatalf("second UpGroups: %v", err)
	}
	for _, it := range rep.Items {
		if !it.Reused || it.Started {
			t.Fatalf("second UpGroups must reuse %s: %+v", it.Name, it)
		}
	}
	_, startsAfterSecond, kills := runner.snapshot()
	if len(startsAfterSecond) != len(startsAfterFirst) {
		t.Fatalf("second UpGroups restarted items: %v -> %v", startsAfterFirst, startsAfterSecond)
	}
	if len(kills) != 0 {
		t.Fatalf("second UpGroups killed processes: %v", kills)
	}
}

// TestEnsureGroupsReusesOn50013Probe keeps the documented reuse signal: the
// harnessloop probe, even when other items are down.
func TestEnsureGroupsReusesOn50013Probe(t *testing.T) {
	m, prober, runner := newTestManager(t)
	prober.set("harnessloop", true)
	rep, err := m.EnsureGroups(context.Background(), GroupInfra, GroupAgent)
	if err != nil {
		t.Fatalf("EnsureGroups: %v", err)
	}
	if !rep.Reused {
		t.Fatal("EnsureGroups must report reuse when 50013 is healthy")
	}
	if len(rep.Items) != 13 {
		t.Fatalf("status items = %d, want 13", len(rep.Items))
	}
	runs, starts, _ := runner.snapshot()
	if len(runs)+len(starts) != 0 {
		t.Fatalf("EnsureGroups started items on a healthy stack: runs=%v starts=%v", runs, starts)
	}
}

// A selection without harnessloop cannot use the 50013 probe; it must fall back
// to "all selected healthy" instead of reporting a bogus reuse.
func TestEnsureGroupsWithoutHarnessLoopFallsBackToPerItemHealth(t *testing.T) {
	m, prober, runner := newTestManager(t)
	prober.set("harnessloop", true) // healthy, but outside the selection
	rep, err := m.EnsureGroups(context.Background(), GroupBoard)
	if err != nil {
		t.Fatalf("EnsureGroups: %v", err)
	}
	if rep.Reused {
		t.Fatal("board-only selection must not reuse on the 50013 probe")
	}
	if len(rep.Items) != 1 || rep.Items[0].Name != "vite" || !rep.Items[0].Started {
		t.Fatalf("board-only report = %+v", rep.Items)
	}
	if _, starts, _ := runner.snapshot(); strings.Join(starts, ",") != "vite" {
		t.Fatalf("starts = %v", starts)
	}
}

func TestGroupsRejectUnknownOrEmptySelection(t *testing.T) {
	m, _, runner := newTestManager(t)
	if _, err := m.UpGroups(context.Background(), "vm"); err == nil {
		t.Fatal("UpGroups must reject an unknown group")
	}
	if _, err := m.EnsureGroups(context.Background(), "vm"); err == nil {
		t.Fatal("EnsureGroups must reject an unknown group")
	}
	if _, err := m.UpGroups(context.Background()); err == nil {
		t.Fatal("UpGroups must reject an empty group list")
	}
	if _, err := m.EnsureGroups(context.Background()); err == nil {
		t.Fatal("EnsureGroups must reject an empty group list")
	}
	runs, starts, _ := runner.snapshot()
	if len(runs)+len(starts) != 0 {
		t.Fatalf("a rejected group must start nothing: runs=%v starts=%v", runs, starts)
	}
}

// Up keeps the whole-stack semantics: 14 items, vite included.
func TestUpStillStartsAll14IncludingBoard(t *testing.T) {
	m, _, runner := newTestManager(t)
	rep, err := m.Up(context.Background())
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(rep.Items) != 14 {
		t.Fatalf("items = %d, want 14", len(rep.Items))
	}
	_, starts, _ := runner.snapshot()
	if indexOf(starts, "vite") < 0 {
		t.Fatalf("dev up must still start vite: %v", starts)
	}
}

func TestMissingHarnessLoopItemFails(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.Contract = &Contract{Version: SupportedVersion, Items: []Item{{
		Name: "x", Kind: KindProcess, Port: 1, Command: "true",
		Health: Health{Type: HealthPort, Address: "127.0.0.1:1"},
	}}}
	if _, err := m.Ensure(context.Background()); err == nil {
		t.Fatal("Ensure without harnessloop must fail")
	}
}

func TestHealthTimeoutRollsBackStartedProcesses(t *testing.T) {
	m, _, runner := newTestManager(t)
	runner.noHealth["chat"] = true
	// memory starts before chat; chat never becomes healthy.
	_, err := m.Up(context.Background())
	if err == nil {
		t.Fatal("Up must fail when chat never becomes healthy")
	}
	var step *StepError
	if !errors.As(err, &step) || step.Item != "chat" {
		t.Fatalf("error = %v, want StepError(chat)", err)
	}
	if !strings.Contains(err.Error(), "health check") {
		t.Fatalf("error must mention health check: %v", err)
	}

	_, starts, kills := runner.snapshot()
	wantStarts := []string{"memory", "knowledge", "chat"}
	if strings.Join(starts, ",") != strings.Join(wantStarts, ",") {
		t.Fatalf("starts = %v, want %v", starts, wantStarts)
	}
	if len(kills) != 3 {
		t.Fatalf("rollback kills = %v, want memory+knowledge+chat", kills)
	}
	// Rollback must be newest-first.
	if kills[0] < kills[1] {
		t.Fatalf("kills not newest-first: %v", kills)
	}
	for _, name := range wantStarts {
		item, _ := m.Contract.Item(name)
		if _, statErr := os.Stat(m.pidPath(item)); !os.IsNotExist(statErr) {
			t.Fatalf("pid file for %s must be removed after rollback", name)
		}
	}
	// Items after chat must never start.
	for _, name := range starts {
		if name == "a2a" {
			t.Fatal("items after the failed step must not start")
		}
	}
}

func TestStartErrorRollsBackPreviousProcesses(t *testing.T) {
	m, _, runner := newTestManager(t)
	runner.failStart["knowledge"] = errors.New("boom")
	_, err := m.Up(context.Background())
	if err == nil {
		t.Fatal("Up must fail")
	}
	var step *StepError
	if !errors.As(err, &step) || step.Item != "knowledge" {
		t.Fatalf("error = %v", err)
	}
	_, starts, kills := runner.snapshot()
	if strings.Join(starts, ",") != "memory" {
		t.Fatalf("starts = %v", starts)
	}
	if len(kills) != 1 {
		t.Fatalf("kills = %v, want memory rolled back", kills)
	}
}

func TestContainerStartFailureStopsBeforeProcesses(t *testing.T) {
	m, _, runner := newTestManager(t)
	runner.failRun["postgres"] = errors.New("docker down")
	_, err := m.Up(context.Background())
	if err == nil {
		t.Fatal("Up must fail")
	}
	_, starts, kills := runner.snapshot()
	if len(starts) != 0 || len(kills) != 0 {
		t.Fatalf("no process should start: starts=%v kills=%v", starts, kills)
	}
}

func TestStatusReportsPerItemHealth(t *testing.T) {
	m, prober, _ := newTestManager(t)
	prober.set("gateway", true)
	rep, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(rep.Items) != 14 {
		t.Fatalf("items = %d", len(rep.Items))
	}
	for _, it := range rep.Items {
		if want := it.Name == "gateway"; it.Healthy != want {
			t.Fatalf("%s healthy = %v", it.Name, it.Healthy)
		}
	}
	if rep.Healthy() {
		t.Fatal("report with unhealthy items must not be healthy")
	}
}

func TestDownStopsReverseOrderAndRemovesPIDFiles(t *testing.T) {
	m, _, runner := newTestManager(t)
	if err := m.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	// Give every process a PID file; containers are stopped by command.
	var processNames []string
	for _, it := range m.Contract.Items {
		if it.IsProcess() {
			processNames = append(processNames, it.Name)
			if err := writePIDFile(m.pidPath(it), 7000+len(processNames)); err != nil {
				t.Fatal(err)
			}
		}
	}
	rep, err := m.Down(context.Background(), false)
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if len(rep.Items) != 14 {
		t.Fatalf("items = %d", len(rep.Items))
	}
	_, _, kills := runner.snapshot()
	if len(kills) != len(processNames) {
		t.Fatalf("kills = %d, want %d", len(kills), len(processNames))
	}
	// Reverse contract order: vite first, memory last.
	wantFirst := 7000 + indexOf(processNames, "vite") + 1
	if kills[0] != wantFirst {
		t.Fatalf("first kill = %d, want vite pid %d", kills[0], wantFirst)
	}
	for _, it := range m.Contract.Items {
		if _, statErr := os.Stat(m.pidPath(it)); !os.IsNotExist(statErr) {
			t.Fatalf("pid file for %s must be removed", it.Name)
		}
	}
	runs, _, _ := runner.snapshot()
	if strings.Join(runs, ",") != "redis-stop,qdrant-stop,mongo-stop,postgres-stop" {
		t.Fatalf("container stop order = %v", runs)
	}
}

func TestDownKeepDataSkipsComposeContainers(t *testing.T) {
	m, _, runner := newTestManager(t)
	rep, err := m.Down(context.Background(), true)
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	runs, _, _ := runner.snapshot()
	if strings.Join(runs, ",") != "redis-stop" {
		t.Fatalf("keep-db must only stop redis, got %v", runs)
	}
	for _, it := range rep.Items {
		if it.Name == "postgres" && it.Detail != "kept (--keep-db)" {
			t.Fatalf("postgres detail = %q", it.Detail)
		}
	}
}

func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

// --- T-P2-43: the reuse gate is the declared probe, not a service name ---

// agentProc builds a minimal valid agent-group process item for fixtures.
func agentProc(name string, port int) Item {
	return Item{
		Name: name, Kind: KindProcess, Group: GroupAgent, Port: port,
		Command: "true",
		Health:  Health{Type: HealthPort, Address: "127.0.0.1:" + strconv.Itoa(port)},
	}
}

// probeOf flips the probe:true flag on a fixture item.
func probeOf(it Item) Item {
	it.Probe = true
	return it
}

func boardFrontend() Item {
	return Item{
		Name: "vite", Kind: KindFrontend, Group: GroupBoard, Port: 5174,
		Command: "node vite.js",
		Health:  Health{Type: HealthHTTP, URL: "http://127.0.0.1:5174/harness-loop/monitor", ExpectStatus: 200},
	}
}

// newFixtureManager wires a Manager around an in-memory contract so probe
// behaviour can be tested without touching scripts/dev-stack.json.
func newFixtureManager(t *testing.T, items ...Item) (*Manager, *fakeProber, *fakeRunner) {
	t.Helper()
	contract := &Contract{Version: SupportedVersion, Items: items}
	if err := contract.Validate(); err != nil {
		t.Fatalf("fixture contract invalid: %v", err)
	}
	m := New(t.TempDir(), contract)
	prober := newFakeProber()
	runner := newFakeRunner(prober, contract)
	m.Prober = prober
	m.Runner = runner
	m.PrepareConfig = func(string, Item) error { return nil }
	m.PollInterval = 2 * time.Millisecond
	m.StartTimeout = 40 * time.Millisecond
	m.KillGrace = 20 * time.Millisecond
	return m, prober, runner
}

// probe 缺失时回退到 HarnessLoop()：老 fixture（只有 harnessloop 名字、没有
// probe 标志）必须保持原有复用语义。
func TestEnsureFallsBackToHarnessLoopWhenProbeMissing(t *testing.T) {
	m, prober, runner := newFixtureManager(t,
		agentProc("harnessloop", 50013),
		agentProc("memory", 50003),
	)

	prober.set("harnessloop", true) // memory stays down on purpose
	rep, err := m.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !rep.Reused {
		t.Fatal("Ensure must reuse on the legacy harnessloop probe")
	}
	if len(rep.Items) != 2 {
		t.Fatalf("status items = %d, want 2", len(rep.Items))
	}
	runs, starts, _ := runner.snapshot()
	if len(runs)+len(starts) != 0 {
		t.Fatalf("reuse must start nothing: runs=%v starts=%v", runs, starts)
	}

	sub, subProber, subRunner := newFixtureManager(t,
		agentProc("harnessloop", 50013),
		agentProc("memory", 50003),
	)
	subProber.set("harnessloop", true)
	rep, err = sub.EnsureGroups(context.Background(), GroupAgent)
	if err != nil {
		t.Fatalf("EnsureGroups: %v", err)
	}
	if !rep.Reused {
		t.Fatal("EnsureGroups must reuse on the legacy harnessloop probe")
	}
	if r, s, _ := subRunner.snapshot(); len(r)+len(s) != 0 {
		t.Fatalf("reuse must start nothing: runs=%v starts=%v", r, s)
	}
}

// probe 在选中集：探针健康即复用，其余选中项短暂 unhealthy 不影响判断。
func TestEnsureGroupsReusesOnDeclaredProbeInSelection(t *testing.T) {
	m, prober, runner := newFixtureManager(t,
		probeOf(agentProc("probe-svc", 50013)),
		agentProc("memory", 50003),
		agentProc("gateway", 9000),
	)
	prober.set("probe-svc", true)

	rep, err := m.EnsureGroups(context.Background(), GroupAgent)
	if err != nil {
		t.Fatalf("EnsureGroups: %v", err)
	}
	if !rep.Reused {
		t.Fatal("a healthy declared probe inside the selection must reuse")
	}
	if len(rep.Items) != 3 {
		t.Fatalf("status items = %d, want 3", len(rep.Items))
	}
	if r, s, _ := runner.snapshot(); len(r)+len(s) != 0 {
		t.Fatalf("reuse must start nothing: runs=%v starts=%v", r, s)
	}

	// 探针不健康时必须建栈，而不是靠「其他项都健康」蒙混。
	prober.set("probe-svc", false)
	prober.set("memory", true)
	prober.set("gateway", true)
	rep, err = m.EnsureGroups(context.Background(), GroupAgent)
	if err != nil {
		t.Fatalf("EnsureGroups: %v", err)
	}
	if rep.Reused {
		t.Fatal("an unhealthy probe must not reuse, even if the rest is healthy")
	}
	if _, starts, _ := runner.snapshot(); strings.Join(starts, ",") != "probe-svc" {
		t.Fatalf("starts = %v, want only the down probe", starts)
	}
}

// probe 不在选中集：没有全局信号可信，退化为「选中项全健康才复用」。
func TestEnsureGroupsProbeOutsideSelectionDegradesToPerItemHealth(t *testing.T) {
	m, prober, runner := newFixtureManager(t,
		probeOf(agentProc("probe-svc", 50013)),
		boardFrontend(),
	)
	prober.set("probe-svc", true) // healthy but outside the selection

	rep, err := m.EnsureGroups(context.Background(), GroupBoard)
	if err != nil {
		t.Fatalf("EnsureGroups: %v", err)
	}
	if rep.Reused {
		t.Fatal("board-only selection must not reuse on the agent-group probe")
	}
	if len(rep.Items) != 1 || rep.Items[0].Name != "vite" || !rep.Items[0].Started {
		t.Fatalf("board-only report = %+v", rep.Items)
	}
	if _, starts, _ := runner.snapshot(); strings.Join(starts, ",") != "vite" {
		t.Fatalf("starts = %v, want [vite]", starts)
	}

	// 全健康时仍然复用。
	prober.set("vite", true)
	rep, err = m.EnsureGroups(context.Background(), GroupBoard)
	if err != nil {
		t.Fatalf("EnsureGroups: %v", err)
	}
	if !rep.Reused {
		t.Fatal("a fully healthy selection must reuse without the probe")
	}
}

// 命名不产生隐式耦合：新增一个叫 harnessloop 的项（非探针）不得取代声明的探针，
// 也不得让复用判定回到「按名字猜」。
func TestNamingDoesNotCreateImplicitProbeCoupling(t *testing.T) {
	m, prober, runner := newFixtureManager(t,
		agentProc("harnessloop", 50013),            // 只是普通服务，healthy
		probeOf(agentProc("obs-collector", 50020)), // 声明的探针，down
		agentProc("memory", 50003),
	)
	prober.set("harnessloop", true)
	prober.set("memory", true)

	rep, err := m.EnsureGroups(context.Background(), GroupAgent)
	if err != nil {
		t.Fatalf("EnsureGroups: %v", err)
	}
	if rep.Reused {
		t.Fatal("an item named harnessloop must not act as the probe")
	}
	if _, starts, _ := runner.snapshot(); strings.Join(starts, ",") != "obs-collector" {
		t.Fatalf("starts = %v, want only the declared probe", starts)
	}
}

// 没有任何探针也没有 harnessloop 时，Ensure 必须显式报错而不是静默全量起栈。
func TestEnsureWithoutProbeOrLegacyItemFails(t *testing.T) {
	m, _, _ := newFixtureManager(t, agentProc("memory", 50003))
	_, err := m.Ensure(context.Background())
	if !errors.Is(err, ErrNoProbe) {
		t.Fatalf("Ensure error = %v, want ErrNoProbe", err)
	}
}

func TestDockerStopCommand(t *testing.T) {
	tests := []struct {
		command string
		want    string
		ok      bool
	}{
		{"docker compose -f docker/docker-compose.data.yaml up -d postgres", "docker compose -f docker/docker-compose.data.yaml stop postgres", true},
		{"docker run -d --name hlp-dev-redis -p 56379:6379 redis:7-alpine", "docker stop hlp-dev-redis", true},
		{".harness/dev/bin/gateway", "", false},
		{"docker", "", false},
	}
	for _, tc := range tests {
		got, ok := dockerStopCommand(tc.command)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("%q -> %q,%v want %q,%v", tc.command, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTimeoutForKind(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.StartTimeout = 0
	if got := m.timeoutFor(Item{Kind: KindContainer}); got != 90*time.Second {
		t.Fatalf("container timeout = %s", got)
	}
	if got := m.timeoutFor(Item{Kind: KindFrontend}); got != 120*time.Second {
		t.Fatalf("frontend timeout = %s", got)
	}
	if got := m.timeoutFor(Item{Kind: KindProcess}); got != 60*time.Second {
		t.Fatalf("process timeout = %s", got)
	}
	m.StartTimeout = time.Second
	if got := m.timeoutFor(Item{Kind: KindContainer}); got != time.Second {
		t.Fatalf("override timeout = %s", got)
	}
}

func TestResolveCommandAddsExeOnWindows(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, ".harness", "dev", "bin", "memory-service.exe")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := resolveCommand(".harness/dev/bin/memory-service", root, "windows", KindProcess)
	want := `.\` + filepath.FromSlash(".harness/dev/bin/memory-service") + ".exe"
	if got != want {
		t.Fatalf("windows command = %q, want %q", got, want)
	}
	if got := resolveCommand(".harness/dev/bin/memory-service", root, "linux", KindProcess); got != ".harness/dev/bin/memory-service" {
		t.Fatalf("linux command = %q", got)
	}
	if got := resolveCommand(".harness/dev/bin/missing", root, "windows", KindProcess); got != ".harness/dev/bin/missing" {
		t.Fatalf("missing binary must keep command, got %q", got)
	}
	if got := resolveCommand("node frontend/node_modules/vite/bin/vite.js", root, "windows", KindFrontend); got != "node frontend/node_modules/vite/bin/vite.js" {
		t.Fatalf("frontend command = %q", got)
	}
}
