package main

import (
	"time"

	"github.com/nstranquist/docs-puller/internal/providers"
	"github.com/nstranquist/docs-puller/searchruntime"
)

// === Corpus-agnostic ===
// Text Embeddings Inference (TEI) /rerank adapter. TEI is HuggingFace's
// official Rust HTTP server for serving rerankers locally — Apache 2.0,
// runs natively on Apple Silicon with Metal acceleration via the `metal`
// cargo feature. The model is selected at TEI startup, not per-request,
// so changing rerankers means restarting the TEI process.
//
// Why a separate file from rerank_cohere.go: the response shape differs
// (TEI returns a top-level JSON array; Cohere wraps in {results: [...]}),
// and TEI has no auth header. Mirroring the cohere file shape per the
// rule-of-three discipline — same pattern, separate file, dedup on the
// third consumer.

func callTEIForRerank(provider providers.Provider, model, query string, candidates []searchruntime.LLMRerankCandidate) ([]int, error) {
	return searchruntime.CallTEIRanker(searchruntime.TEIRankerInput{
		BaseURL:      provider.BaseURL,
		DefaultModel: provider.DefaultModel,
		Query:        query,
		Candidates:   candidates,
		UserAgent:    userAgent,
		HTTPClient:   httpClient,
		Sleep:        time.Sleep,
		RecordUsage: func(documentCount int) {
			// TEI is local + free — record the call count with zero cost so the
			// audit log preserves usage patterns without polluting cost totals.
			recordRerankUsage(provider, model, documentCount, 0)
		},
	})
}
