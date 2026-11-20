package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/metadata"

	"agent-platform/internal/cli/output"
	"agent-platform/internal/cli/tui"
	"agent-platform/internal/devstack"
	"agent-platform/pkg/harnessclient"
	agentpb "agent-platform/pkg/pb/agent"
	harnesspb "agent-platform/pkg/pb/harnessloop"
)

// loopSpec carries the flag-derived settings shared by the resident session
// and the legacy single-task path.
type loopSpec struct {
	goal       string
	workspace  string
	project    string
	budgetTok  int64
	budgetCost float64
	timeout    time.Duration
}

func newLoopCmd() *cobra.Command {
	var (
		goal           string
		goalFile       string
		workspace      string
		project        string
		budgetTok      int64
		budgetCost     float64
		timeout        time.Duration
		exitOnComplete bool
		stackRoot      string
		contractPath   string
	)

	cmd := &cobra.Command{
		Use:   "loop",
		Short: "Open the resident Harness Loop agent session (TUI only)",
		Long: "Start one resident Harness Loop session in this terminal: the agent " +
			"conversation TUI.\n\n" +
			"A plain line is answered directly by agent-service (main-agent, no " +
			"stages): you get a streamed reply in the conversation a few seconds " +
			"later. Only /task <goal> starts the a~e loop, which creates a task and " +
			"switches the screen to the stage/event panels. When a task reaches a " +
			"terminal state the interface returns to the input box. Type exit (or " +
			"/harness loop exit) to end only this terminal. Use --exit-on-complete " +
			"for the legacy one-task-then-exit behaviour; --goal is still a one-shot " +
			"a~e task.\n\n" +
			"The direct-chat backend is agent-service on 127.0.0.1:50006; override " +
			"with --agent-server or $HARNESS_AGENT_SERVER. A chat failure is shown in " +
			"the conversation instead of being retried as a task.\n\n" +
			"Only the agent side of the stack is ensured: the data containers and the " +
			"9 services (groups infra+agent) are probed on 127.0.0.1:50013, a healthy " +
			"stack is reused and a cold one is started without vite, so `dev up` is " +
			"never a prerequisite.\n\n" +
			"This command does not manage the monitor board and never prints its URL. " +
			"Run `loop start` for the board (and its URL), `loop stop` to release it.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if goalFile != "" {
				raw, err := os.ReadFile(goalFile)
				if err != nil {
					return fmt.Errorf("read goal file: %w", err)
				}
				goal = strings.TrimSpace(string(raw))
			}
			if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
				return fmt.Errorf("loop requires an interactive terminal; use `harness-cli run` in non-interactive contexts")
			}

			// T-P2-21 leftover: a cold `loop` must be self-sufficient. It only
			// ensures infra+agent — the board belongs to `loop start`.
			if err := ensureBackend(cmd, stackRoot, contractPath, cmd.ErrOrStderr()); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: dev stack not ready: %v\n", err)
			}

			hyperlink := tui.HyperlinkSupported(isatty.IsTerminal(os.Stdout.Fd()), os.Getenv)

			if strings.TrimSpace(workspace) == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("resolve workspace: %w", err)
				}
				workspace = cwd
			}

			client, conn, tenant, err := newClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			spec := loopSpec{
				goal:       goal,
				workspace:  workspace,
				project:    project,
				budgetTok:  budgetTok,
				budgetCost: budgetCost,
				timeout:    timeout,
			}
			if exitOnComplete {
				return runLoopOnce(cmd, client, tenant, hyperlink, spec)
			}
			return runLoopSession(cmd, client, tenant, hyperlink, spec)
		},
	}

	cmd.Flags().StringVar(&goal, "goal", "", "task goal (prompted interactively when empty)")
	cmd.Flags().StringVar(&goalFile, "goal-file", "", "read goal from file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace root for the task (default: current directory)")
	cmd.Flags().StringVar(&project, "project", "", "project id")
	cmd.Flags().Int64Var(&budgetTok, "budget-tokens", 0, "token budget")
	cmd.Flags().Float64Var(&budgetCost, "budget-cost", 0, "cost budget")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "max wait for terminal state (0 means no limit)")
	cmd.Flags().BoolVar(&exitOnComplete, "exit-on-complete", false, "exit as soon as the task reaches a terminal state (legacy behaviour)")
	cmd.Flags().StringVar(&stackRoot, "stack-root", "", "repository root of the dev stack (default: executable dir or $HARNESS_STACK_ROOT)")
	cmd.Flags().StringVar(&contractPath, "contract", devstack.DefaultContractPath, "dev stack contract file (relative to --stack-root)")

	cmd.AddCommand(newLoopStartCmd(), newLoopStopCmd())
	return cmd
}

