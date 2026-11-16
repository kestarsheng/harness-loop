package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"agent-platform/internal/cli/tui/views"
)

const (
	flushInterval = 100 * time.Millisecond
	pollInterval  = time.Second
	reconnectMin  = time.Second
	reconnectMax  = 10 * time.Second
	maxEvents     = 500
	maxStream     = 500
	maxEvidence   = 3
)

type ViewMode int

const (
	ViewMain ViewMode = iota
	ViewLogs
	ViewMemory
	ViewHelp
	ViewSubtasks
	ViewModels
	ViewHealth
	ViewArtifacts
	ViewKB
)

type Evidence struct {
	Kind    string
	Ref     string
	Excerpt string
}

type FailedCriterion struct {
	ID       string
	Category string
}

type TaskState struct {
	TaskID         string
	Status         string
	TerminalReason string
	CurrentStage   string
	Attempt        int

	Attempts            map[string]int
	RetryCount          int
	AcceptanceFailCount int

	BudgetTokens   int64
	BudgetCost     float64
	ConsumedTokens int64
	ConsumedCost   float64

	StageStartedAt time.Time
}

func (s TaskState) terminal() bool {
	switch s.Status {
	case "completed", "failed", "aborted":
		return true
	}
	return false
}

type Event struct {
	Seq      int64
	Type     string
	Stage    string
	Severity string
	Attempt  int
	Time     time.Time
	Payload  []byte
}

type WatchEvent struct {
	Event Event
	Err   error
}

type Source interface {
	Task(ctx context.Context) (TaskState, error)
	Abort(ctx context.Context) error
	ListEvents(ctx context.Context, sinceSeq int64) ([]Event, int64, error)
	Watch(ctx context.Context, sinceSeq int64) (<-chan WatchEvent, error)
	Subtasks(ctx context.Context) ([]Subtask, error)
	ModelSelections(ctx context.Context) ([]ModelSelection, error)
	Handoffs(ctx context.Context) ([]Handoff, error)
	Checkpoints(ctx context.Context) ([]Checkpoint, error)
}

type Options struct {
	NoColor bool
	Now     func() time.Time

	// WebURL is the fixed monitoring board URL shown at the top of the TUI.
	// When empty the URL bar is omitted (legacy run behaviour).
	WebURL    string
	Hyperlink bool
	// OpenURL opens a URL with the system browser. Defaults to DefaultOpenURL.
	OpenURL func(string) error
	// KB is the optional knowledge-service source used by the TUI sidebar.
	KB     KBSource
	KBTopK int
	// InitialView and InitialQuery open a panel on boot for automated terminal
	// evidence runs (HARNESS_TUI_VIEW / HARNESS_TUI_KB_QUERY).
	InitialView  string
	InitialQuery string
}

type Model struct {
	state TaskState

	verdicts   map[string]string
	verifiedBy map[string]string

	retryByCategory map[string]int
	evidence        []Evidence
	failed          []FailedCriterion

	events  []views.EventLine
	stream  []views.StreamLine
	pending []views.StreamLine

	lastSeq int64

	mode     ViewMode
	prevMode ViewMode

	width  int
	height int

	src      Source
	ctx      context.Context
	watchCh  <-chan WatchEvent
	theme    Theme
	now      func() time.Time
	terminal bool

	webURL    string
	hyperlink bool
	openURL   func(string) error
	opening   bool

	connected        bool
	reconnecting     bool
	reconnectAttempt int
	reconnectDelay   time.Duration

	aborting bool
	errText  string

	subagents views.SubagentData
	models    views.ModelData
	kb        views.KBData
	kbSrc     KBSource
	kbTopK    int

	healthEvents      int
	healthErrors      int
	healthWarnings    int
	healthLastEvent   time.Time
	incidents         []views.Incident
	pendingApprovals  map[string]bool
	modelFallbacks    int
	modelSwitches     int
	modelRecovered    int
	modelUnavailable  int
	modelRejected     int
	subtaskFailed     int
	circuitOpen       int
	checkpointDrift   int
	memoryDegraded    int
	compactionDegrade int
	stateRebuilds     int
	securityDenied    int
	toolDenied        int

	pollTick int
}

