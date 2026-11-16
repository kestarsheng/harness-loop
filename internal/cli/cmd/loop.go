package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"agent-platform/internal/cli/output"
	"agent-platform/internal/cli/tui"
	harnesspb "agent-platform/pkg/pb/harnessloop"
)

func newLoopCmd() *cobra.Command {
	var (
		goal       string
		goalFile   string
		workspace  string
		project    string
		budgetTok  int64
		budgetCost float64
		timeout    time.Duration
	)

	cmd := &cobra.Command{
		Use:   "loop",
		Short: "Start an interactive Harness Loop session and show the fixed monitor board",
		Long: "Start one interactive Harness Loop session.\n\n" +
			"Every invocation creates a new task and shows the same fixed monitor URL. " +
			"When --goal is omitted the TUI asks for a single-line goal first.",
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

			session := newSessionIdentity()
			created, err := client.CreateTask(callContext(cmd, tenant), &harnesspb.CreateTaskRequest{
				TenantId:      tenant,
				ProjectId:     project,
				WorkspaceRoot: workspace,
				Goal:          goal,
				Budget:        &harnesspb.Budget{Tokens: budgetTok, Cost: budgetCost},
				SessionId:     session.ID,
				TerminalLabel: session.Label,
			})
			if err != nil {
				emit(cmd, describeError(err))
				return &ExitError{Code: exitCodeForError(err)}
			}
			taskID := created.GetTask().GetTaskId()

			runCtx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				runCtx, cancel = context.WithTimeout(runCtx, timeout)
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
		},
	}

	cmd.Flags().StringVar(&goal, "goal", "", "task goal (prompted interactively when empty)")
	cmd.Flags().StringVar(&goalFile, "goal-file", "", "read goal from file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace root for the task (default: current directory)")
	cmd.Flags().StringVar(&project, "project", "", "project id")
	cmd.Flags().Int64Var(&budgetTok, "budget-tokens", 0, "token budget")
	cmd.Flags().Float64Var(&budgetCost, "budget-cost", 0, "cost budget")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "max wait for terminal state (0 means no limit)")
	return cmd
}
