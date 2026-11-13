package modelregistry

import (
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	ResolvedFromOverride = "override"
	ResolvedFromExplicit = "explicit"
	ResolvedFromStrategy = "strategy"
	ResolvedFromDefault  = "default"
)

const (
	EventModelSelected          = "model.selected"
	EventModelSwitched          = "model.switched"
	EventModelFallback          = "model.fallback"
	EventModelCapabilityRejected = "model.capability_rejected"
	EventModelRecovered         = "model.recovered"
	EventModelUnavailable       = "model.unavailable"
	EventModelRegistryChanged   = "model.registry_changed"
	EventModelConfigApplied     = "model.config_applied"
)

const (
	TieThreshold                 = 0.02
	DefaultCompactionRatio       = 0.80
	DefaultMemoryInjectRatio     = 0.15
	DefaultCompactionBudgetRatio = 0.10
)

var ErrSelectionRecord = fmt.Errorf("modelregistry: selection record failed")

type ModelEvent struct {
	Type    string
	Payload map[string]any
}

type ReliabilityFunc func(modelID, provider string) float64

type UnavailableFunc func() map[string]bool

type SelectRequest struct {
	TenantID                string
	ProjectID               string
	TaskID                  string
	Stage                   string
	ModelID                 string
	Strategy                string
	RequireToolCall         bool
	RequireStructuredOutput bool
	StageBudgetTokens       int
	RequiredContextWindow   int
	TaskBudgetTokens        int64
	EstimatedTokensIn       int64
	EstimatedTokensOut      int64
	StageOverrides          map[string]StageOverride
	DefaultModel            string
	RecentErrorRates        map[string]float64
	Unavailable             map[string]bool
}

type CandidateScore struct {
	ModelID         string  `json:"model_id"`
	Score           float64 `json:"score"`
	CostEfficiency  float64 `json:"cost_efficiency"`
	Quality         float64 `json:"quality"`
	Reliability     float64 `json:"reliability"`
	WindowHeadroom  float64 `json:"window_headroom"`
	CapabilityMatch float64 `json:"capability_match"`
	Window          int     `json:"window,omitempty"`
	Chosen          bool    `json:"chosen"`
}

type Selection struct {
	Stage              string
	Model              *Model
	Strategy           string
	ResolvedFrom       string
	Candidates         []CandidateScore
	Window             int
	ThresholdTokens    int
	MemoryInjectBudget int
	CompactionBudget   int64
	EstimatedCost      float64
	Event              ModelEvent
}

type SelectionRecord struct {
	SelectionID     string
	TenantID        string
	ProjectID       string
	TaskID          string
	Stage           string
	ModelID         string
	Strategy        string
	ResolvedFrom    string
	Candidates      []CandidateScore
	Window          int
	ThresholdTokens int
	TokensIn        int64
	TokensOut       int64
	Cost            float64
	CreatedAt       time.Time
}

type SelectionRecorder interface {
	RecordSelection(SelectionRecord) error
}

type Selector struct {
	reg         *Registry
	recorder    SelectionRecorder
	reliability ReliabilityFunc
	unavailable UnavailableFunc
	now         func() time.Time
}

func NewSelector(reg *Registry) *Selector {
	if reg == nil {
		reg = DefaultRegistry()
	}
	return &Selector{reg: reg, now: time.Now}
}

func (s *Selector) WithRecorder(recorder SelectionRecorder) *Selector {
	s.recorder = recorder
	return s
}

func (s *Selector) WithReliability(fn ReliabilityFunc) *Selector {
	s.reliability = fn
	return s
}

func (s *Selector) WithUnavailable(fn UnavailableFunc) *Selector {
	s.unavailable = fn
	return s
}

func (s *Selector) Registry() *Registry { return s.reg }

