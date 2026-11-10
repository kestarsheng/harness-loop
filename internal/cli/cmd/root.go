package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"agent-platform/internal/cli/output"
	harnesspb "agent-platform/pkg/pb/harnessloop"

	"gopkg.in/yaml.v3"
)

type Options struct {
	Server  string
	Tenant  string
	Config  string
	JSON    bool
	NoColor bool
}

var opts Options

type ExitError struct {
	Code int
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit code %d", e.Code) }

type fileConfig struct {
	Server string `yaml:"server"`
	Tenant string `yaml:"tenant"`
}

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "harness-cli",
		Short:         "Harness Loop task CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&opts.Server, "server", "", "harnessloop-service gRPC address (default 127.0.0.1:50008)")
	pf.StringVar(&opts.Tenant, "tenant", "", "tenant id (required)")
	pf.StringVar(&opts.Config, "config", "", "config file (default ~/.harness/config.yaml)")
	pf.BoolVar(&opts.JSON, "json", false, "emit JSON envelope")
	pf.BoolVar(&opts.NoColor, "no-color", false, "disable colored output")

	root.AddCommand(newRunCmd(), newStatusCmd(), newAbortCmd(), newResumeCmd(), newLogsCmd(), newVersionCmd())
	return root
}

func Execute() int {
	root := NewRootCmd()
	err := root.Execute()
	if err == nil {
		return output.ExitOK
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	if opts.JSON {
		raw, _ := json.MarshalIndent(output.Failure("E_INVALID_ARGUMENT", err.Error(), nil), "", "  ")
		fmt.Fprintln(os.Stdout, output.Redact(string(raw)))
	} else {
		fmt.Fprintln(os.Stderr, output.Redact(err.Error()))
	}
	return output.ExitInvalidUsage
}

func loadFileConfig(path string) fileConfig {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fileConfig{}
		}
		path = filepath.Join(home, ".harness", "config.yaml")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fileConfig{}
	}
	var cfg fileConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return fileConfig{}
	}
	return cfg
}

func resolveServer(cfg fileConfig) string {
	if opts.Server != "" {
		return opts.Server
	}
	if env := os.Getenv("HARNESS_SERVER"); env != "" {
		return env
	}
	if cfg.Server != "" {
		return cfg.Server
	}
	return "127.0.0.1:50008"
}

func resolveTenant(cfg fileConfig) (string, error) {
	if opts.Tenant != "" {
		return opts.Tenant, nil
	}
	if env := os.Getenv("HARNESS_TENANT"); env != "" {
		return env, nil
	}
	if cfg.Tenant != "" {
		return cfg.Tenant, nil
	}
	return "", errors.New("tenant is required: pass --tenant or set HARNESS_TENANT")
}

func newClient(cmd *cobra.Command) (harnesspb.HarnessLoopServiceClient, *grpc.ClientConn, string, error) {
	cfg := loadFileConfig(opts.Config)
	server := resolveServer(cfg)
	tenant, err := resolveTenant(cfg)
	if err != nil {
		return nil, nil, "", err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, server, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, nil, "", &ExitError{Code: output.ExitConnectionFail}
	}
	return harnesspb.NewHarnessLoopServiceClient(conn), conn, tenant, nil
}

func callContext(cmd *cobra.Command, tenant string) context.Context {
	ctx := cmd.Context()
	traceID := os.Getenv("HARNESS_TRACE_ID")
	pairs := []string{"x-tenant-id", tenant}
	if traceID != "" {
		pairs = append(pairs, "x-trace-id", traceID)
	}
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}

func emit(cmd *cobra.Command, env output.Envelope) {
	if opts.JSON {
		raw, err := json.MarshalIndent(env, "", "  ")
		if err != nil {
			fmt.Fprintln(cmd.OutOrStdout(), output.Redact(err.Error()))
			return
		}
		fmt.Fprintln(cmd.OutOrStdout(), output.Redact(string(raw)))
		return
	}
	if env.Error != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "ERROR %s: %s\n", env.Error.Code, env.Error.Message)
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), "task=%s status=%s stage=%s reason=%s tokens=%d cost=%.4f\n",
		env.TaskID, env.Status, env.CurrentStage, env.TerminalReason, env.Tokens, env.Cost)
	for _, s := range env.Stages {
		fmt.Fprintf(cmd.OutOrStdout(), "  stage %s attempts=%d accepted=%v\n", s.Stage, s.Attempts, s.Accepted)
	}
	if len(env.Events) > 0 {
		for _, e := range env.Events {
			raw, _ := json.Marshal(e)
			fmt.Fprintln(cmd.OutOrStdout(), output.Redact(string(raw)))
		}
	}
}

func isTerminal(status string) bool {
	switch status {
	case "completed", "aborted", "failed":
		return true
	}
	return false
}

func stageSummary(task *harnesspb.TaskView) []output.StageSummary {
	out := make([]output.StageSummary, 0, len(task.GetAttemptNoByStage()))
	for _, stage := range []string{"a", "b", "c", "d", "e"} {
		attempts := task.GetAttemptNoByStage()[stage]
		if attempts == 0 {
			continue
		}
		out = append(out, output.StageSummary{
			Stage:    stage,
			Attempts: int(attempts),
			Accepted: stage != task.GetCurrentStage(),
		})
	}
	return out
}

func envelopeFromTask(task *harnesspb.TaskView) output.Envelope {
	return output.Success(output.Envelope{
		TaskID:         task.GetTaskId(),
		Status:         task.GetStatus(),
		TerminalReason: task.GetTerminalReason(),
		CurrentStage:   task.GetCurrentStage(),
		Stages:         stageSummary(task),
		Tokens:         task.GetConsumedTokens(),
		Cost:           task.GetConsumedCost(),
	})
}

func describeError(err error) output.Envelope {
	code := "E_UNKNOWN"
	message := err.Error()
	if strings.Contains(message, "E_TENANT") {
		code = "E_TENANT_REQUIRED"
	}
	return output.Failure(code, message, nil)
}
