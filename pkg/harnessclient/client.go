package harnessclient

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentpb "agent-platform/pkg/pb/agent"
)

type Config struct {
	Target      string
	DialTimeout time.Duration
	CallTimeout time.Duration
}

func DefaultConfig(target string) Config {
	return Config{Target: target, DialTimeout: 5 * time.Second, CallTimeout: 10 * time.Minute}
}

type Client struct {
	conn  *grpc.ClientConn
	agent agentpb.AgentServiceClient
	call  time.Duration
}

func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Target == "" {
		return nil, fmt.Errorf("harnessclient: target is required")
	}
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := grpc.DialContext(dialCtx, cfg.Target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("harnessclient: dial %s: %w", cfg.Target, err)
	}
	call := cfg.CallTimeout
	if call <= 0 {
		call = 10 * time.Minute
	}
	return &Client{conn: conn, agent: agentpb.NewAgentServiceClient(conn), call: call}, nil
}

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *Client) CallTimeout() time.Duration { return c.call }

func DialLazy(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Target == "" {
		return nil, fmt.Errorf("harnessclient: target is required")
	}
	conn, err := grpc.DialContext(ctx, cfg.Target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("harnessclient: dial %s: %w", cfg.Target, err)
	}
	call := cfg.CallTimeout
	if call <= 0 {
		call = 10 * time.Minute
	}
	return &Client{conn: conn, agent: agentpb.NewAgentServiceClient(conn), call: call}, nil
}