// ensureBackend makes a cold `loop` self-sufficient without touching the board:
// it probes 127.0.0.1:50013 (harnessloop) and only on failure calls
// devstack.EnsureGroups(infra, agent). A running stack is reused untouched, so
// `loop` never requires a manual `dev up` first, and vite is never started here
// — the board is `loop start`'s job.
func ensureBackend(cmd *cobra.Command, stackRoot, contractPath string, out io.Writer) error {
	m, err := newDevManager(stackRoot, contractPath)
	if err != nil {
		return err
	}
	loop, ok := m.Contract.HarnessLoop()
	if !ok {
		return fmt.Errorf("contract has no %q item", "harnessloop")
	}
	addr := loop.Health.Address
	if addr == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", loop.Port)
	}
	if healthy, detail := m.Prober.Healthy(cmd.Context(), loop); healthy {
		fmt.Fprintf(out, "devstack: %s already up (%s), reusing the running stack\n", addr, detail)
		return nil
	}

	fmt.Fprintf(out, "devstack: %s is down, starting the stack (groups %s+%s) from %s\n",
		addr, devstack.GroupInfra, devstack.GroupAgent, m.Root)
	rep, err := m.EnsureGroups(cmd.Context(), devstack.GroupInfra, devstack.GroupAgent)
	if rep != nil {
		ready := 0
		for _, it := range rep.Items {
			if it.Healthy {
				ready++
			}
			if !it.Healthy {
				fmt.Fprintf(out, "devstack: %-16s DOWN %s\n", it.Name, it.Detail)
			}
		}
		fmt.Fprintf(out, "devstack: %d/%d item(s) healthy after ensure\n", ready, len(rep.Items))
	}
	return err
}

// boardRef resolves this terminal's reference id. The parent process is the
// shell, so every `loop start`/`loop stop` typed in the same terminal inherits
// the same ref while a second terminal gets its own. --ref and HARNESS_LOOP_REF
// override it for scripted or nested invocations.
func boardRef(flag string) string {
	if v := strings.TrimSpace(flag); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("HARNESS_LOOP_REF")); v != "" {
		return v
	}
	return "terminal:" + strconv.Itoa(os.Getppid())
}

func newLoopStartCmd() *cobra.Command {
	var (
		stackRoot    string
		contractPath string
		ref          string
	)
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start or reuse the shared monitor board and print its URL",
		Long: "Start or reuse the single shared monitor board and print its URL.\n\n" +
			"This is the only command that manages and reports the board: it starts " +
			"(or reuses) the vite process on its fixed port and prints the URL every " +
			"terminal shares. It does not open the agent session TUI — use `loop` " +
			"for that.\n\n" +
			"The board is a singleton: the first call launches the process, every " +
			"later call only records one more reference and prints the same URL. " +
			"Registering twice from the same terminal does not add a second ref.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := newBoard(stackRoot, contractPath)
			if err != nil {
				return emitBoardState(cmd, "start", nil, err)
			}
			st, runErr := b.Start(cmd.Context(), boardRef(ref))
			return emitBoardState(cmd, "start", st, runErr)
		},
	}
	cmd.Flags().StringVar(&stackRoot, "stack-root", "", "repository root of the dev stack (default: executable dir or $HARNESS_STACK_ROOT)")
	cmd.Flags().StringVar(&contractPath, "contract", devstack.DefaultContractPath, "dev stack contract file (relative to --stack-root)")
	cmd.Flags().StringVar(&ref, "ref", "", "terminal reference id (default: the parent terminal, or $HARNESS_LOOP_REF)")
	return cmd
}

