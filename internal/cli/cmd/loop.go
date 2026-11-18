package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/metadata"

	"agent-platform/internal/cli/output"
	"agent-platform/internal/cli/tui"
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
	)

	cmd := &cobra.Command{
		Use:   "loop",
		Short: "Start a resident Harness Loop session and show the fixed monitor board",
		Long: "Start one resident Harness Loop session.\n\n" +
			"Each line you submit in the session creates a task; when a task reaches " +
			"a terminal state the interface returns to the input box. Type exit (or " +
			"/harness loop exit) to end only this terminal. Use --exit-on-complete " +
			"for the legacy one-task-then-exit behaviour.",
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

			cfg := loadFileConfig(opts.Config)
			monitorURL := tui.URLFor(resolveWebBaseURL(cfg))
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
				return runLoopOnce(cmd, client, tenant, monitorURL, hyperlink, spec)
			}
			return runLoopSession(cmd, client, tenant, monitorURL, hyperlink, spec)
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
	return cmd
}

// runLoopSession is the resident default: submit as many goals as needed and
// return to the input box after every terminal state.
func runLoopSession(cmd *cobra.Command, client harnesspb.HarnessLoopServiceClient, tenant, monitorURL string, hyperlink bool, spec loopSpec) error {
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
		WebURL:       monitorURL,
		Hyperlink:    hyperlink,
		KB:           kbSrc,
		InitialView:  os.Getenv("HARNESS_TUI_VIEW"),
		InitialQuery: os.Getenv("HARNESS_TUI_KB_QUERY"),
	}, tui.SessionConfig{
		InitialGoal: initialGoal,
		CreateTask:  createTask,
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
	fmt.Fprintf(cmd.OutOrStdout(), "monitor=%s\n", monitorURL)
	return nil
}

// runLoopOnce keeps the original one-task-then-exit behaviour behind
// --exit-on-complete.
func runLoopOnce(cmd *cobra.Command, client harnesspb.HarnessLoopServiceClient, tenant, monitorURL string, hyperlink bool, spec loopSpec) error {
	goal := spec.goal
	if strings.TrimSpace(goal) == "" {
		entered, canceled, err := tui.RunPrompt(cmd.Context(), "Harness Loop", monitorURL, tui.PromptOptions{
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
		WebURL:       monitorURL,
		Hyperlink:    hyperlink,
		KB:           kbSrc,
		InitialView:  os.Getenv("HARNESS_TUI_VIEW"),
		InitialQuery: os.Getenv("HARNESS_TUI_KB_QUERY"),
	})

	fmt.Fprintf(cmd.OutOrStdout(), "task_id=%s\nmonitor=%s\n", taskID, monitorURL)
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

// tenantCallContext mirrors callContext for contexts built inside the TUI.
func tenantCallContext(ctx context.Context, tenant string) context.Context {
	pairs := []string{"x-tenant-id", tenant}
	if traceID := os.Getenv("HARNESS_TRACE_ID"); traceID != "" {
		pairs = append(pairs, "x-trace-id", traceID)
	}
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}
