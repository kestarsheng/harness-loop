package orchestrator

import (
	"time"

	"agent-platform/pkg/harnessloop/subtask"
)

// HandoffParty identifies one side of a handoff contract. The role must be a
// role declared by agent-service (T-P1-22 contract).
type HandoffParty struct {
	AgentID   string `json:"agent_id"`
	Role      string `json:"role"`
	SubtaskID string `json:"subtask_id,omitempty"`
}

// ContextSummary is the five-element summary required by REQ-131. The field
// set is the same as compaction template.v1 (design 4.5.3), so the validator
// can reuse compaction.ParseSummary.
type ContextSummary struct {
	GoalRef     string              `json:"goal_ref"`
	Done        string              `json:"done"`
	Todo        string              `json:"todo"`
	Constraints string              `json:"constraints"`
	Background  string              `json:"background"`
	SourceRefs  map[string][]string `json:"source_refs,omitempty"`
}

// UnfinishedItem is one piece of work the receiver must finish. A handoff with
// no unfinished item is pointless and is rejected (design 4.13.4).
type UnfinishedItem struct {
	Item     string   `json:"item"`
	Reason   string   `json:"reason,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
}

// HandoffConstraints bound what the receiver may do. Tools never exceed the
// receiver's role whitelist.
type HandoffConstraints struct {
	Role              string           `json:"role"`
	Tools             []string         `json:"tools"`
	WorkspaceSubpaths []string         `json:"workspace_subpaths,omitempty"`
	TokenQuota        int64            `json:"token_quota,omitempty"`
	Acceptance        []AcceptanceSpec `json:"acceptance"`
}

// HandoffContract is HandoffContract/v1.
type HandoffContract struct {
	HandoffID      string                `json:"handoff_id"`
	ParentTaskID   string                `json:"parent_task_id"`
	SubtaskID      string                `json:"subtask_id"`
	TenantID       string                `json:"tenant_id"`
	ProjectID      string                `json:"project_id"`
	From           HandoffParty          `json:"from"`
	To             HandoffParty          `json:"to"`
	Artifacts      []subtask.ArtifactRef `json:"artifacts"`
	ContextSummary ContextSummary        `json:"context_summary"`
	Unfinished     []UnfinishedItem      `json:"unfinished"`
	Constraints    HandoffConstraints    `json:"constraints"`
	CreatedAt      time.Time             `json:"created_at"`
}
