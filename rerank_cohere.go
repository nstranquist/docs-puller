package main

import (
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/nstranquist/docs-puller/internal/providers"
	"github.com/nstranquist/docs-puller/searchruntime"
)

// Cohere trial keys are throttled to 10 calls / 60s. Production keys allow
// ~1k/min. To respect both without bespoke trial-detection logic, we expose
// COHERE_MIN_INTERVAL_MS — a minimum delay between any two cohere calls,
// enforced globally (mutex-guarded so concurrent search requests serialize).
//
// Trial-key recipe: COHERE_MIN_INTERVAL_MS=6500 (=> ~9.2 calls/min, safely
// under the 10/min trial budget). Production: leave unset.
var (
	cohereLastCallMu sync.Mutex
	cohereLastCallAt time.Time
)

func cohereRespectMinInterval() {
	intervalMS, _ := strconv.Atoi(os.Getenv("COHERE_MIN_INTERVAL_MS"))
	if intervalMS <= 0 {
		return
	}
	cohereLastCallMu.Lock()
	defer cohereLastCallMu.Unlock()
	wait := time.Duration(intervalMS)*time.Millisecond - time.Since(cohereLastCallAt)
	if wait > 0 {
		time.Sleep(wait)
	}
	cohereLastCallAt = time.Now()
}

// cohereRetryAfter parses the Retry-After header (RFC 7231 — either an
// integer seconds value, or an HTTP-date). Returns 0 if missing/unparseable.
func cohereRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// === Corpus-agnostic ===
// Cohere v2 /rerank adapter. Mirrors the contract of callLLMForRerank in
// rerank_llm.go: takes a query + N candidate snippets, returns a 0-based
// permutation of the input indices best-to-worst. The shape is reusable
// across docs-vs-refs corpora; candidate to extract to internal/corpora/
// once a third consumer materializes (see corpus-core extraction strategy
// at docs/active/05-02-2244-corpus-core-extraction-strategy.md).
//
// Why a separate file from rerank_llm.go: cohere is a TRAINED reranker
// (not LLM-as-judge), and its request/response shape is fundamentally
// different — no chat messages, no JSON-mode prompt, no retry-and-parse
// loop. Mixing the shapes would muddy both code paths. Instead we keep
// the same retry/timeout discipline but speak cohere's protocol.

// callCohereForRerank sends candidates to Cohere v2 /rerank and returns
// the permuted index order. The candidates are sent as `documents` (a
// concatenation of title + snippet — title alone often lacks signal for
// near-duplicate docs).
func callCohereForRerank(provider providers.Provider, model, apiKey, query string, candidates []searchruntime.LLMRerankCandidate) ([]int, error) {
	return searchruntime.CallCohereRanker(searchruntime.CohereRankerInput{
		BaseURL:       provider.BaseURL,
		Model:         model,
		APIKey:        apiKey,
		Query:         query,
		Candidates:    candidates,
		UserAgent:     userAgent,
		HTTPClient:    httpClient,
		Sleep:         time.Sleep,
		BeforeAttempt: cohereRespectMinInterval,
		RetryAfter:    cohereRetryAfter,
		RecordUsage: func() {
			// Cohere bills per search-unit, not per token. Map one rerank call
			// to one "search unit" worth of input (≈1k tokens proxy) so the
			// recordAIUsage cost telemetry stays in the same ballpark as
			// chat-completions reranks. Output stays 0 (cohere doesn't bill output).
			recordRerankUsage(provider, model, 1000, 0)
		},
	})
}
