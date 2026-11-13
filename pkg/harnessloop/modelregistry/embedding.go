package modelregistry

import "fmt"

// CodeEmbeddingDimMismatch is returned when a vector written to the index has a
// dimension different from the embedding model declared in the registry.
const CodeEmbeddingDimMismatch = "E_EMBEDDING_DIM_MISMATCH"

// EmbeddingDimError describes a dimension mismatch between the model and the
// index (or an actual vector) that is about to be written.
type EmbeddingDimError struct {
	ModelID  string
	ModelDim int
	IndexDim int
	Reason   string
}

func (e *EmbeddingDimError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = "embedding_dim_mismatch"
	}
	return fmt.Sprintf("%s: model %q dim %d != index dim %d (%s)",
		CodeEmbeddingDimMismatch, e.ModelID, e.ModelDim, e.IndexDim, reason)
}

// ValidateEmbeddingWrite is the write-path guard: the target index dimension
// must match both the model's embedding_dim and the registry-level enforce_dim
// (when configured). Callers must run it before issuing any partial write.
func (r *Registry) ValidateEmbeddingWrite(modelID string, indexDim int) error {
	if r == nil {
		return fmt.Errorf("%w: registry is nil", ErrUnknownModel)
	}
	m, ok := r.Get(modelID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownModel, modelID)
	}
	if m.Kind != KindEmbedding {
		return &EmbeddingDimError{ModelID: modelID, ModelDim: m.EmbeddingDim, IndexDim: indexDim, Reason: "not_an_embedding_model"}
	}
	if m.EmbeddingDim != indexDim {
		return &EmbeddingDimError{ModelID: modelID, ModelDim: m.EmbeddingDim, IndexDim: indexDim, Reason: "model_dimension"}
	}
	if enforce := r.embedding.EnforceDim; enforce > 0 && enforce != indexDim {
		return &EmbeddingDimError{ModelID: modelID, ModelDim: m.EmbeddingDim, IndexDim: indexDim, Reason: "enforce_dim"}
	}
	return nil
}

// ValidateEmbeddingVector checks the concrete vector length before it is
// written. It lets the index writer reject a batch before any row is inserted,
// so a mismatch cannot leave a partially written vector column.
func (r *Registry) ValidateEmbeddingVector(modelID string, vector []float32) error {
	return r.ValidateEmbeddingWrite(modelID, len(vector))
}