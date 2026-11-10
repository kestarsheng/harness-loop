package modelregistry

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	CodeMetaIncomplete     = "E_MODEL_META_INCOMPLETE"
	CodeCapabilityMismatch = "E_MODEL_CAPABILITY_MISMATCH"
	CodeDuplicateID        = "E_MODEL_DUPLICATE_ID"
	CodeUnknownModel       = "E_MODEL_UNKNOWN_MODEL"
	CodeUnsupportedStage   = "E_MODEL_UNSUPPORTED_STAGE"
)

var ErrMetaIncomplete = errors.New("model registry: model metadata is incomplete")
var ErrCapabilityMismatch = errors.New("model registry: no model satisfies the capability requirements")
var ErrUnknownModel = errors.New("model registry: unknown model")

const (
	KindChat      = "chat"
	KindEmbedding = "embedding"

	StatusAvailable = "available"
	StatusDisabled  = "disabled"

	StrategyCostFirst    = "cost_first"
	StrategyQualityFirst = "quality_first"
	StrategyBalanced     = "balanced"
)

type Model struct {
	ID               string   `yaml:"id" json:"id"`
	Provider         string   `yaml:"provider" json:"provider"`
	Kind             string   `yaml:"kind" json:"kind"`
	ContextWindow    int      `yaml:"context_window" json:"context_window"`
	MaxOutputTokens  int      `yaml:"max_output_tokens" json:"max_output_tokens"`
	ToolCall         bool     `yaml:"tool_call" json:"tool_call"`
	StructuredOutput bool     `yaml:"structured_output" json:"structured_output"`
	PriceInPer1K     *float64 `yaml:"price_in_per_1k" json:"price_in_per_1k"`
	PriceOutPer1K    *float64 `yaml:"price_out_per_1k" json:"price_out_per_1k"`
	Status           string   `yaml:"status" json:"status"`
	Tags             []string `yaml:"tags" json:"tags,omitempty"`
	EmbeddingDim     int      `yaml:"embedding_dim" json:"embedding_dim,omitempty"`
	APIKeyRef        string   `yaml:"api_key_ref" json:"-"`
}

func (m *Model) Validate() error {
	var missing []string
	if m.ID == "" {
		missing = append(missing, "id")
	}
	if m.Provider == "" {
		missing = append(missing, "provider")
	}
	if m.Kind == "" {
		missing = append(missing, "kind")
	}
	if m.ContextWindow <= 0 {
		missing = append(missing, "context_window")
	}
	if m.MaxOutputTokens <= 0 {
		missing = append(missing, "max_output_tokens")
	}
	if m.PriceInPer1K == nil || *m.PriceInPer1K < 0 {
		missing = append(missing, "price_in_per_1k")
	}
	if m.PriceOutPer1K == nil || *m.PriceOutPer1K < 0 {
		missing = append(missing, "price_out_per_1k")
	}
	if len(missing) > 0 {
		return &MetaError{Code: CodeMetaIncomplete, ModelID: m.ID, Fields: missing}
	}
	switch m.Kind {
	case KindChat, KindEmbedding:
	default:
		return &MetaError{Code: CodeMetaIncomplete, ModelID: m.ID, Fields: []string{"kind"}}
	}
	if m.MaxOutputTokens > m.ContextWindow {
		return &MetaError{Code: CodeMetaIncomplete, ModelID: m.ID, Fields: []string{"max_output_tokens > context_window"}}
	}
	if m.Kind == KindEmbedding {
		if m.EmbeddingDim <= 0 {
			return &MetaError{Code: CodeMetaIncomplete, ModelID: m.ID, Fields: []string{"embedding_dim"}}
		}
		if m.ToolCall {
			return &MetaError{Code: CodeMetaIncomplete, ModelID: m.ID, Fields: []string{"tool_call must be false for embedding models"}}
		}
	} else if m.EmbeddingDim != 0 {
		return &MetaError{Code: CodeMetaIncomplete, ModelID: m.ID, Fields: []string{"embedding_dim must be empty for chat models"}}
	}
	switch m.Status {
	case StatusAvailable, StatusDisabled, "":
	default:
		return &MetaError{Code: CodeMetaIncomplete, ModelID: m.ID, Fields: []string{"status"}}
	}
	return nil
}

