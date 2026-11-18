// Command artifactprobe exercises the harnessloop -> agent-service stream
// directly, so a stage failure can be traced to the RPC instead of the
// harness loop's generic "unclassified stage error" classification.
//
//	go run ./bench/p2/scripts/cmd/artifactprobe -addr 127.0.0.1:50106
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"agent-platform/pkg/harnessclient"
	agentpb "agent-platform/pkg/pb/agent"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50106", "agent-service gRPC address")
	tenant := flag.String("tenant", "tenant-artifact", "tenant id")
	agent := flag.String("agent", "main-agent", "entry agent")
	workspace := flag.String("workspace", "", "workspace root passed as __workspace_root__")
	stage := flag.String("stage", "a", "stage letter")
	requireTool := flag.Bool("require-tool-call", false, "set __requires_tool_call__")
	lazy := flag.Bool("lazy", false, "use harnessclient.DialLazy like harnessloop-service does")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dial := harnessclient.Dial
	if *lazy {
		dial = harnessclient.DialLazy
	}
	client, err := dial(ctx, harnessclient.DefaultConfig(*addr))
	if err != nil {
		fmt.Println("dial error:", err)
		os.Exit(1)
	}
	defer client.Close()

	req := &agentpb.ExecuteStreamRequest{
		SessionId:  "artifact-probe",
		TenantId:   *tenant,
		Message:    "stage=a attempt=1",
		EntryAgent: *agent,
		Goal:       "确认 agent-service 流式接口可用",
		ContextVars: map[string]string{
			"__stage_mode__":         "true",
			"__policy_snapshot__":    "{}",
			"__policy_hash__":        "sha256:snapshot",
			"__stage_model__":        "qwen-plus",
			"__model_window__":       "131072",
			"__requires_tool_call__": strconv.FormatBool(*requireTool),
			"__stage__":              *stage,
			"__tenant_id__":          *tenant,
			"__agent_id__":           *agent,
			"__workspace_root__":     *workspace,
			"__task_id__":            "artifact-probe-task",
			"__project_id__":         "default",
		},
	}

	chunks := 0
	start := time.Now()
	err = client.ExecuteStream(ctx, req, func(c *agentpb.ExecuteStreamChunk) error {
		chunks++
		name := ""
		if c.ToolCall != nil {
			name = " tool=" + c.ToolCall.Name
		}
		fmt.Printf("chunk=%d type=%s%s content=%.80q\n", chunks, c.Type, name, c.Content)
		return nil
	})
	fmt.Printf("chunks=%d elapsed=%s error=%v\n", chunks, time.Since(start).Round(time.Millisecond), err)
}