func (s *Selector) Select(req SelectRequest) (*Selection, error) {
	reg := s.reg
	overrides := req.StageOverrides
	if overrides == nil {
		overrides = reg.StageOverrides()
	}
	override := overrides[req.Stage]

	strategy := req.Strategy
	if strategy == "" {
		strategy = override.Strategy
	}
	if strategy == "" {
		strategy = reg.DefaultStrategy()
	}
	if !validStrategy(strategy) {
		strategy = StrategyBalanced
	}

	unavailable := mergeUnavailable(req.Unavailable, s.unavailable)
	candidates := eligibleCandidates(reg, req.RequireToolCall, req.RequireStructuredOutput, req.RequiredContextWindow, unavailable)
	suggested := candidateIDs(candidates)

	resolvedFrom := ResolvedFromStrategy
	modelID := ""
	switch {
	case override.Model != "":
		modelID, resolvedFrom = override.Model, ResolvedFromOverride
	case req.ModelID != "":
		modelID, resolvedFrom = req.ModelID, ResolvedFromExplicit
	case strategyConfigured(req, reg):
		modelID, resolvedFrom = "", ResolvedFromStrategy
	case req.DefaultModel != "":
		modelID, resolvedFrom = req.DefaultModel, ResolvedFromDefault
	case reg.DefaultTaskModel() != "":
		modelID, resolvedFrom = reg.DefaultTaskModel(), ResolvedFromDefault
	}

	var chosen *Model
	if modelID != "" {
		m, ok := reg.Get(modelID)
		if !ok {
			err := &CapabilityError{Suggested: suggested, Reason: fmt.Sprintf("%s: %s", CodeUnknownModel, modelID)}
			return nil, err
		}
		if violations := selectionViolations(m, req, unavailable); len(violations) > 0 {
			err := &CapabilityError{Suggested: suggested, Reason: fmt.Sprintf("%s: model %s violates stage requirements: %v", CodeCapabilityMismatch, modelID, violations)}
			return nil, err
		}
		chosen = m
	}

	if resolvedFrom == ResolvedFromStrategy {
		if len(candidates) == 0 {
			err := &CapabilityError{Reason: "no available chat model satisfies the requirements"}
			return nil, err
		}
		scored := scoreCandidates(req, candidates, strategy, s.reliability)
		rankCandidates(scored)
		chosen = reg.models[scored[0].ModelID]
	}

	scores := scoreCandidates(req, candidates, strategy, s.reliability)
	for i := range scores {
		scores[i].Chosen = scores[i].ModelID == chosen.ID
	}
	rankCandidates(scores)

	sel := &Selection{
		Stage:        req.Stage,
		Model:        chosen,
		Strategy:     strategy,
		ResolvedFrom: resolvedFrom,
		Candidates:   scores,
		Window:       chosen.ContextWindow,
		EstimatedCost: estimateCost(chosen, req.EstimatedTokensIn, req.EstimatedTokensOut),
	}
	applyWindowBudget(sel, req.TaskBudgetTokens)
	sel.Event = ModelEvent{
		Type: EventModelSelected,
		Payload: map[string]any{
			"stage":             req.Stage,
			"model_id":          chosen.ID,
			"strategy":          strategy,
			"resolved_from":     resolvedFrom,
			"candidates":        scores,
			"window":            sel.Window,
			"threshold_tokens":  sel.ThresholdTokens,
			"estimated_cost":    sel.EstimatedCost,
			"tenant_id":         req.TenantID,
			"project_id":        req.ProjectID,
			"task_id":           req.TaskID,
		},
	}

	if s.recorder != nil {
		record := SelectionRecord{
			SelectionID:     fmt.Sprintf("sel-%s-%s-%d", req.TaskID, req.Stage, s.now().UnixNano()),
			TenantID:        req.TenantID,
			ProjectID:       req.ProjectID,
			TaskID:          req.TaskID,
			Stage:           req.Stage,
			ModelID:         chosen.ID,
			Strategy:        strategy,
			ResolvedFrom:    resolvedFrom,
			Candidates:      scores,
			Window:          sel.Window,
			ThresholdTokens: sel.ThresholdTokens,
			TokensIn:        req.EstimatedTokensIn,
			TokensOut:       req.EstimatedTokensOut,
			Cost:            sel.EstimatedCost,
			CreatedAt:       s.now().UTC(),
		}
		if err := s.recorder.RecordSelection(record); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSelectionRecord, err)
		}
	}
	return sel, nil
}

func strategyConfigured(req SelectRequest, reg *Registry) bool {
	return req.Strategy != "" || reg.defaults.Strategy != ""
}

func mergeUnavailable(local map[string]bool, fn UnavailableFunc) map[string]bool {
	out := make(map[string]bool, len(local))
	for id, v := range local {
		if v {
			out[id] = true
		}
	}
	if fn != nil {
		for id, v := range fn() {
			if v {
				out[id] = true
			}
		}
	}
	return out
}

func eligibleCandidates(reg *Registry, requireTool, requireStructured bool, requiredWindow int, unavailable map[string]bool) []*Model {
	out := make([]*Model, 0, len(reg.order))
	for _, id := range reg.order {
		m := reg.models[id]
		if m == nil || m.Kind != KindChat || m.Status != StatusAvailable || unavailable[id] {
			continue
		}
		if requireTool && !m.ToolCall {
			continue
		}
		if requireStructured && !m.StructuredOutput {
			continue
		}
		if requiredWindow > 0 && m.ContextWindow < requiredWindow {
			continue
		}
		out = append(out, m)
	}
	return out
}

