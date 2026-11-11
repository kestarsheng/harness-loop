package handler

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	codeindexpb "agent-platform/pkg/pb/codeindex"
	"agent-platform/services/harnessloop-service/internal/service"
)

type CodeIndexHandler struct {
	codeindexpb.UnimplementedCodeIndexServiceServer
	svc *service.IndexService
}

func NewCodeIndexHandler(svc *service.IndexService) *CodeIndexHandler {
	return &CodeIndexHandler{svc: svc}
}

func strictTenant(ctx context.Context, explicit string) (string, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-tenant-id"); len(vals) > 0 && vals[0] != "" {
			return vals[0], nil
		}
	}
	if explicit != "" {
		return explicit, nil
	}
	return "", status.Error(codes.InvalidArgument, "E_TENANT_REQUIRED: tenant_id is required")
}

func mapIndexError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, service.ErrTenantRequired):
		return status.Error(codes.InvalidArgument, "E_TENANT_REQUIRED: tenant_id and project_id are required")
	case errors.Is(err, service.ErrIndexDisabled):
		return status.Error(codes.Unavailable, "E_INDEX_DISABLED: code index is not configured")
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func (h *CodeIndexHandler) SearchCode(ctx context.Context, req *codeindexpb.SearchCodeRequest) (*codeindexpb.SearchCodeResponse, error) {
	tenantID, err := strictTenant(ctx, req.GetTenantId())
	if err != nil {
		return nil, err
	}
	res, err := h.svc.Search(ctx, tenantID, req.GetProjectId(), req.GetQuery(), int(req.GetTopK()), nil, req.GetEnableGraph())
	if err != nil {
		return nil, mapIndexError(err)
	}
	resp := &codeindexpb.SearchCodeResponse{
		VectorHits:  int32(res.Stats.VectorHits),
		KeywordHits: int32(res.Stats.KeywordHits),
		GraphHits:   int32(res.Stats.GraphHits),
		LatencyMs:   res.Stats.LatencyMs,
	}
	for _, hit := range res.Hits {
		resp.Hits = append(resp.Hits, &codeindexpb.CodeSearchHit{
			ChunkId:   hit.ChunkID,
			FilePath:  hit.FilePath,
			StartLine: int32(hit.StartLine),
			EndLine:   int32(hit.EndLine),
			Symbol:    hit.Symbol,
			Snippet:   hit.Snippet,
			Score:     hit.Scores,
			Sources:   hit.Sources,
		})
	}
	return resp, nil
}

func (h *CodeIndexHandler) IndexFile(ctx context.Context, req *codeindexpb.IndexFileRequest) (*codeindexpb.IndexFileResponse, error) {
	tenantID, err := strictTenant(ctx, req.GetTenantId())
	if err != nil {
		return nil, err
	}
	res, err := h.svc.Index(ctx, tenantID, req.GetProjectId(), req.GetFilePath(), req.GetContent(), time.Now().UTC(), req.GetForce())
	if err != nil {
		return nil, mapIndexError(err)
	}
	return &codeindexpb.IndexFileResponse{
		Skipped: res.Skipped,
		Chunks:  int32(res.Chunks),
		Edges:   int32(res.Edges),
		Status:  res.Status,
	}, nil
}
