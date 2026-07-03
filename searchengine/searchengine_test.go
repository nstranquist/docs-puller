package searchengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nstranquist/docs-puller/searchcore"
	"github.com/nstranquist/docs-puller/searchruntime"
)

func TestScanSearcherUsesRuntimeDispatchConstructor(t *testing.T) {
	root := t.TempDir()
	writeTestDoc(t, filepath.Join(root, "demo"), "guide.md", "# Demo Guide\n\nneedle appears here\n")

	searcher := NewScanSearcher(ScanOptions{Root: root, Limit: 10}, DefaultScanCallbacks())
	var _ searchcore.Searcher = searcher

	hits, err := searcher.Search(context.Background(), searchcore.Query{Text: "needle", Limit: 5, Source: "demo"})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1: %+v", len(hits), hits)
	}
	if hits[0].Path != "demo/guide.md" || hits[0].Source != "demo" || hits[0].Title != "Demo Guide" {
		t.Fatalf("unexpected hit: %+v", hits[0])
	}
}

func TestDispatchScanReturnsStructuredResult(t *testing.T) {
	root := t.TempDir()
	writeTestDoc(t, filepath.Join(root, "demo"), "guide.md", "# Demo Guide\n\nneedle appears here\n")

	dispatch := DispatchScan(DefaultScanCallbacks())
	result := dispatch(context.Background(), searchruntime.DispatchRequest[ScanOptions, ScanSharedIndex]{
		Query: "needle",
		Opts:  ScanOptions{Root: root, Source: "demo", Limit: 5},
	})

	if result.Mode != "scan" || result.Scanned != 1 {
		t.Fatalf("unexpected dispatch result metadata: %+v", result)
	}
	if len(result.Hits) != 1 || result.Hits[0].Path != "demo/guide.md" {
		t.Fatalf("unexpected hits: %+v", result.Hits)
	}
}

func TestRunScanAppliesSnippetOverrides(t *testing.T) {
	root := t.TempDir()
	writeTestDoc(t, filepath.Join(root, "demo"), "guide.md", "# Demo Guide\n\nneedle appears on this deliberately long first matching line\nneedle appears on another long matching line\n")

	result := RunScan("needle", ScanOptions{
		Root:        root,
		Source:      "demo",
		Limit:       5,
		MaxSnippets: 1,
		SnippetLen:  24,
	}, DefaultScanCallbacks())

	if len(result.Hits) != 1 {
		t.Fatalf("got %d hits, want 1: %+v", len(result.Hits), result.Hits)
	}
	if got := len(result.Hits[0].Snippets); got != 1 {
		t.Fatalf("snippet count = %d, want 1: %+v", got, result.Hits[0].Snippets)
	}
	if got := len(result.Hits[0].Snippets[0].Text); got > 24 {
		t.Fatalf("snippet length = %d, want <= 24: %q", got, result.Hits[0].Snippets[0].Text)
	}
}

func TestRunScanClampsTinySnippetLength(t *testing.T) {
	root := t.TempDir()
	writeTestDoc(t, filepath.Join(root, "demo"), "guide.md", "# Demo Guide\n\nneedle appears on a long line\n")

	result := RunScan("needle", ScanOptions{
		Root:       root,
		Source:     "demo",
		Limit:      5,
		SnippetLen: 1,
	}, DefaultScanCallbacks())

	if len(result.Hits) != 1 || len(result.Hits[0].Snippets) != 1 {
		t.Fatalf("unexpected hits: %+v", result.Hits)
	}
	if got := len(result.Hits[0].Snippets[0].Text); got > 4 {
		t.Fatalf("snippet length = %d, want <= 4: %q", got, result.Hits[0].Snippets[0].Text)
	}
}

func TestDispatchScanRespectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dispatch := DispatchScan(DefaultScanCallbacks())
	result := dispatch(ctx, searchruntime.DispatchRequest[ScanOptions, ScanSharedIndex]{
		Query: "needle",
		Opts:  ScanOptions{Root: t.TempDir(), Limit: 5},
	})

	if len(result.Hits) != 0 || result.Scanned != 0 || result.Mode != "" {
		t.Fatalf("canceled dispatch should return an empty result, got %+v", result)
	}
}

func writeTestDoc(t *testing.T, dir string, name string, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
