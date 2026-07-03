package main

import (
	"strings"
	"testing"

	"github.com/nstranquist/docs-puller/searchruntime"
)

// TestHydePromptShape checks the prompt asks for a passage in the right
// shape: ~150 words, no preamble, plain text, includes the query.
func TestHydePromptShape(t *testing.T) {
	prompt := hydePrompt("how do I configure row level security in supabase")
	checks := []string{
		"row level security", // includes the query verbatim
		"hypothetical",       // signals the task
		"Output ONLY",        // anti-preamble guard
		"150 words",          // length anchor
		"Match the tone of authoritative technical reference", // style anchor
	}
	for _, c := range checks {
		if !strings.Contains(prompt, c) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", c, prompt)
		}
	}
	if strings.Contains(prompt, "```") {
		t.Errorf("prompt should not request markdown formatting")
	}
}

// TestGenerateHyDEDocFallsBackOnMissingKey covers the graceful-fallback
// path: missing OPENAI_API_KEY → returns the raw query unchanged with
// ok=false, doesn't panic, doesn't error out the search.
func TestGenerateHyDEDocFallsBackOnMissingKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	got, ok := generateHyDEDoc("how do I do X", searchOpts{rerankLLMModel: "gpt-4.1-mini"})
	if ok {
		t.Errorf("expected ok=false when key missing, got ok=true")
	}
	if got != "how do I do X" {
		t.Errorf("expected raw query as fallback, got %q", got)
	}
}

// TestHyDEUsageEvent covers the price-table fall-through for known/unknown models.
func TestHyDEUsageEvent(t *testing.T) {
	// gpt-4.1-mini default: $0.15 input / $0.60 output per 1M
	got := searchruntime.BuildHyDEUsageEvent("gpt-4.1-mini", 1000, 500)
	want := (1000.0/1_000_000*0.15 + 500.0/1_000_000*0.60) * 100.0
	if got.Cents != want {
		t.Errorf("gpt-4.1-mini cents = %v, want %v", got.Cents, want)
	}
	if got.ProviderName != "openai" || got.CallSite != searchruntime.DefaultHyDEUsageCallSite {
		t.Errorf("gpt-4.1-mini event = %+v, want openai/default callsite", got)
	}
	// Unknown model falls through to gpt-4.1-mini default
	got2 := searchruntime.BuildHyDEUsageEvent("gpt-some-other", 1000, 500)
	if got2.Cents != want {
		t.Errorf("unknown model cents = %v, want fallback to gpt-4.1-mini = %v", got2.Cents, want)
	}
	// gpt-4o: $2.50 input / $10.00 output per 1M
	got3 := searchruntime.BuildHyDEUsageEvent("gpt-4o", 1000, 500)
	want3 := (1000.0/1_000_000*2.50 + 500.0/1_000_000*10.00) * 100.0
	if got3.Cents != want3 {
		t.Errorf("gpt-4o cents = %v, want %v", got3.Cents, want3)
	}
}
