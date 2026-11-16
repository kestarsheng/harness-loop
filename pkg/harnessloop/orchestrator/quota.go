package orchestrator

import (
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// FairnessSettings is the WFQ block of policy/subtask.yaml (T-P2-09,
// design 4.13.8). It lives inside QuotaConfig so the existing loader picks it up
// without a second configuration file.
type FairnessSettings struct {
	// Weight is the default per-parent WFQ weight (subtask.fairness.weight).
	Weight float64 `yaml:"weight"`
	// ParentShare is the hard cap share of the tenant slot pool. Zero means
	// "dynamic": 1/N over the parents currently registered with the tenant.
	ParentShare float64 `yaml:"parent_share"`
	// TenantTokens is the tenant-wide token ceiling. Zero falls back to the
	// per-parent budget in ParentBudgetTokens.
	TenantTokens int64 `yaml:"tenant_tokens"`
	// MinWeight/MaxWeight bound what a tenant policy may set.
	MinWeight float64 `yaml:"min_weight"`
	MaxWeight float64 `yaml:"max_weight"`
}

// QuotaConfig holds the scheduler limits from DFX-P-11 / U13-A. Defaults are
// deliberately closed: raising them requires an explicit configuration change.
type QuotaConfig struct {
	MaxParallelPerParent int     `yaml:"max_parallel_per_parent"`
	MaxAgentsPerTenant   int     `yaml:"max_agents_per_tenant"`
	MaxDirectChildren    int     `yaml:"max_direct_children"`
	MaxDepth             int     `yaml:"max_depth"`
	TimeoutMinutes       int     `yaml:"timeout_minutes"`
	DefaultSubtaskTokens int64   `yaml:"default_subtask_tokens"`
	ParentBudgetTokens   int64   `yaml:"parent_budget_tokens"`
	CircuitFailureRatio  float64 `yaml:"circuit_failure_ratio"`
	CircuitMinSamples    int     `yaml:"circuit_min_samples"`
	// CircuitSampleWindow is the rolling window of terminal outcomes used by
	// the class breaker. Zero keeps every sample (design 4.13.3 default).
	CircuitSampleWindow int              `yaml:"circuit_sample_window"`
	RetryMaxAttempts    int              `yaml:"retry_max_attempts"`
	TolerateRatioMax    float64          `yaml:"tolerate_ratio_max"`
	MinSamplesForRatio  int              `yaml:"min_samples_for_ratio"`
	Fairness            FairnessSettings `yaml:"fairness"`
}

func DefaultQuotaConfig() QuotaConfig {
	return QuotaConfig{
		MaxParallelPerParent: 8,
		MaxAgentsPerTenant:   20,
		MaxDirectChildren:    20,
		MaxDepth:             2,
		TimeoutMinutes:       15,
		DefaultSubtaskTokens: 1024,
		ParentBudgetTokens:   1 << 20,
		CircuitFailureRatio:  0.5,
		CircuitMinSamples:    4,
		CircuitSampleWindow:  0,
		RetryMaxAttempts:     2,
		TolerateRatioMax:     0.5,
		MinSamplesForRatio:   5,
		Fairness: FairnessSettings{
			Weight:    1.0,
			MinWeight: 0.5,
			MaxWeight: 4.0,
		},
	}
}

// LoadQuotaConfig overlays YAML on the closed defaults.
func LoadQuotaConfig(path string) (QuotaConfig, error) {
	cfg := DefaultQuotaConfig()
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("orchestrator: read quota config: %w", err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("orchestrator: parse quota config: %w", err)
	}
	cfg.normalize()
	return cfg, nil
}

func (c *QuotaConfig) normalize() {
	def := DefaultQuotaConfig()
	if c.MaxParallelPerParent <= 0 {
		c.MaxParallelPerParent = def.MaxParallelPerParent
	}
	if c.MaxAgentsPerTenant <= 0 {
		c.MaxAgentsPerTenant = def.MaxAgentsPerTenant
	}
	if c.MaxDirectChildren <= 0 {
		c.MaxDirectChildren = def.MaxDirectChildren
	}
	if c.MaxDepth <= 0 || c.MaxDepth > 2 {
		c.MaxDepth = def.MaxDepth
	}
	if c.TimeoutMinutes <= 0 {
		c.TimeoutMinutes = def.TimeoutMinutes
	}
	if c.DefaultSubtaskTokens <= 0 {
		c.DefaultSubtaskTokens = def.DefaultSubtaskTokens
	}
	if c.ParentBudgetTokens <= 0 {
		c.ParentBudgetTokens = def.ParentBudgetTokens
	}
	if c.CircuitFailureRatio <= 0 || c.CircuitFailureRatio > 1 {
		c.CircuitFailureRatio = def.CircuitFailureRatio
	}
	if c.CircuitMinSamples <= 0 {
		c.CircuitMinSamples = def.CircuitMinSamples
	}
	if c.CircuitSampleWindow < 0 {
		c.CircuitSampleWindow = def.CircuitSampleWindow
	}
	if c.CircuitSampleWindow > 0 && c.CircuitSampleWindow < c.CircuitMinSamples {
		// A window smaller than the minimum sample count could never open the
		// breaker; refuse the combination instead of silently disabling it.
		c.CircuitSampleWindow = c.CircuitMinSamples
	}
	if c.RetryMaxAttempts <= 0 {
		c.RetryMaxAttempts = def.RetryMaxAttempts
	}
	if c.TolerateRatioMax <= 0 || c.TolerateRatioMax > 0.5 {
		c.TolerateRatioMax = def.TolerateRatioMax
	}
	if c.MinSamplesForRatio <= 0 {
		c.MinSamplesForRatio = def.MinSamplesForRatio
	}
	if c.Fairness.Weight <= 0 {
		c.Fairness.Weight = def.Fairness.Weight
	}
	if c.Fairness.MinWeight <= 0 {
		c.Fairness.MinWeight = def.Fairness.MinWeight
	}
	if c.Fairness.MaxWeight <= 0 || c.Fairness.MaxWeight < c.Fairness.MinWeight {
		c.Fairness.MaxWeight = def.Fairness.MaxWeight
	}
	if c.Fairness.Weight < c.Fairness.MinWeight {
		c.Fairness.Weight = c.Fairness.MinWeight
	}
	if c.Fairness.Weight > c.Fairness.MaxWeight {
		c.Fairness.Weight = c.Fairness.MaxWeight
	}
	if c.Fairness.ParentShare < 0 || c.Fairness.ParentShare > 1 {
		c.Fairness.ParentShare = 0
	}
	if c.Fairness.TenantTokens < 0 {
		c.Fairness.TenantTokens = 0
	}
}

// TenantGate is the tenant-wide concurrency ceiling shared by every parent
// scheduler. Exceeding it queues work; it never fails a subtask (REQ-128).
type TenantGate struct {
	mu     sync.Mutex
	max    int
	active int
}

func NewTenantGate(max int) *TenantGate {
	if max <= 0 {
		max = DefaultQuotaConfig().MaxAgentsPerTenant
	}
	return &TenantGate{max: max}
}

func (g *TenantGate) Max() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.max
}