type taskMsg struct {
	State TaskState
	Err   error
}

type catchUpMsg struct {
	Events []Event
	Next   int64
	Err    error
}

type watchStartedMsg struct {
	Ch <-chan WatchEvent
}

type eventMsg struct {
	Event Event
}

type watchErrMsg struct {
	Err error
}

type reconnectMsg struct{}

type abortMsg struct {
	Err error
}

type openURLMsg struct {
	URL string
	Err error
}

type flushMsg struct{}

type pollMsg struct{}

func NewModel(state TaskState, src Source, opts Options) *Model {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if state.Attempts == nil {
		state.Attempts = map[string]int{}
	}
	if state.Attempt == 0 {
		state.Attempt = state.Attempts[state.CurrentStage]
	}
	if state.StageStartedAt.IsZero() {
		state.StageStartedAt = now()
	}
	open := opts.OpenURL
	if open == nil {
		open = DefaultOpenURL
	}
	m := &Model{
		state:            state,
		verdicts:         map[string]string{},
		verifiedBy:       map[string]string{},
		retryByCategory:  map[string]int{},
		width:            100,
		height:           30,
		src:              src,
		theme:            NewTheme(opts.NoColor),
		now:              now,
		connected:        true,
		reconnectDelay:   reconnectMin,
		webURL:           opts.WebURL,
		hyperlink:        opts.Hyperlink,
		openURL:          open,
		kbSrc:            opts.KB,
		kbTopK:           opts.KBTopK,
		pendingApprovals: map[string]bool{},
	}
	if m.kbTopK <= 0 {
		m.kbTopK = 8
	}
	if mode, ok := parseViewMode(opts.InitialView); ok {
		m.mode = mode
	}
	if strings.TrimSpace(opts.InitialQuery) != "" {
		m.kb.Query = strings.TrimSpace(opts.InitialQuery)
	}
	return m
}

func (m *Model) WithContext(ctx context.Context) *Model {
	m.ctx = ctx
	return m
}

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		tea.Tick(flushInterval, func(time.Time) tea.Msg { return flushMsg{} }),
		tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} }),
	}
	if m.src != nil {
		cmds = append(cmds, m.catchUpCmd())
	}
	if cmd := m.refreshPanelCmd(m.mode); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.mode == ViewKB && m.kb.Query != "" {
		if cmd := m.kbSearchCmd(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case flushMsg:
		m.flushPending()
		if m.terminal {
			return m, nil
		}
		return m, tea.Tick(flushInterval, func(time.Time) tea.Msg { return flushMsg{} })

	case pollMsg:
		if m.terminal {
			return m, nil
		}
		m.pollTick++
		cmds := []tea.Cmd{
			m.fetchTaskCmd(),
			tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} }),
		}
		if m.pollTick%5 == 0 {
			if cmd := m.refreshPanelCmd(m.mode); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)

	case panelMsg:
		m.applyPanelMsg(msg)
		return m, nil

	case kbSearchMsg:
		m.applyKBSearchMsg(msg)
		return m, nil

	case taskMsg:
		if msg.Err != nil {
			m.errText = "task refresh failed: " + msg.Err.Error()
			return m, nil
		}
		m.mergeTask(msg.State)
		if m.terminal {
			return m, tea.Quit
		}
		return m, nil

	case catchUpMsg:
		if msg.Err != nil {
			return m, m.scheduleReconnect(msg.Err)
		}
		for _, ev := range msg.Events {
			m.applyEvent(ev)
		}
		if msg.Next > m.lastSeq {
			m.lastSeq = msg.Next
		}
		if m.terminal {
			return m, tea.Quit
		}
		return m, m.connectCmd()

	case watchStartedMsg:
		m.watchCh = msg.Ch
		m.connected = true
		m.reconnecting = false
		m.reconnectAttempt = 0
		m.reconnectDelay = reconnectMin
		m.errText = ""
		return m, m.nextEventCmd()

	case eventMsg:
		m.applyEvent(msg.Event)
		if m.terminal {
			return m, tea.Quit
		}
		return m, m.nextEventCmd()

	case watchErrMsg:
		return m, m.scheduleReconnect(msg.Err)

	case reconnectMsg:
		if m.terminal {
			return m, nil
		}
		return m, m.catchUpCmd()

	case abortMsg:
		m.aborting = false
		if msg.Err != nil {
			m.errText = "abort failed: " + msg.Err.Error()
		}
		return m, nil

	case openURLMsg:
		m.opening = false
		if msg.Err != nil {
			m.errText = "opening monitor failed: " + msg.Err.Error()
		} else {
			m.errText = "opened " + msg.URL + " in the system browser"
		}
		return m, nil
	}
	return m, nil
}

