package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"sync/atomic"

	"google.golang.org/grpc"

	agentpb "agent-platform/pkg/pb/agent"
)

type demoAgent struct {
	agentpb.UnimplementedAgentServiceServer
	calls int32
}

func (d *demoAgent) ExecuteStream(req *agentpb.ExecuteStreamRequest, srv agentpb.AgentService_ExecuteStreamServer) error {
	n := atomic.AddInt32(&d.calls, 1)
	log.Printf("stage execution #%d: session=%s agent=%s", n, req.GetSessionId(), req.GetEntryAgent())

	if err := srv.Send(&agentpb.ExecuteStreamChunk{Type: "response", Content: fmt.Sprintf("working on stage #%d", n)}); err != nil {
		return err
	}
	if err := srv.Send(&agentpb.ExecuteStreamChunk{Type: "tool_call", ToolCall: &agentpb.ToolCall{
		Id:        fmt.Sprintf("call-%d", n),
		Name:      "stage_finish",
		Arguments: `{"attempt_id":"demo-attempt","stage":"a","summary":"demo stage finished"}`,
	}}); err != nil {
		return err
	}
	return srv.Send(&agentpb.ExecuteStreamChunk{Type: "final", Content: "stage finished"})
}

func main() {
	addr := flag.String("addr", "127.0.0.1:50002", "listen address")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("fakeagent: listen: %v", err)
	}
	srv := grpc.NewServer()
	agentpb.RegisterAgentServiceServer(srv, &demoAgent{})
	log.Printf("fake agent listening on %s", *addr)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("fakeagent: serve: %v", err)
	}
}
