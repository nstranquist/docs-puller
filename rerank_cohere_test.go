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

// TestCallCohereForRerankParsesOrder stands up a fake cohere-shaped HTTP
// server that returns a known permutation, and asserts the returned []int
// order matches the response.results[].index sequence.
func TestCallCohereForRerankParsesOrder(t *testing.T) {
	var gotReq struct {
		Model     string   `json:"model"`
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
		TopN      int      `json:"top_n,omitempty"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/rerank") {
			t.Errorf("expected /rerank path, got %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fake-key" {
			t.Errorf("missing or wrong Authorization header: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		// Return the candidates re-ordered: input was [0,1,2,3], we say best→worst is [2,0,3,1].
		if err := json.NewEncoder(w).Encode(struct {
			Results []struct {
				Index          int     `json:"index"`
				RelevanceScore float64 `json:"relevance_score"`
			} `json:"results"`
		}{
			Results: []struct {
				Index          int     `json:"index"`
				RelevanceScore float64 `json:"relevance_score"`
			}{
				{Index: 2, RelevanceScore: 0.95},
				{Index: 0, RelevanceScore: 0.81},
				{Index: 3, RelevanceScore: 0.42},
				{Index: 1, RelevanceScore: 0.10},
			},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	provider := providers.Provider{
		Name:    "cohere",
		BaseURL: server.URL,
		KeyEnv:  "COHERE_API_KEY",
		Pricing: map[string]providers.ModelPricing{
			"rerank-3.5": {InputPer1M: 2.00, OutputPer1M: 0},
		},
	}

	candidates := []searchruntime.LLMRerankCandidate{
		{Index: 0, Path: "a.md", Title: "alpha", Snippet: "a doc"},
		{Index: 1, Path: "b.md", Title: "bravo", Snippet: "b doc"},
		{Index: 2, Path: "c.md", Title: "charlie", Snippet: "c doc"},
		{Index: 3, Path: "d.md", Title: "delta", Snippet: "d doc"},
	}

	order, err := callCohereForRerank(provider, "rerank-3.5", "fake-key", "test query", candidates)
	if err != nil {
		t.Fatalf("callCohereForRerank failed: %v", err)
	}

	want := []int{2, 0, 3, 1}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}

	// Verify the request was shaped correctly.
	if gotReq.Model != "rerank-3.5" {
		t.Errorf("request model = %q, want rerank-3.5", gotReq.Model)
	}
	if gotReq.Query != "test query" {
		t.Errorf("request query = %q, want %q", gotReq.Query, "test query")
	}
	if len(gotReq.Documents) != 4 {
		t.Errorf("request documents len = %d, want 4", len(gotReq.Documents))
	}
	if gotReq.TopN != 4 {
		t.Errorf("request top_n = %d, want 4 (full permutation)", gotReq.TopN)
	}
	// Title + snippet concatenation
	if !strings.HasPrefix(gotReq.Documents[0], "alpha") || !strings.Contains(gotReq.Documents[0], "a doc") {
		t.Errorf("documents[0] = %q, want title+snippet shape", gotReq.Documents[0])
	}
}

// TestResolveRerankProviderAcceptsCohere checks that the registry lookup
// allows the cohere provider through the OpenAICompat gate.
func TestResolveRerankProviderAcceptsCohere(t *testing.T) {
	p, model, err := resolveRerankProviderAndModel("cohere", "rerank-v3.5")
	if err != nil {
		t.Fatalf("resolveRerankProvider(cohere) failed: %v", err)
	}
	if p.Name != "cohere" {
		t.Errorf("provider name = %q, want cohere", p.Name)
	}
	if model != "rerank-v3.5" {
		t.Errorf("model = %q, want rerank-v3.5", model)
	}
	if p.OpenAICompat {
		t.Errorf("cohere should NOT be OpenAICompat (it has its own /rerank endpoint)")
	}
}