func (m *Model) mergeTask(next TaskState) {
	if next.Attempts == nil {
		next.Attempts = map[string]int{}
	}
	if next.Attempt == 0 {
		next.Attempt = next.Attempts[next.CurrentStage]
	}
	if !m.state.StageStartedAt.IsZero() && next.CurrentStage == m.state.CurrentStage {
		next.StageStartedAt = m.state.StageStartedAt
	}
	if next.StageStartedAt.IsZero() {
		next.StageStartedAt = m.now()
	}
	m.state = next
	if next.terminal() {
		m.terminal = true
	}
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.mode == ViewKB {
		switch key {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.mode = ViewMain
			return m, nil
		case "enter":
			return m, m.kbSearchCmd()
		case "backspace", "ctrl+h":
			runes := []rune(m.kb.Query)
			if len(runes) > 0 {
				m.kb.Query = string(runes[:len(runes)-1])
			}
			return m, nil
		}
		if msg.Type == tea.KeyRunes {
			runes := []rune(m.kb.Query)
			for _, r := range msg.Runes {
				if len(runes) < 200 {
					runes = append(runes, r)
				}
			}
			m.kb.Query = string(runes)
			return m, nil
		}
		if msg.Type == tea.KeySpace {
			if len([]rune(m.kb.Query)) < 200 {
				m.kb.Query += " "
			}
			return m, nil
		}
		return m, nil
	}
	switch key {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "a":
		if m.terminal || m.aborting {
			return m, nil
		}
		cmd := m.abortCmd()
		if cmd == nil {
			return m, nil
		}
		m.aborting = true
		return m, cmd
	case "l":
		return m.toggleView(ViewLogs, nil)
	case "m":
		return m.toggleView(ViewMemory, nil)
	case "s":
		return m.toggleView(ViewSubtasks, m.refreshPanelCmd(ViewSubtasks))
	case "x":
		return m.toggleView(ViewModels, m.refreshPanelCmd(ViewModels))
	case "h", "!":
		return m.toggleView(ViewHealth, nil)
	case "c":
		return m.toggleView(ViewArtifacts, m.refreshPanelCmd(ViewArtifacts))
	case "k":
		if m.mode != ViewKB {
			m.prevMode = m.mode
			m.mode = ViewKB
		}
		return m, nil
	case "o":
		if m.opening {
			return m, nil
		}
		cmd := m.openURLCmd()
		if cmd == nil {
			return m, nil
		}
		m.opening = true
		return m, cmd
	case "?":
		return m.toggleView(ViewHelp, nil)
	case "esc":
		m.mode = ViewMain
		return m, nil
	}
	return m, nil
}

func (m *Model) toggleView(target ViewMode, load tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mode == target {
		m.mode = ViewMain
		return m, nil
	}
	m.prevMode = m.mode
	m.mode = target
	return m, load
}

