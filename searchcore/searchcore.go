// Package searchcore is the shared search contract used by every consumer
// of the docs-puller search pipeline.
//
// Status as of M7+1d: this package defines the shared interface, canonical
// child-process adapter, and importable in-process wrapper. The concrete
// docs-puller dispatch callbacks still live in cmd/docs-puller/, while
// package-independent dispatch planning and stage sequencing are being
// extracted into cmd/docs-puller/internal/searchdispatch. The production
// search-svc still shells out via ExecAdapter. Flipping the in-process path
// to default-on is gated on a real importable dispatch-engine boundary plus a
// baseline diff against cmd/docs-puller/eval/baseline-suite-2026-05-04.json
// (rule from docs/docs-puller/13-stripe-and-searchcore.md).
//
// Why this package lives here: cmd/docs-puller/control-plane/ is already
// established as the cross-module-importable home for shared docs-puller
// types (alongside `observe/` and `gen/`). Placing searchcore alongside
// them lets search-svc, an eventual MCP server, language-server consumers,
// and the console all import via the same `replace` pattern.
//
// Migration path:
//
//  1. M7 — types + Searcher interface only.
//  2. M8 — canonical ExecAdapter; search-svc aliases it instead of duplicating
//     exec/parsing logic.
//  3. M7+1 — introduce InProcessSearcher under this package and supply
//     cmd/docs-puller's dispatch function from a build-tagged bridge.
//  4. M7+2 — move the concrete dispatch caller boundary out of cmd/docs-puller's
//     package main into an importable package that search-svc can select,
//     keeping ExecAdapter as fallback.
//  5. M7+3 — flip the in-process path to default-on, run `make docs-eval`
//     with `--diff` against the frozen baseline. Per-source regressions >5pp
//     Hit@5 must be investigated before merging.
//  6. M7+4 — delete the duplicate code in cmd/docs-puller/.
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
// stays in cmd/docs-puller/svc/search/internal/handler/handler.go.
