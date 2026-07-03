package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/nstranquist/docs-puller/internal/providers"
	"github.com/nstranquist/docs-puller/searchruntime"
)

// TestCallTEIForRerankParsesArrayResponse stands up a fake TEI-shaped HTTP
// server that returns a top-level JSON array (TEI's actual response shape,
// distinct from cohere's wrapped {results: []}). Asserts the returned []int
// order matches the response.
func TestCallTEIForRerankParsesArrayResponse(t *testing.T) {
	var gotReq struct {
		Query     string   `json:"query"`
		Texts     []string `json:"texts"`
		RawScores bool     `json:"raw_scores,omitempty"`
		Truncate  bool     `json:"truncate,omitempty"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/rerank") {
			t.Errorf("expected /rerank path, got %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("TEI runs on localhost; Authorization header should be unset, got %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		// Top-level array, NOT wrapped — this is the shape difference from cohere.
		if err := json.NewEncoder(w).Encode([]struct {
			Index int     `json:"index"`
			Score float64 `json:"score"`
		}{
			{Index: 1, Score: 0.91},
			{Index: 3, Score: 0.74},
			{Index: 0, Score: 0.55},
			{Index: 2, Score: 0.22},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	provider := providers.Provider{
		Name:         "tei",
		BaseURL:      server.URL,
		KeyEnv:       "",
		DefaultModel: "BAAI/bge-reranker-v2-m3",
		Pricing: map[string]providers.ModelPricing{
			"BAAI/bge-reranker-v2-m3": {InputPer1M: 0, OutputPer1M: 0},
		},
	}

	candidates := []searchruntime.LLMRerankCandidate{
		{Index: 0, Path: "a.md", Title: "alpha", Snippet: "first"},
		{Index: 1, Path: "b.md", Title: "bravo", Snippet: "second"},
		{Index: 2, Path: "c.md", Title: "charlie", Snippet: "third"},
		{Index: 3, Path: "d.md", Title: "delta", Snippet: "fourth"},
	}

	order, err := callTEIForRerank(provider, "BAAI/bge-reranker-v2-m3", "test query", candidates)
	if err != nil {
		t.Fatalf("callTEIForRerank failed: %v", err)
	}

	want := []int{1, 3, 0, 2}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}

	if gotReq.Query != "test query" {
		t.Errorf("request query = %q, want %q", gotReq.Query, "test query")
	}
	if len(gotReq.Texts) != 4 {
		t.Errorf("request texts len = %d, want 4", len(gotReq.Texts))
	}
	if !gotReq.Truncate {
		t.Errorf("request truncate = false, want true (safety against over-long docs)")
	}
	// Title + snippet shape (matches cohere adapter for apples-to-apples comparisons)
	if !strings.HasPrefix(gotReq.Texts[1], "bravo") || !strings.Contains(gotReq.Texts[1], "second") {
		t.Errorf("texts[1] = %q, want title+snippet shape", gotReq.Texts[1])
	}
}

// TestCallTEIForRerankReportsActionableErrorWhenServerDown checks that
// connection-refused errors include a copy-paste startup command, since
// "TEI not running" is the most common user mistake.
func TestCallTEIForRerankReportsActionableErrorWhenServerDown(t *testing.T) {
	provider := providers.Provider{
		Name:         "tei",
		BaseURL:      "http://127.0.0.1:1", // port 1 — guaranteed-refused
		KeyEnv:       "",
		DefaultModel: "BAAI/bge-reranker-v2-m3",
		Pricing: map[string]providers.ModelPricing{
			"BAAI/bge-reranker-v2-m3": {InputPer1M: 0, OutputPer1M: 0},
		},
	}
	candidates := []searchruntime.LLMRerankCandidate{{Index: 0, Path: "a.md", Title: "alpha"}}

	_, err := callTEIForRerank(provider, "BAAI/bge-reranker-v2-m3", "q", candidates)
	if err == nil {
		t.Fatal("expected error when TEI not reachable")
	}
	if !strings.Contains(err.Error(), "text-embeddings-router") {
		t.Errorf("error message should include startup command; got: %v", err)
	}
	if !strings.Contains(err.Error(), "BAAI/bge-reranker-v2-m3") {
		t.Errorf("error message should suggest the default model; got: %v", err)
	}
}

// TestResolveRerankProviderAcceptsTEI confirms the registry lookup permits
// the tei provider through the OpenAICompat / cohere / tei gate.
func TestResolveRerankProviderAcceptsTEI(t *testing.T) {
	p, model, err := resolveRerankProviderAndModel("tei", "BAAI/bge-reranker-v2-m3")
	if err != nil {
		t.Fatalf("resolveRerankProvider(tei) failed: %v", err)
	}
	if p.Name != "tei" {
		t.Errorf("provider name = %q, want tei", p.Name)
	}
	if model != "BAAI/bge-reranker-v2-m3" {
		t.Errorf("model = %q, want BAAI/bge-reranker-v2-m3", model)
	}
	if p.OpenAICompat {
		t.Errorf("tei should NOT be OpenAICompat (has its own /rerank endpoint)")
	}
	if p.KeyEnv != "" {
		t.Errorf("tei runs on localhost, should have empty KeyEnv; got %q", p.KeyEnv)
	}
}