func newLoopStopCmd() *cobra.Command {
	var (
		stackRoot    string
		contractPath string
		ref          string
	)
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Release this terminal's reference to the shared board",
		Long: "Release this terminal's reference to the shared monitor board.\n\n" +
			"While other terminals still hold a reference the board keeps running and " +
			"only the remaining count is printed. The last release stops the process " +
			"and removes the lock, so the URL stops answering. Stopping twice is a no-op.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := newBoard(stackRoot, contractPath)
			if err != nil {
				return emitBoardState(cmd, "stop", nil, err)
			}
			st, runErr := b.Stop(cmd.Context(), boardRef(ref))
			return emitBoardState(cmd, "stop", st, runErr)
		},
	}
	cmd.Flags().StringVar(&stackRoot, "stack-root", "", "repository root of the dev stack (default: executable dir or $HARNESS_STACK_ROOT)")
	cmd.Flags().StringVar(&contractPath, "contract", devstack.DefaultContractPath, "dev stack contract file (relative to --stack-root)")
	cmd.Flags().StringVar(&ref, "ref", "", "terminal reference id (default: the parent terminal, or $HARNESS_LOOP_REF)")
	return cmd
}

func emitBoardState(cmd *cobra.Command, action string, st *devstack.BoardState, runErr error) error {
	out := cmd.OutOrStdout()
	if opts.JSON {
		payload := map[string]any{"ok": runErr == nil, "action": action}
		if st != nil {
			payload["url"] = st.URL
			payload["ref"] = st.Ref
			payload["refs"] = st.Refs
			payload["port"] = st.Port
			payload["pid"] = st.PID
			payload["started"] = st.Started
			payload["reused"] = st.Reused
			payload["released"] = st.Released
			payload["healthy"] = st.Healthy
			payload["detail"] = st.Detail
		}
		if runErr != nil {
			payload["error"] = output.Redact(runErr.Error())
		}
		raw, err := json.MarshalIndent(payload, "", "  ")
		if err == nil {
			fmt.Fprintln(out, output.Redact(string(raw)))
		}
	} else if st != nil {
		fmt.Fprintf(out, "url=%s\n", st.URL)
		fmt.Fprintf(out, "refs=%d ref=%s state=%s\n", st.Refs, st.Ref, boardStateWord(st))
		if st.PID > 0 {
			fmt.Fprintf(out, "pid=%d port=%d\n", st.PID, st.Port)
		}
		if st.Detail != "" {
			fmt.Fprintf(out, "detail=%s\n", st.Detail)
		}
	}
	if runErr != nil {
		if !opts.JSON {
			fmt.Fprintln(out, output.Redact(runErr.Error()))
		}
		return &ExitError{Code: output.ExitFailed}
	}
	return nil
}

func boardStateWord(st *devstack.BoardState) string {
	switch {
	case st.Refs == 0:
		return "stopped"
	case st.Started:
		return "started"
	case st.Released:
		return "released"
	case st.Reused:
		return "reused"
	default:
		return "released"
	}
}

// runLoopSession is the resident default: submit as many goals as needed and
// return to the input box after every terminal state.
//
// The session never receives a board URL: `loop` is the agent conversation only
// and `loop start` is the single command that prints the monitor URL.
func runLoopSession(cmd *cobra.Command, client harnesspb.HarnessLoopServiceClient, tenant string, hyperlink bool, spec loopSpec) error {
	initialGoal := ""
	switch dispatched := tui.DispatchSessionLine(spec.goal); dispatched.Action {
	case tui.SessionExit:
		return nil
	case tui.SessionSubmit:
		initialGoal = dispatched.Goal
	}

	runCtx := cmd.Context()
	if spec.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, spec.timeout)
		defer cancel()
	}

	kbSrc, kbConn := newTUIKBSource(cmd)
	if kbConn != nil {
		defer kbConn.Close()
	}

	session := newSessionIdentity()

	// A plain line in the session is answered by agent-service directly
	// (main-agent, no stage mode); only /task reaches the a~e loop.
	agentClient, agentTarget, agentDialErr := dialAgentService(cmd)
	if agentClient != nil {
		defer agentClient.Close()
	}
	chat := newAgentChatFunc(agentClient, agentTarget, agentDialErr, tenant, session.ID, spec.workspace)

	createTask := func(ctx context.Context, goal string) (*harnesspb.TaskView, error) {
		created, err := client.CreateTask(tenantCallContext(ctx, tenant), &harnesspb.CreateTaskRequest{
			TenantId:      tenant,
			ProjectId:     spec.project,
			WorkspaceRoot: spec.workspace,
			Goal:          goal,
			Budget:        &harnesspb.Budget{Tokens: spec.budgetTok, Cost: spec.budgetCost},
			SessionId:     session.ID,
			TerminalLabel: session.Label,
		})
		if err != nil {
			return nil, err
		}
		return created.GetTask(), nil
	}

	result, tuiErr := tui.RunSession(runCtx, client, tenant, tui.RunOptions{
		NoColor:      opts.NoColor,
		WebURL:       "",
		Hyperlink:    hyperlink,
		KB:           kbSrc,
		InitialView:  os.Getenv("HARNESS_TUI_VIEW"),
		InitialQuery: os.Getenv("HARNESS_TUI_KB_QUERY"),
	}, tui.SessionConfig{
		InitialGoal: initialGoal,
		CreateTask:  createTask,
		Chat:        chat,
	})
	if tuiErr != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			emit(cmd, output.Failure("E_TIMEOUT", "session did not end before --timeout", nil))
			return &ExitError{Code: output.ExitConnectionFail}
		}
		emit(cmd, describeError(tuiErr))
		return &ExitError{Code: exitCodeForError(tuiErr)}
	}

	for _, taskID := range result.TaskIDs {
		fmt.Fprintf(cmd.OutOrStdout(), "task_id=%s\n", taskID)
	}
	if len(result.TaskIDs) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "tasks=0\n")
	}
	return nil
}

