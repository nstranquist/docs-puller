//go:build docs_puller_searchcore_inprocess

package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nstranquist/docs-puller/searchcore"
)

func TestInProcessSearchcoreAdapterCallsDispatchBoundary(t *testing.T) {
	out := t.TempDir()
	writeDoc(t, filepath.Join(out, "demo"), "guide.md", "# Demo Guide\n\nneedle appears here\n")

	searcher := newInProcessSearchcoreAdapter(searchOpts{out: out, useScan: true, limit: 10, noSnippets: true})
	var _ searchcore.Searcher = searcher

	hits, err := searcher.Search(context.Background(), searchcore.Query{Text: "needle", Limit: 5, Source: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1: %+v", len(hits), hits)
	}
	if hits[0].Path != "demo/guide.md" || hits[0].Source != "demo" || hits[0].Title != "Demo Guide" {
		t.Fatalf("unexpected hit: %+v", hits[0])
	}
}

func TestInProcessSearchcoreAdapterRejectsEmptyQuery(t *testing.T) {
	searcher := newInProcessSearchcoreAdapter(searchOpts{useScan: true})
	_, err := searcher.Search(context.Background(), searchcore.Query{Text: "   "})
	if err == nil || !strings.Contains(err.Error(), "query text is required") {
		t.Fatalf("expected query-required error, got %v", err)
	}
}

func TestInProcessSearchcoreAdapterRespectsCanceledContext(t *testing.T) {
	searcher := newInProcessSearchcoreAdapter(searchOpts{useScan: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := searcher.Search(ctx, searchcore.Query{Text: "needle"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
}
