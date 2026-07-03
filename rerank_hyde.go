package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/nstranquist/docs-puller/internal/providers"
	"github.com/nstranquist/docs-puller/searchruntime"
)

type hydeChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type hydeChatReq struct {
	Model       string        `json:"model"`
	Messages    []hydeChatMsg `json:"messages"`
	Temperature float64       `json:"temperature"`
}

// === Corpus-agnostic ===
// HyDE (Hypothetical Document Embedding) query rewriting. Classic IR
// technique from Gao et al. 2022 (https://arxiv.org/abs/2212.10496).
//
// The shape is: rather than embedding the raw query for first-stage
// retrieval, ask an LLM to generate a hypothetical canonical answer
// passage, and embed THAT. The hypothetical passage is in the same
// "shape" as real documentation — full sentences, proper terminology,
// likely keywords — so its embedding lives in the same neighborhood as
// the actual canonical doc. Cosine similarity then surfaces the real
// canonical more reliably than embedding the literal query, which
// often lacks the vocabulary the canonical actually uses.
//
// Activated by --rerank-hyde (off by default). Composes with hybrid
// first-stage retrieval: when both are on, the embedding side of the
// BM25 ∪ embedding-cosine union uses the HyDE doc's vector instead of
// the raw query's vector.
//
// Cost: 1 extra LLM call per uncached query (~$0.0001 at gpt-4.1-mini).
// Latency: +500-1000ms per query before the embedding step.

// hydePrompt builds the system+user prompt that asks the LLM to imagine
// the canonical doc passage. We bias toward terse, factual style so the
// hypothetical sits in the right embedding neighborhood.
func hydePrompt(query string) string {
	return searchruntime.BuildHyDEPrompt(query)
}

// callLLMForHyDE asks the configured rerank LLM (gpt-4.1-mini by default,
// or whatever the user has set via --rerank-llm-model) to generate the
// hypothetical doc. Reuses the chat-completions wiring rather than spinning
// up a separate provider — keeps surface area minimal and the same auth
// path as rerank.
func callLLMForHyDE(query, model, apiKey string) (string, error) {
	body, err := json.Marshal(hydeChatReq{
		Model: model,
		Messages: []hydeChatMsg{
			{Role: "user", Content: hydePrompt(query)},
		},
		Temperature: 0,
	})
	if err != nil {
		return "", err
	}
	openai, ok := providers.Find("openai")
	if !ok {
		return "", searchruntime.HyDEProviderNotRegisteredError()
	}
	req, err := http.NewRequest(http.MethodPost, openai.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", userAgent)

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(searchruntime.HyDERetryBackoff(attempt))
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if searchruntime.HyDERetryableStatus(resp.StatusCode) {
			lastErr = searchruntime.HyDEOpenAIStatusError(resp.StatusCode, string(respBody), 200)
			continue
		}
		if resp.StatusCode != 200 {
			return "", searchruntime.HyDEOpenAIStatusError(resp.StatusCode, string(respBody), 500)
		}
		parsed, err := searchruntime.ParseHyDEChatResponse(respBody)
		if err != nil {
			return "", err
		}
		if parsed.ProviderError != "" {
			return "", searchruntime.HyDEProviderError(parsed.ProviderError)
		}
		hyde, ok := searchruntime.NormalizeHyDEDocument(parsed.Document)
		if !ok {
			return "", searchruntime.HyDEEmptyResponseError()
		}
		if parsed.Usage.Present {
			// Cost telemetry: tag this call distinctly from rerank so audit
			// shows HyDE separately from its sibling rerank calls. Same
			// pricing path as rerank since the model is the same.
			event := searchruntime.BuildHyDEUsageEvent(model, parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens)
			recordAIUsage(event.ProviderName, event.Model, event.CallSite,
				event.InputTokens, event.OutputTokens, event.Cents)
		}
		return hyde, nil
	}
	return "", lastErr
}

// generateHyDEDoc is the entry point used by applyHybridRetrieval when
// --rerank-hyde is set. Resolves the chat model + key from opts (mirroring
// the rerank-llm provider lookup), generates the hypothetical, and
// returns the string. On any error returns the original query as a
// graceful fallback so retrieval doesn't fail closed.
func generateHyDEDoc(query string, o searchOpts) (string, bool) {
	apiKey := os.Getenv(searchruntime.DefaultHyDEAPIKeyEnv)
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, searchruntime.HyDEMissingAPIKeyWarning(searchruntime.DefaultHyDEAPIKeyEnv))
		return query, false
	}
	model := searchruntime.ResolveHyDEModel(o.rerankLLMModel, searchruntime.DefaultLLMRerankModel)
	hyde, err := callLLMForHyDE(query, model, apiKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, searchruntime.HyDEGenerationFallbackWarning(err))
		return query, false
	}
	return hyde, true
}
