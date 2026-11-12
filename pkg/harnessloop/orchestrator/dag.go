package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"agent-platform/pkg/harnessloop/subtask"
)

// Contract is the input/output contract of one DAG node (design 4.13.2).
type Contract struct {
	Type             string   `json:"type" yaml:"type"`
	Paths            []string `json:"paths,omitempty" yaml:"paths"`
	Requires         []string `json:"requires,omitempty" yaml:"requires"`
	Verify           []string `json:"verify,omitempty" yaml:"verify"`
	ConsistencyGroup string   `json:"consistency_group,omitempty" yaml:"consistency_group"`
}

// AcceptanceSpec is one node-level acceptance item.
type AcceptanceSpec struct {
	Type string `json:"type" yaml:"type"`
	Expr string `json:"expr" yaml:"expr"`
}

// PlanNode is one subtask node in the b-stage plan.
type PlanNode struct {
	NodeID           string           `json:"node_id" yaml:"node_id"`
	AgentID          string           `json:"agent_id,omitempty" yaml:"agent_id"`
	Role             string           `json:"role" yaml:"role"`
	InputContract    Contract         `json:"input_contract" yaml:"input_contract"`
	OutputContract   Contract         `json:"output_contract" yaml:"output_contract"`
	Acceptance       []AcceptanceSpec `json:"acceptance" yaml:"acceptance"`
	Splittable       *bool            `json:"splittable,omitempty" yaml:"splittable"`
	Children         []PlanNode       `json:"children,omitempty" yaml:"children"`
	Tools            []string         `json:"tools,omitempty" yaml:"tools"`
	RequiresApproval bool             `json:"requires_approval,omitempty" yaml:"requires_approval"`
}

// PlanEdge is a dependency edge pointing from the prerequisite to the successor.
type PlanEdge struct {
	From string `json:"from" yaml:"from"`
	To   string `json:"to" yaml:"to"`
}

// ParallelSet declares a group of nodes that may run at the same time.
type ParallelSet struct {
	SetID           string                  `json:"set_id" yaml:"set_id"`
	Nodes           []string                `json:"nodes" yaml:"nodes"`
	FailureStrategy subtask.FailureStrategy `json:"failure_strategy" yaml:"failure_strategy"`
	TolerateRatio   float64                 `json:"tolerate_ratio,omitempty" yaml:"tolerate_ratio"`
}

// Plan is the SubtaskPlan/v1 artifact produced by stage b.
type Plan struct {
	DagID        string        `json:"dag_id" yaml:"dag_id"`
	Nodes        []PlanNode    `json:"nodes" yaml:"nodes"`
	Edges        []PlanEdge    `json:"edges" yaml:"edges"`
	ParallelSets []ParallelSet `json:"parallel_sets" yaml:"parallel_sets"`
}

// PlanConfig carries the plan-time limits. Defaults mirror U13-A and are never
// silently widened at runtime.
type PlanConfig struct {
	MaxDirectChildren int               `yaml:"max_direct_children"`
	MaxDepth          int               `yaml:"max_depth"`
	TolerateRatioMax  float64           `yaml:"tolerate_ratio_max"`
	ApprovalTools     []string          `yaml:"approval_tools"`
	WorkspaceRoot     string            `yaml:"-"`
	Roles             *subtask.Registry `yaml:"-"`
}

func DefaultPlanConfig() PlanConfig {
	return PlanConfig{
		MaxDirectChildren: 20,
		MaxDepth:          2,
		TolerateRatioMax:  0.5,
	}
}

// LoadPlanConfig overlays a YAML file on top of the closed defaults. Unknown
// files are reported instead of silently falling back.
func LoadPlanConfig(path string) (PlanConfig, error) {
	cfg := DefaultPlanConfig()
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("orchestrator: read plan config: %w", err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("orchestrator: parse plan config: %w", err)
	}
	if cfg.MaxDirectChildren <= 0 {
		cfg.MaxDirectChildren = 20
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = 2
	}
	if cfg.TolerateRatioMax <= 0 || cfg.TolerateRatioMax > 0.5 {
		cfg.TolerateRatioMax = 0.5
	}
	return cfg, nil
}

