package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"agent-platform/internal/cli/tui/views"
)

const (
	flushInterval       = 100 * time.Millisecond
	pollInterval        = time.Second
	spinnerInterval     = 385 * time.Millisecond
	cursorBlinkInterval = 530 * time.Millisecond
	thinkingInterval    = time.Second
	reconnectMin        = time.Second
	reconnectMax        = 10 * time.Second
	maxEvents           = 500
	maxStream           = 500
	maxEvidence         = 3
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

// ChatLine is one rendered turn of the direct-chat conversation.
type ChatLine struct {
	Role      string // "user", "assistant" or "tool"
	Text      string
	Err       bool
	Streaming bool
	Time      time.Time

	// ToolName/ToolArgs are kept for the pending tool row so a tool_result
	// without a name can still rewrite the right label.
	ToolName string
	ToolArgs string
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
	// Session turns this model into a resident multi-task session. It is only
	// set by RunSession; Run keeps the legacy single-task behaviour.
	Session *SessionConfig

	// Status-bar overrides. When empty the model reads the current working
	// directory, the git branch of that directory and defaults the model label
	// to "main-agent"; tests pin them for deterministic output.
	WorkDir   string
	GitBranch string
	ModelName string
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

	// session is nil for the legacy single-task monitor. When set, terminal
	// tasks return to the input box instead of quitting.
	session       *SessionConfig
	input         *PromptModel
	awaitingInput bool
	submitting    bool
	exitRequested bool
	taskIDs       []string

	// chat is the direct-chat conversation shown by default in a session.
	// chatMode switches the main view between the conversation and the a~e
	// stage/event panels; /task flips it off, a plain line flips it on.
	chat          []ChatLine
	chatMode      bool
	chatStreaming bool
	chatCh        <-chan ChatChunk
	chatGen       int
	chatCancel    context.CancelFunc
	// Spinner / thinking state. spinnerFrame drives both the braille spinner
	// and the alternating block cursor; thinkingStarted feeds "Thinking… 3s".
	spinnerFrame    int
	thinking        bool
	thinkingText    string
	thinkingStarted time.Time
	cursorOn        bool
	// blinkRunning guards the single caret-blink chain (see blinkTickCmd).
	blinkRunning bool

	// taskGen tags async messages so a late reply from a previous task can
	// never be applied to the task now on screen.
	taskGen    int
	taskCtx    context.Context
	taskCancel context.CancelFunc

	// Status-bar identity: model label, working directory and git branch.
	workDir   string
	gitBranch string
	modelName string
	// tokensUsed is the direct-chat token usage reported by the reply stream.
	tokensUsed int64
}

type taskMsg struct {
	Gen   int
	State TaskState
	Err   error
}

type catchUpMsg struct {
	Gen    int
	Events []Event
	Next   int64
	Err    error
}

type watchStartedMsg struct {
	Gen int
	Ch  <-chan WatchEvent
}

type eventMsg struct {
	Gen   int
	Event Event
}

type watchErrMsg struct {
	Gen int
	Err error
}

type reconnectMsg struct {
	Gen int
}

type abortMsg struct {
	Err error
}

type openURLMsg struct {
	URL string
	Err error
}

type flushMsg struct {
	Gen int
}

type pollMsg struct {
	Gen int
}

// taskCreatedMsg is the reply to a session goal submission.
type taskCreatedMsg struct {
	Task TaskState
	Src  Source
	Goal string
	Err  error
}

// chatStartedMsg carries the opened reply stream for one direct-chat message.
type chatStartedMsg struct {
	Gen int
	Ch  <-chan ChatChunk
	Err error
}

// chatChunkMsg is one chunk read off the direct-chat stream.
type chatChunkMsg struct {
	Gen   int
	Chunk ChatChunk
	OK    bool
}

// spinTickMsg advances the spinner, the blinking cursor and the thinking clock.
type spinTickMsg struct {
	Gen int
}

// blinkTickMsg toggles the composer caret while the session waits for input.
type blinkTickMsg struct{}

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
	workDir := strings.TrimSpace(opts.WorkDir)
	if workDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			workDir = cwd
		}
	}
	branch := strings.TrimSpace(opts.GitBranch)
	if branch == "" {
		branch = detectGitBranch(workDir)
	}
	modelName := strings.TrimSpace(opts.ModelName)
	if modelName == "" {
		modelName = "main-agent"
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
		workDir:          workDir,
		gitBranch:        branch,
		modelName:        modelName,
		cursorOn:         true,
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
	if opts.Session != nil {
		m.session = opts.Session
		// A session starts in the conversation: a plain line is answered
		// directly, only /task opens the a~e stage view.
		m.chatMode = true
		m.input = NewPromptModel("Harness Loop", opts.WebURL, PromptOptions{
			NoColor:   opts.NoColor,
			Hyperlink: opts.Hyperlink,
			Embedded:  true,
		})
	}
	return m
}

