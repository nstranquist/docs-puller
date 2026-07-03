// Package providers is a vendored subset of the docs-puller AI provider registry
// (rerank/embedding pricing + model metadata). docs-puller uses it for LLM
// rerank cost telemetry and provider resolution without importing a host CLI.
package providers

import (
	"sort"
	"strings"
)

// Provider describes one AI provider plus its default models and
// pricing. KeyEnv is the env var the call sites read; an empty KeyEnv
// means "no key required" (Ollama, local inference).
type Provider struct {
	Name         string                  `json:"name"`
	DisplayName  string                  `json:"display_name"`
	KeyEnv       string                  `json:"key_env,omitempty"`
	BaseURL      string                  `json:"base_url"`
	OpenAICompat bool                    `json:"openai_compat"`
	DefaultModel string                  `json:"default_model"`
	Models       []string                `json:"models"`
	Pricing      map[string]ModelPricing `json:"pricing"`
	DocsURL      string                  `json:"docs_url,omitempty"`
}

// ModelPricing is per-1M tokens (USD). Storing per-1M instead of per-1K
// keeps the source readable for current model card pricing and matches
// the units used by the rerank_llm pricing table; conversion to cents
// happens inside EstimateCents.
type ModelPricing struct {
	InputPer1M  float64 `json:"input_per_1m"`
	OutputPer1M float64 `json:"output_per_1m"`
}

// EstimateCents returns a fractional USD-cents estimate for the given
// token usage. Returns 0 when pricing is unknown — that is signal, not
// an error: the usage event still records the token counts, the audit
// just won't have a dollar figure for that model.
//
// The return is float64 so sub-cent calls (e.g. one rerank query at
// gpt-4o-mini ~ 0.17¢) don't collapse to zero in per-event rounding.
// Float64 has 15+ significant digits — drift over millions of events
// is microscopic for accounting purposes.
func (p ModelPricing) EstimateCents(inputTokens, outputTokens int) float64 {
	usd := float64(inputTokens)/1_000_000*p.InputPer1M +
		float64(outputTokens)/1_000_000*p.OutputPer1M
	cents := usd * 100.0
	if cents < 0 {
		cents = 0
	}
	return cents
}

