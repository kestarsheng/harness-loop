package modelregistry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	TriggerRateLimit      = "rate_limit"
	TriggerTimeout        = "timeout"
	TriggerErrorThreshold = "error_threshold"

	SwitchReasonUnavailable    = "unavailable"
	SwitchReasonRegistryChange = "registry_change"
	SwitchReasonManualUpgrade  = "manual_upgrade"

	CodeFallbackExhausted = "E_MODEL_FALLBACK_EXHAUSTED"
	CodeFallbackUnavailable = "E_MODEL_FALLBACK_UNAVAILABLE"
	CodeModelCallFailed   = "E_MODEL_CALL_FAILED"
	CodeBudgetExceeded    = "E_MODEL_BUDGET_EXCEEDED"
	CodeSwitchForbidden   = "E_MODEL_SWITCH_FORBIDDEN"
)

var (
	ErrFallbackExhausted = errors.New("modelregistry: fallback chain exhausted")
	ErrBudgetExceeded    = errors.New("modelregistry: model budget exceeded")
	ErrSwitchForbidden   = errors.New("modelregistry: runtime model switch is forbidden")
)

type FailureKind string

const (
	FailureRateLimit FailureKind = "rate_limit"
	FailureTimeout   FailureKind = "timeout"
	FailureError     FailureKind = "error"
	FailureQuota     FailureKind = "quota"
)

type FallbackError struct {
	Code    string
	Trigger string
	ModelID string
	Err     error
}

func (e *FallbackError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: model %s failed (%s): %v", e.Code, e.ModelID, e.Trigger, e.Err)
	}
	return fmt.Sprintf("%s: model %s failed (%s)", e.Code, e.ModelID, e.Trigger)
}

func (e *FallbackError) Unwrap() error { return e.Err }

type FallbackConfig struct {
	CandidatesPerStage    int
	MaxPerStage           int
	ConsecutiveTimeouts   int
	ConsecutiveErrors     int
	ProbeFailThreshold    int
	ProbeWindow           time.Duration
	CompactionRatio       float64
	MemoryInjectRatio     float64
	CompactionBudgetRatio float64
}

func DefaultFallbackConfig() FallbackConfig {
	return FallbackConfig{
		CandidatesPerStage:    2,
		MaxPerStage:           3,
		ConsecutiveTimeouts:   3,
		ConsecutiveErrors:     3,
		ProbeFailThreshold:    5,
		ProbeWindow:           time.Hour,
		CompactionRatio:       DefaultCompactionRatio,
		MemoryInjectRatio:     DefaultMemoryInjectRatio,
		CompactionBudgetRatio: DefaultCompactionBudgetRatio,
	}
}

func (c FallbackConfig) normalized() FallbackConfig {
	out := DefaultFallbackConfig()
	if c.CandidatesPerStage > 0 {
		out.CandidatesPerStage = c.CandidatesPerStage
	}
	if c.MaxPerStage > 0 {
		out.MaxPerStage = c.MaxPerStage
	}
	if c.ConsecutiveTimeouts > 0 {
		out.ConsecutiveTimeouts = c.ConsecutiveTimeouts
	}
	if c.ConsecutiveErrors > 0 {
		out.ConsecutiveErrors = c.ConsecutiveErrors
	}
	if c.ProbeFailThreshold > 0 {
		out.ProbeFailThreshold = c.ProbeFailThreshold
	}
	if c.ProbeWindow > 0 {
		out.ProbeWindow = c.ProbeWindow
	}
	if c.CompactionRatio > 0 && c.CompactionRatio < 1 {
		out.CompactionRatio = c.CompactionRatio
	}
	if c.MemoryInjectRatio > 0 && c.MemoryInjectRatio <= 1 {
		out.MemoryInjectRatio = c.MemoryInjectRatio
	}
	if c.CompactionBudgetRatio > 0 && c.CompactionBudgetRatio <= 1 {
		out.CompactionBudgetRatio = c.CompactionBudgetRatio
	}
	return out
}

type Budget struct {
	LimitTokens int64
	LimitCost   float64
	UsedTokens  int64
	UsedCost    float64
}

func (b *Budget) Charge(tokens int64, cost float64) bool {
	if b == nil {
		return false
	}
	b.UsedTokens += tokens
	b.UsedCost += cost
	return b.exceeded()
}

func (b *Budget) exceeded() bool {
	if b == nil {
		return false
	}
	if b.LimitTokens > 0 && b.UsedTokens > b.LimitTokens {
		return true
	}
	if b.LimitCost > 0 && b.UsedCost > b.LimitCost {
		return true
	}
	return false
}

