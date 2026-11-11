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
		Goal:       "这是链路验证任务：请用一句话概括阶段目标，然后必须调用 stage_finish 工具。",
		ContextVars: map[string]string{
			"__stage_mode__": "true",
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