// builtInProviders is the curated set we ship today. Adding a new
// provider here is a one-liner; tests assert the table is sorted
// and self-consistent.
var builtInProviders = []Provider{
	{
		Name:         "openai",
		DisplayName:  "OpenAI",
		KeyEnv:       "OPENAI_API_KEY",
		BaseURL:      "https://api.openai.com/v1",
		OpenAICompat: true,
		DefaultModel: "gpt-4o-mini",
		Models:       []string{"gpt-4o", "gpt-4o-mini", "gpt-4.1-mini", "gpt-4.1-nano", "o1", "o3-mini", "text-embedding-3-small", "text-embedding-3-large"},
		Pricing: map[string]ModelPricing{
			"gpt-4o":                 {InputPer1M: 2.50, OutputPer1M: 10.00},
			"gpt-4o-mini":            {InputPer1M: 0.15, OutputPer1M: 0.60},
			"gpt-4.1-mini":           {InputPer1M: 0.15, OutputPer1M: 0.60},
			"gpt-4.1-nano":           {InputPer1M: 0.10, OutputPer1M: 0.40},
			"o1":                     {InputPer1M: 15.00, OutputPer1M: 60.00},
			"o3-mini":                {InputPer1M: 1.10, OutputPer1M: 4.40},
			"text-embedding-3-small": {InputPer1M: 0.02, OutputPer1M: 0},
			"text-embedding-3-large": {InputPer1M: 0.13, OutputPer1M: 0},
		},
		DocsURL: "https://platform.openai.com/docs",
	},
	{
		Name:         "anthropic",
		DisplayName:  "Anthropic",
		KeyEnv:       "ANTHROPIC_API_KEY",
		BaseURL:      "https://api.anthropic.com/v1",
		OpenAICompat: false,
		DefaultModel: "claude-3-5-sonnet-20241022",
		Models:       []string{"claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022", "claude-3-opus-20240229", "claude-3-7-sonnet-20250219"},
		Pricing: map[string]ModelPricing{
			"claude-3-5-sonnet-20241022": {InputPer1M: 3.00, OutputPer1M: 15.00},
			"claude-3-5-haiku-20241022":  {InputPer1M: 0.80, OutputPer1M: 4.00},
			"claude-3-opus-20240229":     {InputPer1M: 15.00, OutputPer1M: 75.00},
			"claude-3-7-sonnet-20250219": {InputPer1M: 3.00, OutputPer1M: 15.00},
		},
		DocsURL: "https://docs.anthropic.com",
	},
	{
		// claude-code is a CLI-backed, subscription-auth provider — NOT an HTTP
		// API. Generation shells out to the local `claude -p` binary (see
		// internal/inference/generator/claudecode.go), which authenticates via
		// the user's Claude subscription. KeyEnv is empty (no API key, like
		// ollama); BaseURL is a non-HTTP sentinel for display only. Pricing is
		// omitted because the subscription is flat-rate, not per-token.
		Name:         "claude-code",
		DisplayName:  "Claude Code (subscription CLI)",
		KeyEnv:       "",
		BaseURL:      "cli://claude -p",
		OpenAICompat: false,
		DefaultModel: "sonnet",
		Models:       []string{"haiku", "sonnet", "opus"},
		DocsURL:      "https://docs.claude.com/en/docs/claude-code",
	},
	{
		Name:         "xai",
		DisplayName:  "xAI (Grok)",
		KeyEnv:       "XAI_API_KEY",
		BaseURL:      "https://api.x.ai/v1",
		OpenAICompat: true,
		DefaultModel: "grok-3",
		Models: []string{
			"grok-4.20-0309-non-reasoning",
			"grok-4-1-fast-non-reasoning",
			"grok-4-fast-non-reasoning",
			"grok-4.3",
			"grok-3",
			"grok-3-mini",
			"grok-2-1212",
			"grok-2-vision-1212",
		},
		// Pricing pulled live from xAI /v1/language-models on 2026-05-04.
		// Source uses USD cents per 100M tokens; $/1M = price / 10000.
		Pricing: map[string]ModelPricing{
			"grok-4.20-0309-non-reasoning": {InputPer1M: 1.25, OutputPer1M: 2.50},
			"grok-4-1-fast-non-reasoning":  {InputPer1M: 0.20, OutputPer1M: 0.50},
			"grok-4-fast-non-reasoning":    {InputPer1M: 0.20, OutputPer1M: 0.50},
			"grok-4.3":                     {InputPer1M: 1.25, OutputPer1M: 2.50},
			"grok-3":                       {InputPer1M: 3.00, OutputPer1M: 15.00},
			"grok-3-mini":                  {InputPer1M: 0.30, OutputPer1M: 0.50},
			"grok-2-1212":                  {InputPer1M: 2.00, OutputPer1M: 10.00},
			"grok-2-vision-1212":           {InputPer1M: 2.00, OutputPer1M: 10.00},
		},
		DocsURL: "https://docs.x.ai",
	},
	{
		Name:         "ollama",
		DisplayName:  "Ollama (local)",
		KeyEnv:       "",
		BaseURL:      "http://localhost:11434",
		OpenAICompat: false,
		DefaultModel: "llama3",
		Models:       []string{"llama3", "qwen3-json", "mistral", "gemma2"},
		Pricing:      map[string]ModelPricing{},
		DocsURL:      "https://github.com/ollama/ollama/blob/main/docs/api.md",
	},
	{
		Name:         "opencode-zen",
		DisplayName:  "OpenCode Zen",
		KeyEnv:       "OPENCODE_ZEN_API_KEY",
		BaseURL:      "https://api.opencode.zen/v1",
		OpenAICompat: true,
		DefaultModel: "",
		Models:       []string{},
		Pricing:      map[string]ModelPricing{},
	},
	{
		Name:         "groq",
		DisplayName:  "Groq",
		KeyEnv:       "GROQ_API_KEY",
		BaseURL:      "https://api.groq.com/openai/v1",
		OpenAICompat: true,
		DefaultModel: "llama-3.3-70b-versatile",
		Models:       []string{"llama-3.3-70b-versatile", "llama-3.1-8b-instant", "mixtral-8x7b-32768"},
		Pricing: map[string]ModelPricing{
			"llama-3.3-70b-versatile": {InputPer1M: 0.59, OutputPer1M: 0.79},
			"llama-3.1-8b-instant":    {InputPer1M: 0.05, OutputPer1M: 0.08},
		},
		DocsURL: "https://console.groq.com/docs",
	},
	{
		Name:         "openrouter",
		DisplayName:  "OpenRouter",
		KeyEnv:       "OPENROUTER_API_KEY",
		BaseURL:      "https://openrouter.ai/api/v1",
		OpenAICompat: true,
		DefaultModel: "",
		Models:       []string{},
		Pricing:      map[string]ModelPricing{},
		DocsURL:      "https://openrouter.ai/docs",
	},
	{
		Name:         "gemini",
		DisplayName:  "Google Gemini",
		KeyEnv:       "GEMINI_API_KEY",
		BaseURL:      "https://generativelanguage.googleapis.com/v1beta/openai",
		OpenAICompat: true,
		// Default stays gemini-3.1-flash-lite: it is the newest AND cheapest tier
		// ($0.25/$1.50 per 1M). The newer non-lite Flash models are registered for
		// opt-in (--model) but cost materially more — gemini-3.5-flash is ~6x
		// ($1.50/$9), gemini-3-flash-preview ~2x ($0.50/$3). Use those only when
		// flash-lite quality is insufficient for a given run.
		DefaultModel: "gemini-3.1-flash-lite",
		Models: []string{
			"gemini-3.1-flash-lite",
			"gemini-3-flash-preview",
			"gemini-3.5-flash",
			"gemini-2.5-flash-lite",
			"gemini-2.5-flash",
			"gemini-2.5-pro",
			"gemini-2.0-flash",
			"gemini-2.0-flash-lite",
		},
		// Pricing per Google AI Studio public pricing pages (3.x verified
		// 2026-06-02 via ai.google.dev + provider listings). Verify via
		// https://ai.google.dev/gemini-api/docs/pricing if rates drift.
		Pricing: map[string]ModelPricing{
			"gemini-3.5-flash":       {InputPer1M: 1.50, OutputPer1M: 9.00},
			"gemini-3-flash-preview": {InputPer1M: 0.50, OutputPer1M: 3.00},
			"gemini-3.1-flash-lite":  {InputPer1M: 0.25, OutputPer1M: 1.50},
			"gemini-2.5-flash-lite":  {InputPer1M: 0.10, OutputPer1M: 0.40},
			"gemini-2.5-flash":       {InputPer1M: 0.30, OutputPer1M: 2.50},
			"gemini-2.5-pro":         {InputPer1M: 1.25, OutputPer1M: 10.00},
			"gemini-2.0-flash":       {InputPer1M: 0.10, OutputPer1M: 0.40},
			"gemini-2.0-flash-lite":  {InputPer1M: 0.075, OutputPer1M: 0.30},
		},
		DocsURL: "https://ai.google.dev/gemini-api/docs/openai",
	},
	{
		Name:         "tei",
		DisplayName:  "Text Embeddings Inference (local)",
		KeyEnv:       "", // localhost — no auth
		BaseURL:      "http://localhost:8080",
		OpenAICompat: false, // bespoke /rerank endpoint; response shape is a top-level array
		DefaultModel: "BAAI/bge-reranker-v2-m3",
		// Models supported by TEI's /rerank endpoint. The model is selected
		// at TEI server start time (`text-embeddings-router --model-id ...`),
		// not per-request — so the registry's "model" here is really a label
		// used for cost telemetry. The user is responsible for matching the
		// running TEI process to the model name they pass to docs-puller.
		Models: []string{
			"BAAI/bge-reranker-v2-m3",                   // 568M params, ~600 MB disk, ~1-2 GB RAM, SOTA-class
			"BAAI/bge-reranker-large",                   // 560M params, English-only
			"mixedbread-ai/mxbai-rerank-large-v2",       // 1.5B params, ~3 GB disk
			"jinaai/jina-reranker-v2-base-multilingual", // 278M params, ~300 MB
			"cross-encoder/ms-marco-MiniLM-L-6-v2",      // 22M params, ~25 MB, very fast
		},
		// All open-weights models are free at runtime — no per-token cost.
		// Cost telemetry stays at zero for tei calls; the audit log just
		// records the call count for usage-pattern visibility.
		Pricing: map[string]ModelPricing{
			"BAAI/bge-reranker-v2-m3":                   {InputPer1M: 0, OutputPer1M: 0},
			"BAAI/bge-reranker-large":                   {InputPer1M: 0, OutputPer1M: 0},
			"mixedbread-ai/mxbai-rerank-large-v2":       {InputPer1M: 0, OutputPer1M: 0},
			"jinaai/jina-reranker-v2-base-multilingual": {InputPer1M: 0, OutputPer1M: 0},
			"cross-encoder/ms-marco-MiniLM-L-6-v2":      {InputPer1M: 0, OutputPer1M: 0},
		},
		DocsURL: "https://github.com/huggingface/text-embeddings-inference",
	},
	{
		Name:         "cohere",
		DisplayName:  "Cohere (rerank-only)",
		KeyEnv:       "COHERE_API_KEY",
		BaseURL:      "https://api.cohere.com/v2",
		OpenAICompat: false, // bespoke /rerank endpoint, not /chat/completions
		DefaultModel: "rerank-v3.5",
		// Model IDs verified via GET /v1/models?endpoint=rerank on 2026-05-04.
		Models: []string{
			"rerank-v4.0-pro",          // newest flagship (2025)
			"rerank-v4.0-fast",         // newest fast variant (2025)
			"rerank-v3.5",              // previous flagship; default endpoint
			"rerank-english-v3.0",      // older English-only
			"rerank-multilingual-v3.0", // older multilingual
		},
		// Cohere prices per 1k search-units. One search-unit ≈ one query
		// against ≤100 documents (we send ~10). Stored as $/1M "input tokens"
		// so recordAIUsage cost telemetry stays comparable to chat-completions
		// reranks: $2.00/1k searches → recorded as $2.00/1M with a 1k-token
		// proxy per call. Output stays 0 (cohere doesn't bill output).
		// Public pricing page: https://cohere.com/pricing
		Pricing: map[string]ModelPricing{
			"rerank-v4.0-pro":          {InputPer1M: 1.50, OutputPer1M: 0}, // $1.50 / 1k searches
			"rerank-v4.0-fast":         {InputPer1M: 0.05, OutputPer1M: 0}, // $0.05 / 1k searches — 30× cheaper
			"rerank-v3.5":              {InputPer1M: 2.00, OutputPer1M: 0}, // $2.00 / 1k searches
			"rerank-english-v3.0":      {InputPer1M: 2.00, OutputPer1M: 0},
			"rerank-multilingual-v3.0": {InputPer1M: 2.00, OutputPer1M: 0},
		},
		DocsURL: "https://docs.cohere.com/v2/docs/rerank-overview",
	},
}

