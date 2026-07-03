package main

import (
	"os"
	"time"

	"github.com/nstranquist/docs-puller/internal/providers"
	"github.com/nstranquist/docs-puller/searchruntime"
)

// === Corpus-agnostic ===
// This file operates on []searchHit (path/title/score) and reorders by
// LLM-as-judge cross-encoder scoring. The candidate-snippet builder reads
// raw markdown from disk; nothing in here depends on the docs-vs-refs
// distinction. Reranker logic, prompt template, retry loop, and the
// reorder-by-LLM-output algorithm are all generic. Candidate for
// extraction to internal/corpora/rerank/llm/ once a second consumer
// (e.g. cmd/refs-puller/) needs the same shape. Until then: copy on
// Phase 2, extract on Phase 3 per the rule-of-three.
// See docs/active/05-02-2244-corpus-core-extraction-strategy.md.

// LLM-as-judge reranker. Sends BM25 top-K candidates (title + brief body
// snippet) to a chat-completions model and asks it to rank them by
// relevance to the query. The model returns a JSON list of candidate
// indices in best-to-worst order; we re-sort hits by that ordering.
//
// This is the cross-encoder pattern via LLM. It's the architectural shape
// most likely to lift Hit@1 on identifier queries (where embedding-cosine
// rerank failed) because the model sees the query and the candidate
// together and reasons about whether THIS doc is the right answer for
// THIS query — not just whether they're thematically similar.
//
// Cost: ~$0.001/query at gpt-4.1-mini, ~$0.17 to eval the full 168-query
// fixture. Latency: p50 ~660ms, p99 ~1450ms per rerank call (vs 200ms BM25
// alone). Default flipped from gpt-4o-mini to gpt-4.1-mini on 2026-05-04
// after a 2-run head-to-head: identical Hit@N + MRR at the same per-token
// price, with -18% p50 and -40% p99. See FOLLOW-UPS.md "Default flip
// (2026-05-04)" for the full bake-off table.

// resolveRerankProviderAndModel looks up the provider in the vendored
// providers registry, then delegates supported-provider and
// model policy to searchruntime.
func resolveRerankProviderAndModel(name, model string) (providers.Provider, string, error) {
	resolution, err := searchruntime.ResolveRerankProviderPolicy(searchruntime.RerankProviderPolicyInput{
		RequestedProvider: name,
		DefaultProvider:   searchruntime.DefaultRerankProvider,
		RequestedModel:    model,
		DefaultModel:      searchruntime.DefaultLLMRerankModel,
		Providers:         rerankProviderPolicyProviders(),
	})
	if err != nil {
		return providers.Provider{}, "", err
	}
	p, ok := providers.Find(resolution.ProviderName)
	if !ok {
		return providers.Provider{}, "", searchruntime.RerankProviderRegistryDisappearedError(resolution.ProviderName)
	}
	return p, resolution.Model, nil
}

func rerankProviderPolicyProviders() []searchruntime.RerankProviderPolicyProvider {
	registry := providers.Registry()
	out := make([]searchruntime.RerankProviderPolicyProvider, len(registry))
	for i, provider := range registry {
		models := make([]string, 0, len(provider.Pricing))
		for model := range provider.Pricing {
			models = append(models, model)
		}
		out[i] = searchruntime.NewRerankProviderPolicyProvider(provider.Name, provider.OpenAICompat, models)
	}
	return out
}

func applyLLMRerank(query string, hits []searchHit, o searchOpts) ([]searchHit, error) {
	provider, model, err := resolveRerankProviderAndModel(o.rerankLLMProvider, o.rerankLLMModel)
	if err != nil {
		return nil, err
	}
	apiKey, err := searchruntime.ResolveProviderAPIKey(searchruntime.ProviderAPIKeyInput{
		KeyEnv: provider.KeyEnv,
		// env, then optional external secrets CLI — composed at the call site so
		// ResolveProviderAPIKey stays hermetic.
		Getenv: func(name string) string {
			if v := os.Getenv(name); v != "" {
				return v
			}
			return searchruntime.SecretViaNdev(name)
		},
	})
	if err != nil {
		return nil, err
	}

	return searchruntime.RunLLMRerank(searchruntime.LLMRerankRunInput{
		Query:        query,
		Hits:         hits,
		OutDir:       o.out,
		ProviderName: provider.Name,
		Chat: func(prompt string) ([]int, error) {
			return callLLMForRerank(provider, model, apiKey, prompt)
		},
		Cohere: func(candidates []searchruntime.LLMRerankCandidate) ([]int, error) {
			return callCohereForRerank(provider, model, apiKey, query, candidates)
		},
		TEI: func(candidates []searchruntime.LLMRerankCandidate) ([]int, error) {
			return callTEIForRerank(provider, model, query, candidates)
		},
	})
}

func callLLMForRerank(provider providers.Provider, model, apiKey, prompt string) ([]int, error) {
	return searchruntime.CallChatCompletionRanker(searchruntime.ChatCompletionRankerInput{
		ProviderName: provider.Name,
		BaseURL:      provider.BaseURL,
		Model:        model,
		APIKey:       apiKey,
		Prompt:       prompt,
		UserAgent:    userAgent,
		HTTPClient:   httpClient,
		Sleep:        time.Sleep,
		RecordUsage: func(usage searchruntime.LLMRankUsage) {
			recordRerankUsage(provider, model, usage.PromptTokens, usage.CompletionTokens)
		},
	})
}

// recordRerankUsage delegates to recordAIUsage with cents computed by the
// central registry's per-model pricing table. Sub-cent calls are preserved
// (float64 cents).
func recordRerankUsage(provider providers.Provider, model string, inputTokens, outputTokens int) {
	pricing := provider.Pricing[model]
	searchruntime.EmitRerankUsageEvent(searchruntime.RerankUsageEventInput{
		ProviderName:    provider.Name,
		Model:           model,
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		ProviderPricing: searchruntime.NewRerankUsagePricing(pricing.InputPer1M, pricing.OutputPer1M),
	}, func(event searchruntime.RerankUsageEvent) {
		recordAIUsage(event.ProviderName, event.Model, event.CallSite, event.InputTokens, event.OutputTokens, event.Cents)
	})
}