type MetaError struct {
	Code    string
	ModelID string
	Fields  []string
}

func (e *MetaError) Error() string {
	return fmt.Sprintf("%s: model %q is missing/invalid fields: %s", e.Code, e.ModelID, strings.Join(e.Fields, ", "))
}

type CapabilityError struct {
	Suggested []string
	Reason    string
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("%s: %s (suggested: %s)", CodeCapabilityMismatch, e.Reason, strings.Join(e.Suggested, ", "))
}

type fileFormat struct {
	Version int     `yaml:"version"`
	Models  []Model `yaml:"models"`
}

type Registry struct {
	Version int
	models  map[string]*Model
	order   []string
}

//go:embed seed_models.yaml
var seedYAML []byte

func DefaultRegistry() *Registry {
	r, err := loadBytes(seedYAML)
	if err != nil {
		return &Registry{Version: 1, models: map[string]*Model{}}
	}
	return r
}

func Load(path string) (*Registry, error) {
	if path == "" {
		return DefaultRegistry(), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return loadBytes(raw)
}

func loadBytes(raw []byte) (*Registry, error) {
	var f fileFormat
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	r := &Registry{Version: f.Version, models: make(map[string]*Model, len(f.Models))}
	if r.Version <= 0 {
		r.Version = 1
	}
	for i := range f.Models {
		m := f.Models[i]
		if err := m.Validate(); err != nil {
			return nil, err
		}
		if _, exists := r.models[m.ID]; exists {
			return nil, fmt.Errorf("%s: duplicate model id %q", CodeDuplicateID, m.ID)
		}
		if m.Status == "" {
			m.Status = StatusAvailable
		}
		cp := m
		r.models[m.ID] = &cp
		r.order = append(r.order, m.ID)
	}
	if len(r.models) == 0 {
		return nil, fmt.Errorf("%w: registry is empty", ErrMetaIncomplete)
	}
	return r, nil
}

func (r *Registry) Get(id string) (*Model, bool) {
	m, ok := r.models[id]
	return m, ok
}

func (r *Registry) Available() []*Model {
	out := make([]*Model, 0, len(r.order))
	for _, id := range r.order {
		if m := r.models[id]; m != nil && m.Status == StatusAvailable {
			out = append(out, m)
		}
	}
	return out
}

func (r *Registry) EmbeddingModel() (*Model, bool) {
	for _, m := range r.Available() {
		if m.Kind == KindEmbedding {
			return m, true
		}
	}
	return nil, false
}

func (r *Registry) ContextWindowOf(id string) (int, error) {
	m, ok := r.Get(id)
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrUnknownModel, id)
	}
	return m.ContextWindow, nil
}

func (r *Registry) IDs() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

func sortByStrategy(models []*Model, strategy string) {
	sort.SliceStable(models, func(i, j int) bool {
		a, b := models[i], models[j]
		switch strategy {
		case StrategyCostFirst:
			return a.priceIn() < b.priceIn()
		case StrategyQualityFirst:
			as, bs := hasTag(a, "strong"), hasTag(b, "strong")
			if as != bs {
				return as
			}
			return a.ContextWindow > b.ContextWindow
		default:
			return a.priceIn() < b.priceIn()
		}
	})
}

func (m *Model) priceIn() float64 {
	if m.PriceInPer1K == nil {
		return 0
	}
	return *m.PriceInPer1K
}

func (m *Model) priceOut() float64 {
	if m.PriceOutPer1K == nil {
		return 0
	}
	return *m.PriceOutPer1K
}

func hasTag(m *Model, tag string) bool {
	for _, t := range m.Tags {
		if t == tag {
			return true
		}
	}
	return false
}