func (m *Model) WithContext(ctx context.Context) *Model {
	m.ctx = ctx
	return m
}

func (m *Model) Init() tea.Cmd {
	if m.session != nil {
		m.awaitingInput = true
		m.input.Reset()
		if goal := strings.TrimSpace(m.session.InitialGoal); goal != "" {
			// --goal / --goal-file keep the one-shot a~e behaviour: the seed
			// always creates a task, even without a /task prefix. Only exit
			// words still short-circuit.
			if DispatchSessionLine(goal).Action != SessionSubmit {
				return m.blinkTickCmd()
			}
			// The seeded task hides the input, so the blink chain starts when
			// the task finishes (terminalCmd) instead of now.
			return m.startTaskSubmit(goal)
		}
		return m.blinkTickCmd()
	}
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
		if m.input != nil {
			_, _ = m.input.Update(msg)
		}
		return m, nil

	case tea.KeyMsg:
		m.setCursorVisible(true)
		if m.chatStreaming && msg.Type == tea.KeyEsc {
			m.interruptChat()
			return m, nil
		}
		if m.session != nil && m.awaitingInput {
			// ? opens the help layer at the composer; Esc closes a panel
			// before it clears the input buffer.
			if msg.String() == "?" && m.input.Empty() {
				return m.toggleView(ViewHelp, nil)
			}
			if msg.Type == tea.KeyEsc && m.mode != ViewMain {
				m.mode = ViewMain
				return m, nil
			}
			return m, m.input.HandleKey(msg)
		}
		return m.handleKey(msg)

	case tea.MouseMsg:
		m.handleMouse(msg)
		return m, nil

	case promptSubmitMsg:
		return m.handleSessionSubmit(msg.Value)

	case promptQuitMsg:
		m.exitRequested = true
		return m, tea.Quit

	case chatStartedMsg:
		if msg.Gen != m.chatGen {
			return m, nil
		}
		if msg.Err != nil {
			m.failChat(msg.Err)
			return m, nil
		}
		m.chatCh = msg.Ch
		return m, tea.Batch(m.waitChatCmd(msg.Gen, msg.Ch), m.spinTickCmd())

	case chatChunkMsg:
		if msg.Gen != m.chatGen {
			return m, nil
		}
		if !msg.OK {
			m.chatCh = nil
			m.finishChat()
			return m, nil
		}
		m.applyChatChunk(msg.Chunk)
		return m, m.waitChatCmd(msg.Gen, m.chatCh)

	case spinTickMsg:
		if msg.Gen != m.chatGen || !m.chatStreaming {
			return m, nil
		}
		m.spinnerFrame++
		return m, m.spinTickCmd()

	case blinkTickMsg:
		m.blinkRunning = false
		if m.session == nil || m.exitRequested {
			return m, nil
		}
		m.cursorOn = !m.cursorOn
		if m.awaitingInput && !m.chatStreaming {
			m.setInputCursorVisible(m.cursorOn)
		}
		return m, m.blinkTickCmd()

	case taskCreatedMsg:
		if msg.Err != nil {
			m.submitting = false
			m.awaitingInput = true
			m.setCursorVisible(true)
			m.errText = "create task failed: " + msg.Err.Error()
			return m, nil
		}
		m.startTask(msg.Task, msg.Src)
		m.taskIDs = append(m.taskIDs, msg.Task.TaskID)
		gen := m.taskGen
		return m, tea.Batch(
			tea.Tick(flushInterval, func(time.Time) tea.Msg { return flushMsg{Gen: gen} }),
			tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{Gen: gen} }),
			m.catchUpCmd(),
		)

	case flushMsg:
		if msg.Gen != m.taskGen {
			return m, nil
		}
		m.flushPending()
		if m.terminal {
			return m, nil
		}
		gen := m.taskGen
		return m, tea.Tick(flushInterval, func(time.Time) tea.Msg { return flushMsg{Gen: gen} })

	case pollMsg:
		if msg.Gen != m.taskGen || m.terminal {
			return m, nil
		}
		m.pollTick++
		gen := m.taskGen
		cmds := []tea.Cmd{
			m.fetchTaskCmd(),
			tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{Gen: gen} }),
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
		if msg.Gen != m.taskGen {
			return m, nil
		}
		if msg.Err != nil {
			m.errText = "task refresh failed: " + msg.Err.Error()
			return m, nil
		}
		m.mergeTask(msg.State)
		if m.terminal {
			return m, m.terminalCmd()
		}
		return m, nil

	case catchUpMsg:
		if msg.Gen != m.taskGen {
			return m, nil
		}
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
			return m, m.terminalCmd()
		}
		return m, m.connectCmd()

	case watchStartedMsg:
		if msg.Gen != m.taskGen {
			return m, nil
		}
		m.watchCh = msg.Ch
		m.connected = true
		m.reconnecting = false
		m.reconnectAttempt = 0
		m.reconnectDelay = reconnectMin
		m.errText = ""
		return m, m.nextEventCmd()

	case eventMsg:
		if msg.Gen != m.taskGen {
			return m, nil
		}
		m.applyEvent(msg.Event)
		if m.terminal {
			return m, m.terminalCmd()
		}
		return m, m.nextEventCmd()

	case watchErrMsg:
		if msg.Gen != m.taskGen {
			return m, nil
		}
		return m, m.scheduleReconnect(msg.Err)

	case reconnectMsg:
		if msg.Gen != m.taskGen || m.terminal {
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

// handleSessionSubmit turns an input-box line into one of three things:
// /task <goal> creates a task and shows the a~e stage view, exit ends the
// session, and anything else is answered directly in the conversation.
func (m *Model) handleSessionSubmit(line string) (tea.Model, tea.Cmd) {
	if m.session == nil || !m.awaitingInput {
		return m, nil
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return m, nil
	}
	if goal, ok := taskCommandArg(trimmed); ok {
		if goal == "" {
			m.errText = "usage: /task <goal>"
			return m, nil
		}
		return m, m.startTaskSubmit(goal)
	}
	dispatched := DispatchSessionLine(trimmed)
	switch dispatched.Action {
	case SessionExit:
		m.exitRequested = true
		return m, tea.Quit
	case SessionSubmit:
		return m, m.startChatSubmit(dispatched.Goal)
	default:
		return m, nil
	}
}

// taskCommandPrefix is the only way to reach the a~e loop from the composer.
const taskCommandPrefix = "/task"

// taskCommandArg reports whether line starts with the /task command and
// returns the remaining goal text (may be empty when the prefix is bare).
func taskCommandArg(line string) (string, bool) {
	lower := strings.ToLower(line)
	if lower == taskCommandPrefix {
		return "", true
	}
	if !strings.HasPrefix(lower, taskCommandPrefix) {
		return "", false
	}
	rest := line[len(taskCommandPrefix):]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		// /tasks or /taskfoo are plain chat words, not the command.
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// startTaskSubmit switches the session into a~e for one goal.
func (m *Model) startTaskSubmit(goal string) tea.Cmd {
	m.awaitingInput = false
	m.submitting = true
	m.chatMode = false
	m.errText = ""
	return m.createTaskCmd(goal)
}

// startChatSubmit sends one plain message to agent-service and opens an
// assistant turn that grows as tokens arrive.
func (m *Model) startChatSubmit(message string) tea.Cmd {
	cfg := m.session
	if cfg == nil || cfg.Chat == nil {
		m.errText = "direct chat is not configured for this session"
		return nil
	}
	m.awaitingInput = false
	m.submitting = false
	m.chatMode = true
	m.errText = ""
	now := m.now()
	m.chat = append(m.chat,
		ChatLine{Role: "user", Text: message, Time: now},
		ChatLine{Role: "assistant", Streaming: true, Time: now},
	)
	m.chatStreaming = true
	m.chatCh = nil
	m.chatGen++
	m.spinnerFrame = 0
	m.thinking = true
	m.thinkingText = ""
	m.thinkingStarted = now
	return m.chatCmd(m.chatGen, message)
}

func (m *Model) chatCmd(gen int, message string) tea.Cmd {
	cfg := m.session
	if cfg == nil || cfg.Chat == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(m.baseCtx())
	m.chatCancel = cancel
	chat := cfg.Chat
	return func() tea.Msg {
		ch, err := chat(ctx, message)
		return chatStartedMsg{Gen: gen, Ch: ch, Err: err}
	}
}

// spinTickCmd drives the spinner while a reply streams.
func (m *Model) spinTickCmd() tea.Cmd {
	gen := m.chatGen
	return tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinTickMsg{Gen: gen} })
}

