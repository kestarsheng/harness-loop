package service

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	memorypb "agent-platform/pkg/pb/memory"
)

type MemoryGRPCRecaller struct {
	conn   *grpc.ClientConn
	client memorypb.MemoryServiceClient
}

func DialMemoryRecaller(ctx context.Context, target string, timeout time.Duration) (*MemoryGRPCRecaller, error) {
	if target == "" {
		return nil, fmt.Errorf("memory recaller: target is required")
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, target, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, fmt.Errorf("memory recaller: dial %s: %w", target, err)
	}
	return &MemoryGRPCRecaller{conn: conn, client: memorypb.NewMemoryServiceClient(conn)}, nil
}

func (r *MemoryGRPCRecaller) Recall(ctx context.Context, tenantID, projectID, query string, categories []string, topK int) ([]MemoryHit, error) {
	resp, err := r.client.RecallTopics(ctx, &memorypb.RecallTopicsRequest{
		TenantId: tenantID, ProjectId: projectID, Query: query,
		Categories: categories, TopK: int32(topK),
	})
	if err != nil {
		return nil, fmt.Errorf("memory recaller: %w", err)
	}
	hits := make([]MemoryHit, 0, len(resp.GetHits()))
	for _, h := range resp.GetHits() {
		hits = append(hits, MemoryHit{
			TopicKey:   h.GetTopicKey(),
			Title:      h.GetTitle(),
			Body:       h.GetBody(),
			Category:   h.GetCategory(),
			SourceFile: h.GetSourceFile(),
			UpdatedAt:  time.UnixMilli(h.GetUpdatedAtMs()).UTC(),
			Score:      h.GetScore(),
		})
	}
	return hits, nil
}

func (r *MemoryGRPCRecaller) Close() error {
	if r == nil || r.conn == nil {
		return nil
	}
	return r.conn.Close()
}