// runLoopOnce keeps the original one-task-then-exit behaviour behind
// --exit-on-complete.
func runLoopOnce(cmd *cobra.Command, client harnesspb.HarnessLoopServiceClient, tenant string, hyperlink bool, spec loopSpec) error {
	goal := spec.goal
	if strings.TrimSpace(goal) == "" {
		entered, canceled, err := tui.RunPrompt(cmd.Context(), "Harness Loop", "", tui.PromptOptions{
			NoColor:   opts.NoColor,
			Hyperlink: hyperlink,
		})
		if err != nil {
			return err
		}
		if canceled {
			return nil
		}
		goal = entered
	}
	dispatched := tui.DispatchSessionLine(goal)
	if dispatched.Action != tui.SessionSubmit {
		return nil
	}
	goal = dispatched.Goal

	session := newSessionIdentity()
	created, err := client.CreateTask(callContext(cmd, tenant), &harnesspb.CreateTaskRequest{
		TenantId:      tenant,
		ProjectId:     spec.project,
		WorkspaceRoot: spec.workspace,
		Goal:          goal,
		Budget:        &harnesspb.Budget{Tokens: spec.budgetTok, Cost: spec.budgetCost},
		SessionId:     session.ID,
		TerminalLabel: session.Label,
	})
	if err != nil {
		emit(cmd, describeError(err))
		return &ExitError{Code: exitCodeForError(err)}
	}
	taskID := created.GetTask().GetTaskId()

	runCtx := cmd.Context()
	if spec.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, spec.timeout)
		defer cancel()
	}

	kbSrc, kbConn := newTUIKBSource(cmd)
	if kbConn != nil {
		defer kbConn.Close()
	}
	result, tuiErr := tui.Run(runCtx, client, tenant, created.Task, tui.RunOptions{
		NoColor:      opts.NoColor,
		WebURL:       "",
		Hyperlink:    hyperlink,
		KB:           kbSrc,
		InitialView:  os.Getenv("HARNESS_TUI_VIEW"),
		InitialQuery: os.Getenv("HARNESS_TUI_KB_QUERY"),
	})

	fmt.Fprintf(cmd.OutOrStdout(), "task_id=%s\n", taskID)
	if tuiErr != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			emit(cmd, output.Failure("E_TIMEOUT", "task did not reach a terminal state before --timeout", nil))
			return &ExitError{Code: output.ExitConnectionFail}
		}
		emit(cmd, describeError(tuiErr))
		return &ExitError{Code: exitCodeForError(tuiErr)}
	}
	if result.QuitEarly {
		return nil
	}

	current, getErr := client.GetTask(callContext(cmd, tenant), &harnesspb.GetTaskRequest{
		TenantId: tenant, TaskId: taskID,
	})
	if getErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: task state unavailable after TUI exit: %v\n", getErr)
		return nil
	}
	emit(cmd, envelopeFromTask(current))
	if code := output.ExitCodeForStatus(current.GetStatus(), current.GetTerminalReason()); code != output.ExitOK {
		return &ExitError{Code: code}
	}
	return nil
}

