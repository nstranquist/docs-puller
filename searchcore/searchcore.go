// Package searchcore is the shared search contract used by every consumer
// of the docs-puller search pipeline.
//
// It defines the stable public interface, canonical child-process adapter, and
// importable in-process wrapper used by downstream services. Concrete CLI
// callbacks remain in the root package; package-independent dispatch planning
// lives under internal/searchdispatch. Retrieval changes should be checked
// against the frozen public baselines under eval/ before release.
package searchcore

import "context"

// Hit mirrors the JSON shape emitted by `docs-puller search --json`. It is
// the lingua franca for any consumer of the search pipeline.
type Hit struct {
	Path         string    `json:"path"`
	Source       string    `json:"source"`
	SourceFamily string    `json:"source_family,omitempty"`
	SourceID     string    `json:"source_id,omitempty"`
	DocsVersion  string    `json:"docs_version,omitempty"`
	Title        string    `json:"title,omitempty"`
	URL          string    `json:"url,omitempty"`
	Score        int       `json:"score"`
	Snippets     []Snippet `json:"snippets,omitempty"`
}

// Snippet is a single excerpt from a doc.
type Snippet struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Query is the request shape. CorpusID is reserved for the multi-tenant
// future (M7+); current callers pass an empty string and the underlying
// pipeline reads from the operator's local docs cache.
type Query struct {
	CorpusID string
	Text     string
	Limit    int
	Source   string
	Rerank   bool
}

// Searcher is the canonical contract. Implementations:
//   - ExecAdapter (this package, M8): shells out to `docs-puller search`.
//   - InProcessSearcher (this package, M7+1c): direct dispatcher wrapper.
//   - StubSearcher (test only): canned results for handler unit tests.
type Searcher interface {
	Search(ctx context.Context, q Query) ([]Hit, error)
}

// HitsToProto and friends will live in this package once the gen pb types
// can import searchcore without an import cycle. For now, the conversion
// stays in the downstream hosted search service.