// blinkTickCmd keeps exactly one caret-blink tick in flight. The flag makes
// the chain idempotent, so a path that hands the input back (terminalCmd) can
// ask for it without starting a second chain that would halve the interval.
func (m *Model) blinkTickCmd() tea.Cmd {
	if m.blinkRunning {
		return nil
	}
	m.blinkRunning = true
	return tea.Tick(cursorBlinkInterval, func(time.Time) tea.Msg { return blinkTickMsg{} })
}

func (m *Model) setCursorVisible(visible bool) {
	m.cursorOn = visible
	m.setInputCursorVisible(visible)
}

func (m *Model) setInputCursorVisible(visible bool) {
	if m.input != nil {
		m.input.SetCursorVisible(visible)
	}
}

// composerRegion returns the first row, the height and the row of the input's
// first display line inside the fixed composer block. inputRow is -1 when the
// block holds no input, so a mouse press can tell "focus the composer" from
// "move the caret there".
func (m *Model) composerRegion() (top, height, inputRow int) {
	width := m.viewWidth()
	height = m.viewHeight()
	lines, row := m.composerLines(width)
	if len(lines) == 0 {
		return 0, 0, -1
	}
	top = height - len(lines) - 1
	if top < 0 {
		top = 0
	}
	if row >= len(lines) {
		row = -1
	}
	return top, len(lines), row
}