func (b *Budget) Exhausted() bool {
	if b == nil {
		return false
	}
	if b.LimitTokens > 0 && b.UsedTokens >= b.LimitTokens {
		return true
	}
	if b.LimitCost > 0 && b.UsedCost >= b.LimitCost {
		return true
	}
	return false
}

type FallbackRequirement struct {
	RequireToolCall         bool
	RequireStructuredOutput bool
	RequiredContextWindow   int
	StageBudgetTokens       int
	Fallbacks               []string
}

type FallbackRequest struct {
	TaskID                string
	Stage                 string
	Primary               string
	Requirement           FallbackRequirement
	Budget                *Budget
	TaskBudgetTokens      int64
	EstimatedTokensIn     int64
	EstimatedTokensOut    int64
}

type CallOutcome struct {
	Kind      FailureKind
	Err       error
	TokensIn  int64
	TokensOut int64
	Latency   time.Duration
}

type FallbackCaller func(ctx context.Context, modelID string) CallOutcome

type FallbackAttempt struct {
	ModelID   string
	Trigger   string
	AttemptNo int
	TokensIn  int64
	TokensOut int64
	Cost      float64
	LatencyMS int64
	Success   bool
	Error     string
}

type FallbackResult struct {
	Primary            string
	Model              *Model
	Switched           bool
	Trigger            string
	AttemptNo          int
	StageFallbacks     int
	Attempts           []FallbackAttempt
	Exhausted          bool
	BudgetExceeded     bool
	Window             int
	ThresholdTokens    int
	MemoryInjectBudget int
	CompactionBudget   int64
	EstimatedCost      float64
	Events             []ModelEvent
	Err                error
}

type probeState struct {
	consecutiveFailures int
	consecutiveTimeouts int
	consecutiveErrors   int
	lastFailure         time.Time
	unavailable         bool
}

type FallbackChain struct {
	reg  *Registry
	cfg  FallbackConfig
	now  func() time.Time

	mu     sync.Mutex
	probes map[string]*probeState
	stages map[string]int
}

func NewFallbackChain(reg *Registry, cfg FallbackConfig) *FallbackChain {
	if reg == nil {
		reg = DefaultRegistry()
	}
	return &FallbackChain{
		reg:    reg,
		cfg:    cfg.normalized(),
		now:    time.Now,
		probes: map[string]*probeState{},
		stages: map[string]int{},
	}
}

func (c *FallbackChain) Config() FallbackConfig { return c.cfg }

func (c *FallbackChain) Registry() *Registry { return c.reg }

func (c *FallbackChain) StageFallbacks(stage string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stages[stage]
}

func (c *FallbackChain) ResetStage(stage string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.stages, stage)
}

func (c *FallbackChain) UnavailableModels() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]bool, len(c.probes))
	for id, p := range c.probes {
		if p.unavailable {
			out[id] = true
		}
	}
	return out
}

func (c *FallbackChain) IsUnavailable(modelID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.probes[modelID]
	return p != nil && p.unavailable
}

func (c *FallbackChain) ObserveFailure(modelID string, kind FailureKind) []ModelEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.observeFailureLocked(modelID, kind)
}

func (c *FallbackChain) observeFailureLocked(modelID string, kind FailureKind) []ModelEvent {
	now := c.now()
	p := c.probes[modelID]
	if p == nil {
		p = &probeState{}
		c.probes[modelID] = p
	}
	if !p.lastFailure.IsZero() && now.Sub(p.lastFailure) > c.cfg.ProbeWindow {
		p.consecutiveFailures = 0
		p.consecutiveTimeouts = 0
		p.consecutiveErrors = 0
	}
	p.lastFailure = now
	p.consecutiveFailures++
	switch kind {
	case FailureTimeout:
		p.consecutiveTimeouts++
	case FailureError:
		p.consecutiveErrors++
	}
	var events []ModelEvent
	if !p.unavailable && p.consecutiveFailures >= c.cfg.ProbeFailThreshold {
		p.unavailable = true
		events = append(events, ModelEvent{
			Type: EventModelUnavailable,
			Payload: map[string]any{
				"model_id":     modelID,
				"fail_count":   p.consecutiveFailures,
				"probe_window": c.cfg.ProbeWindow.String(),
			},
		})
	}
	return events
}

func (c *FallbackChain) ObserveSuccess(modelID string) []ModelEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.observeSuccessLocked(modelID)
}

