// Command mcpprobe calls a built-in MCP tool through the real mcp-service gRPC
// surface with a harness policy context, so tool-level enforcement (workspace
// root resolution + escape refusal + audit trail) can be exercised without a
// model in the loop.
//
//	go run ./bench/p2/scripts/cmd/mcpprobe -addr 127.0.0.1:50105 \
//	  -root C:/tmp/ws -tool file_write -path cmd/server/healthz.go -content "package main"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "agent-platform/pkg/pb/mcp"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50105", "mcp-service gRPC address")
	root := flag.String("root", "", "workspace root passed through __policy_context__")
	tenant := flag.String("tenant", "tenant-artifact", "tenant id")
	stage := flag.String("stage", "c", "harness stage")
	tool := flag.String("tool", "file_write", "tool name")
	path := flag.String("path", "", "path argument")
	content := flag.String("content", "", "content argument")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Println("dial error:", err)
		os.Exit(1)
	}
	defer conn.Close()
	client := pb.NewMCPServiceClient(conn)

	args, _ := json.Marshal(map[string]string{"path": *path, "content": *content})
	cfg, _ := json.Marshal(map[string]any{
		"__policy_context__": map[string]string{
			"stage":          *stage,
			"tenant_id":      *tenant,
			"workspace_root": *root,
		},
	})

	resp, err := client.CallTool(ctx, &pb.CallToolRequest{
		Name:       *tool,
		Arguments:  string(args),
		ToolConfig: string(cfg),
		TenantId:   *tenant,
	})
	if err != nil {
		fmt.Println("call error:", err)
		os.Exit(1)
	}
	fmt.Printf("is_error=%v\ncontent=%s\n", resp.IsError, resp.Content)
	if resp.IsError {
		os.Exit(2)
	}
}