func (m *Model) viewWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 100
}

func (m *Model) viewHeight() int {
	if m.height > 0 {
		return m.height
	}
	return 30
}

// handleMouse focuses the composer on a left click and, when the click lands on
// the input rows, moves the caret to the clicked display column. Wide runes
// (CJK, emoji) are measured in cells, never in runes.
func (m *Model) handleMouse(msg tea.MouseMsg) {
	if msg.Button != tea.MouseButtonLeft || msg.Action != tea.MouseActionPress {
		return
	}
	if m.session == nil || !m.awaitingInput {
		return
	}
	width := m.viewWidth()
	top, height, inputRow := m.composerRegion()
	if height == 0 || msg.Y < top || msg.Y >= top+height {
		return // click outside the composer is ignored
	}
	m.setCursorVisible(true)
	if inputRow < 0 || msg.Y < top+inputRow {
		return // hint / top rule: focus only
	}
	row := msg.Y - (top + inputRow)
	col := msg.X - views.ComposerIndent
	m.input.SetCursorByCell(row, col, views.ComposerContentWidth(width))
}

func (m *Model) waitChatCmd(gen int, ch <-chan ChatChunk) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		chunk, ok := <-ch
		return chatChunkMsg{Gen: gen, Chunk: chunk, OK: ok}
	}
}

// applyChatChunk folds one streamed chunk into the trailing assistant turn.
func (m *Model) applyChatChunk(chunk ChatChunk) {
	if chunk.Usage > 0 {
		m.tokensUsed = chunk.Usage
	}
	switch chunk.Type {
	case "think":
		m.thinking = true
		if chunk.Thinking != "" {
			m.thinkingText = chunk.Thinking
		}
	case "tool_start":
		m.thinking = false
		name := chunk.ToolName
		if name == "" {
			name = strings.TrimSpace(chunk.Content)
		}
		m.chat = append(m.chat, ChatLine{
			Role:      "tool",
			Text:      toolCallLabel(views.ToolMarkPending, name, chunk.ToolArgs),
			ToolName:  name,
			ToolArgs:  chunk.ToolArgs,
			Streaming: true,
			Time:      m.now(),
		})
	case "tool_result":
		m.thinking = false
		m.finishToolLine(chunk.ToolName, chunk.ToolArgs, chunk.ToolResult)
	case "token":
		m.thinking = false
		m.appendAssistantText(chunk.Content)
	case "response":
		// Legacy alias for streamed text; kept so an older server still works.
		m.thinking = false
		m.appendAssistantText(chunk.Content)
	case "final":
		m.thinking = false
		// final carries the full authoritative reply; replacing (instead of
		// appending) keeps the text from being duplicated after the tokens.
		if chunk.Content != "" {
			m.setAssistantText(chunk.Content)
		}
		m.finishChat()
	case "error":
		m.thinking = false
		content := strings.TrimSpace(chunk.Content)
		if content == "" {
			content = "agent-service returned an error chunk"
		}
		m.finishChat()
		m.chat = append(m.chat, ChatLine{Role: "assistant", Text: content, Err: true, Time: m.now()})
	case "handoff", "approval":
		// Handoffs and approvals stay out of the conversation view.
	default:
		if chunk.Content != "" {
			m.appendAssistantText(chunk.Content)
		}
	}
}

