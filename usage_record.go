package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nstranquist/docs-puller/internal/apppaths"
)

// recordAIUsage appends one event to the docs-puller usage log
// (~/.docs-puller/state/ai-usage.jsonl by default; the legacy state dir
// applies under DOCS_PULLER_LEGACY_NDEV_PATHS=1 — see internal/apppaths).
// This lets a host CLI roll provider spend up alongside its other surfaces.
//
// Best-effort: failures (missing HOME, read-only FS, marshalling errors)
// are swallowed so the calling AI path never blocks on auditing.
//
// The event schema is a stable append-only JSONL contract shared with
// host-CLI usage ledgers; keep field names and types backward-compatible.
//
// Pass estCents as 0 when the caller doesn't have pricing handy — the
// audit retains token counts; only the dollar figure is partial. Pass
// a non-zero fractional cents value when the caller already computed
// cost (e.g. rerank_llm has its own pricing table that pre-dates the
// central registry). EstCents is fractional USD cents (float64) so
// sub-cent calls don't collapse to zero in per-event rounding.
func recordAIUsage(provider, model, callSite string, inputTokens, outputTokens int, estCents float64) {
	dir, err := apppaths.UsageLogDir()
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	ev := struct {
		TS           string  `json:"ts"`
		Provider     string  `json:"provider"`
		Model        string  `json:"model"`
		CallSite     string  `json:"call_site"`
		InputTokens  int     `json:"input_tokens"`
		OutputTokens int     `json:"output_tokens"`
		EstCents     float64 `json:"est_cents"`
	}{
		TS:           time.Now().UTC().Format(time.RFC3339Nano),
		Provider:     provider,
		Model:        model,
		CallSite:     callSite,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		EstCents:     estCents,
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	usageRecordMu.Lock()
	defer usageRecordMu.Unlock()
	f, err := os.OpenFile(filepath.Join(dir, "ai-usage.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(body, '\n'))
}

// usageRecordMu serializes appends from concurrent goroutines in the
// same docs-puller invocation. Cross-process safety relies on POSIX
// O_APPEND atomicity for small writes; we do not flock.
var usageRecordMu sync.Mutex