// ParsePlan decodes a SubtaskPlan/v1 JSON document.
func ParsePlan(raw []byte) (*Plan, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, newError(CodeDAGIncomplete, "subtask plan is empty")
	}
	var plan Plan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, newError(CodeDAGIncomplete, "decode subtask plan: %v", err)
	}
	return &plan, nil
}

// Validate applies the eight rules from design 4.13.2 plus the four
// non-splittable conditions from REQ-126.
func (p *Plan) Validate(cfg PlanConfig) error {
	if cfg.MaxDirectChildren <= 0 {
		cfg.MaxDirectChildren = 20
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = 2
	}
	if cfg.TolerateRatioMax <= 0 || cfg.TolerateRatioMax > 0.5 {
		cfg.TolerateRatioMax = 0.5
	}
	if strings.TrimSpace(p.DagID) == "" {
		return newError(CodeDAGIncomplete, "dag_id is required")
	}

	// Rule 1: node count within [1, max_direct_children].
	if len(p.Nodes) == 0 || len(p.Nodes) > cfg.MaxDirectChildren {
		return newError(CodeSubtaskLimitExceeded,
			"subtask node count %d is outside [1,%d]", len(p.Nodes), cfg.MaxDirectChildren)
	}

	nodes, err := p.nodeIndex()
	if err != nil {
		return err
	}

	// Rule 2: a multi-node plan must declare dependencies or explicit parallel sets.
	if len(p.Nodes) > 1 && len(p.Edges) == 0 && len(p.ParallelSets) == 0 {
		return newError(CodeDAGIncomplete, "multi-node plan declares neither edges nor parallel_sets")
	}

	// Rule 3: edges must reference known nodes and form a DAG.
	for _, edge := range p.Edges {
		if edge.From == "" || edge.To == "" {
			return newError(CodeDAGIncomplete, "edge with empty from/to is not allowed")
		}
		if _, ok := nodes[edge.From]; !ok {
			return newError(CodeDAGCycle, "edge references unknown node %q", edge.From)
		}
		if _, ok := nodes[edge.To]; !ok {
			return newError(CodeDAGCycle, "edge references unknown node %q", edge.To)
		}
	}
	if err := detectCycle(nodes, p.Edges); err != nil {
		return err
	}

	// Rule 5 + role/contract completeness for every node, including children.
	for _, node := range flattenNodes(p.Nodes) {
		if strings.TrimSpace(node.Role) == "" {
			return newError(subtask.CodeAgentRoleUndeclared, "node %q declares no role", node.NodeID)
		}
		if err := validateContract(cfg, node); err != nil {
			return err
		}
		// Rule 6: non-splittable units are rejected at plan time.
		if err := checkSplittable(cfg, node); err != nil {
			return err
		}
	}

	// Rule 7: depth is capped at max_depth (default 2).
	if err := p.validateDepth(cfg); err != nil {
		return err
	}

	// Rule 4 + 8: parallel sets must be dependency-free and declare a strategy.
	if err := p.validateParallelSets(nodes); err != nil {
		return err
	}
	return nil
}

func (p *Plan) nodeIndex() (map[string]PlanNode, error) {
	out := make(map[string]PlanNode)
	for _, node := range flattenNodes(p.Nodes) {
		if strings.TrimSpace(node.NodeID) == "" {
			return nil, newError(CodeDAGIncomplete, "node_id is required for every node")
		}
		if _, ok := out[node.NodeID]; ok {
			return nil, newError(CodeDAGIncomplete, "duplicate node_id %q", node.NodeID)
		}
		out[node.NodeID] = node
	}
	return out, nil
}