// finishToolLine rewrites the pending tool row with its ✓/✗ outcome.
func (m *Model) finishToolLine(name, args, result string) {
	for i := len(m.chat) - 1; i >= 0; i-- {
		if m.chat[i].Role != "tool" {
			continue
		}
		m.chat[i].Streaming = false
		mark := "✓"
		if toolFailed(result) {
			mark = "✗"
			m.chat[i].Err = true
		}
		if name == "" {
			name = m.chat[i].ToolName
		}
		if args == "" {
			args = m.chat[i].ToolArgs
		}
		m.chat[i].Text = toolCallLabel(mark, name, args)
		return
	}
}

func toolFailed(result string) bool {
	low := strings.ToLower(result)
	if low == "" {
		return false
	}
	for _, marker := range []string{"error", "fail", "denied", "timeout", "refused"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

func toolCallLabel(mark, name, args string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	args = compactToolArgs(args)
	if args == "" {
		return mark + " " + name
	}
	return mark + " " + name + "(" + args + ")"
}

func compactToolArgs(args string) string {
	args = strings.TrimSpace(strings.ReplaceAll(args, "\n", " "))
	if args == "" {
		return ""
	}
	runes := []rune(args)
	if len(runes) > 48 {
		return string(runes[:47]) + "…"
	}
	return args
}

func (m *Model) appendAssistantText(text string) {
	if text == "" {
		return
	}
	for i := len(m.chat) - 1; i >= 0; i-- {
		if m.chat[i].Role != "assistant" || m.chat[i].Err {
			continue
		}
		m.chat[i].Text += text
		return
	}
	m.chat = append(m.chat, ChatLine{Role: "assistant", Text: text, Streaming: true, Time: m.now()})
}

func (m *Model) setAssistantText(text string) {
	for i := len(m.chat) - 1; i >= 0; i-- {
		if m.chat[i].Role != "assistant" || m.chat[i].Err {
			continue
		}
		m.chat[i].Text = text
		return
	}
	m.chat = append(m.chat, ChatLine{Role: "assistant", Text: text, Time: m.now()})
}

// finishChat closes the streaming turn and gives the composer back. The read
// loop keeps draining the channel until the server closes it, so a trailing
// chunk is never left unread; finishChat is idempotent.
func (m *Model) finishChat() {
	m.chatStreaming = false
	m.thinking = false
	m.chatCancel = nil
	m.clearToolSpinners()
	for i := len(m.chat) - 1; i >= 0; i-- {
		if m.chat[i].Role == "assistant" && m.chat[i].Streaming && !m.chat[i].Err {
			if strings.TrimSpace(m.chat[i].Text) == "" {
				m.chat[i].Text = "(empty reply)"
			}
			m.chat[i].Streaming = false
			break
		}
	}
	if m.session != nil && !m.session.ExitOnComplete {
		m.awaitingInput = true
		m.submitting = false
		if m.input != nil {
			m.input.Reset()
		}
		m.setCursorVisible(true)
	}
}

func (m *Model) clearToolSpinners() {
	for i := range m.chat {
		if m.chat[i].Role == "tool" {
			m.chat[i].Streaming = false
		}
	}
}

// interruptChat cancels the in-flight reply when the user presses Esc.
func (m *Model) interruptChat() {
	if m.chatCancel != nil {
		m.chatCancel()
		m.chatCancel = nil
	}
	m.chatGen++
	m.chatCh = nil
	m.chatStreaming = false
	m.thinking = false
	m.clearToolSpinners()
	for i := len(m.chat) - 1; i >= 0; i-- {
		if m.chat[i].Role == "assistant" && m.chat[i].Streaming && !m.chat[i].Err {
			if strings.TrimSpace(m.chat[i].Text) == "" {
				m.chat[i].Text = "(interrupted)"
			} else {
				m.chat[i].Text += "\n\n(interrupted)"
			}
			m.chat[i].Streaming = false
			break
		}
	}
	if m.session != nil && !m.session.ExitOnComplete {
		m.awaitingInput = true
		m.submitting = false
		if m.input != nil {
			m.input.Reset()
		}
		m.setCursorVisible(true)
	}
}

// failChat surfaces a stream-open failure instead of silently returning to
// the prompt.
func (m *Model) failChat(err error) {
	m.finishChat()
	m.chat = append(m.chat, ChatLine{
		Role: "assistant",
		Text: "chat failed: " + err.Error(),
		Err:  true,
		Time: m.now(),
	})
}

// terminalCmd decides what happens when the current task reaches a terminal
// state: resident sessions go back to the input box, legacy runs quit.
func (m *Model) terminalCmd() tea.Cmd {
	if m.session == nil || m.session.ExitOnComplete {
		return tea.Quit
	}
	if !m.awaitingInput {
		m.awaitingInput = true
		m.submitting = false
		m.mode = ViewMain
		m.input.Reset()
		m.setCursorVisible(true)
		m.cancelTaskContext()
		// Idempotent: for a plain session the chain is already alive, for a
		// --goal session it starts now that the input is back.
		return m.blinkTickCmd()
	}
	return nil
}

// startTask swaps the monitor over to a freshly created task and resets all
// per-task rendering state.
func (m *Model) startTask(state TaskState, src Source) {
	m.cancelTaskContext()
	if state.Attempts == nil {
		state.Attempts = map[string]int{}
	}
	if state.Attempt == 0 {
		state.Attempt = state.Attempts[state.CurrentStage]
	}
	state.StageStartedAt = m.now()
	m.state = state
	m.src = src
	m.taskGen++
	// A task takes over the screen: the conversation view is hidden and any
	// in-flight chat chunk is dropped on arrival.
	m.chatGen++
	m.chatCh = nil
	m.chatStreaming = false
	m.chatMode = false
	m.watchCh = nil
	m.terminal = false
	m.awaitingInput = false
	m.submitting = false
	m.mode = ViewMain
	m.connected = true
	m.reconnecting = false
	m.reconnectAttempt = 0
	m.reconnectDelay = reconnectMin
	m.aborting = false
	m.opening = false
	m.lastSeq = 0
	m.events = nil
	m.stream = nil
	m.pending = nil
	m.verdicts = map[string]string{}
	m.verifiedBy = map[string]string{}
	m.retryByCategory = map[string]int{}
	m.evidence = nil
	m.failed = nil
	m.subagents = views.SubagentData{}
	m.models = views.ModelData{}
	m.errText = ""
	m.pollTick = 0
	if m.input != nil {
		m.input.Reset()
	}
	m.taskCtx, m.taskCancel = context.WithCancel(m.baseCtx())
}

func (m *Model) cancelTaskContext() {
	if m.taskCancel != nil {
		m.taskCancel()
		m.taskCancel = nil
		m.taskCtx = nil
	}
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

// View renders the fixed Claude-style skeleton: wordmark, the scrollable body,
// the status bar, the hint line and finally the composer. The input line is
// the last row of the whole view, so the terminal caret and the IME candidate
// window sit at the bottom edge. The body height is whatever is left after the
// fixed rows.
func (m *Model) View() string {
	width := m.width
	if width <= 0 {
		width = 100
	}
	height := m.height
	if height <= 0 {
		height = 30
	}

	logo := views.RenderWordmark(width, m.theme.NoColor)
	composer := m.composerBlock(width)
	status := m.statusBar(width)

	bodyHeight := height - lineCount(logo) - len(composer) - 1
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	body := padLines(splitLines(m.renderBody(width, bodyHeight)), bodyHeight)

	out := splitLines(logo)
	out = append(out, body...)
	out = append(out, status)
	out = append(out, composer...)
	return strings.Join(out, "\n")
}

// renderBody draws whatever occupies the scrollable middle area.
func (m *Model) renderBody(width, height int) string {
	switch m.mode {
	case ViewLogs:
		return views.RenderLogs(m.events, width, height, m.theme)
	case ViewHelp:
		return views.RenderHelp(m.theme)
	case ViewMemory:
		return views.RenderMemory(m.theme)
	case ViewSubtasks:
		half := height / 2
		if half < 4 {
			half = 4
		}
		return strings.Join([]string{
			views.RenderSubtasks(m.subagents, width, half, m.theme),
			views.RenderHandoffs(m.subagents, width, height-half, m.theme),
		}, "\n")
	case ViewModels:
		return views.RenderModels(m.models, width, maxLinesFor(height, 12), m.theme)
	case ViewHealth:
		return views.RenderHealth(m.healthData(), width, maxLinesFor(height, 12), m.theme)
	case ViewArtifacts:
		return views.RenderArtifacts(m.subagents, width, maxLinesFor(height, 12), m.theme)
	case ViewKB:
		return views.RenderKB(m.kbData(), width, maxLinesFor(height, 12), m.theme)
	}
	if m.chatMode && m.session != nil {
		return views.RenderChatWithCursor(m.chatLines(), width, height, m.theme, m.cursorChar())
	}
	header := views.RenderHeader(m.stageData())
	stage := views.RenderStage(m.stageData())
	remaining := height - lineCount(header) - lineCount(stage)
	if remaining < 4 {
		remaining = 4
	}
	streamMax := remaining / 3
	if streamMax < 2 {
		streamMax = 2
	}
	eventsMax := remaining - streamMax - 1
	if eventsMax < 2 {
		eventsMax = 2
	}
	return strings.Join([]string{
		header,
		stage,
		views.RenderStream(m.stream, width, streamMax, m.theme),
		views.RenderEvents(m.events, width, eventsMax, m.theme),
	}, "\n")
}

// composerBlock is the fixed bottom stack. Its last row is always the input
// line (or the streaming notice), so the terminal caret and the IME candidate
// window attach to the bottom of the screen instead of floating under it.
func (m *Model) composerBlock(width int) []string {
	lines, _ := m.composerLines(width)
	return lines
}

// composerLines renders the composer and reports which row of the returned
// slice holds the first display row of the input (-1 when there is none).
func (m *Model) composerLines(width int) ([]string, int) {
	if m.session == nil {
		return nil, -1
	}
	switch {
	case m.awaitingInput:
		// Hint and validation errors stay above the frame: nothing may follow
		// the input line.
		lines := []string{"", clipRunes("  "+PromptHint, width)}
		if errText := m.input.ErrorText(); errText != "" {
			lines = append(lines, clipRunes("  "+errText, width))
		}
		if m.errText != "" {
			lines = append(lines, clipRunes("  "+m.errText, width))
		}
		// Reserved row: the complete box drawn after a submit is one row
		// taller, so this blank keeps the layout from jumping on focus change.
		lines = append(lines, "")
		block := splitLines(views.RenderOpenComposer(m.theme, m.input.InputLine(), width))
		inputRow := len(lines) + 1 // block[0] is the top rule
		return append(lines, block...), inputRow
	case m.submitting && m.state.TaskID == "":
		// Blurred: the complete frame returns while the task is created.
		lines := []string{"", ""}
		return append(lines, splitLines(views.RenderBoxWithTheme(m.theme, "", "creating task…", width))...), -1
	case m.chatStreaming:
		return []string{"", "  " + m.streamingStatusLine(width)}, -1
	}
	return nil, -1
}

// statusBar is the single-line footer: model · cwd · branch · tokens · ? help.
func (m *Model) statusBar(width int) string {
	tokens := m.state.ConsumedTokens
	if tokens == 0 {
		tokens = m.tokensUsed
	}
	branch := m.gitBranch
	if branch == "" {
		branch = "-"
	}
	segments := []string{m.modelName, shortWorkDir(m.workDir), branch}
	prefix := ""
	if m.mode != ViewMain {
		prefix = "[" + m.modeName() + "] "
	}
	tail := " · " + formatStatusTokens(tokens) + " · ? help"
	for len(segments) > 0 {
		text := prefix + strings.Join(segments, " · ")
		if width <= 0 || len([]rune(text))+len([]rune(tail)) <= width {
			return m.theme.Secondary.Render(text + tail)
		}
		segments = segments[:len(segments)-1]
	}
	return m.theme.Secondary.Render(prefix + "? help")
}

const spinnerFrames = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"

func (m *Model) spinnerChar() string {
	frames := []rune(spinnerFrames)
	return string(frames[m.spinnerFrame%len(frames)])
}

// cursorChar alternates between two block cursors every spinner tick.
func (m *Model) cursorChar() string {
	if m.spinnerFrame%2 == 0 {
		return "▍"
	}
	return "▌"
}

func (m *Model) streamingStatusLine(width int) string {
	elapsed := 0
	if !m.thinkingStarted.IsZero() {
		elapsed = int(m.now().Sub(m.thinkingStarted) / time.Second)
		if elapsed < 0 {
			elapsed = 0
		}
	}
	line := fmt.Sprintf("%s Thinking… %ds · esc to interrupt", m.spinnerChar(), elapsed)
	if width > 0 {
		runes := []rune(line)
		if len(runes) > width-3 && width > 4 {
			line = string(runes[:width-4]) + "…"
		}
	}
	return line
}

func shortWorkDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "~"
	}
	dir = filepath.ToSlash(dir)
	runes := []rune(dir)
	if len(runes) <= 28 {
		return dir
	}
	return "…" + string(runes[len(runes)-27:])
}

func formatStatusTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM tokens", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk tokens", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d tokens", n)
	}
}

