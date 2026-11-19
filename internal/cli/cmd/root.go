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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"agent-platform/internal/cli/output"
	"agent-platform/internal/cli/tui"
	harnesspb "agent-platform/pkg/pb/harnessloop"

	"gopkg.in/yaml.v3"
)

type Options struct {
	Server      string
	KBServer    string
	AgentServer string
	Tenant      string
	Config      string
	WebBaseURL  string
	JSON        bool
	NoColor     bool
}

const (
	DefaultServerAddr   = "127.0.0.1:50013"
	DefaultKBServerAddr = "127.0.0.1:50002"
	// DefaultAgentServerAddr is agent-service, the direct-chat backend the
	// resident session talks to when a line is not a /task command.
	DefaultAgentServerAddr = "127.0.0.1:50006"
	DefaultTenant          = "default"
)

var opts Options

type ExitError struct {
	Code int
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit code %d", e.Code) }

type fileConfig struct {
	Server      string `yaml:"server"`
	KBServer    string `yaml:"kb_server"`
	AgentServer string `yaml:"agent_server"`
	Tenant      string `yaml:"tenant"`
	WebBaseURL  string `yaml:"web_base_url"`
}

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "harness-cli",
		Short:         "Harness Loop task CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&opts.Server, "server", "", "harnessloop-service gRPC address (default 127.0.0.1:50013)")
	pf.StringVar(&opts.KBServer, "kb-server", "", "knowledge-service gRPC address (default 127.0.0.1:50002)")
	pf.StringVar(&opts.AgentServer, "agent-server", "", "agent-service gRPC address for direct chat (default 127.0.0.1:50006, or $HARNESS_AGENT_SERVER)")
	pf.StringVar(&opts.Tenant, "tenant", "", "tenant id (default default)")
	pf.StringVar(&opts.Config, "config", "", "config file (default ~/.harness/config.yaml)")
	pf.StringVar(&opts.WebBaseURL, "web-base-url", "", "local web base URL for the fixed monitor board (default http://localhost:5174)")
	pf.BoolVar(&opts.JSON, "json", false, "emit JSON envelope")
	pf.BoolVar(&opts.NoColor, "no-color", false, "disable colored output")

	root.AddCommand(newLoopCmd(), newRunCmd(), newStatusCmd(), newAbortCmd(), newResumeCmd(), newLogsCmd(), newKBCmd(), newIndexCmd(), newDevCmd(), newVersionCmd())
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
	return DefaultServerAddr
}

func resolveKBServer(cfg fileConfig) string {
	if opts.KBServer != "" {
		return opts.KBServer
	}
	if env := os.Getenv("HARNESS_KB_SERVER"); env != "" {
		return env
	}
	if cfg.KBServer != "" {
		return cfg.KBServer
	}
	return DefaultKBServerAddr
}

// resolveAgentServer picks the agent-service address used for direct chat.
// Precedence: --agent-server, then HARNESS_AGENT_SERVER, then the config file,
// then the fixed local default.
func resolveAgentServer(cfg fileConfig) string {
	if opts.AgentServer != "" {
		return opts.AgentServer
	}
	if env := os.Getenv("HARNESS_AGENT_SERVER"); env != "" {
		return env
	}
	if cfg.AgentServer != "" {
		return cfg.AgentServer
	}
	return DefaultAgentServerAddr
}

func resolveTenant(cfg fileConfig) string {
	if opts.Tenant != "" {
		return opts.Tenant
	}
	if env := os.Getenv("HARNESS_TENANT"); env != "" {
		return env
	}
	if cfg.Tenant != "" {
		return cfg.Tenant
	}
	return DefaultTenant
}

func resolveWebBaseURL(cfg fileConfig) string {
	if opts.WebBaseURL != "" {
		return opts.WebBaseURL
	}
	if env := os.Getenv("HARNESS_WEB_BASE_URL"); env != "" {
		return env
	}
	if cfg.WebBaseURL != "" {
		return cfg.WebBaseURL
	}
	return tui.DefaultWebBaseURL
}

func newClient(cmd *cobra.Command) (harnesspb.HarnessLoopServiceClient, *grpc.ClientConn, string, error) {
	cfg := loadFileConfig(opts.Config)
	server := resolveServer(cfg)
	tenant := resolveTenant(cfg)
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

func exitCodeForError(err error) int {
	st, ok := status.FromError(err)
	if !ok {
		return output.ExitConnectionFail
	}
	switch st.Code() {
	case codes.InvalidArgument, codes.FailedPrecondition:
		return output.ExitInvalidUsage
	case codes.NotFound:
		return output.ExitFailed
	case codes.PermissionDenied, codes.Unauthenticated:
		return output.ExitInvalidUsage
	default:
		return output.ExitConnectionFail
	}
}

var knownErrorCodes = []string{
	"E_FSM_DISABLED", "E_TENANT_REQUIRED", "E_TOOL_DENIED", "E_PATH_ESCAPE",
	"E_EXEC_DENIED", "E_ILLEGAL_TRANSITION", "E_ACCEPTANCE_CONTEXT_INCOMPLETE",
	"E_AGGREGATION_MISSING", "E_VERSION_CONFLICT", "E_MODEL_CAPABILITY_MISMATCH",
}

func describeError(err error) output.Envelope {
	code := "E_UNKNOWN"
	message := err.Error()
	for _, known := range knownErrorCodes {
		if strings.Contains(message, known) {
			code = known
			break
		}
	}
	if st, ok := status.FromError(err); ok && code == "E_UNKNOWN" {
		switch st.Code() {
		case codes.NotFound:
			code = "E_NOT_FOUND"
		case codes.InvalidArgument:
			code = "E_INVALID_ARGUMENT"
		case codes.FailedPrecondition:
			code = "E_PRECONDITION_FAILED"
		case codes.Unavailable:
			code = "E_SERVICE_UNAVAILABLE"
		}
	}
	return output.Failure(code, message, nil)
}
