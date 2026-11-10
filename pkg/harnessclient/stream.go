package harnessclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	agentpb "agent-platform/pkg/pb/agent"
)

type ChunkHandler func(*agentpb.ExecuteStreamChunk) error

var ErrEmptyStream = errors.New("harnessclient: stream ended without any chunk")

func (c *Client) ExecuteStream(ctx context.Context, req *agentpb.ExecuteStreamRequest, onChunk ChunkHandler) error {
	if req == nil {
		return errors.New("harnessclient: request is nil")
	}
	callCtx := ctx
	if c.call > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, c.call)
		defer cancel()
	}

	stream, err := c.agent.ExecuteStream(callCtx, req)
	if err != nil {
		return fmt.Errorf("harnessclient: open stream: %w", err)
	}

	chunks := 0
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if chunks == 0 {
				return ErrEmptyStream
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("harnessclient: recv: %w", err)
		}
		chunks++
		if onChunk != nil {
			if herr := onChunk(chunk); herr != nil {
				return herr
			}
		}
	}
}

type StreamSummary struct {
	Chunks   int
	Tokens   int
	Errors   []string
	Finished bool
	Elapsed  time.Duration
}

func Collect(stream func(ctx context.Context, req *agentpb.ExecuteStreamRequest, onChunk ChunkHandler) error, ctx context.Context, req *agentpb.ExecuteStreamRequest) (*StreamSummary, error) {
	start := time.Now()
	sum := &StreamSummary{}
	err := stream(ctx, req, func(chunk *agentpb.ExecuteStreamChunk) error {
		sum.Chunks++
		if chunk.Type == "error" && chunk.Content != "" {
			sum.Errors = append(sum.Errors, chunk.Content)
		}
		if chunk.Type == "final" || (chunk.Step != nil && chunk.Step.Result == "done") {
			sum.Finished = true
		}
		return nil
	})
	sum.Elapsed = time.Since(start)
	return sum, err
}