func clipRunes(s string, width int) string {
	if width <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width == 1 {
		return string(runes[:1])
	}
	return string(runes[:width-1]) + "…"
}

func splitLines(s string) []string { return strings.Split(s, "\n") }

func lineCount(s string) int { return len(splitLines(s)) }

func padLines(lines []string, height int) []string {
	if height <= 0 {
		return lines
	}
	if len(lines) > height {
		return lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return lines
}

// detectGitBranch reads .git/HEAD (walking up from dir) without shelling out.
func detectGitBranch(dir string) string {
	if dir == "" {
		return ""
	}
	cur := dir
	for i := 0; i < 40; i++ {
		gitPath := filepath.Join(cur, ".git")
		if info, err := os.Stat(gitPath); err == nil {
			head := filepath.Join(gitPath, "HEAD")
			if !info.IsDir() {
				// Worktree: .git is a file pointing at the real git dir.
				if real, err := readGitDir(gitPath); err == nil {
					head = filepath.Join(real, "HEAD")
				}
			}
			return readHEAD(head)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return ""
}

func readGitDir(gitFile string) (string, error) {
	f, err := os.Open(gitFile)
	if err != nil {
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "gitdir:") {
			dir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(filepath.Dir(gitFile), dir)
			}
			return dir, nil
		}
	}
	return "", fmt.Errorf("no gitdir in %s", gitFile)
}

func readHEAD(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(raw))
	if strings.HasPrefix(head, "ref:") {
		ref := strings.TrimSpace(strings.TrimPrefix(head, "ref:"))
		ref = strings.TrimPrefix(ref, "refs/heads/")
		return ref
	}
	if len(head) >= 7 {
		return head[:7]
	}
	return head
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

// chatLines maps the conversation onto the view-layer type.
func (m *Model) chatLines() []views.ChatMessage {
	if len(m.chat) == 0 {
		return nil
	}
	out := make([]views.ChatMessage, 0, len(m.chat))
	for _, line := range m.chat {
		out = append(out, views.ChatMessage{
			Role:      line.Role,
			Text:      line.Text,
			Err:       line.Err,
			Streaming: line.Streaming,
			Time:      line.Time,
		})
	}
	return out
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
	gen := m.taskGen
	return tea.Tick(delay, func(time.Time) tea.Msg { return reconnectMsg{Gen: gen} })
}

func (m *Model) baseCtx() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

// taskBaseCtx is the per-task context; cancelling it stops the previous task's
// poll and watch goroutines when the session moves on.
func (m *Model) taskBaseCtx() context.Context {
	if m.taskCtx != nil {
		return m.taskCtx
	}
	return m.baseCtx()
}

// createTaskCmd asks the session factory for one new task.
func (m *Model) createTaskCmd(goal string) tea.Cmd {
	cfg := m.session
	if cfg == nil || cfg.CreateTask == nil {
		return nil
	}
	ctx := m.baseCtx()
	return func() tea.Msg {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		view, err := cfg.CreateTask(callCtx, goal)
		if err != nil {
			return taskCreatedMsg{Goal: goal, Err: err}
		}
		var src Source
		if cfg.SourceFor != nil {
			src = cfg.SourceFor(view.GetTaskId())
		}
		return taskCreatedMsg{Task: taskStateFromProto(view), Src: src, Goal: goal}
	}
}

func (m *Model) fetchTaskCmd() tea.Cmd {
	src := m.src
	if src == nil {
		return nil
	}
	ctx := m.taskBaseCtx()
	gen := m.taskGen
	return func() tea.Msg {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		state, err := src.Task(callCtx)
		return taskMsg{Gen: gen, State: state, Err: err}
	}
}

func (m *Model) catchUpCmd() tea.Cmd {
	src := m.src
	if src == nil {
		return nil
	}
	ctx := m.taskBaseCtx()
	gen := m.taskGen
	since := m.lastSeq
	return func() tea.Msg {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		events, next, err := src.ListEvents(callCtx, since)
		return catchUpMsg{Gen: gen, Events: events, Next: next, Err: err}
	}
}

func (m *Model) connectCmd() tea.Cmd {
	src := m.src
	if src == nil {
		return nil
	}
	ctx := m.taskBaseCtx()
	gen := m.taskGen
	since := m.lastSeq
	return func() tea.Msg {
		ch, err := src.Watch(ctx, since)
		if err != nil {
			return watchErrMsg{Gen: gen, Err: err}
		}
		return watchStartedMsg{Gen: gen, Ch: ch}
	}
}

func (m *Model) nextEventCmd() tea.Cmd {
	ch := m.watchCh
	if ch == nil {
		return nil
	}
	gen := m.taskGen
	return func() tea.Msg {
		item, ok := <-ch
		if !ok {
			return watchErrMsg{Gen: gen, Err: fmt.Errorf("event stream closed")}
		}
		if item.Err != nil {
			return watchErrMsg{Gen: gen, Err: item.Err}
		}
		return eventMsg{Gen: gen, Event: item.Event}
	}
}

func (m *Model) abortCmd() tea.Cmd {
	src := m.src
	if src == nil || m.state.TaskID == "" {
		return nil
	}
	ctx := m.taskBaseCtx()
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