func (m *Model) View() string {
	width := m.width
	if width <= 0 {
		width = 100
	}
	parts := make([]string, 0, 6)
	if bar := views.RenderURLBar(RenderHyperlink(m.webURL, m.hyperlink), m.theme); bar != "" {
		parts = append(parts, bar)
	}
	parts = append(parts, views.RenderHeader(m.stageData()))
	switch m.mode {
	case ViewLogs:
		parts = append(parts, views.RenderLogs(m.events, width, m.height-4, m.theme))
	case ViewHelp:
		parts = append(parts, views.RenderHelp(m.theme))
	case ViewMemory:
		parts = append(parts, views.RenderMemory(m.theme))
	case ViewSubtasks:
		height := m.height - 5
		if height < 8 {
			height = 16
		}
		parts = append(parts, views.RenderSubtasks(m.subagents, width, height/2, m.theme))
		parts = append(parts, views.RenderHandoffs(m.subagents, width, height-height/2, m.theme))
	case ViewModels:
		parts = append(parts, views.RenderModels(m.models, width, maxLinesFor(m.height-5, 12), m.theme))
	case ViewHealth:
		parts = append(parts, views.RenderHealth(m.healthData(), width, maxLinesFor(m.height-5, 12), m.theme))
	case ViewArtifacts:
		parts = append(parts, views.RenderArtifacts(m.subagents, width, maxLinesFor(m.height-5, 12), m.theme))
	case ViewKB:
		parts = append(parts, views.RenderKB(m.kbData(), width, maxLinesFor(m.height-5, 12), m.theme))
	default:
		parts = append(parts, views.RenderStage(m.stageData()))
		fixed := 7
		if m.webURL != "" {
			fixed++
		}
		budget := m.height - fixed
		if budget < 6 {
			budget = 12
		}
		streamMax := budget / 3
		if streamMax < 2 {
			streamMax = 2
		}
		eventsMax := budget - streamMax - 1
		if eventsMax < 2 {
			eventsMax = 2
		}
		parts = append(parts, views.RenderStream(m.stream, width, streamMax, m.theme))
		parts = append(parts, views.RenderEvents(m.events, width, eventsMax, m.theme))
	}
	parts = append(parts, views.RenderFooter(m.modeName(), width, m.theme))
	return strings.Join(parts, "\n")
}

func parseViewMode(name string) (ViewMode, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "logs":
		return ViewLogs, true
	case "memory":
		return ViewMemory, true
	case "subtasks", "subagents", "dag":
		return ViewSubtasks, true
	case "models":
		return ViewModels, true
	case "health", "incidents":
		return ViewHealth, true
	case "artifacts", "checkpoints":
		return ViewArtifacts, true
	case "kb", "knowledge":
		return ViewKB, true
	}
	return ViewMain, false
}

func maxLinesFor(height, fallback int) int {
	if height <= 0 {
		return fallback
	}
	if height < 3 {
		return 3
	}
	return height
}

func (m *Model) modeName() string {
	switch m.mode {
	case ViewLogs:
		return "logs"
	case ViewHelp:
		return "help"
	case ViewMemory:
		return "memory"
	case ViewSubtasks:
		return "subtasks"
	case ViewModels:
		return "models"
	case ViewHealth:
		return "health"
	case ViewArtifacts:
		return "artifacts"
	case ViewKB:
		return "knowledge"
	default:
		return "main"
	}
}

func (m *Model) stageData() views.StageData {
	attempts := make(map[string]int, len(m.state.Attempts))
	for stage, attempt := range m.state.Attempts {
		attempts[stage] = attempt
	}
	retry := make(map[string]int, len(m.retryByCategory))
	for category, count := range m.retryByCategory {
		retry[category] = count
	}
	verdicts := make(map[string]string, len(m.verdicts))
	for stage, verdict := range m.verdicts {
		verdicts[stage] = verdict
	}
	verified := make(map[string]string, len(m.verifiedBy))
	for stage, by := range m.verifiedBy {
		verified[stage] = by
	}
	elapsed := time.Duration(0)
	if !m.state.StageStartedAt.IsZero() {
		elapsed = m.now().Sub(m.state.StageStartedAt)
	}
	return views.StageData{
		TaskID:              m.state.TaskID,
		Status:              m.state.Status,
		TerminalReason:      m.state.TerminalReason,
		CurrentStage:        m.state.CurrentStage,
		Attempt:             m.state.Attempt,
		Attempts:            attempts,
		RetryByCategory:     retry,
		RetryCount:          m.state.RetryCount,
		AcceptanceFailCount: m.state.AcceptanceFailCount,
		Verdicts:            verdicts,
		VerifiedBy:          verified,
		Tokens:              m.state.ConsumedTokens,
		BudgetTokens:        m.state.BudgetTokens,
		Cost:                m.state.ConsumedCost,
		BudgetCost:          m.state.BudgetCost,
		Elapsed:             elapsed,
		Connected:           m.connected,
		Reconnecting:        m.reconnecting,
		ReconnectAttempt:    m.reconnectAttempt,
		Aborting:            m.aborting,
		Error:               m.errText,
	}
}