func (g *TenantGate) Active() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

func (g *TenantGate) Available() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active < g.max
}

// TryAcquire reserves one tenant agent slot.
func (g *TenantGate) TryAcquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active >= g.max {
		return false
	}
	g.active++
	return true
}

func (g *TenantGate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active > 0 {
		g.active--
	}
}

// ParentQuota is the per-parent double quota: one agent slot plus the
// estimated token budget must be acquired together (design 4.13.3).
type ParentQuota struct {
	mu         sync.Mutex
	maxSlots   int
	budget     int64
	usedSlots  int
	usedTokens int64
}

func NewParentQuota(cfg QuotaConfig) *ParentQuota {
	return &ParentQuota{maxSlots: cfg.MaxParallelPerParent, budget: cfg.ParentBudgetTokens}
}

func (q *ParentQuota) UsedSlots() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.usedSlots
}

func (q *ParentQuota) RemainingTokens() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.budget - q.usedTokens
}

func (q *ParentQuota) CanAcquire(tokens int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.usedSlots >= q.maxSlots {
		return false
	}
	if tokens > 0 && q.usedTokens+tokens > q.budget {
		return false
	}
	return true
}

// Acquire takes one slot and the predicted tokens. Both are granted or neither.
func (q *ParentQuota) Acquire(tokens int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.usedSlots >= q.maxSlots {
		return false
	}
	if tokens > 0 && q.usedTokens+tokens > q.budget {
		return false
	}
	q.usedSlots++
	if tokens > 0 {
		q.usedTokens += tokens
	}
	return true
}

func (q *ParentQuota) Release(tokens int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.usedSlots > 0 {
		q.usedSlots--
	}
	if tokens > 0 {
		q.usedTokens -= tokens
		if q.usedTokens < 0 {
			q.usedTokens = 0
		}
	}
}

// Reclaim returns unused tokens to the parent pool (the parent remains the
// single account; subtasks never get a standalone budget).
func (q *ParentQuota) Reclaim(tokens int64) {
	if tokens <= 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.usedTokens -= tokens
	if q.usedTokens < 0 {
		q.usedTokens = 0
	}
}

// SplitBudget divides a parallel set budget evenly. The remainder is handed to
// the earliest nodes by stable index order.
func SplitBudget(total int64, nodeIDs []string) map[string]int64 {
	out := make(map[string]int64, len(nodeIDs))
	if total <= 0 || len(nodeIDs) == 0 {
		return out
	}
	share := total / int64(len(nodeIDs))
	remainder := total % int64(len(nodeIDs))
	for i, id := range nodeIDs {
		value := share
		if int64(i) < remainder {
			value++
		}
		out[id] = value
	}
	return out
}