func (p *Plan) validateDepth(cfg PlanConfig) error {
	for _, node := range p.Nodes {
		if len(node.Children) == 0 {
			continue
		}
		if cfg.MaxDepth < 2 {
			return newError(CodeSubtaskDepthExceeded,
				"node %q declares children but max_depth is %d", node.NodeID, cfg.MaxDepth)
		}
		if len(node.Children) > cfg.MaxDirectChildren {
			return newError(CodeSubtaskLimitExceeded,
				"node %q declares %d children, limit is %d", node.NodeID, len(node.Children), cfg.MaxDirectChildren)
		}
		for _, child := range node.Children {
			if len(child.Children) > 0 {
				return newError(CodeSubtaskDepthExceeded,
					"node %q nests a third level under %q", node.NodeID, child.NodeID)
			}
			if len(node.Children) == 1 && child.NodeID == node.NodeID {
				return newError(CodeDAGIncomplete, "child node %q reuses its parent node_id", child.NodeID)
			}
		}
	}
	return nil
}

func (p *Plan) validateParallelSets(nodes map[string]PlanNode) error {
	seenSets := make(map[string]struct{}, len(p.ParallelSets))
	graph := buildAdjacency(p.Edges)
	for _, set := range p.ParallelSets {
		if strings.TrimSpace(set.SetID) == "" {
			return newError(CodeDAGIncomplete, "parallel set id is required")
		}
		if _, ok := seenSets[set.SetID]; ok {
			return newError(CodeDAGIncomplete, "duplicate parallel set id %q", set.SetID)
		}
		seenSets[set.SetID] = struct{}{}
		if len(set.Nodes) == 0 {
			return newError(CodeDAGIncomplete, "parallel set %q declares no nodes", set.SetID)
		}
		for _, nodeID := range set.Nodes {
			if _, ok := nodes[nodeID]; !ok {
				return newError(CodeDAGIncomplete,
					"parallel set %q references unknown node %q", set.SetID, nodeID)
			}
		}
		// Rule 4: any dependency path between two members is a conflict.
		for i := 0; i < len(set.Nodes); i++ {
			for j := i + 1; j < len(set.Nodes); j++ {
				a, b := set.Nodes[i], set.Nodes[j]
				if reachable(graph, a, b) || reachable(graph, b, a) {
					return newError(CodeDAGParallelConflict,
						"parallel set %q contains dependent nodes %q and %q", set.SetID, a, b)
				}
			}
		}
		// Rule 8: failure strategy is mandatory.
		if set.FailureStrategy == "" || !set.FailureStrategy.Valid() {
			return newError(CodePropagationUndeclared,
				"parallel set %q must declare fail-fast, tolerate or retry", set.SetID)
		}
		if set.FailureStrategy == subtask.StrategyTolerate {
			if set.TolerateRatio <= 0 || set.TolerateRatio > 0.5 {
				return newError(CodePropagationUndeclared,
					"parallel set %q tolerate_ratio must be in (0,0.5]", set.SetID)
			}
		}
	}
	return nil
}

func validateContract(cfg PlanConfig, node PlanNode) error {
	if strings.TrimSpace(node.InputContract.Type) == "" {
		return newError(CodeContractIncomplete, "node %q input_contract.type is required", node.NodeID)
	}
	if strings.TrimSpace(node.OutputContract.Type) == "" {
		return newError(CodeContractIncomplete, "node %q output_contract.type is required", node.NodeID)
	}
	if len(node.OutputContract.Paths) == 0 {
		return newError(CodeContractIncomplete, "node %q output_contract.paths is required", node.NodeID)
	}
	paths := append([]string(nil), node.InputContract.Paths...)
	paths = append(paths, node.OutputContract.Paths...)
	for _, path := range paths {
		if err := validateSandboxPath(cfg.WorkspaceRoot, path); err != nil {
			return newError(CodeContractIncomplete, "node %q path %q is outside the workspace sandbox: %v", node.NodeID, path, err)
		}
	}
	return nil
}