func (m *Model) flushPending() {
	if len(m.pending) == 0 {
		return
	}
	m.stream = append(m.stream, m.pending...)
	m.pending = nil
	if len(m.stream) > maxStream {
		m.stream = m.stream[len(m.stream)-maxStream:]
	}
}

func (m *Model) applyEvent(ev Event) {
	if ev.Seq > 0 && ev.Seq <= m.lastSeq {
		return
	}
	payload := parsePayload(ev.Payload)
	typ := ev.Type
	stage := ev.Stage
	if stage == "" {
		stage = payloadString(payload, "stage")
	}
	attempt := ev.Attempt
	if attempt == 0 {
		attempt = payloadInt(payload, "attempt")
	}
	if attempt == 0 {
		attempt = payloadInt(payload, "attempt_no")
	}
	if ev.Stage == "" {
		ev.Stage = stage
	}
	m.recordEventHealth(ev, typ, payload)

	line := views.EventLine{
		Seq:      ev.Seq,
		Time:     ev.Time,
		Type:     typ,
		Stage:    stage,
		Attempt:  attempt,
		Severity: ev.Severity,
	}

	switch typ {
	case "stage.entered", "stage.transition":
		to := payloadString(payload, "to")
		if to == "" {
			to = stage
		}
		if to != "" {
			if m.state.CurrentStage != to {
				m.state.StageStartedAt = ev.Time
			}
			m.state.CurrentStage = to
		}
		if attempt > 0 {
			m.state.Attempt = attempt
			if to != "" {
				m.state.Attempts[to] = attempt
			}
		}
		line.Summary = transitionSummary(payload, typ)

	case "acceptance.started":
		line.Summary = "acceptance started"

	case "acceptance.passed", "acceptance.failed", "acceptance.invalid":
		verdict := payloadString(payload, "verdict")
		if verdict == "" {
			verdict = strings.TrimPrefix(typ, "acceptance.")
		}
		if stage != "" {
			m.verdicts[stage] = verdict
			m.verifiedBy[stage] = payloadString(payload, "verified_by")
		}
		if verdict == "failed" || verdict == "invalid" {
			m.evidence = parseEvidence(payload, maxEvidence)
			m.failed = parseFailedCriteria(payload)
			for _, item := range m.evidence {
				m.appendEventLine(views.EventLine{
					Seq: ev.Seq, Time: ev.Time, Type: "evidence", Stage: stage, Kind: "evidence",
					Summary: strings.TrimSpace(strings.Join([]string{item.Kind, item.Ref, item.Excerpt}, " ")),
				})
			}
			for _, item := range m.failed {
				m.appendEventLine(views.EventLine{
					Seq: ev.Seq, Time: ev.Time, Type: "failed_criteria", Stage: stage, Kind: "failed_criteria",
					Summary: strings.TrimSpace(item.ID + " " + item.Category),
				})
			}
			m.appendStream(views.StreamLine{Time: ev.Time, Stage: stage, Text: "acceptance " + verdict + " → " + rollbackTarget(payload), Error: true})
		} else {
			m.evidence = nil
			m.failed = nil
			m.appendStream(views.StreamLine{Time: ev.Time, Stage: stage, Text: "acceptance passed"})
		}
		line.Summary = acceptanceSummary(payload, verdict)

	case "retry.scheduled", "retry.exhausted":
		category := payloadString(payload, "category")
		if category == "" {
			category = "unknown"
		}
		m.retryByCategory[category]++
		line.Summary = payloadString(payload, "reason")
		if line.Summary == "" {
			line.Summary = category
		}
		m.appendStream(views.StreamLine{Time: ev.Time, Stage: stage, Text: fmt.Sprintf("retry %s: %s", category, line.Summary)})

	case "error":
		category := payloadString(payload, "category")
		if category == "" {
			category = "system"
		}
		reason := payloadString(payload, "reason")
		m.retryByCategory[category]++
		line.Error = true
		line.Summary = reason
		if line.Summary == "" {
			line.Summary = category
		}
		m.appendStream(views.StreamLine{Time: ev.Time, Stage: stage, Text: fmt.Sprintf("error(%s): %s", category, line.Summary), Error: true})

	case "tool.started", "tool.result", "tool.progress":
		line.Summary = toolSummary(payload)
		m.appendStream(views.StreamLine{Time: ev.Time, Stage: stage, Text: toolStreamText(typ, payload)})

	case "invalid_finish":
		line.Error = true
		line.Summary = "stage_finish missing or malformed"
		m.appendStream(views.StreamLine{Time: ev.Time, Stage: stage, Text: "no_finish: " + line.Summary, Error: true})

	case "security.violation":
		line.Error = true
		line.Summary = payloadString(payload, "decision") + " " + payloadString(payload, "target")
		m.appendStream(views.StreamLine{Time: ev.Time, Stage: stage, Text: "security: " + line.Summary, Error: true})

	case "task.completed", "task.failed", "task.aborted":
		reason := payloadString(payload, "terminal_reason")
		if reason == "" {
			reason = strings.TrimPrefix(typ, "task.")
		}
		m.state.Status = strings.TrimPrefix(typ, "task.")
		m.state.TerminalReason = reason
		m.terminal = true
		line.Summary = reason

	case "checkpoint.drift", "checkpoint.corrupted", "compaction.degraded", "memory.index_degraded":
		line.Error = true
		line.Summary = eventHint(typ)
	}

	line.Summary = strings.TrimSpace(line.Summary)
	if line.Summary == "" {
		line.Summary = strings.TrimSpace(defaultSummary(payload))
	}
	if line.Summary == "" {
		line.Summary = eventHint(typ)
	}
	m.appendEventLine(line)
	if ev.Seq > m.lastSeq {
		m.lastSeq = ev.Seq
	}
}