// dialAgentService connects to agent-service for direct chat. It dials lazily so
// a cold or missing agent-service never blocks `loop` from opening the
// composer: the failure is reported as a chat error in the conversation.
func dialAgentService(cmd *cobra.Command) (*harnessclient.Client, string, error) {
	target := resolveAgentServer(loadFileConfig(opts.Config))
	client, err := harnessclient.DialLazy(cmd.Context(), harnessclient.DefaultConfig(target))
	if err != nil {
		return nil, target, err
	}
	return client, target, nil
}

// buildAgentChatRequest is the single place that defines a direct-chat call:
// EntryAgent=main-agent, StageMode off (no __stage_mode__), tenant / session /
// workspace attached, and the user message forwarded verbatim.
func buildAgentChatRequest(tenant, sessionID, workspace, message string) *agentpb.ExecuteStreamRequest {
	req := &agentpb.ExecuteStreamRequest{
		SessionId:  sessionID,
		TenantId:   tenant,
		UserId:     "harness-cli",
		Message:    message,
		EntryAgent: "main-agent",
		// StageMode deliberately absent: direct chat must not be forced into
		// the "no tool call means not finished" stage contract.
		ContextVars: map[string]string{},
	}
	if workspace != "" {
		req.ContextVars["workspace"] = workspace
	}
	return req
}

// newAgentChatFunc builds the session's Chat callable: one ExecuteStream per
// message, EntryAgent=main-agent, StageMode off (no __stage_mode__), tenant /
// session / workspace attached, and the user message forwarded verbatim.
func newAgentChatFunc(client *harnessclient.Client, target string, dialErr error, tenant, sessionID, workspace string) tui.ChatFunc {
	return func(ctx context.Context, message string) (<-chan tui.ChatChunk, error) {
		if client == nil {
			if dialErr == nil {
				dialErr = fmt.Errorf("not connected")
			}
			return nil, fmt.Errorf("agent-service %s: %w", target, dialErr)
		}
		req := buildAgentChatRequest(tenant, sessionID, workspace, message)

		ch := make(chan tui.ChatChunk, 64)
		callCtx := tenantCallContext(ctx, tenant)
		go func() {
			defer close(ch)
			err := client.ExecuteStream(callCtx, req, func(c *agentpb.ExecuteStreamChunk) error {
				select {
				case ch <- chatChunkFromProto(c):
					return nil
				case <-callCtx.Done():
					return callCtx.Err()
				}
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				select {
				case ch <- tui.ChatChunk{Type: "error", Content: fmt.Sprintf("agent-service %s: %v", target, err)}:
				case <-callCtx.Done():
				}
			}
		}()
		return ch, nil
	}
}

// chatChunkFromProto maps one agent-service chunk onto the TUI's flat chunk,
// passing thinking, tool and usage metadata through so the conversation can
// render reasoning and tool rows instead of dropping them.
func chatChunkFromProto(c *agentpb.ExecuteStreamChunk) tui.ChatChunk {
	cc := tui.ChatChunk{
		Type:    c.GetType(),
		Content: c.GetContent(),
		Agent:   c.GetCurrentAgent(),
	}
	if call := c.GetToolCall(); call != nil {
		cc.ToolName = call.GetName()
		cc.ToolArgs = call.GetArguments()
	}
	if step := c.GetStep(); step != nil {
		cc.ToolResult = step.GetResult()
		cc.Thinking = step.GetThought()
		cc.Usage = int64(step.GetTokensUsed())
	}
	if c.GetType() == "think" {
		if cc.Thinking == "" {
			cc.Thinking = c.GetContent()
		}
		if name := c.GetStep().GetAction(); name != "" && cc.ToolName == "" {
			cc.ToolName = name
		}
	}
	if c.GetType() == "tool_start" && cc.ToolName == "" {
		cc.ToolName = c.GetStep().GetAction()
	}
	if c.GetType() == "tool_result" && cc.ToolName == "" {
		cc.ToolName = c.GetStep().GetAction()
		cc.ToolArgs = c.GetStep().GetArguments()
	}
	return cc
}

// tenantCallContext mirrors callContext for contexts built inside the TUI.
func tenantCallContext(ctx context.Context, tenant string) context.Context {
	pairs := []string{"x-tenant-id", tenant}
	if traceID := os.Getenv("HARNESS_TRACE_ID"); traceID != "" {
		pairs = append(pairs, "x-trace-id", traceID)
	}
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}
