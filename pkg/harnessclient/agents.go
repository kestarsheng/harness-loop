package harnessclient

import (
	"context"
	"fmt"

	agentpb "agent-platform/pkg/pb/agent"
)

// ListAgentTools returns the tools and roles declared by an agent.
func (c *Client) ListAgentTools(ctx context.Context, agentID string) (*agentpb.ListAgentToolsResponse, error) {
	if c == nil || c.agent == nil {
		return nil, fmt.Errorf("harnessclient: client is not initialised")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.call)
	defer cancel()
	return c.agent.ListAgentTools(callCtx, &agentpb.ListAgentToolsRequest{AgentId: agentID})
}

// GetAgentRoles returns the roles declared by an agent.
func (c *Client) GetAgentRoles(ctx context.Context, agentID string) (*agentpb.GetAgentRolesResponse, error) {
	if c == nil || c.agent == nil {
		return nil, fmt.Errorf("harnessclient: client is not initialised")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.call)
	defer cancel()
	return c.agent.GetAgentRoles(callCtx, &agentpb.GetAgentRolesRequest{AgentId: agentID})
}