func (m *Model) appendEventLine(line views.EventLine) {
	m.events = append(m.events, line)
	if len(m.events) > maxEvents {
		m.events = m.events[len(m.events)-maxEvents:]
	}
}

func (m *Model) appendStream(line views.StreamLine) {
	m.pending = append(m.pending, line)
}

func (m *Model) scheduleReconnect(err error) tea.Cmd {
	m.connected = false
	m.reconnecting = true
	m.reconnectAttempt++
	if err != nil {
		m.errText = "event stream: " + err.Error()
	}
	delay := m.reconnectDelay
	m.reconnectDelay *= 2
	if m.reconnectDelay > reconnectMax {
		m.reconnectDelay = reconnectMax
	}
	return tea.Tick(delay, func(time.Time) tea.Msg { return reconnectMsg{} })
}

func (m *Model) baseCtx() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func (m *Model) fetchTaskCmd() tea.Cmd {
	src := m.src
	if src == nil {
		return nil
	}
	ctx := m.baseCtx()
	return func() tea.Msg {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		state, err := src.Task(callCtx)
		return taskMsg{State: state, Err: err}
	}
}

func (m *Model) catchUpCmd() tea.Cmd {
	src := m.src
	if src == nil {
		return nil
	}
	ctx := m.baseCtx()
	since := m.lastSeq
	return func() tea.Msg {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		events, next, err := src.ListEvents(callCtx, since)
		return catchUpMsg{Events: events, Next: next, Err: err}
	}
}

func (m *Model) connectCmd() tea.Cmd {
	src := m.src
	if src == nil {
		return nil
	}
	ctx := m.baseCtx()
	since := m.lastSeq
	return func() tea.Msg {
		ch, err := src.Watch(ctx, since)
		if err != nil {
			return watchErrMsg{Err: err}
		}
		return watchStartedMsg{Ch: ch}
	}
}

func (m *Model) nextEventCmd() tea.Cmd {
	ch := m.watchCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		item, ok := <-ch
		if !ok {
			return watchErrMsg{Err: fmt.Errorf("event stream closed")}
		}
		if item.Err != nil {
			return watchErrMsg{Err: item.Err}
		}
		return eventMsg{Event: item.Event}
	}
}