func selectionViolations(m *Model, req SelectRequest, unavailable map[string]bool) []string {
	var out []string
	if m.Kind != KindChat {
		out = append(out, "not_a_chat_model")
	}
	if m.Status != StatusAvailable {
		out = append(out, "status_not_available")
	}
	if unavailable[m.ID] {
		out = append(out, "model_unavailable")
	}
	if req.RequireToolCall && !m.ToolCall {
		out = append(out, "tool_call_missing")
	}
	if req.RequireStructuredOutput && !m.StructuredOutput {
		out = append(out, "structured_output_missing")
	}
	if req.RequiredContextWindow > 0 && m.ContextWindow < req.RequiredContextWindow {
		out = append(out, "context_window_too_small")
	}
	return out
}

func candidateIDs(models []*Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

func blendedPrice(m *Model) float64 {
	return m.priceIn()*0.7 + m.priceOut()*0.3
}

func estimateCost(m *Model, tokensIn, tokensOut int64) float64 {
	return float64(tokensIn)/1000*m.priceIn() + float64(tokensOut)/1000*m.priceOut()
}

func scoreCandidates(req SelectRequest, models []*Model, strategy string, relFn ReliabilityFunc) []CandidateScore {
	maxPrice := 0.0
	for _, m := range models {
		if p := blendedPrice(m); p > maxPrice {
			maxPrice = p
		}
	}
	out := make([]CandidateScore, 0, len(models))
	for _, m := range models {
		costEff := 1.0
		if maxPrice > 0 {
			costEff = 1 - blendedPrice(m)/maxPrice
		}
		headroom := 1.0
		if req.StageBudgetTokens > 0 && m.ContextWindow > 0 {
			headroom = float64(m.ContextWindow-req.StageBudgetTokens) / float64(m.ContextWindow)
			if headroom < 0 {
				headroom = 0
			}
			if headroom > 1 {
				headroom = 1
			}
		}
		capability := 1.0
		if !m.ToolCall || !m.StructuredOutput {
			capability = 0.5
		}
		reliability := reliabilityOf(m, req.RecentErrorRates, relFn)
		quality := 0.5*capability + 0.3*headroom + 0.2*reliability
		score := weightedScore(strategy, quality, costEff, reliability)
		out = append(out, CandidateScore{
			ModelID:         m.ID,
			Score:           score,
			CostEfficiency:  costEff,
			Quality:         quality,
			Reliability:     reliability,
			WindowHeadroom:  headroom,
			CapabilityMatch: capability,
			Window:          m.ContextWindow,
		})
	}
	return out
}

func reliabilityOf(m *Model, rates map[string]float64, fn ReliabilityFunc) float64 {
	var rate float64
	if fn != nil {
		rate = fn(m.ID, m.Provider)
	} else if rates != nil {
		if v, ok := rates[m.ID]; ok {
			rate = v
		} else {
			rate = rates[m.Provider]
		}
	}
	reliability := 1 - rate
	if reliability < 0 {
		reliability = 0
	}
	if reliability > 1 {
		reliability = 1
	}
	return reliability
}

func weightedScore(strategy string, quality, costEff, reliability float64) float64 {
	switch strategy {
	case StrategyCostFirst:
		return 0.85*costEff + 0.10*quality + 0.05*reliability
	case StrategyQualityFirst:
		return 0.80*quality + 0.10*costEff + 0.10*reliability
	default:
		return 0.45*quality + 0.40*costEff + 0.15*reliability
	}
}

func rankCandidates(scores []CandidateScore) {
	if len(scores) == 0 {
		return
	}
	best := scores[0].Score
	for _, s := range scores {
		if s.Score > best {
			best = s.Score
		}
	}
	top := make([]CandidateScore, 0, len(scores))
	rest := make([]CandidateScore, 0, len(scores))
	for _, s := range scores {
		if best-s.Score < TieThreshold {
			top = append(top, s)
		} else {
			rest = append(rest, s)
		}
	}
	sort.SliceStable(top, func(i, j int) bool {
		if math.Abs(top[i].Reliability-top[j].Reliability) > 1e-9 {
			return top[i].Reliability > top[j].Reliability
		}
		if top[i].Window != top[j].Window {
			return top[i].Window > top[j].Window
		}
		return top[i].ModelID < top[j].ModelID
	})
	sort.SliceStable(rest, func(i, j int) bool {
		if math.Abs(rest[i].Score-rest[j].Score) > 1e-9 {
			return rest[i].Score > rest[j].Score
		}
		return rest[i].ModelID < rest[j].ModelID
	})
	copy(scores, append(top, rest...))
}

func applyWindowBudget(sel *Selection, taskBudgetTokens int64) {
	if sel.Window > 0 {
		sel.ThresholdTokens = int(float64(sel.Window) * DefaultCompactionRatio)
	}
	sel.MemoryInjectBudget = int(float64(sel.ThresholdTokens) * DefaultMemoryInjectRatio)
	if taskBudgetTokens > 0 {
		sel.CompactionBudget = int64(float64(taskBudgetTokens) * DefaultCompactionBudgetRatio)
		if sel.CompactionBudget < 1 {
			sel.CompactionBudget = 1
		}
	}
}

