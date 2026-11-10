package harnessclient

import (
	"context"
	"testing"
	"time"

	agentpb "agent-platform/pkg/pb/agent"
)

func TestExecuteStreamRejectsNilRequest(t *testing.T) {
	c := &Client{}
	if err := c.ExecuteStream(context.Background(), nil, nil); err == nil {
		t.Fatal("nil request must be rejected")
	}
}

func TestCollectSummarizesStream(t *testing.T) {
	stream := func(ctx context.Context, req *agentpb.ExecuteStreamRequest, onChunk ChunkHandler) error {
		_ = onChunk(&agentpb.ExecuteStreamChunk{Type: "response", Content: "a"})
		_ = onChunk(&agentpb.ExecuteStreamChunk{Type: "error", Content: "boom"})
		_ = onChunk(&agentpb.ExecuteStreamChunk{Type: "final", Content: "done"})
		return nil
	}
	sum, err := Collect(stream, context.Background(), &agentpb.ExecuteStreamRequest{Message: "x"})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if sum.Chunks != 3 {
		t.Fatalf("chunks = %d", sum.Chunks)
	}
	if len(sum.Errors) != 1 || sum.Errors[0] != "boom" {
		t.Fatalf("errors = %v", sum.Errors)
	}
	if !sum.Finished {
		t.Fatal("final chunk must mark finished")
	}
	if sum.Elapsed < 0 || sum.Elapsed > time.Minute {
		t.Fatalf("elapsed = %v", sum.Elapsed)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig("agent-service:50002")
	if cfg.Target != "agent-service:50002" || cfg.DialTimeout != 5*time.Second || cfg.CallTimeout != 10*time.Minute {
		t.Fatalf("config = %+v", cfg)
	}
	if _, err := Dial(context.Background(), Config{}); err == nil {
		t.Fatal("empty target must fail")
	}
}
