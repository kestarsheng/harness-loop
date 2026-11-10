package modelregistry

import "fmt"

type StageModel struct {
	Model           string
	Strategy        string
	RequireToolCall bool
}

type ResolveRequest struct {
	ModelID               string
	Stage                 string
	Strategy              string
	RequireToolCall       bool
	RequiredContextWindow int
	StageOverrides        map[string]StageModel
}

type ResolveResult struct {
	Model      *Model
	Strategy   string
	Candidates []string
}

func (r *Registry) Resolve(req ResolveRequest) (*ResolveResult, error) {
	strategy := req.Strategy
	if strategy == "" {
		strategy = StrategyBalanced
	}
	requireTool := req.RequireToolCall
	modelID := req.ModelID

	if override, ok := req.StageOverrides[req.Stage]; ok {
		if override.Model != "" {
			modelID = override.Model
		}
		if override.Strategy != "" {
			strategy = override.Strategy
		}
		requireTool = requireTool || override.RequireToolCall
	}

	eligible := r.eligible(requireTool, req.RequiredContextWindow)
	suggested := make([]string, 0, len(eligible))
	for _, m := range eligible {
		suggested = append(suggested, m.ID)
	}

	if modelID != "" {
		m, ok := r.Get(modelID)
		if !ok {
			return nil, &CapabilityError{Suggested: suggested, Reason: fmt.Sprintf("%s: %s", CodeUnknownModel, modelID)}
		}
		if m.Kind != KindChat {
			return nil, &CapabilityError{Suggested: suggested, Reason: fmt.Sprintf("model %s is not a chat model", modelID)}
		}
		if m.Status == StatusDisabled {
			return nil, &CapabilityError{Suggested: suggested, Reason: fmt.Sprintf("model %s is disabled", modelID)}
		}
		if requireTool && !m.ToolCall {
			return nil, &CapabilityError{Suggested: suggested, Reason: fmt.Sprintf("model %s does not support tool calls", modelID)}
		}
		if req.RequiredContextWindow > 0 && m.ContextWindow < req.RequiredContextWindow {
			return nil, &CapabilityError{
				Suggested: suggested,
				Reason:    fmt.Sprintf("model %s window %d < required %d", modelID, m.ContextWindow, req.RequiredContextWindow),
			}
		}
		return &ResolveResult{Model: m, Strategy: strategy, Candidates: suggested}, nil
	}

	if len(eligible) == 0 {
		return nil, &CapabilityError{Suggested: nil, Reason: "no available chat model satisfies the requirements"}
	}
	sortByStrategy(eligible, strategy)
	chosen := eligible[0]
	ids := make([]string, 0, len(eligible))
	for _, m := range eligible {
		ids = append(ids, m.ID)
	}
	return &ResolveResult{Model: chosen, Strategy: strategy, Candidates: ids}, nil
}

func (r *Registry) eligible(requireTool bool, requiredWindow int) []*Model {
	out := make([]*Model, 0, len(r.order))
	for _, m := range r.Available() {
		if m.Kind != KindChat {
			continue
		}
		if requireTool && !m.ToolCall {
			continue
		}
		if requiredWindow > 0 && m.ContextWindow < requiredWindow {
			continue
		}
		out = append(out, m)
	}
	return out
}

func (r *Registry) SupportsToolCall(id string) bool {
	m, ok := r.Get(id)
	return ok && m.ToolCall
}

func (r *Registry) CheckStageCapability(id, stage string, requireTool bool) error {
	m, ok := r.Get(id)
	if !ok {
		return &CapabilityError{Reason: fmt.Sprintf("%s: %s", CodeUnknownModel, id)}
	}
	if requireTool && !m.ToolCall {
		return &CapabilityError{
			Suggested: r.eligibleIDs(requireTool),
			Reason:    fmt.Sprintf("stage %s requires tool calls, model %s does not support them", stage, id),
		}
	}
	return nil
}

func (r *Registry) eligibleIDs(requireTool bool) []string {
	models := r.eligible(requireTool, 0)
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}