// Registry returns the built-in providers, sorted by Name for stable
// CLI output. Callers are free to mutate the returned slice; each call
// allocates a fresh copy.
func Registry() []Provider {
	out := make([]Provider, len(builtInProviders))
	copy(out, builtInProviders)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Find returns the provider with the given name, or false if unknown.
func Find(name string) (Provider, bool) {
	for _, p := range builtInProviders {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// FindByKeyEnv returns the provider that owns a given env-var key, e.g.
// FindByKeyEnv("OPENAI_API_KEY") → openai. Empty/unknown returns false.
// Used by call-site bookkeeping that only knows the env var.
func FindByKeyEnv(env string) (Provider, bool) {
	if env == "" {
		return Provider{}, false
	}
	for _, p := range builtInProviders {
		if p.KeyEnv == env {
			return p, true
		}
	}
	return Provider{}, false
}

// PricingFor returns the pricing entry for (provider, model). Falls
// back to a zero-pricing struct so estimation always returns 0 cents
// for unknown models (the usage event still records token counts).
//
// When the exact model key is missing, falls back to the LONGEST
// matching prefix in the pricing map. Providers commonly return dated
// model variants in API responses (`gpt-4o-mini-2024-07-18`,
// `claude-3-5-sonnet-20241022`) while the registry keys the base name
// — prefix matching keeps the estimates accurate without requiring an
// alias-table refresh on every model snapshot.
func PricingFor(providerName, model string) ModelPricing {
	p, ok := Find(providerName)
	if !ok {
		return ModelPricing{}
	}
	if mp, ok := p.Pricing[model]; ok {
		return mp
	}
	// Longest-prefix fallback: pick the most-specific key that the
	// model string starts with.
	bestKey := ""
	for k := range p.Pricing {
		if k == "" || !strings.HasPrefix(model, k) {
			continue
		}
		if len(k) > len(bestKey) {
			bestKey = k
		}
	}
	if bestKey != "" {
		return p.Pricing[bestKey]
	}
	return ModelPricing{}
}