func validateSandboxPath(root, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("empty path")
	}
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(path) || filepath.VolumeName(path) != "" || strings.HasPrefix(path, "/") || strings.HasPrefix(path, "\\") {
		return fmt.Errorf("absolute paths are not allowed")
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("parent traversal is not allowed")
	}
	if root == "" {
		return nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	// Globs are checked at their static prefix; wildcards cannot escape the root.
	static := path
	if idx := strings.IndexAny(static, "*?["); idx >= 0 {
		static = static[:idx]
	}
	absPath, err := filepath.Abs(filepath.Join(root, static))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes workspace root")
	}
	return nil
}

func checkSplittable(cfg PlanConfig, node PlanNode) error {
	if strings.TrimSpace(node.InputContract.ConsistencyGroup) != "" ||
		strings.TrimSpace(node.OutputContract.ConsistencyGroup) != "" {
		return newError(CodeSubtaskNotSplittable,
			"node %q declares a cross-file consistency group", node.NodeID)
	}
	if node.RequiresApproval {
		return newError(CodeSubtaskNotSplittable,
			"node %q contains an external approval point", node.NodeID)
	}
	for _, tool := range node.Tools {
		for _, approval := range cfg.ApprovalTools {
			if tool == approval {
				return newError(CodeSubtaskNotSplittable,
					"node %q uses approval-gated tool %q", node.NodeID, tool)
			}
		}
	}
	if node.Splittable != nil {
		if !*node.Splittable {
			return newError(CodeSubtaskNotSplittable, "node %q is declared non-splittable", node.NodeID)
		}
		return nil
	}
	// Heuristic: a single deterministic verification on a single concrete file
	// is an atomic modification and must not be split into parallel subtasks.
	if len(node.Acceptance) == 1 && len(node.OutputContract.Paths) == 1 {
		kind := node.Acceptance[0].Type
		if kind == "tests_pass" || kind == "build_pass" {
			if !strings.ContainsAny(node.OutputContract.Paths[0], "*?[") {
				return newError(CodeSubtaskNotSplittable,
					"node %q combines a single file with a single deterministic verification", node.NodeID)
			}
		}
	}
	return nil
}

func flattenNodes(nodes []PlanNode) []PlanNode {
	out := make([]PlanNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node)
		out = append(out, node.Children...)
	}
	return out
}

func buildAdjacency(edges []PlanEdge) map[string][]string {
	graph := make(map[string][]string, len(edges))
	for _, edge := range edges {
		graph[edge.From] = append(graph[edge.From], edge.To)
	}
	return graph
}

func detectCycle(nodes map[string]PlanNode, edges []PlanEdge) error {
	graph := buildAdjacency(edges)
	state := make(map[string]uint8, len(nodes)) // 0=unvisited, 1=visiting, 2=done
	var visit func(string) error
	visit = func(node string) error {
		switch state[node] {
		case 1:
			return newError(CodeDAGCycle, "dependency cycle detected at node %q", node)
		case 2:
			return nil
		}
		state[node] = 1
		for _, next := range graph[node] {
			if err := visit(next); err != nil {
				return err
			}
		}
		state[node] = 2
		return nil
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func reachable(graph map[string][]string, from, to string) bool {
	if from == to {
		return false
	}
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range graph[current] {
			if next == to {
				return true
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

// SuggestedLevels returns the topological level of every node for display only.
// It never influences scheduling.
func (p *Plan) SuggestedLevels() (map[string]int, error) {
	nodes, err := p.nodeIndex()
	if err != nil {
		return nil, err
	}
	if err := detectCycle(nodes, p.Edges); err != nil {
		return nil, err
	}
	levels := make(map[string]int, len(nodes))
	var level func(string) int
	level = func(node string) int {
		if v, ok := levels[node]; ok {
			return v
		}
		best := 1
		for _, edge := range p.Edges {
			if edge.To == node {
				if candidate := level(edge.From) + 1; candidate > best {
					best = candidate
				}
			}
		}
		levels[node] = best
		return best
	}
	for id := range nodes {
		level(id)
	}
	return levels, nil
}