func (m *Model) abortCmd() tea.Cmd {
	src := m.src
	if src == nil || m.state.TaskID == "" {
		return nil
	}
	ctx := m.baseCtx()
	return func() tea.Msg {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return abortMsg{Err: src.Abort(callCtx)}
	}
}

func (m *Model) openURLCmd() tea.Cmd {
	url := m.webURL
	if strings.TrimSpace(url) == "" {
		m.errText = "no monitor URL configured"
		return nil
	}
	open := m.openURL
	return func() tea.Msg {
		if open == nil {
			return openURLMsg{URL: url, Err: fmt.Errorf("no opener configured")}
		}
		return openURLMsg{URL: url, Err: open(url)}
	}
}

func parsePayload(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	return payload
}

func payloadString(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	switch v := payload[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return ""
	}
}

func payloadInt(payload map[string]any, key string) int {
	if payload == nil {
		return 0
	}
	switch v := payload[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	default:
		return 0
	}
}

func parseEvidence(payload map[string]any, limit int) []Evidence {
	items, ok := payload["evidence"].([]any)
	if !ok {
		return nil
	}
	out := make([]Evidence, 0, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, Evidence{
			Kind:    stringField(record, "kind"),
			Ref:     stringField(record, "ref"),
			Excerpt: truncate(stringField(record, "excerpt"), 120),
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func parseFailedCriteria(payload map[string]any) []FailedCriterion {
	items, ok := payload["failed_criteria"].([]any)
	if !ok {
		return nil
	}
	out := make([]FailedCriterion, 0, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := stringField(record, "criterion_id")
		if id == "" {
			id = stringField(record, "id")
		}
		out = append(out, FailedCriterion{ID: id, Category: stringField(record, "category")})
	}
	return out
}

func stringField(record map[string]any, key string) string {
	if v, ok := record[key].(string); ok {
		return v
	}
	return ""
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func transitionSummary(payload map[string]any, typ string) string {
	from := payloadString(payload, "from")
	to := payloadString(payload, "to")
	switch {
	case from != "" && to != "":
		return from + "→" + to
	case to != "":
		return eventHint(typ) + " " + to
	default:
		return eventHint(typ)
	}
}

func acceptanceSummary(payload map[string]any, verdict string) string {
	target := rollbackTarget(payload)
	switch {
	case target != "":
		return "→ rollback " + target
	case payloadString(payload, "verified_by") != "":
		return "verified by " + payloadString(payload, "verified_by")
	case verdict != "":
		return verdict
	default:
		return "acceptance"
	}
}

func rollbackTarget(payload map[string]any) string {
	return payloadString(payload, "rollback_target")
}

func toolSummary(payload map[string]any) string {
	for _, key := range []string{"tool", "name", "command"} {
		if value := payloadString(payload, key); value != "" {
			if result := payloadString(payload, "result"); result != "" {
				return value + " → " + result
			}
			return value
		}
	}
	return defaultSummary(payload)
}

func toolStreamText(typ string, payload map[string]any) string {
	tool := payloadString(payload, "tool")
	if tool == "" {
		tool = payloadString(payload, "name")
	}
	if tool == "" {
		tool = payloadString(payload, "command")
	}
	if tool == "" {
		tool = "tool"
	}
	switch typ {
	case "tool.started":
		return "[tool] " + tool
	case "tool.result":
		if result := payloadString(payload, "result"); result != "" {
			return "[tool] " + tool + " → " + result
		}
		return "[tool] " + tool + " → done"
	default:
		if progress := payloadString(payload, "summary"); progress != "" {
			return "[tool] " + tool + " " + progress
		}
		return "[tool] " + tool + " …"
	}
}

func defaultSummary(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, 3)
	for _, key := range keys {
		value := payloadString(payload, key)
		if value == "" {
			continue
		}
		parts = append(parts, key+"="+value)
		if len(parts) >= 3 {
			break
		}
	}
	return strings.Join(parts, " ")
}
