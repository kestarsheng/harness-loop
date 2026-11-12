package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"time"

	"agent-platform/pkg/harnessloop/subtask"
)

// PlanRequest is the b-stage planning input. Exactly one of Plan or RawPlan is
// expected; RawPlan lets the caller feed the agent's finish payload untouched.
type PlanRequest struct {
	ParentTaskID string
	TenantID     string
	ProjectID    string
	Stage        string
	DagID        string
	Plan         *Plan
	RawPlan      []byte
}

// PlanResult contains the validated DAG and the subtask rows it expands to.
type PlanResult struct {
	Plan     *Plan
	Subtasks []*subtask.Subtask
	Levels   map[string]int
}

// Planner validates and materialises the b-stage subtask DAG.
type Planner struct {
	cfg     PlanConfig
	emitter Emitter
}

func NewPlanner(cfg PlanConfig, emitter Emitter) *Planner {
	if cfg.MaxDirectChildren <= 0 {
		cfg.MaxDirectChildren = 20
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = 2
	}
	if cfg.TolerateRatioMax <= 0 || cfg.TolerateRatioMax > 0.5 {
		cfg.TolerateRatioMax = 0.5
	}
	return &Planner{cfg: cfg, emitter: emitter}
}

// Plan parses, validates and expands the plan. A validation failure is
// returned as a coded error so the runtime can place it in failed_criteria.
func (p *Planner) Plan(ctx context.Context, req PlanRequest) (*PlanResult, error) {
	plan := req.Plan
	if plan == nil {
		parsed, err := ParsePlan(req.RawPlan)
		if err != nil {
			return nil, err
		}
		plan = parsed
	}
	if req.DagID != "" {
		plan.DagID = req.DagID
	}
	if err := plan.Validate(p.cfg); err != nil {
		return nil, err
	}

	stage := strings.TrimSpace(req.Stage)
	if stage == "" {
		stage = "b"
	}
	if err := p.validateRoles(ctx, plan, stage); err != nil {
		return nil, err
	}

	subtasks, err := plan.BuildSubtasks(req.ParentTaskID, req.TenantID, req.ProjectID)
	if err != nil {
		return nil, err
	}
	levels, err := plan.SuggestedLevels()
	if err != nil {
		return nil, err
	}

	if err := emit(ctx, p.emitter, Event{
		Type:         EventSubtaskPlanCreated,
		ParentTaskID: req.ParentTaskID,
		DagID:        plan.DagID,
		Payload: map[string]any{
			"dag_id":             plan.DagID,
			"node_count":         len(plan.Nodes),
			"subtask_count":      len(subtasks),
			"parallel_set_count": len(plan.ParallelSets),
			"edge_count":         len(plan.Edges),
			"levels":             levels,
		},
	}); err != nil {
		return nil, err
	}

	return &PlanResult{Plan: plan, Subtasks: subtasks, Levels: levels}, nil
}

// validateRoles reuses the T-P1-22 role/tool registry contract instead of
// defining a second permission model.
func (p *Planner) validateRoles(ctx context.Context, plan *Plan, stage string) error {
	if p.cfg.Roles == nil {
		return nil
	}
	for _, node := range flattenNodes(plan.Nodes) {
		if strings.TrimSpace(node.AgentID) == "" {
			continue
		}
		binding, err := p.cfg.Roles.Resolve(ctx, node.AgentID, stage)
		if err != nil {
			return err
		}
		if node.Role != "" && !containsString(binding.Roles, node.Role) {
			return newError(subtask.CodeAgentRoleUndeclared,
				"agent %s does not declare role %q", node.AgentID, node.Role)
		}
		for _, tool := range node.Tools {
			if err := p.cfg.Roles.CheckTool(ctx, node.AgentID, stage, tool); err != nil {
				return err
			}
		}
	}
	return nil
}

