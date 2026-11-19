package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"agent-platform/internal/cli/output"
	"agent-platform/internal/devstack"
)

// newDevCmd exposes the dev-stack contract through one command:
//
//	harness-cli dev           ensure (reuse healthy stack, otherwise start 14 items)
//	harness-cli dev up        start everything that is not healthy
//	harness-cli dev down      stop processes + containers
//	harness-cli dev status    probe the 14 items without changing anything
func newDevCmd() *cobra.Command {
	var (
		contractPath string
		root         string
	)
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "Ensure the local 14-item dev stack (scripts/dev-stack.json)",
		Long: "Ensure the local dev stack declared in scripts/dev-stack.json.\n" +
			"When harnessloop (127.0.0.1:50013) is already healthy the running stack is reused;\n" +
			"otherwise the 14 items are started in contract order and health-checked.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := newDevManager(root, contractPath)
			if err != nil {
				return err
			}
			rep, runErr := m.Ensure(cmd.Context())
			return emitDevReport(cmd, "ensure", rep, runErr)
		},
	}
	pf := cmd.PersistentFlags()
	pf.StringVar(&contractPath, "contract", devstack.DefaultContractPath, "stack contract file (relative to --root)")
	pf.StringVar(&root, "root", "", "repository root (default current directory)")

	cmd.AddCommand(&cobra.Command{
		Use:   "up",
		Short: "Start every contract item that is not already healthy",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := newDevManager(root, contractPath)
			if err != nil {
				return err
			}
			rep, runErr := m.Up(cmd.Context())
			return emitDevReport(cmd, "up", rep, runErr)
		},
	})

	var keepData bool
	down := &cobra.Command{
		Use:   "down",
		Short: "Stop the stack (reverse contract order)",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := newDevManager(root, contractPath)
			if err != nil {
				return err
			}
			rep, runErr := m.Down(cmd.Context(), keepData)
			return emitDevReport(cmd, "down", rep, runErr)
		},
	}
	down.Flags().BoolVar(&keepData, "keep-db", false, "keep postgres/mongo/qdrant containers")
	cmd.AddCommand(down)

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Probe the 14 items without changing anything",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := newDevManager(root, contractPath)
			if err != nil {
				return err
			}
			rep, runErr := m.Status(cmd.Context())
			if runErr == nil && !rep.Healthy() {
				runErr = devstack.ErrNotHealthy
			}
			return emitDevReport(cmd, "status", rep, runErr)
		},
	})
	return cmd
}

func newDevManager(root, contractPath string) (*devstack.Manager, error) {
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		root = wd
	}
	if !filepath.IsAbs(contractPath) {
		contractPath = filepath.Join(root, contractPath)
	}
	contract, err := devstack.LoadContract(contractPath)
	if err != nil {
		return nil, err
	}
	m := devstack.New(root, contract)
	m.Out = os.Stderr
	return m, nil
}

// newBoard resolves the singleton monitor board for root. The board item is
// declared by the contract (board:true), so no port or command is duplicated
// here.
func newBoard(root, contractPath string) (*devstack.Board, error) {
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		root = wd
	}
	if !filepath.IsAbs(contractPath) {
		contractPath = filepath.Join(root, contractPath)
	}
	contract, err := devstack.LoadContract(contractPath)
	if err != nil {
		return nil, err
	}
	b, err := devstack.NewBoard(root, contract)
	if err != nil {
		return nil, err
	}
	b.Out = os.Stderr
	return b, nil
}

func emitDevReport(cmd *cobra.Command, verb string, rep *devstack.Report, runErr error) error {
	out := cmd.OutOrStdout()
	if opts.JSON {
		payload := map[string]any{"ok": runErr == nil, "action": verb}
		if rep != nil {
			payload["reused"] = rep.Reused
			payload["items"] = rep.Items
		}
		if runErr != nil {
			payload["error"] = runErr.Error()
		}
		raw, err := json.MarshalIndent(payload, "", "  ")
		if err == nil {
			fmt.Fprintln(out, output.Redact(string(raw)))
		}
	} else {
		if rep != nil {
			reuse := ""
			if rep.Reused {
				reuse = " (reused running stack)"
			}
			fmt.Fprintf(out, "dev %s: %d item(s)%s\n", verb, len(rep.Items), reuse)
			for _, it := range rep.Items {
				status := "FAIL"
				if it.Healthy {
					status = "OK"
				}
				note := it.Detail
				if it.PID > 0 {
					note = fmt.Sprintf("%s pid=%d", note, it.PID)
				}
				fmt.Fprintf(out, "  %-4s %-16s %-9s %-6d %s\n", status, it.Name, it.Kind, it.Port, note)
			}
		}
		if runErr != nil {
			fmt.Fprintln(out, output.Redact(runErr.Error()))
		}
	}
	if runErr != nil {
		return &ExitError{Code: output.ExitFailed}
	}
	return nil
}
