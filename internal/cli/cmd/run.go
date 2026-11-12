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

func newRunCmd() *cobra.Command {
	var (
		goal       string
		goalFile   string
		workspace  string
		project    string
		budgetTok  int64
		budgetCost float64
		timeout    time.Duration
		noWait     bool
		noTUI      bool
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Submit a task and follow it to a terminal state",
		RunE: func(cmd *cobra.Command, args []string) error {
			if goalFile != "" {
				raw, err := os.ReadFile(goalFile)
				if err != nil {
					return fmt.Errorf("read goal file: %w", err)
				}
				goal = strings.TrimSpace(string(raw))
			}
			if strings.TrimSpace(goal) == "" {
				return fmt.Errorf("--goal or --goal-file is required")
			}
			if strings.TrimSpace(workspace) == "" {
				return fmt.Errorf("--workspace is required")
			}
			client, conn, tenant, err := newClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			ctx := callContext(cmd, tenant)
			created, err := client.CreateTask(ctx, &harnesspb.CreateTaskRequest{
				TenantId:      tenant,
				ProjectId:     project,
				WorkspaceRoot: workspace,
				Goal:          goal,
				Budget:        &harnesspb.Budget{Tokens: budgetTok, Cost: budgetCost},
			})
			if err != nil {
				env := describeError(err)
				emit(cmd, env)
				return &ExitError{Code: exitCodeForError(err)}
			}
			if noWait {
				emit(cmd, envelopeFromTask(created.Task))
				return nil
			}

			if !opts.JSON && !noTUI && isatty.IsTerminal(os.Stdout.Fd()) {
				runCtx, cancel := context.WithTimeout(cmd.Context(), timeout)
				defer cancel()
				result, tuiErr := tui.Run(runCtx, client, tenant, created.Task, tui.RunOptions{NoColor: opts.NoColor})
				if tuiErr != nil {
					if runCtx.Err() == context.DeadlineExceeded {
						emit(cmd, output.Failure("E_TIMEOUT", "task did not reach a terminal state before --timeout", nil))
						return &ExitError{Code: output.ExitConnectionFail}
					}
					emit(cmd, describeError(tuiErr))
					return &ExitError{Code: exitCodeForError(tuiErr)}
				}
				current, getErr := client.GetTask(ctx, &harnesspb.GetTaskRequest{TenantId: tenant, TaskId: created.Task.GetTaskId()})
				if getErr != nil {
					emit(cmd, describeError(getErr))
					return &ExitError{Code: exitCodeForError(getErr)}
				}
				emit(cmd, envelopeFromTask(current))
				if result.QuitEarly {
					return nil
				}
				code := output.ExitCodeForStatus(current.GetStatus(), current.GetTerminalReason())
				if code != output.ExitOK {
					return &ExitError{Code: code}
				}
				return nil
			}

			deadline := time.Now().Add(timeout)
			task := created.Task
			for !isTerminal(task.GetStatus()) {
				if time.Now().After(deadline) {
					emit(cmd, output.Failure("E_TIMEOUT", "task did not reach a terminal state before --timeout", nil))
					return &ExitError{Code: output.ExitConnectionFail}
				}
				time.Sleep(time.Second)
				next, getErr := client.GetTask(ctx, &harnesspb.GetTaskRequest{TenantId: tenant, TaskId: task.GetTaskId()})
				if getErr != nil {
					emit(cmd, describeError(getErr))
					return &ExitError{Code: exitCodeForError(getErr)}
				}
				task = next
			}
			env := envelopeFromTask(task)
			emit(cmd, env)
			code := output.ExitCodeForStatus(task.GetStatus(), task.GetTerminalReason())
			if code != output.ExitOK {
				return &ExitError{Code: code}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&goal, "goal", "", "task goal")
	cmd.Flags().StringVar(&goalFile, "goal-file", "", "read goal from file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace root for the task")
	cmd.Flags().StringVar(&project, "project", "", "project id")
	cmd.Flags().Int64Var(&budgetTok, "budget-tokens", 0, "token budget")
	cmd.Flags().Float64Var(&budgetCost, "budget-cost", 0, "cost budget")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "max wait for terminal state")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return immediately after submission")
	cmd.Flags().BoolVar(&noTUI, "no-tui", false, "poll task status without the interactive TUI")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var (
		task  string
		watch bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show task status",
		RunE: func(cmd *cobra.Command, args []string) error {
			if task == "" {
				return fmt.Errorf("--task is required")
			}
			client, conn, tenant, err := newClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx := callContext(cmd, tenant)

			for {
				view, getErr := client.GetTask(ctx, &harnesspb.GetTaskRequest{TenantId: tenant, TaskId: task})
				if getErr != nil {
					env := describeError(getErr)
					emit(cmd, env)
					return &ExitError{Code: exitCodeForError(getErr)}
				}
				if opts.JSON || !watch {
					emit(cmd, envelopeFromTask(view))
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "%s stage=%s status=%s\n", time.Now().Format("15:04:05"), view.GetCurrentStage(), view.GetStatus())
				}
				if !watch || isTerminal(view.GetStatus()) {
					return nil
				}
				time.Sleep(time.Second)
			}
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "task id")
	cmd.Flags().BoolVar(&watch, "watch", false, "poll until the task reaches a terminal state")
	return cmd
}

func newAbortCmd() *cobra.Command {
	var (
		task   string
		reason string
		epoch  int64
	)
	cmd := &cobra.Command{
		Use:   "abort",
		Short: "Abort a running task (idempotent)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if task == "" {
				return fmt.Errorf("--task is required")
			}
			if epoch == 0 {
				epoch = time.Now().Unix()
			}
			client, conn, tenant, err := newClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			view, abortErr := client.AbortTask(callContext(cmd, tenant), &harnesspb.TaskCommand{
				TenantId: tenant, TaskId: task, Reason: reason, AbortEpoch: epoch,
			})
			if abortErr != nil {
				emit(cmd, describeError(abortErr))
				return &ExitError{Code: exitCodeForError(abortErr)}
			}
			emit(cmd, envelopeFromTask(view))
			if view.GetStatus() == "aborted" {
				return &ExitError{Code: output.ExitAborted}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "task id")
	cmd.Flags().StringVar(&reason, "reason", "", "abort reason")
	cmd.Flags().Int64Var(&epoch, "epoch", 0, "abort epoch for idempotency (default now)")
	return cmd
}

func newResumeCmd() *cobra.Command {
	var (
		task       string
		checkpoint string
	)
	cmd := &cobra.Command{
		Use:   "resume",
		Short: "Resume a task from a checkpoint",
		RunE: func(cmd *cobra.Command, args []string) error {
			if task == "" {
				return fmt.Errorf("--task is required")
			}
			client, conn, tenant, err := newClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			view, resumeErr := client.ResumeTask(callContext(cmd, tenant), &harnesspb.ResumeTaskRequest{
				TenantId: tenant, TaskId: task, CheckpointId: checkpoint,
			})
			if resumeErr != nil {
				emit(cmd, describeError(resumeErr))
				return &ExitError{Code: exitCodeForError(resumeErr)}
			}
			emit(cmd, envelopeFromTask(view))
			return nil
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "task id")
	cmd.Flags().StringVar(&checkpoint, "checkpoint", "", "checkpoint id (latest when empty)")
	return cmd
}