// BuildSubtasks expands the DAG into pending subtask rows. It does not persist
// them: subtask.Repo.Insert owns the parent tenant/project check.
func (p *Plan) BuildSubtasks(parentTaskID, tenantID, projectID string) ([]*subtask.Subtask, error) {
	if strings.TrimSpace(parentTaskID) == "" || strings.TrimSpace(tenantID) == "" || strings.TrimSpace(projectID) == "" {
		return nil, newError(CodeDAGIncomplete, "parent_task_id, tenant_id and project_id are required")
	}
	setFor := make(map[string]ParallelSet, len(p.ParallelSets))
	for _, set := range p.ParallelSets {
		for _, nodeID := range set.Nodes {
			setFor[nodeID] = set
		}
	}
	depends := make(map[string][]string, len(p.Nodes))
	for _, edge := range p.Edges {
		depends[edge.To] = append(depends[edge.To], edge.From)
	}

	var out []*subtask.Subtask
	index := 0
	var add func(node PlanNode, depth int, inherited *ParallelSet) error
	add = func(node PlanNode, depth int, inherited *ParallelSet) error {
		set := inherited
		if declared, ok := setFor[node.NodeID]; ok {
			copied := declared
			set = &copied
		}
		strategy := subtask.StrategyFailFast
		setID := ""
		if set != nil {
			if set.FailureStrategy != "" {
				strategy = set.FailureStrategy
			}
			setID = set.SetID
		}
		deps := append([]string(nil), depends[node.NodeID]...)
		deps = append(deps, node.InputContract.Requires...)
		criteria := make([]string, 0, len(node.Acceptance))
		for _, item := range node.Acceptance {
			if strings.TrimSpace(item.Expr) != "" {
				criteria = append(criteria, item.Expr)
			} else if strings.TrimSpace(item.Type) != "" {
				criteria = append(criteria, item.Type)
			}
		}
		subtaskID, err := newSubtaskID()
		if err != nil {
			return err
		}
		row := &subtask.Subtask{
			SubtaskID:          subtaskID,
			ParentTaskID:       parentTaskID,
			TenantID:           tenantID,
			ProjectID:          projectID,
			DagID:              p.DagID,
			DagNodeID:          node.NodeID,
			Depth:              depth,
			IndexInParent:      index,
			ParallelSetID:      setID,
			Role:               node.Role,
			Status:             subtask.StatusPending,
			DependsOn:          dedupeStrings(deps),
			InputContract:      contractToMap(node.InputContract),
			OutputContract:     contractToMap(node.OutputContract),
			AcceptanceCriteria: criteria,
			FailureStrategy:    strategy,
			AttemptNo:          1,
		}
		if err := row.Validate(); err != nil {
			return err
		}
		out = append(out, row)
		index++
		for _, child := range node.Children {
			if err := add(child, depth+1, set); err != nil {
				return err
			}
		}
		return nil
	}
	for _, node := range p.Nodes {
		if err := add(node, 1, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func contractToMap(c Contract) map[string]any {
	out := map[string]any{"type": c.Type}
	if len(c.Paths) > 0 {
		out["paths"] = append([]string(nil), c.Paths...)
	}
	if len(c.Requires) > 0 {
		out["requires"] = append([]string(nil), c.Requires...)
	}
	if len(c.Verify) > 0 {
		out["verify"] = append([]string(nil), c.Verify...)
	}
	if c.ConsistencyGroup != "" {
		out["consistency_group"] = c.ConsistencyGroup
	}
	return out
}

// ContractFromMap restores a Contract from a subtask's JSON map column.
func ContractFromMap(raw map[string]any) Contract {
	var c Contract
	if raw == nil {
		return c
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return c
	}
	_ = json.Unmarshal(blob, &c)
	return c
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, item := range in {
		if strings.TrimSpace(item) == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newSubtaskID returns a ULID (48-bit millisecond timestamp + 80-bit random).
// The platform does not depend on an external ULID module yet.
func newSubtaskID() (string, error) {
	var raw [16]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 0; i < 6; i++ {
		raw[i] = byte(ms >> uint(8*(5-i)))
	}
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", newError(CodeDAGIncomplete, "generate subtask id: %v", err)
	}
	out := make([]byte, 0, 26)
	out = append(out, encodeCrockford(raw[:6], 10, 2)...)
	out = append(out, encodeCrockford(raw[6:], 16, 0)...)
	return string(out), nil
}

// encodeCrockford encodes chars*5 bits from raw into Crockford base32. pad
// declares leading zero bits (ULID uses 2 leading zeros for the 48-bit
// timestamp so every id is exactly 26 characters).
func encodeCrockford(raw []byte, chars, pad int) []byte {
	out := make([]byte, chars)
	for i := 0; i < chars; i++ {
		var v byte
		for j := 0; j < 5; j++ {
			k := i*5 + j
			if k < pad {
				v <<= 1
				continue
			}
			bit := k - pad
			if bit >= len(raw)*8 {
				v <<= 1
				continue
			}
			v = v<<1 | ((raw[bit/8] >> uint(7-bit%8)) & 1)
		}
		out[i] = crockford[v]
	}
	return out
}
