package orchestrator

import (
	"context"
	"sort"

	"agent-platform/pkg/harnessloop/subtask"
)

type PropagationAction string

const (
	ActionContinue  PropagationAction = "continue"
	ActionCancelSet PropagationAction = "cancel_set"
	ActionRetry     PropagationAction = "retry"
)

// FailedSubtask identifies one failed subtask and its abnormal category.
type FailedSubtask struct {
	SubtaskID string
	Category  string
}

// ParentRollback is the fallback target the parent FSM must take.
type ParentRollback struct {
	From string
	To   string
}

type PropagationDecision struct {
	Action        PropagationAction
	ParallelSetID string
	Strategy      subtask.FailureStrategy
	Cancelled     []string
	Retry         []string
	Failures      []FailedSubtask
	Rollback      ParentRollback
	Tolerated     bool
	FailureRatio  float64
	Samples       int
	Reason        string
}

type PropagationResult struct {
	Decisions []PropagationDecision
}

type PropagationRequest struct {
	ParentTaskID string
	Sets         []ParallelSet
	Subtasks     []*subtask.Subtask
	Failed       []FailedSubtask
}

// Propagator applies the parallel-set failure strategy (REQ-134, 4.13.6).
type Propagator struct {
	cfg QuotaConfig
}

func NewPropagator(cfg QuotaConfig) *Propagator {
	cfg.normalize()
	return &Propagator{cfg: cfg}
}

// Propagate returns one decision per affected parallel set. A failure never
// sets the parent to failed directly: fail-fast cancels the rest of the set and
// requests a parent rollback (default d→c).
func (p *Propagator) Propagate(req PropagationRequest) (*PropagationResult, error) {
	if len(req.Failed) == 0 {
		return &PropagationResult{}, nil
	}
	subtasks := make(map[string]*subtask.Subtask, len(req.Subtasks))
	for _, st := range req.Subtasks {
		if st != nil {
			subtasks[st.SubtaskID] = st
		}
	}
	sets := make(map[string]ParallelSet, len(req.Sets))
	nodeToSet := make(map[string]string)
	for _, set := range req.Sets {
		sets[set.SetID] = set
		for _, node := range set.Nodes {
			nodeToSet[node] = set.SetID
		}
	}

	grouped := make(map[string][]FailedSubtask)
	setIDs := make(map[string]struct{})
	for _, failure := range req.Failed {
		st := subtasks[failure.SubtaskID]
		if st == nil {
			return nil, newError(CodePropagationIncomplete, "failed subtask %s is not in the request", failure.SubtaskID)
		}
		setID := st.ParallelSetID
		if setID == "" {
			setID = nodeToSet[st.DagNodeID]
		}
		if setID == "" {
			setID = "__default__"
		}
		grouped[setID] = append(grouped[setID], failure)
		setIDs[setID] = struct{}{}
	}

	ordered := make([]string, 0, len(setIDs))
	for id := range setIDs {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)

	result := &PropagationResult{Decisions: make([]PropagationDecision, 0, len(ordered))}
	for _, setID := range ordered {
		failures := grouped[setID]
		strategy := subtask.StrategyFailFast
		tolerateRatio := 0.0
		if set, ok := sets[setID]; ok {
			if !set.FailureStrategy.Valid() {
				return nil, newError(CodePropagationUndeclared,
					"parallel set %q declares no valid failure strategy", setID)
			}
			strategy = set.FailureStrategy
			tolerateRatio = set.TolerateRatio
		}
		members := membersOf(setID, nodeToSet, req.Subtasks)
		samples := 0
		for _, member := range members {
			if member.Status.Terminal() {
				samples++
			}
		}
		ratio := 0.0
		if samples > 0 {
			ratio = float64(len(failures)) / float64(samples)
		}
		decision := PropagationDecision{
			ParallelSetID: setID,
			Strategy:      strategy,
			Failures:      failures,
			FailureRatio:  ratio,
			Samples:       samples,
		}
		if setID == "__default__" {
			decision.ParallelSetID = ""
		}

		switch strategy {
		case subtask.StrategyTolerate:
			if samples >= p.cfg.MinSamplesForRatio && ratio <= tolerateThreshold(tolerateRatio, p.cfg.TolerateRatioMax) {
				decision.Action = ActionContinue
				decision.Tolerated = true
				decision.Reason = "failures within the declared tolerate ratio"
				result.Decisions = append(result.Decisions, decision)
				continue
			}
			decision.Reason = "tolerate ratio exceeded; escalating to fail-fast"
			applyFailFast(&decision, members, failures)
		case subtask.StrategyRetry:
			canRetry := true
			for _, failure := range failures {
				st := subtasks[failure.SubtaskID]
				if st == nil || st.AttemptNo >= p.cfg.RetryMaxAttempts {
					canRetry = false
					break
				}
			}
			if canRetry {
				decision.Action = ActionRetry
				decision.Reason = "subtask attempt budget remains"
				for _, failure := range failures {
					decision.Retry = append(decision.Retry, failure.SubtaskID)
				}
				result.Decisions = append(result.Decisions, decision)
				continue
			}
			decision.Reason = "retry budget exhausted; degrading to fail-fast"
			applyFailFast(&decision, members, failures)
		default:
			decision.Reason = "fail-fast: cancel the rest of the parallel set"
			applyFailFast(&decision, members, failures)
		}
		result.Decisions = append(result.Decisions, decision)
	}
	return result, nil
}

func tolerateThreshold(declared, cap float64) float64 {
	if declared <= 0 || declared > cap {
		return cap
	}
	return declared
}

func membersOf(setID string, nodeToSet map[string]string, all []*subtask.Subtask) []*subtask.Subtask {
	var out []*subtask.Subtask
	for _, st := range all {
		if st == nil {
			continue
		}
		if st.ParallelSetID == setID {
			out = append(out, st)
			continue
		}
		if st.ParallelSetID == "" && nodeToSet[st.DagNodeID] == setID {
			out = append(out, st)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].IndexInParent < out[j].IndexInParent })
	return out
}

func applyFailFast(decision *PropagationDecision, members []*subtask.Subtask, failures []FailedSubtask) {
	failed := make(map[string]struct{}, len(failures))
	for _, failure := range failures {
		failed[failure.SubtaskID] = struct{}{}
	}
	decision.Action = ActionCancelSet
	decision.Rollback = ParentRollback{From: "d", To: "c"}
	for _, member := range members {
		if _, ok := failed[member.SubtaskID]; ok {
			continue
		}
		if member.Status.Terminal() {
			continue
		}
		decision.Cancelled = append(decision.Cancelled, member.SubtaskID)
	}
}

// Apply cancels the subtasks selected by a fail-fast decision. Retry decisions
// are left to the runtime because they must re-enter the same subtask FSM.
func (p *Propagator) Apply(ctx context.Context, scheduler *Scheduler, result *PropagationResult) error {
	if scheduler == nil || result == nil {
		return nil
	}
	for _, decision := range result.Decisions {
		if decision.Action != ActionCancelSet {
			continue
		}
		for _, id := range decision.Cancelled {
			if err := scheduler.MarkCancelled(ctx, id, "fail-fast:"+decision.ParallelSetID); err != nil {
				return err
			}
		}
	}
	return nil
}