func (c *FallbackChain) observeSuccessLocked(modelID string) []ModelEvent {
	p := c.probes[modelID]
	if p == nil {
		return nil
	}
	wasUnavailable := p.unavailable
	failCount := p.consecutiveFailures
	p.consecutiveFailures = 0
	p.consecutiveTimeouts = 0
	p.consecutiveErrors = 0
	p.unavailable = false
	if wasUnavailable {
		return []ModelEvent{{
			Type: EventModelRecovered,
			Payload: map[string]any{
				"model_id":     modelID,
				"fail_count":   failCount,
				"probe_window": c.cfg.ProbeWindow.String(),
			},
		}}
	}
	return nil
}

func (c *FallbackChain) Probe(modelID string, ok bool) []ModelEvent {
	if ok {
		return c.ObserveSuccess(modelID)
	}
	return c.ObserveFailure(modelID, FailureError)
}

func (c *FallbackChain) ShouldFallback(modelID string, kind FailureKind) (string, bool) {
	if kind == FailureQuota {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.probes[modelID]
	switch kind {
	case FailureRateLimit:
		return TriggerRateLimit, true
	case FailureTimeout:
		return TriggerTimeout, p != nil && p.consecutiveTimeouts >= c.cfg.ConsecutiveTimeouts
	case FailureError:
		return TriggerErrorThreshold, p != nil && p.consecutiveErrors >= c.cfg.ConsecutiveErrors
	}
	return "", false
}

func (c *FallbackChain) Candidates(primary, stage string, req FallbackRequirement) []*Model {
	ids := req.Fallbacks
	if len(ids) == 0 {
		ids = c.reg.DefaultFallbacks()
	}
	if len(ids) == 0 {
		ids = c.reg.IDs()
	}
	c.mu.Lock()
	unavailable := make(map[string]bool, len(c.probes))
	for id, p := range c.probes {
		if p.unavailable {
			unavailable[id] = true
		}
	}
	c.mu.Unlock()

	out := make([]*Model, 0, c.cfg.CandidatesPerStage)
	for _, id := range ids {
		if id == primary || unavailable[id] {
			continue
		}
		m, ok := c.reg.Get(id)
		if !ok || m.Kind != KindChat || m.Status != StatusAvailable {
			continue
		}
		if req.RequireToolCall && !m.ToolCall {
			continue
		}
		if req.RequireStructuredOutput && !m.StructuredOutput {
			continue
		}
		if req.RequiredContextWindow > 0 && m.ContextWindow < req.RequiredContextWindow {
			continue
		}
		out = append(out, m)
		if len(out) >= c.cfg.CandidatesPerStage {
			break
		}
	}
	return out
}

func (c *FallbackChain) Execute(ctx context.Context, req FallbackRequest, call FallbackCaller) (*FallbackResult, error) {
	if call == nil {
		return nil, fmt.Errorf("modelregistry: fallback caller is required")
	}
	primary := req.Primary
	if primary == "" {
		primary = c.reg.DefaultTaskModel()
	}
	if primary == "" {
		return nil, fmt.Errorf("modelregistry: primary model is required")
	}
	result := &FallbackResult{Primary: primary, StageFallbacks: c.StageFallbacks(req.Stage)}

	if req.Budget != nil && req.Budget.Exhausted() {
		result.BudgetExceeded = true
		result.Err = c.budgetError()
		return result, result.Err
	}

	primaryModel, _ := c.reg.Get(primary)
	outcome, attempt := c.invoke(ctx, primaryModel, primary, call, 0, "")
	result.Attempts = append(result.Attempts, attempt)
	if outcome.Err == nil {
		result.Events = append(result.Events, c.ObserveSuccess(primary)...)
		overBudget := c.charge(req.Budget, primaryModel, outcome)
		c.applySuccess(result, primaryModel, req, overBudget)
		result.StageFallbacks = c.StageFallbacks(req.Stage)
		return result, result.Err
	}
	result.Events = append(result.Events, c.ObserveFailure(primary, kindOf(outcome))...)

	trigger, should := c.ShouldFallback(primary, kindOf(outcome))
	if c.charge(req.Budget, primaryModel, outcome) {
		result.BudgetExceeded = true
		result.Err = c.budgetError()
		return result, result.Err
	}
	if !should {
		result.Err = &FallbackError{Code: CodeModelCallFailed, Trigger: triggerFor(kindOf(outcome)), ModelID: primary, Err: outcome.Err}
		return result, result.Err
	}
	if c.StageFallbacks(req.Stage) >= c.cfg.MaxPerStage {
		result.Exhausted = true
		result.Err = &FallbackError{Code: CodeFallbackExhausted, Trigger: trigger, ModelID: primary, Err: ErrFallbackExhausted}
		return result, result.Err
	}

	candidates := c.Candidates(primary, req.Stage, req.Requirement)
	if len(candidates) == 0 {
		result.Exhausted = true
		result.Err = &FallbackError{Code: CodeFallbackUnavailable, Trigger: trigger, ModelID: primary}
		return result, result.Err
	}

	fallbackNo := 0
	for _, candidate := range candidates {
		if c.StageFallbacks(req.Stage) >= c.cfg.MaxPerStage {
			break
		}
		if req.Budget != nil && req.Budget.Exhausted() {
			result.BudgetExceeded = true
			result.Err = c.budgetError()
			return result, result.Err
		}
		fallbackNo++
		c.incStage(req.Stage)
		outcome, attempt := c.invoke(ctx, candidate, candidate.ID, call, fallbackNo, trigger)
		result.Attempts = append(result.Attempts, attempt)
		overBudget := c.charge(req.Budget, candidate, outcome)
		if outcome.Err == nil {
			result.Events = append(result.Events, c.ObserveSuccess(candidate.ID)...)
			result.Switched = true
			result.Trigger = trigger
			result.AttemptNo = fallbackNo
			result.Events = append(result.Events, ModelEvent{
				Type: EventModelSwitched,
				Payload: map[string]any{
					"from":       primary,
					"to":         candidate.ID,
					"reason":     SwitchReasonUnavailable,
					"trigger":    trigger,
					"attempt_no": fallbackNo,
				},
			})
			result.Events = append(result.Events, ModelEvent{
				Type: EventModelFallback,
				Payload: map[string]any{
					"primary":    primary,
					"fallback":   candidate.ID,
					"trigger":    trigger,
					"latency_ms": attempt.LatencyMS,
					"tokens":     attempt.TokensIn + attempt.TokensOut,
					"cost":       attempt.Cost,
					"attempt_no": fallbackNo,
				},
			})
			c.applySuccess(result, candidate, req, overBudget)
			result.StageFallbacks = c.StageFallbacks(req.Stage)
			return result, result.Err
		}
		result.Events = append(result.Events, c.ObserveFailure(candidate.ID, kindOf(outcome))...)
		if overBudget {
			result.BudgetExceeded = true
			result.Err = c.budgetError()
			return result, result.Err
		}
	}

	result.Exhausted = true
	result.StageFallbacks = c.StageFallbacks(req.Stage)
	result.Err = &FallbackError{Code: CodeFallbackExhausted, Trigger: trigger, ModelID: primary, Err: ErrFallbackExhausted}
	return result, result.Err
}

func (c *FallbackChain) invoke(ctx context.Context, model *Model, modelID string, call FallbackCaller, attemptNo int, trigger string) (CallOutcome, FallbackAttempt) {
	outcome := call(ctx, modelID)
	attempt := FallbackAttempt{
		ModelID:   modelID,
		Trigger:   trigger,
		AttemptNo: attemptNo,
		TokensIn:  outcome.TokensIn,
		TokensOut: outcome.TokensOut,
		LatencyMS: outcome.Latency.Milliseconds(),
		Success:   outcome.Err == nil,
	}
	if model != nil {
		attempt.Cost = estimateCost(model, outcome.TokensIn, outcome.TokensOut)
	}
	if outcome.Err != nil {
		attempt.Error = outcome.Err.Error()
	}
	return outcome, attempt
}

func (c *FallbackChain) charge(budget *Budget, model *Model, outcome CallOutcome) bool {
	if budget == nil {
		return false
	}
	cost := 0.0
	if model != nil {
		cost = estimateCost(model, outcome.TokensIn, outcome.TokensOut)
	}
	return budget.Charge(outcome.TokensIn+outcome.TokensOut, cost)
}

func (c *FallbackChain) budgetError() error {
	return &FallbackError{Code: CodeBudgetExceeded, Trigger: SwitchReasonUnavailable, Err: ErrBudgetExceeded}
}

func (c *FallbackChain) incStage(stage string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stages[stage]++
}

func (c *FallbackChain) applySuccess(result *FallbackResult, model *Model, req FallbackRequest, overBudget bool) {
	if model == nil {
		return
	}
	recompute := Recompute(RecomputeRequest{
		Model:                 model,
		TaskBudgetTokens:      req.TaskBudgetTokens,
		EstimatedTokensIn:     req.EstimatedTokensIn,
		EstimatedTokensOut:    req.EstimatedTokensOut,
		CompactionRatio:       c.cfg.CompactionRatio,
		MemoryInjectRatio:     c.cfg.MemoryInjectRatio,
		CompactionBudgetRatio: c.cfg.CompactionBudgetRatio,
		Reason:                SwitchReasonUnavailable,
	})
	result.Model = model
	result.Window = recompute.Window
	result.ThresholdTokens = recompute.ThresholdTokens
	result.MemoryInjectBudget = recompute.MemoryInjectBudget
	result.CompactionBudget = recompute.CompactionBudget
	result.EstimatedCost = recompute.EstimatedCost
	if result.Switched {
		result.Events = append(result.Events, recompute.Event)
	}
	if overBudget {
		result.BudgetExceeded = true
		result.Err = c.budgetError()
	}
}

func kindOf(outcome CallOutcome) FailureKind {
	if outcome.Err == nil {
		return ""
	}
	if outcome.Kind != "" {
		return outcome.Kind
	}
	return FailureError
}

func triggerFor(kind FailureKind) string {
	switch kind {
	case FailureRateLimit:
		return TriggerRateLimit
	case FailureTimeout:
		return TriggerTimeout
	case FailureQuota:
		return "quota"
	default:
		return TriggerErrorThreshold
	}
}

type SwitchCheck struct {
	From       string
	To         string
	Reason     string
	Operator   string
	ApprovalID string
	Approved   bool
}

func ValidateSwitch(check SwitchCheck) error {
	if check.From == "" || check.To == "" {
		return &FallbackError{Code: CodeSwitchForbidden, Err: ErrSwitchForbidden}
	}
	switch check.Reason {
	case SwitchReasonUnavailable, SwitchReasonRegistryChange:
		return nil
	case SwitchReasonManualUpgrade:
		if !check.Approved || check.ApprovalID == "" {
			return &FallbackError{Code: CodeSwitchForbidden, ModelID: check.To, Trigger: check.Reason, Err: ErrSwitchForbidden}
		}
		return nil
	default:
		return &FallbackError{Code: CodeSwitchForbidden, ModelID: check.To, Trigger: check.Reason, Err: ErrSwitchForbidden}
	}
}

type RecomputeRequest struct {
	Model                 *Model
	TaskBudgetTokens      int64
	EstimatedTokensIn     int64
	EstimatedTokensOut    int64
	CompactionRatio       float64
	MemoryInjectRatio     float64
	CompactionBudgetRatio float64
	Reason                string
}

type WindowBudget struct {
	ModelID            string
	Window             int
	ThresholdTokens    int
	MemoryInjectBudget int
	CompactionBudget   int64
	EstimatedCost      float64
	Event              ModelEvent
}

func Recompute(req RecomputeRequest) WindowBudget {
	if req.Model == nil {
		return WindowBudget{Event: ModelEvent{Type: EventModelConfigApplied, Payload: map[string]any{"reason": req.Reason}}}
	}
	ratio := req.CompactionRatio
	if ratio <= 0 || ratio >= 1 {
		ratio = DefaultCompactionRatio
	}
	injectRatio := req.MemoryInjectRatio
	if injectRatio <= 0 || injectRatio > 1 {
		injectRatio = DefaultMemoryInjectRatio
	}
	budgetRatio := req.CompactionBudgetRatio
	if budgetRatio <= 0 || budgetRatio > 1 {
		budgetRatio = DefaultCompactionBudgetRatio
	}

	out := WindowBudget{Window: req.Model.ContextWindow, ModelID: req.Model.ID}
	out.ThresholdTokens = int(float64(out.Window) * ratio)
	out.MemoryInjectBudget = int(float64(out.ThresholdTokens) * injectRatio)
	if req.TaskBudgetTokens > 0 {
		out.CompactionBudget = int64(float64(req.TaskBudgetTokens) * budgetRatio)
		if out.CompactionBudget < 1 {
			out.CompactionBudget = 1
		}
	}
	out.EstimatedCost = estimateCost(req.Model, req.EstimatedTokensIn, req.EstimatedTokensOut)
	out.Event = ModelEvent{
		Type: EventModelConfigApplied,
		Payload: map[string]any{
			"model_id":              req.Model.ID,
			"window":                out.Window,
			"threshold_tokens":      out.ThresholdTokens,
			"memory_inject_budget":  out.MemoryInjectBudget,
			"compaction_budget":     out.CompactionBudget,
			"estimated_cost":        out.EstimatedCost,
			"reason":                req.Reason,
		},
	}
	return out
}