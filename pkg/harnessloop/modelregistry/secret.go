package modelregistry

type View struct {
	ID               string
	Provider         string
	Kind             string
	ContextWindow    int
	MaxOutputTokens  int
	ToolCall         bool
	StructuredOutput bool
	PriceInPer1K     float64
	PriceOutPer1K    float64
	Status           string
	Tags             []string
	EmbeddingDim     int
	HasCredential    bool
}

func (m *Model) View() View {
	tags := append([]string(nil), m.Tags...)
	return View{
		ID:               m.ID,
		Provider:         m.Provider,
		Kind:             m.Kind,
		ContextWindow:    m.ContextWindow,
		MaxOutputTokens:  m.MaxOutputTokens,
		ToolCall:         m.ToolCall,
		StructuredOutput: m.StructuredOutput,
		PriceInPer1K:     m.priceIn(),
		PriceOutPer1K:    m.priceOut(),
		Status:           m.Status,
		Tags:             tags,
		EmbeddingDim:     m.EmbeddingDim,
		HasCredential:    m.APIKeyRef != "",
	}
}

func (r *Registry) Views() []View {
	models := r.Available()
	out := make([]View, 0, len(models))
	for _, m := range models {
		out = append(out, m.View())
	}
	return out
}

func (r *Registry) ResolveCredential(id string) (string, bool) {
	m, ok := r.Get(id)
	if !ok || m.APIKeyRef == "" {
		return "", false
	}
	return m.APIKeyRef, true
}
