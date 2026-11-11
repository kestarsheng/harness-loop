package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"agent-platform/pkg/harnessclient"
	agentpb "agent-platform/pkg/pb/agent"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	client, err := harnessclient.Dial(ctx, harnessclient.DefaultConfig("127.0.0.1:50006"))
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer client.Close()

	req := &agentpb.ExecuteStreamRequest{
		SessionId:  "probe-1",
		Message:    "stage=a attempt=1",
		EntryAgent: "coder-agent",
		Goal:       "完成 harness loop 真实链路验证：依次走过需求理解、计划、执行、验收四个阶段，每个阶段用一句话概括该阶段要做什么，然后按约定格式结束本阶段。",
		ContextVars: map[string]string{
			"__stage_mode__":         "true",
			"__policy_snapshot__":    "{}",
			"__policy_hash__":        "sha256:snapshot",
			"__stage_model__":        "qwen-plus",
			"__model_window__":       "131072",
			"__requires_tool_call__": "true",
		},
	}
	err = client.ExecuteStream(ctx, req, func(c *agentpb.ExecuteStreamChunk) error {
		tc := ""
		if c.ToolCall != nil {
			tc = c.ToolCall.Name + " " + c.ToolCall.Arguments
		}
		fmt.Printf("type=%s content=%q tool=%s\n", c.Type, c.Content, tc)
		return nil
	})
	fmt.Println("stream_err=", err)
	if err != nil {
		os.Exit(2)
	}
}
