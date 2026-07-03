package searchcore

import (
	"context"
	"fmt"
	"strings"
)

// DispatchFunc is the direct search implementation supplied by an owning
// binary or package. It lets searchcore expose an in-process Searcher without
// importing cmd/docs-puller's package main.
type DispatchFunc func(context.Context, Query) ([]Hit, error)

// InProcessSearcher satisfies Searcher by calling a provided in-process
// dispatcher. The docs-puller CLI currently supplies this through a tagged
// bridge while the M7+1d dispatch extraction is still under eval gate.
type InProcessSearcher struct {
	Dispatch DispatchFunc
}

// NewInProcessSearcher returns a Searcher backed by dispatch.
func NewInProcessSearcher(dispatch DispatchFunc) *InProcessSearcher {
	return &InProcessSearcher{Dispatch: dispatch}
}

// Search implements Searcher.
func (s *InProcessSearcher) Search(ctx context.Context, q Query) ([]Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(q.Text) == "" {
		return nil, fmt.Errorf("searchcore: query text is required")
	}
	if s.Dispatch == nil {
		return nil, fmt.Errorf("searchcore: in-process dispatcher is required")
	}
	hits, err := s.Dispatch(ctx, q)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}
