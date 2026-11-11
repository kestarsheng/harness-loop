package rerank

import "context"

type Candidate struct {
	ChunkID      string
	DocumentID   string
	DocumentName string
	Content      string
	PageNo       int
	Section      string
	Score        float64
	RerankScore  float64
}

type Reranker interface {
	ID() string
	Rerank(ctx context.Context, query string, candidates []Candidate, topM int) ([]Candidate, error)
}

func top(candidates []Candidate, topM int) []Candidate {
	if topM <= 0 || topM > len(candidates) {
		topM = len(candidates)
	}
	out := make([]Candidate, topM)
	copy(out, candidates[:topM])
	return out
}
