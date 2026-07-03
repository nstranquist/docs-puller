package searchcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ExecAdapter satisfies Searcher by shelling out to `docs-puller search
// --json`. Search-svc aliases this type so future consumers can import the
// canonical adapter without depending on search-svc internals.
//
// Latency profile: ~150-300ms per query (process startup dominates). Use
// InProcessSearcher (M7+1) when latency matters.
type ExecAdapter struct {
	Binary string
}

// NewExecAdapter is the canonical constructor. Empty binary defaults to
// the docs-puller binary on PATH.
func NewExecAdapter(binary string) *ExecAdapter {
	if strings.TrimSpace(binary) == "" {
		binary = "docs-puller"
	}
	return &ExecAdapter{Binary: binary}
}

// Search implements Searcher.
func (a *ExecAdapter) Search(ctx context.Context, q Query) ([]Hit, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, fmt.Errorf("searchcore: query text is required")
	}
	args := []string{"search", "--json", q.Text}
	if q.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(q.Limit))
	}
	if q.Source != "" {
		args = append(args, "--source", q.Source)
	}
	if q.Rerank {
		args = append(args, "--rerank-llm")
	}
	cmd := exec.CommandContext(ctx, a.Binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docs-puller search: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseHits(stdout.Bytes())
}

// parseHits accepts both shapes the docs-puller search verb has emitted:
// a top-level array OR a wrapper object with a "results"/"hits" field.
func parseHits(b []byte) ([]Hit, error) {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var hits []Hit
		if err := json.Unmarshal(trimmed, &hits); err != nil {
			return nil, err
		}
		return hits, nil
	}
	var wrapper struct {
		Results []Hit `json:"results"`
		Hits    []Hit `json:"hits"`
	}
	if err := json.Unmarshal(trimmed, &wrapper); err != nil {
		return nil, err
	}
	if len(wrapper.Results) > 0 {
		return wrapper.Results, nil
	}
	return wrapper.Hits, nil
}

// StubSearcher is a deterministic Searcher for tests in any module that
// imports searchcore. Returns its Hits unchanged on every call.
type StubSearcher struct {
	Hits []Hit
	Err  error
	Last Query
}

// Search implements Searcher.
func (s *StubSearcher) Search(_ context.Context, q Query) ([]Hit, error) {
	s.Last = q
	return s.Hits, s.Err
}
