package rerank

import "context"

type Noop struct{}

func (Noop) ID() string { return "noop" }

func (Noop) Rerank(ctx context.Context, query string, candidates []Candidate, topM int) ([]Candidate, error) {
	return top(candidates, topM), nil
}
