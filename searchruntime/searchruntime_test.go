package searchruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nstranquist/docs-puller/searchcore"
)

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestSearcherMergesQueryOverrides(t *testing.T) {
	var got Request
	searcher := NewSearcher(Options{Limit: 3, Source: "base"}, func(_ context.Context, req Request) ([]Hit, error) {
		got = req
		return []Hit{{Path: "ok.md", Score: 1}}, nil
	})

	hits, err := searcher.Search(context.Background(), searchcore.Query{
		Text:   "needle",
		Limit:  5,
		Source: "source",
		Rerank: true,
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(hits) != 1 || hits[0].Path != "ok.md" {
		t.Fatalf("unexpected hits: %+v", hits)
	}
	if got.Query != "needle" {
		t.Fatalf("unexpected query: %q", got.Query)
	}
	if got.Options.Limit != 5 || got.Options.Source != "source" || !got.Options.RerankLLM {
		t.Fatalf("unexpected options: %+v", got.Options)
	}
}

func TestSearcherPreservesBaseOptions(t *testing.T) {
	var got Request
	searcher := NewSearcher(Options{Limit: 7, Source: "base", RerankLLM: true}, func(_ context.Context, req Request) ([]Hit, error) {
		got = req
		return nil, nil
	})

	hits, err := searcher.Search(context.Background(), searchcore.Query{Text: "needle"})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("unexpected hits: %+v", hits)
	}
	if got.Options.Limit != 7 || got.Options.Source != "base" || !got.Options.RerankLLM {
		t.Fatalf("unexpected options: %+v", got.Options)
	}
}

func TestSearcherRejectsEmptyQueryBeforeDispatch(t *testing.T) {
	called := false
	searcher := NewSearcher(Options{}, func(context.Context, Request) ([]Hit, error) {
		called = true
		return nil, nil
	})

	hits, err := searcher.Search(context.Background(), searchcore.Query{Text: "   "})
	if err == nil || !strings.Contains(err.Error(), "query text is required") {
		t.Fatalf("expected empty-query error, got %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("unexpected hits: %+v", hits)
	}
	if called {
		t.Fatal("dispatch should not run for an empty query")
	}
}

func TestSearcherPropagatesDispatchError(t *testing.T) {
	want := errors.New("dispatch failed")
	searcher := NewSearcher(Options{}, func(context.Context, Request) ([]Hit, error) {
		return nil, want
	})

	hits, err := searcher.Search(context.Background(), searchcore.Query{Text: "needle"})
	if !errors.Is(err, want) {
		t.Fatalf("expected dispatch error, got %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("unexpected hits: %+v", hits)
	}
}

func TestDispatchEngineSearcherBuildsStructuredRequest(t *testing.T) {
	type engineOptions struct {
		limit     int
		source    string
		rerankLLM bool
	}
	var got DispatchRequest[engineOptions, string]
	searcher := NewDispatchEngineSearcher(DispatchEngineConfig[engineOptions, string]{
		BaseOptions:   Options{Limit: 3, Source: "base"},
		EngineOptions: engineOptions{limit: 3, source: "base"},
		SharedIndex:   "shared-index",
		ApplyOptions: func(opts engineOptions, runtimeOpts Options) engineOptions {
			opts.limit = runtimeOpts.Limit
			opts.source = runtimeOpts.Source
			opts.rerankLLM = runtimeOpts.RerankLLM
			return opts
		},
		Dispatch: func(_ context.Context, req DispatchRequest[engineOptions, string]) DispatchResult[Hit] {
			got = req
			return DispatchResult[Hit]{Hits: []Hit{{Path: "ok.md", Score: 1}}}
		},
	})

	hits, err := searcher.Search(context.Background(), searchcore.Query{
		Text:   "needle",
		Limit:  5,
		Source: "docs",
		Rerank: true,
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(hits) != 1 || hits[0].Path != "ok.md" {
		t.Fatalf("unexpected hits: %+v", hits)
	}
	if got.Query != "needle" || got.SharedIdx != "shared-index" {
		t.Fatalf("unexpected dispatch request: %+v", got)
	}
	if got.Opts.limit != 5 || got.Opts.source != "docs" || !got.Opts.rerankLLM {
		t.Fatalf("unexpected engine opts: %+v", got.Opts)
	}
}

func TestDispatchEngineSearcherRejectsNilDispatch(t *testing.T) {
	searcher := NewDispatchEngineSearcher(DispatchEngineConfig[Options, string]{})
	hits, err := searcher.Search(context.Background(), searchcore.Query{Text: "needle"})
	if err == nil || !strings.Contains(err.Error(), "in-process dispatcher is required") {
		t.Fatalf("expected missing-dispatch error, got %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("unexpected hits: %+v", hits)
	}
}

func TestDispatchRequestResultAliases(t *testing.T) {
	req := DispatchRequest[Options, string]{
		Query:     "needle",
		Opts:      Options{Limit: 5, Source: "docs"},
		SharedIdx: "shared",
	}
	if req.Query != "needle" || req.Opts.Limit != 5 || req.SharedIdx != "shared" {
		t.Fatalf("unexpected dispatch request alias behavior: %+v", req)
	}
	result := DispatchResult[Hit]{
		Hits:    []Hit{{Path: "docs/guide.md"}},
		Scanned: 12,
		Mode:    "scan",
	}
	if len(result.Hits) != 1 || result.Hits[0].Path != "docs/guide.md" || result.Scanned != 12 || result.Mode != "scan" {
		t.Fatalf("unexpected dispatch result alias behavior: %+v", result)
	}
}

func TestBM25IsConfident(t *testing.T) {
	hits := []Hit{{Score: 100}, {Score: 80}}
	if !BM25IsConfident(hits, 0.10) {
		t.Fatal("20% top-1 gap should satisfy 10% rerank gate")
	}
	if BM25IsConfident(hits, 0.25) {
		t.Fatal("20% top-1 gap should not satisfy 25% rerank gate")
	}
	if BM25IsConfident(hits, 0) {
		t.Fatal("gate=0 should disable confidence gating")
	}
	if BM25IsConfident(hits[:1], 0.10) {
		t.Fatal("single hit has no top-2 comparison and should not gate")
	}
}

func TestDefaultSourceHygiene(t *testing.T) {
	if got := ExpandedSourceHygieneLimit(3); got <= 3 {
		t.Fatalf("ExpandedSourceHygieneLimit(3) = %d, want expanded candidate window", got)
	}
	generated := DefaultSourceHygienePenalty(Hit{
		Path: "generated/local-ai/human-command-prompts/export.md",
		URL:  "https://example.com/generated/local-ai/human-command-prompts/export",
	})
	guide := DefaultSourceHygienePenalty(Hit{
		Path: "docs/guides/getting-started.md",
		URL:  "https://example.com/guides/getting-started",
	})
	if generated <= guide {
		t.Fatalf("generated penalty = %d, guide penalty = %d, want generated page downranked more", generated, guide)
	}
}

func TestPlanPreRetrievalUsesRerankCandidateLimit(t *testing.T) {
	got := PlanPreRetrieval(PreRetrievalOptions{
		UserLimit:     5,
		CurrentLimit:  5,
		Rerank:        true,
		RerankK:       10,
		CurrentSource: "supabase",
	})
	if got.UserLimit != 5 || got.BM25Limit != 10 {
		t.Fatalf("limits = %+v, want user=5 bm25=10", got)
	}
	if got.Source != "supabase" {
		t.Fatalf("source = %q, want supabase", got.Source)
	}
}

func TestPlanPreRetrievalUsesHygieneLimitWithoutRerank(t *testing.T) {
	got := PlanPreRetrieval(PreRetrievalOptions{
		UserLimit:    5,
		CurrentLimit: 5,
		HygieneLimit: 25,
	})
	if got.UserLimit != 5 || got.BM25Limit != 25 {
		t.Fatalf("limits = %+v, want user=5 bm25=25", got)
	}
}

func TestPlanPreRetrievalUsesWideVersionCandidatePool(t *testing.T) {
	got := PlanPreRetrieval(PreRetrievalOptions{
		UserLimit:              5,
		CurrentLimit:           10,
		NeedsWideCandidatePool: true,
	})
	if got.BM25Limit != 200 {
		t.Fatalf("bm25 limit = %d, want wide pool 200", got.BM25Limit)
	}
}

func TestPlanPreRetrievalUsesPolicySource(t *testing.T) {
	got := PlanPreRetrieval(PreRetrievalOptions{
		UserLimit:      5,
		CurrentLimit:   5,
		CurrentSource:  "supabase",
		PolicySourceID: "supabase__v2",
	})
	if got.Source != "supabase__v2" {
		t.Fatalf("source = %q, want policy source", got.Source)
	}
}

func TestPlanCompactOutputPreset(t *testing.T) {
	cases := []struct {
		name string
		opts CompactOutputPresetOptions
		want CompactOutputPreset
	}{
		{
			name: "non compact preserves existing values",
			opts: CompactOutputPresetOptions{
				SnippetLen:  120,
				MaxSnippets: 2,
			},
			want: CompactOutputPreset{SnippetLen: 120, MaxSnippets: 2},
		},
		{
			name: "compact fills unset values",
			opts: CompactOutputPresetOptions{
				Compact:            true,
				DefaultSnippetLen:  80,
				DefaultMaxSnippets: 1,
			},
			want: CompactOutputPreset{SnippetLen: 80, MaxSnippets: 1},
		},
		{
			name: "compact preserves explicit snippet length",
			opts: CompactOutputPresetOptions{
				Compact:            true,
				SnippetLen:         120,
				DefaultSnippetLen:  80,
				DefaultMaxSnippets: 1,
			},
			want: CompactOutputPreset{SnippetLen: 120, MaxSnippets: 1},
		},
		{
			name: "compact preserves explicit snippet count",
			opts: CompactOutputPresetOptions{
				Compact:            true,
				MaxSnippets:        2,
				DefaultSnippetLen:  80,
				DefaultMaxSnippets: 1,
			},
			want: CompactOutputPreset{SnippetLen: 80, MaxSnippets: 2},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanCompactOutputPreset(tt.opts); got != tt.want {
				t.Fatalf("PlanCompactOutputPreset() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestApplyOutputOverridesDropsSnippetsAndURL(t *testing.T) {
	got := ApplyOutputOverrides([]Hit{{
		Path:     "doc.md",
		URL:      "https://example.com/doc",
		Snippets: []Snippet{{Line: 1, Text: "needle"}},
	}}, OutputOverrideOptions{
		Compact:    true,
		NoSnippets: true,
	}, nil)
	if len(got) != 1 {
		t.Fatalf("hits = %+v, want one hit", got)
	}
	if got[0].URL != "" {
		t.Fatalf("URL = %q, want dropped", got[0].URL)
	}
	if len(got[0].Snippets) != 0 {
		t.Fatalf("snippets = %+v, want dropped", got[0].Snippets)
	}
}

func TestApplyOutputOverridesRetunesSnippets(t *testing.T) {
	called := false
	got := ApplyOutputOverrides([]Hit{{
		Path:     "doc.md",
		URL:      "https://example.com/doc",
		Snippets: []Snippet{{Line: 1, Text: "old"}},
	}}, OutputOverrideOptions{
		MaxSnippets: 1,
	}, func(hit Hit) ([]Snippet, error) {
		called = true
		if hit.Path != "doc.md" {
			t.Fatalf("retune hit = %+v, want doc.md", hit)
		}
		return []Snippet{{Line: 2, Text: "new"}}, nil
	})
	if !called {
		t.Fatal("retuner should run")
	}
	if got[0].URL != "https://example.com/doc" {
		t.Fatalf("URL = %q, want preserved", got[0].URL)
	}
	if len(got[0].Snippets) != 1 || got[0].Snippets[0].Text != "new" {
		t.Fatalf("snippets = %+v, want retuned", got[0].Snippets)
	}
}

func TestApplyOutputOverridesIgnoresRetuneError(t *testing.T) {
	got := ApplyOutputOverrides([]Hit{{
		Path:     "doc.md",
		Snippets: []Snippet{{Line: 1, Text: "old"}},
	}}, OutputOverrideOptions{
		SnippetLen: 80,
	}, func(Hit) ([]Snippet, error) {
		return nil, errors.New("read failed")
	})
	if len(got[0].Snippets) != 1 || got[0].Snippets[0].Text != "old" {
		t.Fatalf("snippets = %+v, want preserved on retune error", got[0].Snippets)
	}
}

func TestNewFileSnippetRetuner(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "guide.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("Needle body"), 0o644); err != nil {
		t.Fatal(err)
	}
	retuner := NewFileSnippetRetuner(root, "Needle", 2, 80, func(body string, queryLower string, maxSnippets int, snippetLen int) []Snippet {
		if body != "Needle body" || queryLower != "needle" || maxSnippets != 2 || snippetLen != 80 {
			t.Fatalf("extractor args = body=%q query=%q max=%d len=%d", body, queryLower, maxSnippets, snippetLen)
		}
		return []Snippet{{Line: 1, Text: "retuned"}}
	})
	got, err := retuner(Hit{Path: "docs/guide.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "retuned" {
		t.Fatalf("snippets = %+v, want retuned snippet", got)
	}
}

func TestNewFileSnippetRetunerNilExtractor(t *testing.T) {
	if got := NewFileSnippetRetuner(t.TempDir(), "needle", 1, 80, nil); got != nil {
		t.Fatalf("NewFileSnippetRetuner with nil extractor = %v, want nil", got)
	}
}

func TestRunScanRanksAndLoadsURLs(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "docs")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "guide.md"), []byte("# Guide\n\nfoo foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "_INDEX.md"), []byte("foo foo foo foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, scanned := RunScan(ScanInput{
		Query: "foo",
		Options: ScanOptions{
			Root:       root,
			Limit:      5,
			TitleBoost: 5,
		},
		ListSources: func(root string) ([]string, error) {
			return []string{"docs"}, nil
		},
		LoadURLByPath: func(sourceDir string, sourceName string) map[string]string {
			return map[string]string{"guide.md": "https://example.com/guide"}
		},
		ExtractTitle: func(path string) string {
			return "Foo Guide"
		},
		ExtractSnippets: func(body string, queryLower string, maxSnippets int, snippetLen int) []Snippet {
			if queryLower != "foo" {
				t.Fatalf("queryLower = %q, want foo", queryLower)
			}
			return []Snippet{{Line: 2, Text: "foo foo"}}
		},
	})
	if scanned != 1 {
		t.Fatalf("scanned = %d, want only markdown docs excluding _INDEX.md", scanned)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want one hit", hits)
	}
	if hits[0].Path != filepath.Join("docs", "guide.md") || hits[0].URL != "https://example.com/guide" {
		t.Fatalf("hit path/url = %q %q", hits[0].Path, hits[0].URL)
	}
	if hits[0].Score != 7 {
		t.Fatalf("score = %d, want 2 body hits + title boost 5", hits[0].Score)
	}
}

func TestDedupeSnippetsAcross(t *testing.T) {
	got := DedupeSnippetsAcross([]Hit{
		{
			Path: "first.md",
			Snippets: []Snippet{
				{Line: 1, Text: "shared"},
				{Line: 2, Text: " unique "},
			},
		},
		{
			Path: "second.md",
			Snippets: []Snippet{
				{Line: 3, Text: "shared"},
				{Line: 4, Text: "   "},
				{Line: 5, Text: "other"},
			},
		},
	})
	if len(got) != 2 {
		t.Fatalf("hits = %+v, want two hits preserved", got)
	}
	if len(got[0].Snippets) != 2 {
		t.Fatalf("first snippets = %+v, want unchanged", got[0].Snippets)
	}
	if len(got[1].Snippets) != 1 || got[1].Snippets[0].Text != "other" {
		t.Fatalf("second snippets = %+v, want only new nonblank snippet", got[1].Snippets)
	}
}

func TestNewBackendAdapterAppliesPlan(t *testing.T) {
	type backendOpts struct {
		limit  int
		source string
		cached int
	}
	builder := NewBackendAdapter(BackendAdapter[backendOpts, *struct{}]{
		Options:     BackendRetrievalOptions{UseScan: true},
		BaseOptions: backendOpts{limit: 2, source: "old", cached: 17},
		ApplyPlan: func(opts backendOpts, pre PreRetrievalPlan) backendOpts {
			opts.limit = pre.BM25Limit
			opts.source = pre.Source
			return opts
		},
		CachedTotalDocs: func(opts backendOpts) int {
			return opts.cached
		},
		Scan: func(opts backendOpts) ([]Hit, int) {
			if opts.limit != 20 || opts.source != "docs__v1" {
				t.Fatalf("scan opts = %+v, want planned limit/source", opts)
			}
			return []Hit{{Path: "planned.md"}}, 17
		},
	})
	retrieval := builder(PreRetrievalPlan{BM25Limit: 20, Source: "docs__v1"})
	if retrieval.Options.CachedTotalDocs != 17 {
		t.Fatalf("cached total docs = %d, want 17", retrieval.Options.CachedTotalDocs)
	}
	got := RunBackendRetrieval(retrieval)
	if got.Scanned != 17 || got.Mode != "scan" || len(got.Hits) != 1 || got.Hits[0].Path != "planned.md" {
		t.Fatalf("backend result = %+v, want planned scan result", got)
	}
}

func TestNewBackendAdapterIndexOperations(t *testing.T) {
	type backendOpts struct {
		source string
	}
	type index struct {
		closed bool
	}
	shared := &index{}
	builder := NewBackendAdapter(BackendAdapter[backendOpts, *index]{
		Options: BackendRetrievalOptions{SharedIndexProvided: true},
		BaseOptions: backendOpts{
			source: "docs",
		},
		SharedIndex: shared,
		QueryText:   "needle",
		IndexQuery: func(idx *index, query string, opts backendOpts) ([]Hit, error) {
			if idx != shared || query != "needle" || opts.source != "docs" {
				t.Fatalf("query args idx=%p query=%q opts=%+v", idx, query, opts)
			}
			return []Hit{{Path: "docs/guide.md"}}, nil
		},
		IndexCount: func(idx *index) (int, error) {
			if idx != shared {
				t.Fatalf("count idx=%p, want shared", idx)
			}
			return 12, nil
		},
		IndexClose: func(idx *index) error {
			idx.closed = true
			return nil
		},
	})
	got := RunBackendRetrieval(builder(PreRetrievalPlan{}))
	if got.Mode != "fts5" || got.Scanned != 12 || len(got.Hits) != 1 || got.Hits[0].Path != "docs/guide.md" {
		t.Fatalf("backend result = %+v, want FTS result", got)
	}
	if shared.closed {
		t.Fatal("shared index should not be closed by backend retrieval")
	}
}

func TestNewLocalIndexOpener(t *testing.T) {
	expected := &struct{}{}
	got, ok := NewLocalIndexOpener("root", func(root string) bool {
		if root != "root" {
			t.Fatalf("exists root = %q, want root", root)
		}
		return true
	}, func(root string) (*struct{}, error) {
		if root != "root" {
			t.Fatalf("open root = %q, want root", root)
		}
		return expected, nil
	})()
	if !ok || got != expected {
		t.Fatalf("opener = (%p, %v), want expected index", got, ok)
	}
}

func TestNewLocalIndexOpenerUnavailable(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		got, ok := NewLocalIndexOpener("root", func(string) bool { return false }, func(string) (*struct{}, error) {
			t.Fatal("open should not run when index is missing")
			return &struct{}{}, nil
		})()
		if ok || got != nil {
			t.Fatalf("opener = (%p, %v), want unavailable", got, ok)
		}
	})
	t.Run("open error", func(t *testing.T) {
		got, ok := NewLocalIndexOpener("root", func(string) bool { return true }, func(string) (*struct{}, error) {
			return nil, errors.New("locked")
		})()
		if ok || got != nil {
			t.Fatalf("opener = (%p, %v), want unavailable", got, ok)
		}
	})
}

func TestRunPipelineOrchestratesRuntimeStages(t *testing.T) {
	backendSawPlan := false
	postSawPlan := false
	got := RunPipeline(Pipeline[*struct{}]{
		PreRetrieval: PreRetrievalOptions{
			UserLimit:      2,
			CurrentLimit:   2,
			HygieneLimit:   4,
			CurrentSource:  "docs",
			PolicySourceID: "docs__v1",
		},
		Backend: func(pre PreRetrievalPlan) BackendRetrieval[*struct{}] {
			backendSawPlan = true
			if pre.UserLimit != 2 || pre.BM25Limit != 4 || pre.Source != "docs__v1" {
				t.Fatalf("backend pre plan = %+v", pre)
			}
			return BackendRetrieval[*struct{}]{
				Options: BackendRetrievalOptions{UseScan: true},
				Scan: func() ([]Hit, int) {
					return []Hit{
						{Path: "docs/generated.md", Source: "docs", URL: "https://example.com/generated", Score: 90},
						{Path: "docs/guide.md", Source: "docs", URL: "https://example.com/guide", Score: 80},
					}, 22
				},
			}
		},
		PostRetrieval: func(pre PreRetrievalPlan) PostRetrievalProcessing {
			postSawPlan = true
			if pre.UserLimit != 2 {
				t.Fatalf("post pre plan = %+v", pre)
			}
			return PostRetrievalProcessing{
				Options: PostRetrievalOptions{UserLimit: pre.UserLimit},
				SourceHygienePenalty: func(hit Hit) int {
					if strings.Contains(hit.Path, "generated") {
						return 10
					}
					return 0
				},
			}
		},
		Output: OutputOverrideOptions{Compact: true},
	})
	if !backendSawPlan || !postSawPlan {
		t.Fatalf("callbacks called backend=%v post=%v", backendSawPlan, postSawPlan)
	}
	if got.Scanned != 22 || got.Mode != "scan" {
		t.Fatalf("pipeline result = %+v, want scan with scanned count", got)
	}
	if got.PreRetrieval.Source != "docs__v1" {
		t.Fatalf("pre plan = %+v, want policy source", got.PreRetrieval)
	}
	if len(got.Hits) != 1 || got.Hits[0].Path != "docs/guide.md" {
		t.Fatalf("hits = %+v, want source-hygiene survivor", got.Hits)
	}
	if got.Hits[0].URL != "" {
		t.Fatalf("URL = %q, want compact output drop", got.Hits[0].URL)
	}
}

func TestPipelineWarnings(t *testing.T) {
	got := PipelineWarnings(PipelineResult{
		FTSQueryErr:        errors.New("locked"),
		HybridErr:          errors.New("embed missing"),
		RerankErr:          errors.New("key missing"),
		WarnQueryError:     true,
		WarnMissingIndex:   true,
		WarnHybridFallback: true,
		WarnRerankFallback: true,
	})
	want := []string{
		"fts5: locked — falling back to scan",
		"search: FTS5 index missing — falling back to scan. Run `docs-puller reindex` to build it.",
		"rerank-hybrid: embed missing — using BM25 candidates only",
		"rerank: key missing — falling back to BM25 order",
	}
	if len(got) != len(want) {
		t.Fatalf("warnings = %+v, want %d", got, len(want))
	}
	for i := range want {
		if got[i].Message != want[i] {
			t.Fatalf("warning %d = %q, want %q", i, got[i].Message, want[i])
		}
	}
	if got, want := PipelineWarningsText(PipelineResult{
		FTSQueryErr:    errors.New("locked"),
		WarnQueryError: true,
	}), "fts5: locked — falling back to scan\n"; got != want {
		t.Fatalf("PipelineWarningsText = %q, want %q", got, want)
	}
	if got := PipelineWarningsText(PipelineResult{}); got != "" {
		t.Fatalf("PipelineWarningsText empty = %q, want empty", got)
	}
}

func TestRunBackendRetrievalUsesForcedScan(t *testing.T) {
	got := RunBackendRetrieval(BackendRetrieval[*struct{}]{
		Options: BackendRetrievalOptions{UseScan: true},
		Scan: func() ([]Hit, int) {
			return []Hit{{Path: "scan.md", Score: 3}}, 12
		},
	})
	if got.Mode != "scan" || got.Scanned != 12 {
		t.Fatalf("retrieval = %+v, want scan mode with scanned count", got)
	}
	if len(got.Hits) != 1 || got.Hits[0].Path != "scan.md" {
		t.Fatalf("hits = %+v, want scan hit", got.Hits)
	}
}

func TestRunBackendRetrievalFallsBackWhenLocalIndexMissing(t *testing.T) {
	scanCalled := false
	got := RunBackendRetrieval(BackendRetrieval[*struct{}]{
		OpenLocal: func() (*struct{}, bool) {
			return nil, false
		},
		Scan: func() ([]Hit, int) {
			scanCalled = true
			return []Hit{{Path: "fallback.md", Score: 2}}, 8
		},
	})
	if !scanCalled {
		t.Fatal("scan fallback should run")
	}
	if !got.WarnMissingIndex || got.WarnQueryError || got.Degraded {
		t.Fatalf("warning state = %+v, want missing-index fallback only", got)
	}
	if got.Mode != "scan" || len(got.Hits) != 1 || got.Hits[0].Path != "fallback.md" {
		t.Fatalf("retrieval = %+v, want scan fallback hit", got)
	}
}

func TestRunBackendRetrievalDegradesFTSOnlyWhenLocalIndexMissing(t *testing.T) {
	scanCalled := false
	got := RunBackendRetrieval(BackendRetrieval[*struct{}]{
		Options: BackendRetrievalOptions{FTSOnly: true},
		OpenLocal: func() (*struct{}, bool) {
			return nil, false
		},
		Scan: func() ([]Hit, int) {
			scanCalled = true
			return nil, 0
		},
	})
	if scanCalled {
		t.Fatal("ftsOnly should not scan when FTS is unavailable")
	}
	if !got.Degraded || got.Mode != "" || len(got.Hits) != 0 {
		t.Fatalf("retrieval = %+v, want degraded empty result", got)
	}
}

func TestRunBackendRetrievalUsesSharedIndexWithCachedCount(t *testing.T) {
	shared := &struct{}{}
	countCalled := false
	closed := false
	got := RunBackendRetrieval(BackendRetrieval[*struct{}]{
		Options: BackendRetrievalOptions{
			SharedIndexProvided: true,
			CachedTotalDocs:     42,
		},
		SharedIndex: shared,
		Query: func(idx *struct{}) ([]Hit, error) {
			if idx != shared {
				t.Fatalf("query index = %p, want shared %p", idx, shared)
			}
			return []Hit{{Path: "fts.md", Score: 5}}, nil
		},
		Count: func(*struct{}) int {
			countCalled = true
			return 0
		},
		Close: func(*struct{}) {
			closed = true
		},
	})
	if got.Mode != "fts5" || got.Scanned != 42 {
		t.Fatalf("retrieval = %+v, want fts5 cached count", got)
	}
	if countCalled {
		t.Fatal("cached total docs should avoid Count")
	}
	if closed {
		t.Fatal("shared index should not be closed by runtime retrieval")
	}
	if len(got.Hits) != 1 || got.Hits[0].Path != "fts.md" {
		t.Fatalf("hits = %+v, want FTS hit", got.Hits)
	}
}

func TestApplySourceHygieneDownranksGeneratedWhenDurableHitsExist(t *testing.T) {
	hits := []Hit{
		{Path: "kb/generated/local-ai/human-command-prompts/exports/codex/2026-04-part-14.md", Score: 500},
		{Path: "kb/local-ai/human-command-prompts/ingested/2026-04/replay.md", Score: 490},
		{Path: "kb/repos/ingested/supermemory.md", Score: 120},
		{Path: "anthropic/api/typescript/beta.md", Score: 100},
	}
	got := ApplySourceHygiene(hits, 2, func(hit Hit) int {
		if strings.Contains(hit.Path, "human-command-prompts") {
			return 10
		}
		return 0
	})
	if len(got) != 2 {
		t.Fatalf("hits = %#v, want 2 durable hits", got)
	}
	for _, hit := range got {
		if strings.Contains(hit.Path, "human-command-prompts") {
			t.Fatalf("generated/replay hit survived before durable hit: %#v", got)
		}
	}
	if got[0].Path != "kb/repos/ingested/supermemory.md" {
		t.Fatalf("top hit = %#v, want durable KB note", got[0])
	}
}

type profileMatcherFunc func(source, relPath string) (bool, bool)

func (f profileMatcherFunc) Match(source, relPath string) (bool, bool) {
	return f(source, relPath)
}

func TestAnnotateProfileMembership(t *testing.T) {
	hits := []Hit{
		{Path: "docs/guide.md", Source: "docs"},
		{Path: "other/guide.md", Source: "other"},
	}
	got := AnnotateProfileMembership(hits, profileMatcherFunc(func(source, relPath string) (bool, bool) {
		return source == "docs" && relPath == "guide.md", source == "docs"
	}))
	if !got[0].InProfile || !got[0].InProfileSub {
		t.Fatalf("first hit should be annotated: %+v", got[0])
	}
	if got[1].InProfile || got[1].InProfileSub {
		t.Fatalf("second hit should not be annotated: %+v", got[1])
	}
}

func TestAnnotateProfileMembershipNilProfileIsNoop(t *testing.T) {
	hits := []Hit{{Path: "docs/guide.md", Source: "docs"}}
	got := AnnotateProfileMembership(hits, nil)
	if got[0].InProfile || got[0].InProfileSub {
		t.Fatalf("nil profile should not annotate: %+v", got[0])
	}
}

func TestFilterStrictProfile(t *testing.T) {
	hits := []Hit{
		{Path: "keep.md", InProfile: true},
		{Path: "drop.md"},
		{Path: "keep-sub.md", InProfile: true, InProfileSub: true},
	}
	got := FilterStrictProfile(hits)
	if len(got) != 2 {
		t.Fatalf("got %d hits, want 2: %+v", len(got), got)
	}
	if got[0].Path != "keep.md" || got[1].Path != "keep-sub.md" {
		t.Fatalf("unexpected strict-filter result: %+v", got)
	}
}

func TestResolveSearchProfileLoadsResolvedProfile(t *testing.T) {
	var gotOptions ProfileResolveOptions
	var gotLoadName string
	var gotLoadOut string
	result := ResolveSearchProfile(ProfileSelectionInput[string]{
		Options: ProfileResolveOptions{
			FlagProfile:   "nicos",
			FlagNoProfile: false,
			OutDir:        "docs-out",
			Cwd:           "cwd",
		},
		Resolve: func(options ProfileResolveOptions) (string, string) {
			gotOptions = options
			return "nicos", "flag-explicit"
		},
		Load: func(name, outDir string) (string, error) {
			gotLoadName = name
			gotLoadOut = outDir
			return "loaded-profile", nil
		},
	})
	if gotOptions.FlagProfile != "nicos" || gotOptions.OutDir != "docs-out" || gotOptions.Cwd != "cwd" {
		t.Fatalf("resolve options = %+v", gotOptions)
	}
	if gotLoadName != "nicos" || gotLoadOut != "docs-out" {
		t.Fatalf("load inputs = name=%q out=%q", gotLoadName, gotLoadOut)
	}
	if result.Name != "nicos" || result.Reason != "flag-explicit" || result.Profile != "loaded-profile" || !result.Loaded {
		t.Fatalf("result = %+v, want loaded nicos profile", result)
	}
}

func TestResolveSearchProfileLoadFailureDegradesAndWarns(t *testing.T) {
	var loadWarning string
	var strictWarning bool
	result := ResolveSearchProfile(ProfileSelectionInput[string]{
		Strict: true,
		Options: ProfileResolveOptions{
			OutDir: "docs-out",
		},
		Resolve: func(ProfileResolveOptions) (string, string) {
			return "missing", "out-pin"
		},
		Load: func(string, string) (string, error) {
			return "", errors.New("not found")
		},
		WarnLoadFailed: func(name string, err error) {
			loadWarning = name + ": " + err.Error()
		},
		WarnStrictWithoutProfile: func() {
			strictWarning = true
		},
	})
	if result.Name != "" || result.Reason != "load-failed" || result.Profile != "" || result.Loaded {
		t.Fatalf("result = %+v, want load-failed no profile", result)
	}
	if loadWarning != "missing: not found" {
		t.Fatalf("load warning = %q, want missing: not found", loadWarning)
	}
	if !strictWarning {
		t.Fatalf("strict warning did not fire")
	}
}

func TestResolveSearchProfileMissingLoadWarnsWithRuntimeError(t *testing.T) {
	var loadWarning string
	result := ResolveSearchProfile(ProfileSelectionInput[string]{
		Resolve: func(ProfileResolveOptions) (string, string) {
			return "missing", "out-pin"
		},
		WarnLoadFailed: func(name string, err error) {
			loadWarning = name + ": " + err.Error()
		},
	})
	if result.Name != "" || result.Reason != "load-failed" || result.Profile != "" || result.Loaded {
		t.Fatalf("result = %+v, want load-failed no profile", result)
	}
	if loadWarning != "missing: "+ProfileLoadCallMissingError().Error() {
		t.Fatalf("load warning = %q, want missing runtime profile-load error", loadWarning)
	}
}

func TestResolveSearchProfileStrictWithoutActiveProfileWarns(t *testing.T) {
	var loaded bool
	var strictWarning bool
	result := ResolveSearchProfile(ProfileSelectionInput[string]{
		Strict: true,
		Resolve: func(ProfileResolveOptions) (string, string) {
			return "", "none"
		},
		Load: func(string, string) (string, error) {
			loaded = true
			return "unexpected", nil
		},
		WarnStrictWithoutProfile: func() {
			strictWarning = true
		},
	})
	if result.Name != "" || result.Reason != "none" || result.Loaded {
		t.Fatalf("result = %+v, want inactive profile", result)
	}
	if loaded {
		t.Fatalf("load should not run without a resolved profile name")
	}
	if !strictWarning {
		t.Fatalf("strict warning did not fire")
	}
}

func TestSearchProfileWarnings(t *testing.T) {
	err := errors.New("not found")
	if got, want := SearchProfileLoadFailedWarning("missing", err), "search: profile \"missing\" failed to load (continuing without profile): not found\n"; got != want {
		t.Fatalf("load warning = %q, want %q", got, want)
	}
	if got, want := SearchStrictWithoutProfileWarning(), "search: --strict has no effect without an active profile\n"; got != want {
		t.Fatalf("strict warning = %q, want %q", got, want)
	}
}

func TestProfileLoadErrors(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name  string
		err   error
		want  string
		wraps bool
	}{
		{"missing load callback", ProfileLoadCallMissingError(), "profile load call is nil", false},
		{"required name", ProfileNameRequiredError(), "profile name is required", false},
		{"not found", ProfileNotFoundError("missing"), "profile \"missing\": not found in <out>/profiles or embedded set", false},
		{"parse", ProfileParseError("nicos", cause), "profile \"nicos\": parse: boom", true},
		{"name mismatch", ProfileNameMismatchError("nicos", "other"), "profile \"nicos\": name field is \"other\", expected \"nicos\"", false},
		{"no sources", ProfileNoSourcesError("nicos"), "profile \"nicos\": must declare at least one source", false},
		{"missing source id", ProfileSourceMissingIDError("nicos"), "profile \"nicos\": source entry missing id", false},
		{"empty glob", ProfileEmptyGlobError(), "empty glob", false},
		{"glob", ProfileSourceGlobError("nicos", "react", "**/[", cause), "profile \"nicos\": source \"react\": glob \"**/[\": boom", true},
	}
	for _, tc := range cases {
		if tc.err == nil || tc.err.Error() != tc.want {
			t.Fatalf("%s error = %v, want %q", tc.name, tc.err, tc.want)
		}
		if tc.wraps && !errors.Is(tc.err, cause) {
			t.Fatalf("%s error = %v, want wrapping cause", tc.name, tc.err)
		}
		if !tc.wraps && errors.Is(tc.err, cause) {
			t.Fatalf("%s error unexpectedly wraps cause: %v", tc.name, tc.err)
		}
	}
}

func TestRunVersionPolicyFTSQueryDirectFallback(t *testing.T) {
	var directCalled bool
	got, err := RunVersionPolicyFTSQuery(VersionPolicyFTSQueryInput{
		UseFamilyFanout: false,
		SourceIDs:       []string{"docs__v1"},
		Direct: func() ([]Hit, error) {
			directCalled = true
			return []Hit{{Path: "direct.md", Score: 10}}, nil
		},
		SearchSource: func(string) ([]Hit, error) {
			t.Fatalf("SearchSource should not run when family fan-out is disabled")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("RunVersionPolicyFTSQuery returned error: %v", err)
	}
	if !directCalled || len(got) != 1 || got[0].Path != "direct.md" {
		t.Fatalf("hits = %+v directCalled=%v, want direct fallback", got, directCalled)
	}
}

func TestRunVersionPolicyFTSQueryRejectsMissingDirect(t *testing.T) {
	got, err := RunVersionPolicyFTSQuery(VersionPolicyFTSQueryInput{
		UseFamilyFanout: false,
		SourceIDs:       []string{"docs__v1"},
	})
	if err == nil || err.Error() != DirectFTSQueryCallMissingError().Error() {
		t.Fatalf("expected missing direct FTS query error %q, got hits=%+v err=%v", DirectFTSQueryCallMissingError().Error(), got, err)
	}
}

func TestFTSIndexSetupErrors(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"busy timeout", FTSIndexBusyTimeoutError(cause), "set busy_timeout: boom"},
		{"journal mode", FTSIndexJournalModeError(cause), "set journal_mode=WAL: boom"},
		{"synchronous mode", FTSIndexSynchronousModeError(cause), "set synchronous=NORMAL: boom"},
		{"migrate schema", FTSIndexMigrateSchemaError(cause), "migrate schema: boom"},
		{"init schema", FTSIndexInitSchemaError(cause), "init schema: boom"},
	}
	for _, tc := range cases {
		if tc.err == nil || tc.err.Error() != tc.want || !errors.Is(tc.err, cause) {
			t.Fatalf("%s error = %v, want %q wrapping cause", tc.name, tc.err, tc.want)
		}
	}
}

func TestFTSReadModeErrors(t *testing.T) {
	err := UnsupportedFTSReadModeError("fast")
	if err == nil || err.Error() != `unsupported FTS read mode "fast"` {
		t.Fatalf("UnsupportedFTSReadModeError = %v", err)
	}

	err = ImmutableFTSReadRequiresCheckpointError("/tmp/search.db-wal")
	if err == nil || err.Error() != "immutable FTS read requires checkpointed index: WAL file exists at /tmp/search.db-wal" {
		t.Fatalf("ImmutableFTSReadRequiresCheckpointError = %v", err)
	}
}

func TestSearchBatchFTSErrors(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"index open", SearchBatchFTSIndexOpenError(cause), "open FTS5 index: boom"},
		{"doc count", SearchBatchFTSDocCountError(cause), "count FTS5 docs: boom"},
	}
	for _, tc := range cases {
		if tc.err == nil || tc.err.Error() != tc.want || !errors.Is(tc.err, cause) {
			t.Fatalf("%s error = %v, want %q wrapping cause", tc.name, tc.err, tc.want)
		}
	}

	if err := SearchBatchFTSNotReadyError(30 * time.Second); err == nil || err.Error() != "FTS5 index not ready after 30s" {
		t.Fatalf("SearchBatchFTSNotReadyError = %v", err)
	}
	if err := SearchBatchEmptyQueryError(); err == nil || err.Error() != "empty query" {
		t.Fatalf("SearchBatchEmptyQueryError = %v", err)
	}
	if err := SearchBatchNonFTSModeError("needle"); err == nil || err.Error() != `query "needle" did not use FTS5` {
		t.Fatalf("SearchBatchNonFTSModeError = %v", err)
	}
}

func TestRunVersionPolicyFTSQueryMergesFamilySources(t *testing.T) {
	var gotSources []string
	got, err := RunVersionPolicyFTSQuery(VersionPolicyFTSQueryInput{
		UseFamilyFanout: true,
		SourceIDs:       []string{"docs__v1", "docs__v2"},
		Direct: func() ([]Hit, error) {
			t.Fatalf("Direct should not run when family source IDs exist")
			return nil, nil
		},
		SearchSource: func(sourceID string) ([]Hit, error) {
			gotSources = append(gotSources, sourceID)
			switch sourceID {
			case "docs__v1":
				return []Hit{
					{Path: "b.md", Score: 20},
					{Path: "same.md", Score: 5},
				}, nil
			case "docs__v2":
				return []Hit{
					{Path: "a.md", Score: 20},
					{Path: "same.md", Score: 30},
				}, nil
			default:
				t.Fatalf("unexpected sourceID %q", sourceID)
				return nil, nil
			}
		},
	})
	if err != nil {
		t.Fatalf("RunVersionPolicyFTSQuery returned error: %v", err)
	}
	if strings.Join(gotSources, ",") != "docs__v1,docs__v2" {
		t.Fatalf("sources = %v, want input order", gotSources)
	}
	if len(got) != 3 {
		t.Fatalf("hits = %+v, want 3 merged hits", got)
	}
	if got[0].Path != "same.md" || got[0].Score != 30 || got[1].Path != "a.md" || got[2].Path != "b.md" {
		t.Fatalf("hits = %+v, want best duplicate score then score/path sort", got)
	}
}

func TestRunVersionPolicyFTSQueryRejectsMissingSourceQuery(t *testing.T) {
	got, err := RunVersionPolicyFTSQuery(VersionPolicyFTSQueryInput{
		UseFamilyFanout: true,
		SourceIDs:       []string{"docs__v1"},
		Direct: func() ([]Hit, error) {
			t.Fatalf("Direct should not run when family fan-out is enabled")
			return nil, nil
		},
	})
	if err == nil || err.Error() != SourceFTSQueryCallMissingError().Error() {
		t.Fatalf("expected missing source FTS query error %q, got hits=%+v err=%v", SourceFTSQueryCallMissingError().Error(), got, err)
	}
}

func TestRunVersionPolicyFTSQueryPropagatesSourceError(t *testing.T) {
	got, err := RunVersionPolicyFTSQuery(VersionPolicyFTSQueryInput{
		UseFamilyFanout: true,
		SourceIDs:       []string{"docs__v1"},
		SearchSource: func(string) ([]Hit, error) {
			return nil, errors.New("query failed")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "query failed") {
		t.Fatalf("expected source query error, got hits=%+v err=%v", got, err)
	}
}

func TestVersionPolicyNeedsWideCandidatePool(t *testing.T) {
	if VersionPolicyNeedsWideCandidatePool(VersionPolicyState{}) {
		t.Fatalf("empty version policy state should not need wide candidate pool")
	}
	cases := map[string]VersionPolicyState{
		"family":      {SourceFamily: "react"},
		"source":      {SourceID: "react__v18"},
		"version":     {Version: "18"},
		"latest-only": {LatestOnly: true},
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			if !VersionPolicyNeedsWideCandidatePool(state) {
				t.Fatalf("state %+v should need wide candidate pool", state)
			}
		})
	}
}

func TestVersionPolicyHardFilterOptions(t *testing.T) {
	got := VersionPolicyHardFilterOptions(VersionPolicyState{
		SourceFamily: "react",
		SourceID:     "react__v18",
		Version:      "18",
		LatestOnly:   true,
		CwdScope:     "workspace",
		PreferLatest: true,
	})
	want := VersionPolicyOptions{
		Active:     true,
		SourceID:   "react__v18",
		LatestOnly: true,
		Version:    "18",
	}
	if got != want {
		t.Fatalf("hard filter options = %+v, want %+v", got, want)
	}
	soft := VersionPolicyHardFilterOptions(VersionPolicyState{
		SourceFamily: "react",
		CwdScope:     "workspace",
		PreferLatest: true,
	})
	if !soft.Active || soft.SourceID != "" || soft.LatestOnly || soft.Version != "" {
		t.Fatalf("soft policy hard filter options = %+v, want active with no hard constraints", soft)
	}
}

func TestApplyVersionPolicySourceInfo(t *testing.T) {
	got := ApplyVersionPolicySourceInfo(Hit{Path: "react/hooks.md", Source: "react__v18", Score: 42}, VersionPolicySourceInfo{
		SourceFamily: "react",
		SourceID:     "react__v18",
		DocsVersion:  "18",
		VersionLane:  "latest",
		PinScope:     "workspace",
	})
	if got.Path != "react/hooks.md" || got.Source != "react__v18" || got.Score != 42 {
		t.Fatalf("base hit fields changed: %+v", got)
	}
	if got.SourceFamily != "react" || got.SourceID != "react__v18" || got.DocsVersion != "18" || got.VersionLane != "latest" || got.PinScope != "workspace" {
		t.Fatalf("version source info not applied: %+v", got)
	}
}

func TestApplyAndMatchVersionPolicySourceInfo(t *testing.T) {
	hit := Hit{Path: "react/hooks.md", Source: "react__v18"}
	info := VersionPolicySourceInfo{
		SourceFamily: "react",
		SourceID:     "react__v18",
		DocsVersion:  "18",
		VersionLane:  "latest",
		PinScope:     "workspace",
	}
	got, ok := ApplyAndMatchVersionPolicySourceInfo(hit, info, VersionPolicyState{SourceFamily: "react", Version: "18"})
	if !ok {
		t.Fatalf("expected enriched hit to match")
	}
	if got.SourceFamily != "react" || got.SourceID != "react__v18" || got.DocsVersion != "18" || got.VersionLane != "latest" || got.PinScope != "workspace" {
		t.Fatalf("version source info not applied before match: %+v", got)
	}

	mismatch, ok := ApplyAndMatchVersionPolicySourceInfo(hit, info, VersionPolicyState{SourceFamily: "vue"})
	if ok {
		t.Fatalf("mismatch hit matched: %+v", mismatch)
	}
	if mismatch.SourceFamily != "react" || mismatch.SourceID != "react__v18" {
		t.Fatalf("mismatch should still return enriched hit: %+v", mismatch)
	}
}

func TestResolveVersionPolicySourceInfo(t *testing.T) {
	pinned := ResolveVersionPolicySourceInfo(VersionPolicySourceInfoResolveInput{
		Source:          "react__v18",
		LatestLane:      "latest",
		OtherPinnedLane: "other-pinned",
		PinnedSourceInfo: func(source string) (VersionPolicySourceInfo, bool) {
			if source != "react__v18" {
				t.Fatalf("pinned source lookup = %q, want react__v18", source)
			}
			return VersionPolicySourceInfo{
				SourceFamily: "react",
				SourceID:     "react__v18",
				DocsVersion:  "18",
				VersionLane:  "workspace-pinned",
				PinScope:     "nicos-app",
			}, true
		},
		ParsePinnedSourceID: func(string) (string, string, bool) {
			t.Fatalf("parse should not run after pinned source match")
			return "", "", false
		},
	})
	if pinned.SourceFamily != "react" || pinned.SourceID != "react__v18" || pinned.DocsVersion != "18" || pinned.VersionLane != "workspace-pinned" || pinned.PinScope != "nicos-app" {
		t.Fatalf("pinned source info = %+v", pinned)
	}

	parsed := ResolveVersionPolicySourceInfo(VersionPolicySourceInfoResolveInput{
		Source:          "react__v17",
		LatestLane:      "latest",
		OtherPinnedLane: "other-pinned",
		ParsePinnedSourceID: func(source string) (string, string, bool) {
			if source != "react__v17" {
				t.Fatalf("parse source = %q, want react__v17", source)
			}
			return "react", "17", true
		},
		LatestVersion: func(string) (string, bool) {
			t.Fatalf("latest lookup should not run after parsed pinned source")
			return "", false
		},
	})
	if parsed.SourceFamily != "react" || parsed.SourceID != "react__v17" || parsed.DocsVersion != "17" || parsed.VersionLane != "other-pinned" || parsed.PinScope != "" {
		t.Fatalf("parsed source info = %+v", parsed)
	}

	latest := ResolveVersionPolicySourceInfo(VersionPolicySourceInfoResolveInput{
		Source:          "react",
		LatestLane:      "latest",
		OtherPinnedLane: "other-pinned",
		LatestVersion: func(source string) (string, bool) {
			if source != "react" {
				t.Fatalf("latest source = %q, want react", source)
			}
			return "19", true
		},
	})
	if latest.SourceFamily != "react" || latest.SourceID != "react" || latest.DocsVersion != "19" || latest.VersionLane != "latest" {
		t.Fatalf("latest source info = %+v", latest)
	}

	defaultInfo := ResolveVersionPolicySourceInfo(VersionPolicySourceInfoResolveInput{
		Source:     "postgresql",
		LatestLane: "latest",
	})
	if defaultInfo.SourceFamily != "postgresql" || defaultInfo.SourceID != "postgresql" || defaultInfo.DocsVersion != "" || defaultInfo.VersionLane != "latest" {
		t.Fatalf("default source info = %+v", defaultInfo)
	}
}

func TestVersionPolicyFamilySourceIDs(t *testing.T) {
	got := VersionPolicyFamilySourceIDs("react", []VersionPolicyFamilySource{
		{Source: "vue__v3", SourceFamily: "vue"},
		{Source: "react__v18", SourceFamily: "react"},
		{Source: "react__v17", SourceFamily: "react"},
	})
	if len(got) != 2 || got[0] != "react__v17" || got[1] != "react__v18" {
		t.Fatalf("source IDs = %v, want sorted matching react sources", got)
	}
	if empty := VersionPolicyFamilySourceIDs("", []VersionPolicyFamilySource{{Source: "react__v18", SourceFamily: "react"}}); len(empty) != 0 {
		t.Fatalf("empty family source IDs = %v, want none", empty)
	}
}

func TestVersionPolicyFamilySourceIDsFromSources(t *testing.T) {
	got := VersionPolicyFamilySourceIDsFromSources("react", []string{"vue__v3", "react__v18", "react__v17"}, func(source string) VersionPolicySourceInfo {
		switch source {
		case "react__v18", "react__v17":
			return VersionPolicySourceInfo{SourceFamily: "react"}
		default:
			return VersionPolicySourceInfo{SourceFamily: "vue"}
		}
	})
	if len(got) != 2 || got[0] != "react__v17" || got[1] != "react__v18" {
		t.Fatalf("source IDs from sources = %v, want sorted matching react sources", got)
	}
	if empty := VersionPolicyFamilySourceIDsFromSources("", []string{"react__v18"}, nil); len(empty) != 0 {
		t.Fatalf("empty family source IDs from sources = %v, want none", empty)
	}
}

func TestParseVersionPolicyPinnedSourceID(t *testing.T) {
	family, version, ok := ParseVersionPolicyPinnedSourceID("react__v18", func(family string) bool {
		return family == "react"
	})
	if !ok || family != "react" || version != "18" {
		t.Fatalf("parsed source ID = family %q version %q ok %v, want react 18 true", family, version, ok)
	}
	rejectedFamily, rejectedVersion, ok := ParseVersionPolicyPinnedSourceID("react__v18", func(family string) bool {
		return family == "vue"
	})
	if ok || rejectedFamily != "" || rejectedVersion != "" {
		t.Fatalf("unknown family source ID = family %q version %q ok %v, want empty empty false", rejectedFamily, rejectedVersion, ok)
	}
	unversionedFamily, unversionedVersion, ok := ParseVersionPolicyPinnedSourceID("react", nil)
	if ok || unversionedFamily != "" || unversionedVersion != "" {
		t.Fatalf("unversioned source ID = family %q version %q ok %v, want empty empty false", unversionedFamily, unversionedVersion, ok)
	}
}

func TestVersionPolicyParseErrors(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"embedded", VersionPolicyEmbeddedParseError(cause), "parse embedded version policy: boom"},
		{"file", VersionPolicyFileParseError("/tmp/package.json", cause), "parse /tmp/package.json: boom"},
	}
	for _, tc := range cases {
		if tc.err == nil || tc.err.Error() != tc.want || !errors.Is(tc.err, cause) {
			t.Fatalf("%s error = %v, want %q wrapping cause", tc.name, tc.err, tc.want)
		}
	}
}

func TestVersionPolicyPinnedPageErrors(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"fetch", VersionPolicyPinnedPageFetchError("react__v18", "https://18.react.dev/reference/react", cause), "react__v18 https://18.react.dev/reference/react: boom"},
		{"path", VersionPolicyPinnedPagePathError("react__v18", "../escape.md", cause), "react__v18 ../escape.md: boom"},
	}
	for _, tc := range cases {
		if tc.err == nil || tc.err.Error() != tc.want || !errors.Is(tc.err, cause) {
			t.Fatalf("%s error = %v, want %q wrapping cause", tc.name, tc.err, tc.want)
		}
	}
}

func TestVersionPolicyHeader(t *testing.T) {
	got := VersionPolicyHeader(VersionPolicyState{
		SourceFamily: "react",
		SourceID:     "react__v18",
		Version:      "18",
		CwdScope:     "workspace",
		PreferLatest: true,
	})
	want := "family:react,source:react__v18,override:18,cwd:workspace,prefer:latest"
	if got != want {
		t.Fatalf("header = %q, want %q", got, want)
	}
	if empty := VersionPolicyHeader(VersionPolicyState{}); empty != "" {
		t.Fatalf("empty header = %q, want empty", empty)
	}
}

func TestVersionPolicyHitMatches(t *testing.T) {
	cases := []struct {
		name  string
		hit   Hit
		state VersionPolicyState
		want  bool
	}{
		{
			name:  "empty policy matches",
			hit:   Hit{Source: "react", SourceFamily: "react"},
			state: VersionPolicyState{},
			want:  true,
		},
		{
			name:  "source ID matches enriched source ID",
			hit:   Hit{Source: "react", SourceID: "react__v18"},
			state: VersionPolicyState{SourceID: "react__v18"},
			want:  true,
		},
		{
			name:  "source ID matches raw source",
			hit:   Hit{Source: "react__v18"},
			state: VersionPolicyState{SourceID: "react__v18"},
			want:  true,
		},
		{
			name:  "source family mismatch drops hit",
			hit:   Hit{SourceFamily: "vue"},
			state: VersionPolicyState{SourceFamily: "react"},
			want:  false,
		},
		{
			name:  "latest only matches latest lane",
			hit:   Hit{SourceFamily: "react", VersionLane: "latest"},
			state: VersionPolicyState{SourceFamily: "react", LatestOnly: true},
			want:  true,
		},
		{
			name:  "latest only rejects workspace lane",
			hit:   Hit{SourceFamily: "react", VersionLane: "workspace-pinned"},
			state: VersionPolicyState{SourceFamily: "react", LatestOnly: true},
			want:  false,
		},
		{
			name:  "version matches docs version",
			hit:   Hit{SourceFamily: "react", DocsVersion: "18"},
			state: VersionPolicyState{SourceFamily: "react", Version: "18"},
			want:  true,
		},
		{
			name:  "version matches source ID suffix",
			hit:   Hit{SourceFamily: "react", SourceID: "react__v18"},
			state: VersionPolicyState{SourceFamily: "react", Version: "18"},
			want:  true,
		},
		{
			name:  "version mismatch rejects hit",
			hit:   Hit{SourceFamily: "react", SourceID: "react__v17", DocsVersion: "17"},
			state: VersionPolicyState{SourceFamily: "react", Version: "18"},
			want:  false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := VersionPolicyHitMatches(tt.hit, tt.state); got != tt.want {
				t.Fatalf("VersionPolicyHitMatches(%+v, %+v) = %v, want %v", tt.hit, tt.state, got, tt.want)
			}
		})
	}
}

func TestBuildSearchOutputMetadataVersionOnly(t *testing.T) {
	got := BuildSearchOutputMetadata(SearchOutputOptions{
		Version: VersionPolicyState{
			SourceFamily: "react",
			SourceID:     "react__v18",
			Version:      "18",
			CwdScope:     "workspace",
			PreferLatest: true,
		},
	})
	wantHeader := " version=family:react,source:react__v18,override:18,cwd:workspace,prefer:latest"
	if got.Header != wantHeader {
		t.Fatalf("header = %q, want %q", got.Header, wantHeader)
	}
	if got.Version != "18" || got.PinContext != "workspace" {
		t.Fatalf("version metadata = %+v, want version and pin context", got)
	}
	if got.ProfileMode != "" {
		t.Fatalf("profile mode = %q, want empty", got.ProfileMode)
	}
}

func TestBuildSearchOutputMetadataProfileStrictWithVersion(t *testing.T) {
	got := BuildSearchOutputMetadata(SearchOutputOptions{
		ProfileName:   "nicos",
		ProfileReason: "flag-explicit",
		ProfileStrict: true,
		Version: VersionPolicyState{
			SourceFamily: "react",
			SourceID:     "react__v18",
			Version:      "18",
		},
	})
	wantHeader := " profile=nicos (flag-explicit; strict) version=family:react,source:react__v18,override:18"
	if got.Header != wantHeader {
		t.Fatalf("header = %q, want %q", got.Header, wantHeader)
	}
	if got.ProfileMode != "strict" || got.Version != "18" {
		t.Fatalf("metadata = %+v, want strict profile mode and version", got)
	}
}

func TestBuildSearchOutputMetadataProfileBoostWithoutVersion(t *testing.T) {
	got := BuildSearchOutputMetadata(SearchOutputOptions{
		ProfileName:   "nicos",
		ProfileReason: "cwd",
	})
	wantHeader := " profile=nicos (cwd; boost)"
	if got.Header != wantHeader {
		t.Fatalf("header = %q, want %q", got.Header, wantHeader)
	}
	if got.ProfileMode != "boost" || got.Version != "" || got.PinContext != "" {
		t.Fatalf("metadata = %+v, want boost profile only", got)
	}
}

func TestNewSearchJSONEnvelope(t *testing.T) {
	hits := []Hit{{Path: "react/guide.md", Source: "react", Score: 42}}
	got := NewSearchJSONEnvelope("hooks", "fts5", 123, SearchOutputOptions{
		ProfileName:   "nicos",
		ProfileReason: "flag-explicit",
		ProfileStrict: true,
		Version: VersionPolicyState{
			Version:  "18",
			CwdScope: "workspace",
		},
	}, hits)
	if got.Query != "hooks" || got.Mode != "fts5" || got.Scanned != 123 {
		t.Fatalf("envelope query/mode/scanned = %+v", got)
	}
	if got.Profile != "nicos" || got.ProfileReason != "flag-explicit" || got.ProfileMode != "strict" {
		t.Fatalf("profile metadata = %+v", got)
	}
	if got.Version != "18" || got.PinContext != "workspace" {
		t.Fatalf("version metadata = %+v", got)
	}
	if len(got.Results) != 1 || got.Results[0].Path != "react/guide.md" || got.Results[0].Score != 42 {
		t.Fatalf("results = %+v, want original hits", got.Results)
	}
}

func TestSearchTextHeaderMessages(t *testing.T) {
	header := " profile=nicos (cwd; boost)"
	if got, want := SearchTextNoMatchesMessage("hooks", 12, "fts5", header), "no matches for \"hooks\" across 12 docs (fts5) profile=nicos (cwd; boost)\n"; got != want {
		t.Fatalf("no-match message = %q, want %q", got, want)
	}
	if got, want := SearchTextMatchesHeaderMessage(1, "hooks", 12, "fts5", header), "1 match for \"hooks\" across 12 docs (fts5) profile=nicos (cwd; boost):\n\n"; got != want {
		t.Fatalf("singular header = %q, want %q", got, want)
	}
	if got, want := SearchTextMatchesHeaderMessage(2, "hooks", 12, "fts5", header), "2 matches for \"hooks\" across 12 docs (fts5) profile=nicos (cwd; boost):\n\n"; got != want {
		t.Fatalf("plural header = %q, want %q", got, want)
	}
}

func TestSearchJSONOutput(t *testing.T) {
	got, err := SearchJSONOutput("hooks", "fts5", 123, SearchOutputOptions{
		ProfileName:   "nicos",
		ProfileReason: "flag-explicit",
		ProfileStrict: true,
		Version: VersionPolicyState{
			Version:  "18",
			CwdScope: "workspace",
		},
	}, []Hit{{Path: "react/guide.md", Source: "react", Score: 42}})
	if err != nil {
		t.Fatalf("SearchJSONOutput error: %v", err)
	}
	want := "{\n" +
		"  \"query\": \"hooks\",\n" +
		"  \"mode\": \"fts5\",\n" +
		"  \"scanned\": 123,\n" +
		"  \"profile\": \"nicos\",\n" +
		"  \"profile_reason\": \"flag-explicit\",\n" +
		"  \"profile_mode\": \"strict\",\n" +
		"  \"version\": \"18\",\n" +
		"  \"pin_context\": \"workspace\",\n" +
		"  \"results\": [\n" +
		"    {\n" +
		"      \"path\": \"react/guide.md\",\n" +
		"      \"source\": \"react\",\n" +
		"      \"score\": 42\n" +
		"    }\n" +
		"  ]\n" +
		"}\n"
	if got != want {
		t.Fatalf("json output = %q, want %q", got, want)
	}
}

func TestSearchJSONEncodeFailedMessage(t *testing.T) {
	err := errors.New("bad value")
	if got, want := SearchJSONEncodeFailedMessage(err), "search: encode json: bad value\n"; got != want {
		t.Fatalf("encode warning = %q, want %q", got, want)
	}
}

func TestSearchTextOutput(t *testing.T) {
	opts := SearchOutputOptions{
		ProfileName:   "nicos",
		ProfileReason: "cwd",
	}
	if got, want := SearchTextOutput("hooks", 12, nil, "fts5", opts), "no matches for \"hooks\" across 12 docs (fts5) profile=nicos (cwd; boost)\n"; got != want {
		t.Fatalf("no-hit output = %q, want %q", got, want)
	}

	got := SearchTextOutput("hooks", 12, []Hit{
		{
			Path:  "react/hooks.md",
			Score: 42,
			Title: "Hooks",
			Snippets: []Snippet{
				{Line: 12, Text: "shared"},
			},
		},
		{
			Path:  "react/effect.md",
			Score: 21,
			Title: "Effects",
			Snippets: []Snippet{
				{Line: 8, Text: "shared"},
				{Line: 9, Text: "unique"},
			},
		},
	}, "fts5", opts)
	want := "2 matches for \"hooks\" across 12 docs (fts5) profile=nicos (cwd; boost):\n\n" +
		"react/hooks.md  [42]  Hooks\n" +
		"  L12: shared\n\n" +
		"react/effect.md  [21]  Effects\n" +
		"  L9: unique\n\n"
	if got != want {
		t.Fatalf("text output = %q, want %q", got, want)
	}
}

func TestSearchTextHitBlock(t *testing.T) {
	got := SearchTextHitBlock(Hit{
		Path:         "react/hooks.md",
		Score:        42,
		Title:        "Hooks",
		URL:          "https://react.dev/hooks",
		InProfileSub: true,
		Snippets: []Snippet{
			{Line: 12, Text: "useEffect synchronizes with external systems."},
			{Line: 18, Text: "useMemo caches calculations."},
		},
	})
	want := "[profile*] react/hooks.md  [42]  Hooks\n" +
		"  <https://react.dev/hooks>\n" +
		"  L12: useEffect synchronizes with external systems.\n" +
		"  L18: useMemo caches calculations.\n\n"
	if got != want {
		t.Fatalf("hit block = %q, want %q", got, want)
	}

	got = SearchTextHitBlock(Hit{
		Path:      "react/untitled.md",
		Score:     7,
		InProfile: true,
	})
	want = "[profile] react/untitled.md  [7]  (untitled)\n\n"
	if got != want {
		t.Fatalf("fallback hit block = %q, want %q", got, want)
	}
}

func TestNewSearchTelemetryEntry(t *testing.T) {
	ts := time.Date(2026, 5, 5, 17, 15, 0, 0, time.FixedZone("test", -5*60*60))
	got := NewSearchTelemetryEntry(SearchTelemetryInput{
		Timestamp:    ts,
		Query:        "react hooks",
		Intent:       "support",
		SourceFilter: "react",
		Mode:         "fts5",
		Scanned:      42,
		Limit:        5,
		Output: SearchOutputOptions{
			ProfileName: "nicos",
			Version: VersionPolicyState{
				Version:  "18",
				CwdScope: "workspace",
			},
		},
		Hits: []Hit{{
			Path:         "react/reference/react/hooks.md",
			Source:       "react",
			SourceFamily: "react",
		}},
		RerankLLM:    true,
		RerankHybrid: true,
	})
	if got.Timestamp != "2026-05-05T22:15:00Z" || got.Query != "react hooks" || got.Intent != "support" {
		t.Fatalf("identity metadata = %+v", got)
	}
	if got.SourceFilter != "react" || got.Profile != "nicos" || got.Version != "18" || got.PinContext != "workspace" {
		t.Fatalf("output metadata = %+v", got)
	}
	if got.Mode != "fts5" || got.Scanned != 42 || got.Limit != 5 || got.ResultCount != 1 {
		t.Fatalf("runtime metadata = %+v", got)
	}
	if got.TopPath != "react/reference/react/hooks.md" || got.TopSource != "react" || got.TopSourceFamily != "react" {
		t.Fatalf("top hit metadata = %+v", got)
	}
	if !got.RerankLLM || !got.RerankHybrid || got.Rerank {
		t.Fatalf("rerank flags = %+v", got)
	}
}

func TestShouldLogSearchQuery(t *testing.T) {
	for _, env := range []string{"0", "false", "no", " FALSE "} {
		if ShouldLogSearchQuery(true, env) {
			t.Fatalf("ShouldLogSearchQuery(true, %q) = true, want false", env)
		}
	}
	if ShouldLogSearchQuery(false, "") {
		t.Fatalf("ShouldLogSearchQuery(false, empty env) = true, want false")
	}
	if !ShouldLogSearchQuery(true, "") {
		t.Fatalf("ShouldLogSearchQuery(true, empty env) = false, want true")
	}
	if !ShouldLogSearchQuery(true, "yes") {
		t.Fatalf("ShouldLogSearchQuery(true, yes env) = false, want true")
	}
}

func TestSearchTelemetryQueryKey(t *testing.T) {
	if got, want := SearchTelemetryQueryKey("  Row Level Security  "), "row level security"; got != want {
		t.Fatalf("SearchTelemetryQueryKey = %q, want %q", got, want)
	}
}

func TestSearchTelemetryQueryLogPath(t *testing.T) {
	got := SearchTelemetryQueryLogPath("/tmp/docs")
	want := filepath.Join("/tmp/docs", ".cache", "query-log.jsonl")
	if got != want {
		t.Fatalf("SearchTelemetryQueryLogPath = %q, want %q", got, want)
	}
}

func TestSearchTelemetryEmptyLogMessage(t *testing.T) {
	got := SearchTelemetryEmptyLogMessage("/tmp/docs/.cache/query-log.jsonl")
	want := "no query telemetry at /tmp/docs/.cache/query-log.jsonl\n"
	if got != want {
		t.Fatalf("SearchTelemetryEmptyLogMessage = %q, want %q", got, want)
	}
}

func TestSearchTelemetryUsage(t *testing.T) {
	got := SearchTelemetryUsage()
	want := `docs-puller telemetry — inspect search query logs

Usage:
  docs-puller telemetry log                 [--out DIR] [--limit N] [--json]
  docs-puller telemetry fixture --out-file PATH
                                             [--out DIR] [--limit N] [--intent LABEL] [--since RFC3339]
                                             [--exclude-fixture PATH[,PATH...]]

Search telemetry is ON by default since 2026-05-04 (needed to grow the
production-telemetry fixture). Disable per-call with ` + "`--log-query=false`" + `
or globally with ` + "`DOCS_PULLER_QUERY_LOG=0`" + `.
`
	if got != want {
		t.Fatalf("SearchTelemetryUsage = %q, want %q", got, want)
	}
}

func TestSearchTelemetryFixtureOutFileRequiredMessage(t *testing.T) {
	got := SearchTelemetryFixtureOutFileRequiredMessage()
	want := "telemetry fixture: --out-file is required\n"
	if got != want {
		t.Fatalf("SearchTelemetryFixtureOutFileRequiredMessage = %q, want %q", got, want)
	}
}

func TestSearchTelemetryUnknownSubcommandMessage(t *testing.T) {
	got := SearchTelemetryUnknownSubcommandMessage("wat")
	want := "telemetry: unknown subcommand \"wat\"\n" + SearchTelemetryUsage()
	if got != want {
		t.Fatalf("SearchTelemetryUnknownSubcommandMessage = %q, want %q", got, want)
	}
}

func TestSearchTelemetryFixtureWrittenMessage(t *testing.T) {
	got := SearchTelemetryFixtureWrittenMessage(3, "fixture.yaml")
	want := "wrote 3 telemetry-derived fixture candidates to fixture.yaml\n"
	if got != want {
		t.Fatalf("SearchTelemetryFixtureWrittenMessage = %q, want %q", got, want)
	}
}

func TestSearchTelemetryFixtureNoMatchesMessage(t *testing.T) {
	got := SearchTelemetryFixtureNoMatchesMessage()
	want := "no telemetry entries matched"
	if got != want {
		t.Fatalf("SearchTelemetryFixtureNoMatchesMessage = %q, want %q", got, want)
	}
}

func TestSearchTelemetryFixtureNoMatchesError(t *testing.T) {
	err := SearchTelemetryFixtureNoMatchesError()
	if err == nil || err.Error() != "no telemetry entries matched" {
		t.Fatalf("SearchTelemetryFixtureNoMatchesError = %v", err)
	}
}

func TestSearchTelemetryFixtureCreatePathErrors(t *testing.T) {
	if err := SearchTelemetryFixtureCreateDirError("/tmp/fixtures", nil); err != nil {
		t.Fatalf("SearchTelemetryFixtureCreateDirError nil = %v, want nil", err)
	}
	createDirErr := errors.New("permission denied")
	err := SearchTelemetryFixtureCreateDirError("/tmp/fixtures", createDirErr)
	if err == nil || err.Error() != "telemetry fixture: create output dir /tmp/fixtures: permission denied" {
		t.Fatalf("SearchTelemetryFixtureCreateDirError = %v", err)
	}
	if !errors.Is(err, createDirErr) {
		t.Fatalf("SearchTelemetryFixtureCreateDirError does not wrap create dir error: %v", err)
	}

	if err := SearchTelemetryFixtureCreateFileError("/tmp/fixtures/out.yaml", nil); err != nil {
		t.Fatalf("SearchTelemetryFixtureCreateFileError nil = %v, want nil", err)
	}
	createFileErr := errors.New("read-only filesystem")
	err = SearchTelemetryFixtureCreateFileError("/tmp/fixtures/out.yaml", createFileErr)
	if err == nil || err.Error() != "telemetry fixture: create output file /tmp/fixtures/out.yaml: read-only filesystem" {
		t.Fatalf("SearchTelemetryFixtureCreateFileError = %v", err)
	}
	if !errors.Is(err, createFileErr) {
		t.Fatalf("SearchTelemetryFixtureCreateFileError does not wrap create file error: %v", err)
	}
}

func TestSearchTelemetryFixtureEncodeError(t *testing.T) {
	encodeErr := errors.New("bad fixture")
	err := SearchTelemetryFixtureEncodeError(encodeErr, nil)
	if err == nil || err.Error() != "telemetry fixture: encode yaml: bad fixture" {
		t.Fatalf("SearchTelemetryFixtureEncodeError = %v", err)
	}
	if !errors.Is(err, encodeErr) {
		t.Fatalf("SearchTelemetryFixtureEncodeError does not wrap encode error: %v", err)
	}

	closeErr := errors.New("close failed")
	err = SearchTelemetryFixtureEncodeError(encodeErr, closeErr)
	if err == nil || err.Error() != "telemetry fixture: encode yaml: bad fixture; close output file: close failed" {
		t.Fatalf("SearchTelemetryFixtureEncodeError with close error = %v", err)
	}
	if !errors.Is(err, encodeErr) {
		t.Fatalf("SearchTelemetryFixtureEncodeError does not wrap encode error with close error: %v", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("SearchTelemetryFixtureEncodeError does not wrap close error: %v", err)
	}
}

func TestSearchTelemetryFixtureCloseError(t *testing.T) {
	if err := SearchTelemetryFixtureCloseError(nil); err != nil {
		t.Fatalf("SearchTelemetryFixtureCloseError nil = %v, want nil", err)
	}

	closeErr := errors.New("close failed")
	err := SearchTelemetryFixtureCloseError(closeErr)
	if err == nil || err.Error() != "telemetry fixture: close output file: close failed" {
		t.Fatalf("SearchTelemetryFixtureCloseError = %v", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("SearchTelemetryFixtureCloseError does not wrap close error: %v", err)
	}
}

func TestSearchTelemetryQueryLogAppendPathErrors(t *testing.T) {
	if err := SearchTelemetryQueryLogCreateDirError("/tmp/cache", nil); err != nil {
		t.Fatalf("SearchTelemetryQueryLogCreateDirError nil = %v, want nil", err)
	}
	createErr := errors.New("permission denied")
	err := SearchTelemetryQueryLogCreateDirError("/tmp/cache", createErr)
	if err == nil || err.Error() != "create query-log dir /tmp/cache: permission denied" {
		t.Fatalf("SearchTelemetryQueryLogCreateDirError = %v", err)
	}
	if !errors.Is(err, createErr) {
		t.Fatalf("SearchTelemetryQueryLogCreateDirError does not wrap create error: %v", err)
	}

	if err := SearchTelemetryQueryLogAppendOpenError("/tmp/cache/query-log.jsonl", nil); err != nil {
		t.Fatalf("SearchTelemetryQueryLogAppendOpenError nil = %v, want nil", err)
	}
	openErr := errors.New("read-only filesystem")
	err = SearchTelemetryQueryLogAppendOpenError("/tmp/cache/query-log.jsonl", openErr)
	if err == nil || err.Error() != "open query-log for append /tmp/cache/query-log.jsonl: read-only filesystem" {
		t.Fatalf("SearchTelemetryQueryLogAppendOpenError = %v", err)
	}
	if !errors.Is(err, openErr) {
		t.Fatalf("SearchTelemetryQueryLogAppendOpenError does not wrap open error: %v", err)
	}
}

func TestSearchTelemetryQueryLogWriteErrors(t *testing.T) {
	encodeErr := errors.New("bad entry")
	err := SearchTelemetryQueryLogEncodeError(encodeErr, nil)
	if err == nil || err.Error() != "encode query-log json: bad entry" {
		t.Fatalf("SearchTelemetryQueryLogEncodeError = %v", err)
	}
	if !errors.Is(err, encodeErr) {
		t.Fatalf("SearchTelemetryQueryLogEncodeError does not wrap encode error: %v", err)
	}

	closeErr := errors.New("close failed")
	err = SearchTelemetryQueryLogEncodeError(encodeErr, closeErr)
	if err == nil || err.Error() != "encode query-log json: bad entry; close query-log file: close failed" {
		t.Fatalf("SearchTelemetryQueryLogEncodeError with close error = %v", err)
	}
	if !errors.Is(err, encodeErr) {
		t.Fatalf("SearchTelemetryQueryLogEncodeError does not wrap encode error with close error: %v", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("SearchTelemetryQueryLogEncodeError does not wrap close error: %v", err)
	}

	err = SearchTelemetryQueryLogCloseError(closeErr)
	if err == nil || err.Error() != "close query-log file: close failed" {
		t.Fatalf("SearchTelemetryQueryLogCloseError = %v", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("SearchTelemetryQueryLogCloseError does not wrap close error: %v", err)
	}
}

func TestSearchTelemetryQueryLogOpenError(t *testing.T) {
	if err := SearchTelemetryQueryLogOpenError("/tmp/query-log.jsonl", nil); err != nil {
		t.Fatalf("SearchTelemetryQueryLogOpenError nil = %v, want nil", err)
	}

	openErr := errors.New("permission denied")
	err := SearchTelemetryQueryLogOpenError("/tmp/query-log.jsonl", openErr)
	if err == nil || err.Error() != "open query-log /tmp/query-log.jsonl: permission denied" {
		t.Fatalf("SearchTelemetryQueryLogOpenError = %v", err)
	}
	if !errors.Is(err, openErr) {
		t.Fatalf("SearchTelemetryQueryLogOpenError does not wrap open error: %v", err)
	}
}

func TestSearchTelemetryQueryLogReadError(t *testing.T) {
	if err := SearchTelemetryQueryLogReadError(nil, nil); err != nil {
		t.Fatalf("SearchTelemetryQueryLogReadError nil = %v, want nil", err)
	}

	readErr := errors.New("scan failed")
	err := SearchTelemetryQueryLogReadError(readErr, nil)
	if err == nil || err.Error() != "read query-log: scan failed" {
		t.Fatalf("SearchTelemetryQueryLogReadError read = %v", err)
	}
	if !errors.Is(err, readErr) {
		t.Fatalf("SearchTelemetryQueryLogReadError does not wrap read error: %v", err)
	}

	closeErr := errors.New("close failed")
	err = SearchTelemetryQueryLogReadError(nil, closeErr)
	if err == nil || err.Error() != "close query-log file: close failed" {
		t.Fatalf("SearchTelemetryQueryLogReadError close = %v", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("SearchTelemetryQueryLogReadError does not wrap close error: %v", err)
	}

	err = SearchTelemetryQueryLogReadError(readErr, closeErr)
	if err == nil || err.Error() != "read query-log: scan failed; close query-log file: close failed" {
		t.Fatalf("SearchTelemetryQueryLogReadError read+close = %v", err)
	}
	if !errors.Is(err, readErr) {
		t.Fatalf("SearchTelemetryQueryLogReadError does not wrap read error with close error: %v", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("SearchTelemetryQueryLogReadError does not wrap close error with read error: %v", err)
	}
}

func TestSearchTelemetryJSONEncodeError(t *testing.T) {
	encodeErr := errors.New("broken pipe")
	err := SearchTelemetryJSONEncodeError(encodeErr)
	if err == nil || err.Error() != "telemetry log: encode json: broken pipe" {
		t.Fatalf("SearchTelemetryJSONEncodeError = %v", err)
	}
	if !errors.Is(err, encodeErr) {
		t.Fatalf("SearchTelemetryJSONEncodeError does not wrap encode error: %v", err)
	}
}

func TestSearchTelemetrySinceParseError(t *testing.T) {
	parseErr := errors.New("bad timestamp")
	err := SearchTelemetrySinceParseError(parseErr)
	if err == nil || err.Error() != "parse --since: bad timestamp" {
		t.Fatalf("SearchTelemetrySinceParseError = %v", err)
	}
	if !errors.Is(err, parseErr) {
		t.Fatalf("SearchTelemetrySinceParseError does not wrap parse error: %v", err)
	}
}

func TestSearchTelemetryExcludeFixtureLoadError(t *testing.T) {
	loadErr := errors.New("not found")
	err := SearchTelemetryExcludeFixtureLoadError("fixture.yaml", loadErr)
	if err == nil || err.Error() != "load --exclude-fixture fixture.yaml: not found" {
		t.Fatalf("SearchTelemetryExcludeFixtureLoadError = %v", err)
	}
	if !errors.Is(err, loadErr) {
		t.Fatalf("SearchTelemetryExcludeFixtureLoadError does not wrap load error: %v", err)
	}
}

func TestEvalDiagnoseLoadErrors(t *testing.T) {
	loadErr := errors.New("not found")
	err := EvalDiagnoseBaselineLoadError("baseline.json", loadErr)
	if err == nil || err.Error() != "load baseline baseline.json: not found" {
		t.Fatalf("EvalDiagnoseBaselineLoadError = %v", err)
	}
	if !errors.Is(err, loadErr) {
		t.Fatalf("EvalDiagnoseBaselineLoadError does not wrap load error: %v", err)
	}

	err = EvalDiagnoseCurrentLoadError("current.json", loadErr)
	if err == nil || err.Error() != "load current current.json: not found" {
		t.Fatalf("EvalDiagnoseCurrentLoadError = %v", err)
	}
	if !errors.Is(err, loadErr) {
		t.Fatalf("EvalDiagnoseCurrentLoadError does not wrap load error: %v", err)
	}
}

func TestArticleRedditErrors(t *testing.T) {
	fetchErr := errors.New("connection refused")
	err := ArticleRedditJSONFetchError(fetchErr)
	if err == nil || err.Error() != "reddit json fetch: connection refused" {
		t.Fatalf("ArticleRedditJSONFetchError = %v", err)
	}
	if !errors.Is(err, fetchErr) {
		t.Fatalf("ArticleRedditJSONFetchError does not wrap fetch error: %v", err)
	}

	parseErr := errors.New("bad json")
	err = ArticleRedditJSONParseError(parseErr)
	if err == nil || err.Error() != "reddit json parse: bad json" {
		t.Fatalf("ArticleRedditJSONParseError = %v", err)
	}
	if !errors.Is(err, parseErr) {
		t.Fatalf("ArticleRedditJSONParseError does not wrap parse error: %v", err)
	}

	err = ArticleRedditEmptyListingsError()
	if err == nil || err.Error() != "reddit json: empty listings" {
		t.Fatalf("ArticleRedditEmptyListingsError = %v", err)
	}

	err = ArticleRedditPostParseError(parseErr)
	if err == nil || err.Error() != "reddit post parse: bad json" {
		t.Fatalf("ArticleRedditPostParseError = %v", err)
	}
	if !errors.Is(err, parseErr) {
		t.Fatalf("ArticleRedditPostParseError does not wrap parse error: %v", err)
	}
}

func TestEvalSuiteErrors(t *testing.T) {
	diffErr := errors.New("not found")
	err := EvalSuiteDiffError("baseline.json", diffErr)
	if err == nil || err.Error() != "diff baseline.json: not found" {
		t.Fatalf("EvalSuiteDiffError = %v", err)
	}
	if !errors.Is(err, diffErr) {
		t.Fatalf("EvalSuiteDiffError does not wrap diff error: %v", err)
	}

	parseErr := errors.New("bad json")
	err = EvalSuiteParseError(parseErr)
	if err == nil || err.Error() != "parse: bad json" {
		t.Fatalf("EvalSuiteParseError = %v", err)
	}
	if !errors.Is(err, parseErr) {
		t.Fatalf("EvalSuiteParseError does not wrap parse error: %v", err)
	}

	err = EvalSuiteFixtureIncludeExcludeConflictError("fixture.yaml")
	if err == nil || err.Error() != "fixture fixture.yaml appears in both --include-fixture and --exclude-fixture" {
		t.Fatalf("EvalSuiteFixtureIncludeExcludeConflictError = %v", err)
	}

	err = EvalSuiteIncludeFixtureNotFoundError("missing.yaml", "/tmp/eval")
	if err == nil || err.Error() != "--include-fixture missing.yaml did not match a fixture in /tmp/eval" {
		t.Fatalf("EvalSuiteIncludeFixtureNotFoundError = %v", err)
	}
}

func TestEvalFixtureErrors(t *testing.T) {
	loadErr := errors.New("not found")
	err := EvalFixtureLoadError("fixture.yaml", loadErr)
	if err == nil || err.Error() != "load fixture fixture.yaml: not found" {
		t.Fatalf("EvalFixtureLoadError = %v", err)
	}
	if !errors.Is(err, loadErr) {
		t.Fatalf("EvalFixtureLoadError does not wrap load error: %v", err)
	}

	err = EvalFixtureNoQueriesError("fixture.yaml")
	if err == nil || err.Error() != "fixture fixture.yaml has no queries" {
		t.Fatalf("EvalFixtureNoQueriesError = %v", err)
	}

	parseErr := errors.New("bad yaml")
	err = EvalFixtureParseError(parseErr)
	if err == nil || err.Error() != "parse: bad yaml" {
		t.Fatalf("EvalFixtureParseError = %v", err)
	}
	if !errors.Is(err, parseErr) {
		t.Fatalf("EvalFixtureParseError does not wrap parse error: %v", err)
	}

	err = EvalFixtureEmptyQueryError(2)
	if err == nil || err.Error() != "query[2] has empty q" {
		t.Fatalf("EvalFixtureEmptyQueryError = %v", err)
	}

	err = EvalFixtureNoExpectEntriesError(3, "hello")
	if err == nil || err.Error() != `query[3] "hello" has no expect entries` {
		t.Fatalf("EvalFixtureNoExpectEntriesError = %v", err)
	}

	profileErr := errors.New("missing")
	err = EvalProfileLoadError("nicos", profileErr)
	if err == nil || err.Error() != `load profile "nicos": missing` {
		t.Fatalf("EvalProfileLoadError = %v", err)
	}
	if !errors.Is(err, profileErr) {
		t.Fatalf("EvalProfileLoadError does not wrap profile error: %v", err)
	}

	diffErr := errors.New("bad baseline")
	err = EvalDiffError("baseline.json", diffErr)
	if err == nil || err.Error() != "diff baseline.json: bad baseline" {
		t.Fatalf("EvalDiffError = %v", err)
	}
	if !errors.Is(err, diffErr) {
		t.Fatalf("EvalDiffError does not wrap diff error: %v", err)
	}

	diffParseErr := errors.New("bad json")
	err = EvalDiffParseError(diffParseErr)
	if err == nil || err.Error() != "parse: bad json" {
		t.Fatalf("EvalDiffParseError = %v", err)
	}
	if !errors.Is(err, diffParseErr) {
		t.Fatalf("EvalDiffParseError does not wrap parse error: %v", err)
	}
}

func TestEvalRecordRunErrors(t *testing.T) {
	cause := errors.New("boom")
	err := EvalRecordRunError(cause)
	if err == nil || err.Error() != "record-run: boom" {
		t.Fatalf("EvalRecordRunError = %v", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("EvalRecordRunError does not wrap cause: %v", err)
	}

	err = EvalRunHomeDirEmptyError()
	if err == nil || err.Error() != "home directory is empty" {
		t.Fatalf("EvalRunHomeDirEmptyError = %v", err)
	}
}

func TestEvalRunArtifactPlanning(t *testing.T) {
	root := filepath.Join(t.TempDir(), "evals")
	paths, err := PlanEvalRunArtifacts(EvalRunArtifactPlan{
		Root:      "  " + root + "  ",
		Timestamp: "2026-05-03T01:02:03Z",
	})
	if err != nil {
		t.Fatalf("PlanEvalRunArtifacts returned error: %v", err)
	}
	wantReport := filepath.Join(root, "runs", "20260503T010203Z", "report.json")
	wantLatest := filepath.Join(root, "latest.json")
	if paths.ReportPath != wantReport || paths.LatestPath != wantLatest {
		t.Fatalf("paths = %+v, want report %s latest %s", paths, wantReport, wantLatest)
	}

	paths, err = PlanEvalRunArtifacts(EvalRunArtifactPlan{
		Root:      root,
		Timestamp: "not-rfc3339",
		Now: func() time.Time {
			return time.Date(2026, 5, 8, 14, 30, 31, 0, time.FixedZone("test", -5*60*60))
		},
	})
	if err != nil {
		t.Fatalf("PlanEvalRunArtifacts fallback timestamp returned error: %v", err)
	}
	if filepath.Base(filepath.Dir(paths.ReportPath)) != "20260508T193031Z" {
		t.Fatalf("fallback report path = %s, want UTC now timestamp", paths.ReportPath)
	}

	home := filepath.Join(t.TempDir(), "home")
	paths, err = PlanEvalRunArtifacts(EvalRunArtifactPlan{
		Timestamp: "2026-05-03T01:02:03Z",
		UserHomeDir: func() (string, error) {
			return home, nil
		},
	})
	if err != nil {
		t.Fatalf("PlanEvalRunArtifacts default root returned error: %v", err)
	}
	wantRoot := filepath.Join(home, ".docs-puller", "evals")
	if paths.LatestPath != filepath.Join(wantRoot, "latest.json") {
		t.Fatalf("default latest path = %s, want root %s", paths.LatestPath, wantRoot)
	}

	xdgRoot := filepath.Join(t.TempDir(), "xdg-state")
	t.Setenv("XDG_STATE_HOME", xdgRoot)
	paths, err = PlanEvalRunArtifacts(EvalRunArtifactPlan{
		Timestamp: "2026-05-03T01:02:03Z",
		UserHomeDir: func() (string, error) {
			return home, nil
		},
	})
	if err != nil {
		t.Fatalf("PlanEvalRunArtifacts XDG root returned error: %v", err)
	}
	wantRoot = filepath.Join(xdgRoot, "docs-puller", "evals")
	if paths.LatestPath != filepath.Join(wantRoot, "latest.json") {
		t.Fatalf("XDG latest path = %s, want root %s", paths.LatestPath, wantRoot)
	}
	t.Setenv("DOCS_PULLER_LEGACY_NDEV_PATHS", "1")
	paths, err = PlanEvalRunArtifacts(EvalRunArtifactPlan{
		Timestamp: "2026-05-03T01:02:03Z",
		UserHomeDir: func() (string, error) {
			return home, nil
		},
	})
	if err != nil {
		t.Fatalf("PlanEvalRunArtifacts legacy root returned error: %v", err)
	}
	wantRoot = filepath.Join(home, ".nicos-dev", "evals", "docs-puller")
	if paths.LatestPath != filepath.Join(wantRoot, "latest.json") {
		t.Fatalf("legacy latest path = %s, want root %s", paths.LatestPath, wantRoot)
	}
	t.Setenv("DOCS_PULLER_LEGACY_NDEV_PATHS", "")
	t.Setenv("XDG_STATE_HOME", "")

	errHome := errors.New("home failed")
	defaultRoot, err := DefaultEvalRunRoot(func() (string, error) {
		return "", errHome
	})
	if !errors.Is(err, errHome) {
		t.Fatalf("DefaultEvalRunRoot error = %v, want home error", err)
	}
	if defaultRoot != "" {
		t.Fatalf("DefaultEvalRunRoot root = %q, want empty on error", defaultRoot)
	}
	defaultRoot, err = DefaultEvalRunRoot(func() (string, error) {
		return "", nil
	})
	if err == nil || err.Error() != "home directory is empty" {
		t.Fatalf("DefaultEvalRunRoot empty-home error = %v", err)
	}
	if defaultRoot != "" {
		t.Fatalf("DefaultEvalRunRoot empty-home root = %q, want empty", defaultRoot)
	}
}

func TestEvalRunArtifactWriteOrchestration(t *testing.T) {
	root := t.TempDir()
	paths := EvalRunArtifactPaths{
		ReportPath: filepath.Join(root, "runs", "20260503T010203Z", "report.json"),
		LatestPath: filepath.Join(root, "latest.json"),
	}
	var wrote []string
	if err := WriteEvalRunArtifacts(paths, func(path string) error {
		wrote = append(wrote, path)
		if err := os.WriteFile(path, []byte(filepath.Base(path)+"\n"), 0o644); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("WriteEvalRunArtifacts returned error: %v", err)
	}
	if len(wrote) != 2 || wrote[0] != paths.ReportPath || wrote[1] != paths.LatestPath {
		t.Fatalf("write order = %+v, want report then latest", wrote)
	}
	body, err := os.ReadFile(paths.ReportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if string(body) != "report.json\n" {
		t.Fatalf("report body = %q", body)
	}
	body, err = os.ReadFile(paths.LatestPath)
	if err != nil {
		t.Fatalf("read latest: %v", err)
	}
	if string(body) != "latest.json\n" {
		t.Fatalf("latest body = %q", body)
	}

	writeErr := errors.New("write failed")
	err = WriteEvalRunArtifacts(paths, func(path string) error {
		if path != paths.ReportPath {
			t.Fatalf("unexpected second write after report failure: %s", path)
		}
		return writeErr
	})
	if !errors.Is(err, writeErr) {
		t.Fatalf("WriteEvalRunArtifacts error = %v, want write error", err)
	}

	blocker := filepath.Join(t.TempDir(), "root-file")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = WriteEvalRunArtifacts(EvalRunArtifactPaths{
		ReportPath: filepath.Join(blocker, "runs", "stamp", "report.json"),
		LatestPath: filepath.Join(blocker, "latest.json"),
	}, func(string) error {
		t.Fatal("writer should not run when mkdir fails")
		return nil
	})
	if err == nil {
		t.Fatal("WriteEvalRunArtifacts mkdir error = nil, want error")
	}
}

func TestEvalJSONArtifactControl(t *testing.T) {
	cause := errors.New("short write")
	err := EvalJSONEncodeError(cause)
	if err == nil || err.Error() != "encode eval json: short write" {
		t.Fatalf("EvalJSONEncodeError = %v", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("EvalJSONEncodeError does not wrap cause: %v", err)
	}

	path := filepath.Join(t.TempDir(), "artifact.json")
	if err := WriteEvalJSONArtifact(path, func(w io.Writer) error {
		payload := []byte("ok\n")
		n, err := w.Write(payload)
		if err != nil {
			return err
		}
		if n != len(payload) {
			return io.ErrShortWrite
		}
		return nil
	}); err != nil {
		t.Fatalf("WriteEvalJSONArtifact returned error: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(body) != "ok\n" {
		t.Fatalf("artifact body = %q, want ok newline", body)
	}

	writeErr := errors.New("writer failed")
	err = WriteEvalJSONArtifact(filepath.Join(t.TempDir(), "bad.json"), func(io.Writer) error {
		return writeErr
	})
	if !errors.Is(err, writeErr) {
		t.Fatalf("WriteEvalJSONArtifact error = %v, want writer error", err)
	}
}

func TestEncodeEvalJSONArtifact(t *testing.T) {
	var buf strings.Builder
	result := EvalCaseResult{
		Query:                         "stripe webhook",
		QueryType:                     "natural_language",
		Source:                        "stripe",
		ExpectedSource:                "stripe",
		ExpectedDoc:                   "stripe/webhooks.md",
		Expect:                        []string{"stripe/webhooks.md"},
		GotPaths:                      []string{"stripe/webhooks.md"},
		HitAt1:                        true,
		HitAt3:                        true,
		HitAt5:                        true,
		HitAt10:                       true,
		RecallAt5:                     1,
		FirstHitRank:                  1,
		Rank:                          1,
		TokensReturned:                42,
		TokensToFirstHit:              12,
		TokenBudgetHit:                true,
		AnswerContextTokensReturned:   80,
		AnswerContextTokensToFirstHit: 55,
		AnswerContextBudgetHit:        true,
		LatencyMS:                     12.5,
		BackendMode:                   "fts5",
	}
	summary := EvalSummary{
		Mode:                 "fts5",
		N:                    1,
		HitAt1:               1,
		HitAt3:               1,
		HitAt5:               1,
		HitAt10:              1,
		RecallAt5:            1,
		MRR:                  1,
		P50MS:                12.5,
		P99MS:                12.5,
		BySource:             map[string]float64{"stripe": 1},
		TokenBudgetTokens:    128,
		TokenBudgetHitRate:   1,
		P50TokensReturned:    42,
		P99TokensReturned:    42,
		AnswerContextEnabled: true,
	}
	if err := EncodeEvalJSONArtifact(&buf, []EvalCaseResult{result}, summary); err != nil {
		t.Fatalf("EncodeEvalJSONArtifact returned error: %v", err)
	}
	if !strings.Contains(buf.String(), "\n  \"summary\":") {
		t.Fatalf("EncodeEvalJSONArtifact did not write indented JSON: %q", buf.String())
	}
	var got EvalJSONArtifact
	if err := json.Unmarshal([]byte(buf.String()), &got); err != nil {
		t.Fatalf("unmarshal artifact: %v", err)
	}
	if got.Summary.Mode != "fts5" || got.Summary.BySource["stripe"] != 1 || !got.Summary.AnswerContextEnabled {
		t.Fatalf("summary = %#v, want runtime artifact summary", got.Summary)
	}
	if len(got.Results) != 1 || got.Results[0].Query != "stripe webhook" || got.Results[0].TokensToFirstHit != 12 {
		t.Fatalf("results = %#v, want encoded eval result", got.Results)
	}

	cause := errors.New("writer failed")
	err := EncodeEvalJSONArtifact(failingWriter{err: cause}, nil, EvalSummary{})
	if !errors.Is(err, cause) {
		t.Fatalf("EncodeEvalJSONArtifact error = %v, want writer error", err)
	}
}

func TestEncodeEvalSummaryJSONLRow(t *testing.T) {
	var buf strings.Builder
	summary := EvalSummary{Mode: "fts5", N: 27, HitAt5: 1}
	now := time.Date(2026, 5, 8, 1, 2, 3, 0, time.FixedZone("test", -5*60*60))
	if err := EncodeEvalSummaryJSONLRow(&buf, summary, now); err != nil {
		t.Fatalf("EncodeEvalSummaryJSONLRow returned error: %v", err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("EncodeEvalSummaryJSONLRow did not write one JSONL row: %q", buf.String())
	}
	var got EvalSummaryJSONLRow
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &got); err != nil {
		t.Fatalf("unmarshal summary row: %v", err)
	}
	if got.Timestamp != "2026-05-08T06:02:03Z" {
		t.Fatalf("timestamp = %q, want UTC RFC3339", got.Timestamp)
	}
	if got.Summary.Mode != "fts5" || got.Summary.N != 27 || got.Summary.HitAt5 != 1 {
		t.Fatalf("summary = %#v, want encoded eval summary", got.Summary)
	}

	cause := errors.New("writer failed")
	err := EncodeEvalSummaryJSONLRow(failingWriter{err: cause}, summary, now)
	if !errors.Is(err, cause) {
		t.Fatalf("EncodeEvalSummaryJSONLRow error = %v, want writer error", err)
	}
}

func TestEncodeEvalRunReportJSON(t *testing.T) {
	var buf strings.Builder
	report := EvalRunReport{
		SchemaVersion:     "docs-puller-eval-report/v1",
		Timestamp:         "2026-05-08T06:02:03Z",
		Fixture:           "eval/fixture.yaml",
		DocsRoot:          "/docs",
		Limit:             10,
		TokenBudgetTokens: 4000,
		BaselinePath:      "eval/baseline.json",
		Summary:           EvalSummary{Mode: "fts5", N: 1},
		SourceDrift:       []EvalSourceDrift{{Source: "stripe", BaselineHitAt5: 0.5, CurrentHitAt5: 1, DeltaPP: 50}},
		Results:           []EvalCaseResult{{Query: "alpha", ExpectedDoc: "demo/alpha.md"}},
	}
	if err := EncodeEvalRunReportJSON(&buf, report); err != nil {
		t.Fatalf("EncodeEvalRunReportJSON returned error: %v", err)
	}
	if !strings.Contains(buf.String(), "\n  \"schema_version\":") {
		t.Fatalf("EncodeEvalRunReportJSON did not write indented JSON: %q", buf.String())
	}
	var got EvalRunReport
	if err := json.Unmarshal([]byte(buf.String()), &got); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if got.SchemaVersion != report.SchemaVersion || got.Summary.Mode != "fts5" || got.Results[0].Query != "alpha" {
		t.Fatalf("report = %#v, want encoded eval run report", got)
	}
	if len(got.SourceDrift) != 1 || got.SourceDrift[0].Source != "stripe" {
		t.Fatalf("source drift = %#v, want encoded source drift", got.SourceDrift)
	}

	cause := errors.New("writer failed")
	err := EncodeEvalRunReportJSON(failingWriter{err: cause}, report)
	if !errors.Is(err, cause) {
		t.Fatalf("EncodeEvalRunReportJSON error = %v, want writer error", err)
	}
}

func TestBuildEvalRunReport(t *testing.T) {
	now := time.Date(2026, 5, 8, 1, 2, 3, 0, time.FixedZone("test", -5*60*60))
	report := BuildEvalRunReport(EvalRunReportInput{
		FixturePath:       "eval/fixture.yaml",
		DocsRoot:          "/docs",
		Limit:             10,
		TokenBudgetTokens: 4000,
		BaselinePath:      "eval/baseline.json",
		Summary:           EvalSummary{Mode: "fts5", N: 1},
		SourceDrift:       []EvalSourceDrift{{Source: "stripe", DeltaPP: 50}},
		Results:           []EvalCaseResult{{Query: "alpha"}},
		Now:               now,
	})
	if report.SchemaVersion != "docs-puller-eval-report/v1" {
		t.Fatalf("schema version = %q", report.SchemaVersion)
	}
	if report.Timestamp != "2026-05-08T06:02:03Z" {
		t.Fatalf("timestamp = %q, want UTC RFC3339", report.Timestamp)
	}
	if report.Fixture != "eval/fixture.yaml" || report.DocsRoot != "/docs" || report.Limit != 10 || report.TokenBudgetTokens != 4000 {
		t.Fatalf("report basics = %#v", report)
	}
	if report.Summary.Mode != "fts5" || len(report.SourceDrift) != 1 || report.SourceDrift[0].Source != "stripe" || len(report.Results) != 1 || report.Results[0].Query != "alpha" {
		t.Fatalf("report detail = %#v", report)
	}
}

func TestBuildEvalSourceDrift(t *testing.T) {
	got := BuildEvalSourceDrift(
		map[string]float64{"stripe": 1, "supabase": 0.5, "unchanged": 1},
		map[string]float64{"stripe": 0.75, "playwright": 1, "unchanged": 1},
	)
	want := []EvalSourceDrift{
		{Source: "playwright", BaselineHitAt5: 0, CurrentHitAt5: 1, DeltaPP: 100},
		{Source: "stripe", BaselineHitAt5: 1, CurrentHitAt5: 0.75, DeltaPP: -25},
		{Source: "supabase", BaselineHitAt5: 0.5, CurrentHitAt5: 0, DeltaPP: -50},
	}
	if len(got) != len(want) {
		t.Fatalf("BuildEvalSourceDrift len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("BuildEvalSourceDrift[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestEvalJSONLArtifactAppendControl(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.jsonl")
	for _, payload := range [][]byte{[]byte("one\n"), []byte("two\n")} {
		if err := AppendEvalJSONLArtifact(path, func(w io.Writer) error {
			n, err := w.Write(payload)
			if err != nil {
				return err
			}
			if n != len(payload) {
				return io.ErrShortWrite
			}
			return nil
		}); err != nil {
			t.Fatalf("AppendEvalJSONLArtifact returned error: %v", err)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(body) != "one\ntwo\n" {
		t.Fatalf("artifact body = %q, want appended JSONL-style rows", body)
	}

	writeErr := errors.New("writer failed")
	err = AppendEvalJSONLArtifact(filepath.Join(t.TempDir(), "bad.jsonl"), func(io.Writer) error {
		return writeErr
	})
	if !errors.Is(err, writeErr) {
		t.Fatalf("AppendEvalJSONLArtifact error = %v, want writer error", err)
	}
}

func TestEvalArtifactMessages(t *testing.T) {
	cause := errors.New("disk full")
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"write baseline warning", EvalWriteBaselineWarning(cause), "eval: write-baseline: disk full\n"},
		{"write summary warning", EvalWriteSummaryWarning(cause), "eval: write: disk full\n"},
		{"record report written", EvalRecordRunReportWrittenMessage("/tmp/report.json"), "eval: wrote report /tmp/report.json\n"},
		{"record latest updated", EvalRecordRunLatestUpdatedMessage("/tmp/latest.json"), "eval: updated latest /tmp/latest.json\n"},
		{"sweep baseline captured", EvalSweepBaselineCapturedMessage("/tmp/baseline.json"), "eval-sweep: captured fresh baseline at /tmp/baseline.json\n"},
		{"sweep command required", EvalSweepCommandRequiredMessage(), "eval-sweep: command required after --\nusage: docs-puller eval-sweep [--fixture PATH] [--baseline PATH] -- <command...>\n"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestEvalCheckFixtureMessages(t *testing.T) {
	if got, want := EvalCheckFixturePath("/tmp/docs", "/stripe/docs/webhooks.md"), filepath.Join("/tmp/docs", "stripe", "docs", "webhooks.md"); got != want {
		t.Fatalf("EvalCheckFixturePath = %q, want %q", got, want)
	}

	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "missing line",
			got:  EvalCheckFixtureMissingLine("docs/missing.md", "find docs", "/tmp/docs/missing.md"),
			want: "  ✗ docs/missing.md\n      query: \"find docs\"\n      path: /tmp/docs/missing.md\n",
		},
		{
			name: "ok summary",
			got:  EvalCheckFixtureOKMessage(27),
			want: "OK — all 27 fixture queries' expects exist in the corpus.\n",
		},
		{
			name: "missing summary",
			got:  EvalCheckFixtureMissingSummary(2),
			want: "\n2 fixture entries point at non-existent paths. Update fixture or re-pull the affected source.\n",
		},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	ok := EvalCheckFixtureTextOutput(27, nil)
	if want := EvalCheckFixtureOKMessage(27); ok != want {
		t.Fatalf("EvalCheckFixtureTextOutput ok = %q, want %q", ok, want)
	}
	missing := EvalCheckFixtureTextOutput(27, []EvalCheckFixtureMissingPath{
		{Expect: "docs/one.md", Query: "first", Path: "/tmp/docs/one.md"},
		{Expect: "docs/two.md", Query: "second", Path: "/tmp/docs/two.md"},
	})
	for _, want := range []string{
		EvalCheckFixtureMissingLine("docs/one.md", "first", "/tmp/docs/one.md"),
		EvalCheckFixtureMissingLine("docs/two.md", "second", "/tmp/docs/two.md"),
	} {
		if !strings.Contains(missing, want) {
			t.Fatalf("EvalCheckFixtureTextOutput missing %q:\n%s", want, missing)
		}
	}
}

func TestEvalBySourceMessages(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "profile ignored warning",
			got:  EvalBySourceProfileIgnoredWarning(),
			want: "eval: --profile is ignored under --by-source (within-source ranking is what's measured)\n",
		},
		{
			name: "fts not ready warning",
			got:  EvalBySourceFTSNotReadyWarning(),
			want: "eval: FTS5 index not ready after 30s — by-source proceeding anyway.\n",
		},
		{
			name: "standalone fts not ready warning",
			got:  EvalFTSNotReadyWarning(),
			want: "eval: FTS5 index not ready after 30s — proceeding anyway. Per-query degrade is recorded as backend_mode.\n",
		},
		{
			name: "heading",
			got:  EvalBySourceHeader(),
			want: "=== Per-source eval (source-filtered queries only) ===\n",
		},
		{
			name: "table header",
			got:  EvalBySourceTableHeader(),
			want: "source                       n   hit@1   hit@3   hit@5  hit@10    p50ms    p99ms\n",
		},
		{
			name: "row",
			got:  EvalBySourceRow("stripe", 3, 1, 2.0/3.0, 1, 1, 12, 99),
			want: "stripe                       3   100.0%    66.7%   100.0%   100.0%       12       99\n",
		},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestSortedEvalBySourceMetricRows(t *testing.T) {
	rows := []EvalBySourceMetricRow{
		{Source: "stripe", HitAt5: 1},
		{Source: "supabase", HitAt5: 0.5},
		{Source: "clickhouse", HitAt5: 0.5},
	}
	got := SortedEvalBySourceMetricRows(rows)
	wantSources := []string{"clickhouse", "supabase", "stripe"}
	for i, want := range wantSources {
		if got[i].Source != want {
			t.Fatalf("SortedEvalBySourceMetricRows[%d] = %q, want %q", i, got[i].Source, want)
		}
	}
	if rows[0].Source != "stripe" {
		t.Fatalf("SortedEvalBySourceMetricRows mutated input: %#v", rows)
	}
}

func TestEvalBySourceTextOutput(t *testing.T) {
	rows := []EvalBySourceMetricRow{
		{Source: "stripe", N: 3, HitAt1: 1, HitAt3: 1, HitAt5: 1, HitAt10: 1, P50MS: 12, P99MS: 99},
		{Source: "clickhouse", N: 2, HitAt1: 0.5, HitAt3: 0.5, HitAt5: 0.5, HitAt10: 1, P50MS: 21, P99MS: 42},
	}
	out := EvalBySourceTextOutput(rows)
	for _, want := range []string{
		EvalBySourceHeader(),
		EvalBySourceTableHeader(),
		EvalBySourceRow("stripe", 3, 1, 1, 1, 1, 12, 99),
		EvalBySourceRow("clickhouse", 2, 0.5, 0.5, 0.5, 1, 21, 42),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("EvalBySourceTextOutput missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "clickhouse") > strings.Index(out, "stripe") {
		t.Fatalf("EvalBySourceTextOutput did not sort rows by worst Hit@5 first:\n%s", out)
	}
}

func TestEvalSweepBaselineWriteOrchestration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "baseline.json")
	var wrote []string
	err := WriteEvalSweepBaseline(path, func(got string) error {
		wrote = append(wrote, got)
		return os.WriteFile(got, []byte("baseline\n"), 0o644)
	})
	if err != nil {
		t.Fatalf("WriteEvalSweepBaseline returned error: %v", err)
	}
	if len(wrote) != 1 || wrote[0] != path {
		t.Fatalf("writer paths = %v, want [%s]", wrote, path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	if string(body) != "baseline\n" {
		t.Fatalf("baseline body = %q, want %q", body, "baseline\n")
	}

	writeErr := errors.New("writer failed")
	err = WriteEvalSweepBaseline(filepath.Join(t.TempDir(), "bad", "baseline.json"), func(string) error {
		return writeErr
	})
	if !errors.Is(err, writeErr) || !strings.HasPrefix(err.Error(), "write baseline ") {
		t.Fatalf("WriteEvalSweepBaseline write error = %v, want wrapped writer error", err)
	}

	blocker := filepath.Join(t.TempDir(), "root-file")
	if err := os.WriteFile(blocker, []byte("file\n"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	err = WriteEvalSweepBaseline(filepath.Join(blocker, "baseline.json"), func(string) error {
		t.Fatal("writer should not run when baseline directory creation fails")
		return nil
	})
	if err == nil || !strings.HasPrefix(err.Error(), "create baseline dir: ") {
		t.Fatalf("WriteEvalSweepBaseline mkdir error = %v, want create baseline dir wrapper", err)
	}
}

func TestEvalSweepDiffArgs(t *testing.T) {
	got := EvalSweepDiffArgs(EvalSweepDiffArgsConfig{
		FixturePath:        "fixture.yaml",
		Out:                "/docs",
		UseScan:            true,
		Limit:              7,
		BaselinePath:       "baseline.json",
		ProfileName:        "nicos",
		StrictProfile:      true,
		Rerank:             true,
		RerankK:            25,
		RerankModel:        "text-embedding-3-small",
		RerankGate:         0.1,
		RerankChunkSize:    1500,
		TokenBudget:        0,
		TokenBudgetSet:     true,
		DefaultTokenBudget: 1000,
		AnswerContext:      true,
	})
	want := []string{
		"eval",
		"--fixture", "fixture.yaml",
		"--out", "/docs",
		"--limit", "7",
		"--diff", "baseline.json",
		"--scan",
		"--token-budget", "0",
		"--answer-context",
		"--profile", "nicos",
		"--strict",
		"--rerank",
		"--rerank-k", "25",
		"--rerank-model", "text-embedding-3-small",
		"--rerank-gate", "0.1",
		"--rerank-chunk-size", "1500",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("EvalSweepDiffArgs = %v, want %v", got, want)
	}
}

func TestEvalSweepDiffArgsOmitsDefaultTokenBudget(t *testing.T) {
	got := EvalSweepDiffArgs(EvalSweepDiffArgsConfig{
		FixturePath:        "fixture.yaml",
		Out:                "/docs",
		Limit:              7,
		BaselinePath:       "baseline.json",
		TokenBudget:        1000,
		TokenBudgetSet:     true,
		DefaultTokenBudget: 1000,
	})
	want := []string{
		"eval",
		"--fixture", "fixture.yaml",
		"--out", "/docs",
		"--limit", "7",
		"--diff", "baseline.json",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("EvalSweepDiffArgs = %v, want %v", got, want)
	}
}

func TestDefaultEvalSweepBaselinePath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "docs")
	got := DefaultEvalSweepBaselinePath(root, func() time.Time {
		return time.Date(2026, 5, 8, 16, 30, 31, 0, time.FixedZone("test", -5*60*60))
	})
	want := filepath.Join(root, ".cache", "eval-baseline-20260508T213031Z.json")
	if got != want {
		t.Fatalf("DefaultEvalSweepBaselinePath = %s, want %s", got, want)
	}
}

func TestDefaultEvalFixturePath(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	repoFixture := filepath.Join(repo, "cmd", "docs-puller", "eval", "fixture.yaml")
	relativeFixture := filepath.Join("cmd", "docs-puller", "eval", "fixture.yaml")
	localFixture := filepath.Join("eval", "fixture.yaml")

	cases := []struct {
		name   string
		exists map[string]bool
		want   string
	}{
		{
			name: "prefers repo fixture",
			exists: map[string]bool{
				repoFixture:     true,
				relativeFixture: true,
				localFixture:    true,
			},
			want: repoFixture,
		},
		{
			name: "falls back to relative repo fixture",
			exists: map[string]bool{
				relativeFixture: true,
				localFixture:    true,
			},
			want: relativeFixture,
		},
		{
			name: "falls back to local eval fixture",
			exists: map[string]bool{
				localFixture: true,
			},
			want: localFixture,
		},
		{
			name:   "returns repo candidate when none exist",
			exists: map[string]bool{},
			want:   repoFixture,
		},
	}
	for _, tc := range cases {
		got := DefaultEvalFixturePath(repo, func(path string) bool {
			return tc.exists[path]
		})
		if got != tc.want {
			t.Fatalf("%s = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestDefaultEvalQueryType(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{path: "cmd/docs-puller/eval/fixture.yaml", want: "library_search"},
		{path: "cmd/docs-puller/eval/production-telemetry.yaml", want: "production_telemetry"},
		{path: "cmd/docs-puller/eval/natural-language.yaml", want: "tech_stack_search"},
		{path: "NATURAL-LANGUAGE.yaml", want: "tech_stack_search"},
		{path: "cmd/docs-puller/eval/nl-questions.yaml", want: "tech_stack_search"},
	}
	for _, tc := range cases {
		if got := DefaultEvalQueryType(tc.path); got != tc.want {
			t.Fatalf("DefaultEvalQueryType(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestEvalQueryTextAndTypeResolution(t *testing.T) {
	if EvalQueryTextPresent("   ") {
		t.Fatal("blank eval query reported present")
	}
	if !EvalQueryTextPresent(" search docs ") {
		t.Fatal("nonblank eval query reported missing")
	}
	if got := ResolveEvalQueryType("eval/natural-language.yaml", ""); got != "tech_stack_search" {
		t.Fatalf("ResolveEvalQueryType blank natural = %q, want tech_stack_search", got)
	}
	if got := ResolveEvalQueryType("eval/fixture.yaml", " custom "); got != " custom " {
		t.Fatalf("ResolveEvalQueryType explicit = %q, want preserved explicit type", got)
	}
}

func TestEvalRepoRootFromExecutable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	exe := filepath.Join(root, "bin", "docs-puller")
	goWork := filepath.Join(root, "go.work")
	got := EvalRepoRootFromExecutable(exe, func(path string) bool {
		return path == goWork
	})
	if got != root {
		t.Fatalf("EvalRepoRootFromExecutable = %q, want %q", got, root)
	}
	if got := EvalRepoRootFromExecutable(exe, func(string) bool { return false }); got != "." {
		t.Fatalf("EvalRepoRootFromExecutable without sentinel = %q, want .", got)
	}
	if got := EvalRepoRootFromExecutable("", func(string) bool { return true }); got != "." {
		t.Fatalf("EvalRepoRootFromExecutable empty executable = %q, want .", got)
	}
}

func TestNormalizeEvalPathAndSource(t *testing.T) {
	cases := []struct {
		raw        string
		wantPath   string
		wantSource string
	}{
		{raw: " supabase/guides/auth/rls.md ", wantPath: "supabase/guides/auth/rls.md", wantSource: "supabase"},
		{raw: "/stripe/docs/webhooks.md", wantPath: "stripe/docs/webhooks.md", wantSource: "stripe"},
		{raw: "single-segment.md", wantPath: "single-segment.md", wantSource: "single-segment.md"},
		{raw: "   ", wantPath: "", wantSource: ""},
	}
	for _, tc := range cases {
		if got := NormalizeEvalPath(tc.raw); got != tc.wantPath {
			t.Fatalf("NormalizeEvalPath(%q) = %q, want %q", tc.raw, got, tc.wantPath)
		}
		if got := EvalSourceFromPath(tc.raw); got != tc.wantSource {
			t.Fatalf("EvalSourceFromPath(%q) = %q, want %q", tc.raw, got, tc.wantSource)
		}
	}
}

func TestEvalAnswerContextFilePath(t *testing.T) {
	root := t.TempDir()
	path, ok := EvalAnswerContextFilePath(root, "/supabase/auth/rls.md")
	if !ok {
		t.Fatal("EvalAnswerContextFilePath rejected valid path")
	}
	if want := filepath.Join(root, "supabase", "auth", "rls.md"); path != want {
		t.Fatalf("EvalAnswerContextFilePath valid = %q, want %q", path, want)
	}

	for _, raw := range []string{"", "../secret.md", "/../secret.md"} {
		if got, ok := EvalAnswerContextFilePath(root, raw); ok {
			t.Fatalf("EvalAnswerContextFilePath(%q) = %q, true; want rejected", raw, got)
		}
	}
}

func TestApproxEvalTokens(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{text: "", want: 0},
		{text: "   ", want: 0},
		{text: "abcd", want: 1},
		{text: "abcde", want: 2},
		{text: "  abcde  ", want: 2},
	}
	for _, tc := range cases {
		if got := ApproxEvalTokens(tc.text); got != tc.want {
			t.Fatalf("ApproxEvalTokens(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestApproxEvalHitTokens(t *testing.T) {
	hit := Hit{
		Path:  "docs/guide.md",
		Title: "Guide",
		URL:   "https://example.com/guide",
		Snippets: []Snippet{
			{Line: 10, Text: "first snippet"},
			{Line: 20, Text: "second snippet"},
		},
	}
	wantText := "docs/guide.md\nGuide\nhttps://example.com/guide\nfirst snippet\nsecond snippet"
	if got, want := ApproxEvalHitTokens(hit), ApproxEvalTokens(wantText); got != want {
		t.Fatalf("ApproxEvalHitTokens = %d, want %d", got, want)
	}
}

func TestEvalPercentile(t *testing.T) {
	if got := EvalPercentile(nil, 0.5); got != 0 {
		t.Fatalf("EvalPercentile(nil, 0.5) = %v, want 0", got)
	}
	vals := []float64{10, 1, 9, 2, 8, 3, 7, 4, 6, 5}
	if got := EvalPercentile(vals, 0.50); got != 5 {
		t.Fatalf("EvalPercentile p50 = %v, want 5", got)
	}
	if got := EvalPercentile(vals, 0.99); got != 9 {
		t.Fatalf("EvalPercentile p99 = %v, want 9", got)
	}
	if vals[0] != 10 {
		t.Fatalf("EvalPercentile mutated input: %#v", vals)
	}
}

func TestTruncateEvalStrings(t *testing.T) {
	values := []string{"a", "b", "c"}
	if got := TruncateEvalStrings(values, 2); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("TruncateEvalStrings truncated = %#v, want first two", got)
	}
	if got := TruncateEvalStrings(values, 5); len(got) != 3 {
		t.Fatalf("TruncateEvalStrings unbounded len = %d, want 3", len(got))
	}
	if got := TruncateEvalStrings(nil, 5); got != nil {
		t.Fatalf("TruncateEvalStrings nil = %#v, want nil", got)
	}
}

func TestEvalRankDiffMessages(t *testing.T) {
	longQuery := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456789"
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"rank hit", EvalRankText(7), "7"},
		{"rank miss", EvalRankText(0), "—"},
		{"miss rank hit", EvalMissRankText(4), "rank 4"},
		{"miss rank miss", EvalMissRankText(0), "miss"},
		{"miss line", EvalMissLine("missing docs", 0), "  ✗ missing docs                                        → miss\n"},
		{"gained", EvalDiffGainedLine("new hit", 3), "  ✓ new hit                                            — → 3"},
		{"lost", EvalDiffLostLine("lost hit", 2), "  ✗ lost hit                                           2 → —"},
		{"moved up", EvalDiffMovedLine("better hit", 4, 2), "  ↑ better hit                                         4 → 2"},
		{"moved down", EvalDiffMovedLine("worse hit", 1, 5), "  ↓ worse hit                                          1 → 5"},
		{"added", EvalDiffAddedLine(longQuery), "  + abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456…"},
		{"removed", EvalDiffRemovedLine(longQuery), "  - abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456…"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestEvalMissSectionMessages(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"header", EvalMissesHeader(2, 7), "Misses (2 / 7):\n"},
		{"expect", EvalMissExpectLine("docs/source.md"), "      expect: docs/source.md\n"},
		{"got path", EvalMissGotPathLine("docs/actual.md"), "      got[1]: docs/actual.md\n"},
		{"footer", EvalMissesFooter(), "\n"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestEvalVerboseTextMessages(t *testing.T) {
	longQuery := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456789"
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"header", EvalVerboseTableHeader(), "query                                                       hit@1 @3 @5 @10  rank  ms\n------------------------------------------------------------ ----- -- -- ---  ----  ---\n"},
		{"hit row", EvalVerboseResultLine("alpha", true, false, true, false, 2, 13), "alpha" + strings.Repeat(" ", 55) + "  ✓      ✓       2     13\n"},
		{"miss row", EvalVerboseResultLine("beta", false, false, false, false, 0, 9), "beta" + strings.Repeat(" ", 56) + "                 —     9\n"},
		{"truncated row", EvalVerboseResultLine(longQuery, true, true, true, true, 10, 123), "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456…  ✓   ✓  ✓  ✓    10    123\n"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestEvalDiffMetricTableMessages(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"header", EvalDiffHeader("baseline.json", 3, 4), "=== Diff baseline.json → current (n: 3 → 4) ===\n"},
		{"metric header", EvalDiffMetricHeader(), "metric              baseline         current    delta\n"},
		{"separator", EvalDiffMetricSeparator(), strings.Repeat("-", 64) + "\n"},
		{"percent positive", EvalDiffPercentPointLine("Hit@1", 0.5, 0.75), "Hit@1                    50%             75%    +25.0 pp\n"},
		{"percent negative", EvalDiffPercentPointLine("Hit@5", 1, 0.75), "Hit@5                   100%             75%    -25.0 pp\n"},
		{"float", EvalDiffFloatLine("MRR", 0.883, 0.855, 3), "MRR                    0.883           0.855    -0.028\n"},
		{"integer float", EvalDiffFloatLine("p50 ms", 4389, 4537, 0), "p50 ms                  4389            4537    +148\n"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestEvalDiffSectionMessages(t *testing.T) {
	unsorted := []string{"  zeta", "  alpha"}
	sorted := SortedEvalDiffSectionLines(unsorted)
	if want := []string{"  alpha", "  zeta"}; !slices.Equal(sorted, want) {
		t.Fatalf("SortedEvalDiffSectionLines = %#v, want %#v", sorted, want)
	}
	if !slices.Equal(unsorted, []string{"  zeta", "  alpha"}) {
		t.Fatalf("SortedEvalDiffSectionLines mutated input: %#v", unsorted)
	}
	if got := SortedEvalDiffSectionLines(nil); got != nil {
		t.Fatalf("SortedEvalDiffSectionLines(nil) = %#v, want nil", got)
	}

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"per query header", EvalDiffPerQueryChangesHeader(), "\nPer-query changes:\n"},
		{"additions header", EvalDiffFixtureAdditionsHeader(), "\nFixture additions:\n"},
		{"removals header", EvalDiffFixtureRemovalsHeader(), "\nFixture removals:\n"},
		{"lines", EvalDiffSectionLines([]string{"  + added", "  - removed"}), "  + added\n  - removed\n"},
		{"no lines", EvalDiffSectionLines(nil), ""},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestEvalSummarySourceMessages(t *testing.T) {
	sources := SortedEvalSummarySources(map[string]float64{"supabase": 1, "go": 0.5, "clickhouse": 1})
	if want := []string{"clickhouse", "go", "supabase"}; !slices.Equal(sources, want) {
		t.Fatalf("SortedEvalSummarySources = %#v, want %#v", sources, want)
	}

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"summary header", EvalSummaryHeader("fts5", 27), "=== Summary (fts5, n=27) ===\n"},
		{"summary metrics", EvalSummaryMetricsLine(0.778, 0.889, 1, 1, 0.883, 1), "Hit@1=78%  Hit@3=89%  Hit@5=100%  Hit@10=100%  MRR=0.883  Recall@5=100%\n"},
		{"latency", EvalSummaryLatencyLine(4389, 10274), "Latency p50=4389ms  p99=10274ms\n"},
		{"answer context", EvalSummaryAnswerContextLine(100, 300, 0.5), "Answer context tokens p50=100  p99=300  budget-hit=50%\n"},
		{"by source header", EvalSummaryBySourceHeader(), "By source (Hit@5):\n"},
		{"by source line", EvalSummaryBySourceLine("supabase", 0.75), "  supabase             75%\n"},
		{"diff source header", EvalDiffPerSourceHeader(), "\nPer-source Hit@5 changes:\n"},
		{"diff source positive", EvalDiffPerSourceLine("stripe", 0.5, 0.75), "  stripe               50% → 75%  (+25 pp)\n"},
		{"diff source negative", EvalDiffPerSourceLine("go", 1, 0.5), "  go                   100% → 50%  (-50 pp)\n"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestEvalTextOutput(t *testing.T) {
	rs := []EvalCaseResult{
		{Query: "hit", HitAt1: true, HitAt3: true, HitAt5: true, HitAt10: true, FirstHitRank: 1, RecallAt5: 1, LatencyMS: 10},
		{Query: "miss", Expect: []string{"docs/expected.md"}, GotPaths: []string{"docs/actual.md"}, FirstHitRank: 0, RecallAt5: 0, LatencyMS: 20},
	}
	summary := EvalSummary{
		Mode:                       "fts5",
		N:                          len(rs),
		HitAt1:                     0.5,
		HitAt3:                     0.5,
		HitAt5:                     0.5,
		HitAt10:                    0.5,
		RecallAt5:                  0.5,
		MRR:                        0.5,
		P50MS:                      10,
		P99MS:                      20,
		AnswerContextEnabled:       true,
		P50AnswerContextTokens:     100,
		P99AnswerContextTokens:     300,
		AnswerContextBudgetHitRate: 0.5,
		BySource:                   map[string]float64{"supabase": 1},
	}
	out := EvalTextOutput(rs, summary, false)
	for _, want := range []string{
		EvalMissesHeader(1, len(rs)),
		EvalMissLine("miss", 0),
		EvalMissExpectLine("docs/expected.md"),
		EvalMissGotPathLine("docs/actual.md"),
		EvalSummaryHeader(summary.Mode, summary.N),
		EvalSummaryMetricsLine(summary.HitAt1, summary.HitAt3, summary.HitAt5, summary.HitAt10, summary.MRR, summary.RecallAt5),
		EvalSummaryAnswerContextLine(summary.P50AnswerContextTokens, summary.P99AnswerContextTokens, summary.AnswerContextBudgetHitRate),
		EvalSummaryBySourceLine("supabase", 1),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("EvalTextOutput missing %q:\n%s", want, out)
		}
	}

	verbose := EvalTextOutput(rs, EvalSummary{Mode: "fts5", N: len(rs)}, true)
	for _, want := range []string{
		EvalVerboseTableHeader(),
		EvalVerboseResultLine("hit", true, true, true, true, 1, 10),
		EvalVerboseResultLine("miss", false, false, false, false, 0, 20),
	} {
		if !strings.Contains(verbose, want) {
			t.Fatalf("EvalTextOutput verbose missing %q:\n%s", want, verbose)
		}
	}
}

func TestEvalDiffTextOutput(t *testing.T) {
	gainedA := EvalDiffGainedLine("a gained", 1)
	gainedZ := EvalDiffGainedLine("z gained", 2)
	added := EvalDiffAddedLine("new fixture query")
	removed := EvalDiffRemovedLine("old fixture query")
	out := EvalDiffTextOutput(EvalDiffTextInput{
		BaselinePath: "baseline.json",
		BaselineSummary: EvalSummary{
			N:         2,
			HitAt1:    0.5,
			HitAt3:    0.5,
			HitAt5:    0.5,
			HitAt10:   0.5,
			RecallAt5: 0.5,
			MRR:       0.5,
			P50MS:     10,
			P99MS:     20,
		},
		CurrentSummary: EvalSummary{
			N:         3,
			HitAt1:    1,
			HitAt3:    1,
			HitAt5:    1,
			HitAt10:   1,
			RecallAt5: 1,
			MRR:       1,
			P50MS:     11,
			P99MS:     22,
		},
		SourceDrift: []EvalSourceDrift{{Source: "stripe", BaselineHitAt5: 0.5, CurrentHitAt5: 1}},
		Gained:      []string{gainedZ, gainedA},
		Lost:        []string{EvalDiffLostLine("lost", 2)},
		Moved:       []string{EvalDiffMovedLine("moved", 4, 1)},
		Added:       []string{added},
		Removed:     []string{removed},
	})
	for _, want := range []string{
		EvalDiffHeader("baseline.json", 2, 3),
		EvalDiffMetricHeader(),
		EvalDiffPercentPointLine("Hit@1", 0.5, 1),
		EvalDiffFloatLine("MRR", 0.5, 1, 3),
		EvalDiffPerSourceLine("stripe", 0.5, 1),
		EvalDiffPerQueryChangesHeader(),
		EvalDiffLostLine("lost", 2),
		EvalDiffMovedLine("moved", 4, 1),
		EvalDiffFixtureAdditionsHeader(),
		added,
		EvalDiffFixtureRemovalsHeader(),
		removed,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("EvalDiffTextOutput missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, gainedA) > strings.Index(out, gainedZ) {
		t.Fatalf("EvalDiffTextOutput did not sort section lines:\n%s", out)
	}

	quiet := EvalDiffTextOutput(EvalDiffTextInput{
		BaselinePath:    "baseline.json",
		BaselineSummary: EvalSummary{N: 1},
		CurrentSummary:  EvalSummary{N: 1},
	})
	if strings.Contains(quiet, EvalDiffPerQueryChangesHeader()) {
		t.Fatalf("EvalDiffTextOutput printed empty per-query section:\n%s", quiet)
	}
}

func TestEvalSweepErrors(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"create baseline dir", EvalSweepCreateBaselineDirError(cause), "create baseline dir: boom"},
		{"write baseline", EvalSweepWriteBaselineError("baseline.json", cause), "write baseline baseline.json: boom"},
		{"command", EvalSweepCommandError(cause), "command failed: boom"},
		{"resolve executable", EvalSweepResolveExecutableError(cause), "resolve executable: boom"},
	}
	for _, tc := range cases {
		if tc.err == nil || tc.err.Error() != tc.want {
			t.Fatalf("%s error = %v, want %q", tc.name, tc.err, tc.want)
		}
		if !errors.Is(tc.err, cause) {
			t.Fatalf("%s error does not wrap cause: %v", tc.name, tc.err)
		}
	}
}

func TestGatsbyErrors(t *testing.T) {
	fetchErr := errors.New("connection refused")
	err := GatsbyFetchError("https://example.com/page-data/docs/page-data.json", fetchErr)
	if err == nil || err.Error() != "fetch https://example.com/page-data/docs/page-data.json: connection refused" {
		t.Fatalf("GatsbyFetchError = %v", err)
	}
	if !errors.Is(err, fetchErr) {
		t.Fatalf("GatsbyFetchError does not wrap fetch error: %v", err)
	}

	parseErr := errors.New("bad json")
	err = GatsbyParseError("https://example.com/page-data/docs/page-data.json", parseErr)
	if err == nil || err.Error() != "parse https://example.com/page-data/docs/page-data.json: bad json" {
		t.Fatalf("GatsbyParseError = %v", err)
	}
	if !errors.Is(err, parseErr) {
		t.Fatalf("GatsbyParseError does not wrap parse error: %v", err)
	}

	err = GatsbyBaseURLParseError(parseErr)
	if err == nil || err.Error() != "parse base url: bad json" {
		t.Fatalf("GatsbyBaseURLParseError = %v", err)
	}
	if !errors.Is(err, parseErr) {
		t.Fatalf("GatsbyBaseURLParseError does not wrap parse error: %v", err)
	}

	err = GatsbyNoStaticQueryHashesError("https://example.com/page-data/docs/page-data.json")
	if err == nil || err.Error() != "no staticQueryHashes in https://example.com/page-data/docs/page-data.json" {
		t.Fatalf("GatsbyNoStaticQueryHashesError = %v", err)
	}

	err = GatsbyNoAllMdxSlugsError(3)
	if err == nil || err.Error() != "no allMdx.nodes[].slug found across 3 static queries" {
		t.Fatalf("GatsbyNoAllMdxSlugsError = %v", err)
	}
}

func TestSitemapErrors(t *testing.T) {
	fetchErr := errors.New("connection refused")
	err := SitemapFetchError("https://example.com/sitemap.xml", fetchErr)
	if err == nil || err.Error() != "fetch https://example.com/sitemap.xml: connection refused" {
		t.Fatalf("SitemapFetchError = %v", err)
	}
	if !errors.Is(err, fetchErr) {
		t.Fatalf("SitemapFetchError does not wrap fetch error: %v", err)
	}

	parseErr := errors.New("bad xml")
	err = SitemapParseError("https://example.com/sitemap.xml", parseErr)
	if err == nil || err.Error() != "parse https://example.com/sitemap.xml: bad xml" {
		t.Fatalf("SitemapParseError = %v", err)
	}
	if !errors.Is(err, parseErr) {
		t.Fatalf("SitemapParseError does not wrap parse error: %v", err)
	}

	err = SitemapUnexpectedRootError("feed", "https://example.com/sitemap.xml")
	if err == nil || err.Error() != "unexpected sitemap root <feed> at https://example.com/sitemap.xml" {
		t.Fatalf("SitemapUnexpectedRootError = %v", err)
	}

	err = SitemapGunzipError(parseErr)
	if err == nil || err.Error() != "gunzip: bad xml" {
		t.Fatalf("SitemapGunzipError = %v", err)
	}
	if !errors.Is(err, parseErr) {
		t.Fatalf("SitemapGunzipError does not wrap gzip error: %v", err)
	}
}

func TestWriteLockErrors(t *testing.T) {
	openErr := errors.New("permission denied")
	err := WriteLockOpenError(openErr)
	if err == nil || err.Error() != "open write-lock: permission denied" {
		t.Fatalf("WriteLockOpenError = %v", err)
	}
	if !errors.Is(err, openErr) {
		t.Fatalf("WriteLockOpenError does not wrap open error: %v", err)
	}

	flockErr := errors.New("interrupted")
	err = WriteLockFlockError(flockErr)
	if err == nil || err.Error() != "flock: interrupted" {
		t.Fatalf("WriteLockFlockError = %v", err)
	}
	if !errors.Is(err, flockErr) {
		t.Fatalf("WriteLockFlockError does not wrap flock error: %v", err)
	}
}

func TestDocCErrors(t *testing.T) {
	rootErr := errors.New("bad root")
	err := DocCRootURLError(rootErr)
	if err == nil || err.Error() != "root url: bad root" {
		t.Fatalf("DocCRootURLError = %v", err)
	}
	if !errors.Is(err, rootErr) {
		t.Fatalf("DocCRootURLError does not wrap root error: %v", err)
	}

	err = DocCNotAbsoluteURLError("/documentation/example")
	if err == nil || err.Error() != "not an absolute URL: /documentation/example" {
		t.Fatalf("DocCNotAbsoluteURLError = %v", err)
	}

	err = DocCRootRejectedError("https://developer.apple.com/documentation/example")
	if err == nil || err.Error() != "root URL https://developer.apple.com/documentation/example rejected by filter or already visited" {
		t.Fatalf("DocCRootRejectedError = %v", err)
	}

	parseErr := errors.New("bad json")
	err = DocCParseError(parseErr)
	if err == nil || err.Error() != "parse: bad json" {
		t.Fatalf("DocCParseError = %v", err)
	}
	if !errors.Is(err, parseErr) {
		t.Fatalf("DocCParseError does not wrap parse error: %v", err)
	}
}

func TestIndexErrors(t *testing.T) {
	collectErr := errors.New("walk failed")
	err := IndexCollectSourceError("supabase", collectErr)
	if err == nil || err.Error() != "collect supabase: walk failed" {
		t.Fatalf("IndexCollectSourceError = %v", err)
	}
	if !errors.Is(err, collectErr) {
		t.Fatalf("IndexCollectSourceError does not wrap collect error: %v", err)
	}

	writeErr := errors.New("permission denied")
	err = IndexWriteSourceError("supabase", writeErr)
	if err == nil || err.Error() != "write supabase/_INDEX.md: permission denied" {
		t.Fatalf("IndexWriteSourceError = %v", err)
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("IndexWriteSourceError does not wrap write error: %v", err)
	}
}

func TestManifestErrors(t *testing.T) {
	parseErr := errors.New("bad json")
	err := ManifestParseError("/tmp/docs/manifest.json", parseErr)
	if err == nil || err.Error() != "parse /tmp/docs/manifest.json: bad json" {
		t.Fatalf("ManifestParseError = %v", err)
	}
	if !errors.Is(err, parseErr) {
		t.Fatalf("ManifestParseError does not wrap parse error: %v", err)
	}

	writeErr := errors.New("permission denied")
	err = ManifestMigrationError("/tmp/docs/manifest.jsonl", "/tmp/docs/manifest.json", writeErr)
	if err == nil || err.Error() != "migrate /tmp/docs/manifest.jsonl -> /tmp/docs/manifest.json: permission denied" {
		t.Fatalf("ManifestMigrationError = %v", err)
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("ManifestMigrationError does not wrap write error: %v", err)
	}

	loadErr := errors.New("missing")
	err = ManifestLoadSourceError("supabase", loadErr)
	if err == nil || err.Error() != "load supabase manifest: missing" {
		t.Fatalf("ManifestLoadSourceError = %v", err)
	}
	if !errors.Is(err, loadErr) {
		t.Fatalf("ManifestLoadSourceError does not wrap load error: %v", err)
	}

	err = ManifestWriteSourceError("supabase", writeErr)
	if err == nil || err.Error() != "write supabase manifest: permission denied" {
		t.Fatalf("ManifestWriteSourceError = %v", err)
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("ManifestWriteSourceError does not wrap write error: %v", err)
	}
}

func TestSearchTelemetryLogRow(t *testing.T) {
	got := SearchTelemetryLogRow(SearchTelemetryEntry{
		Timestamp: "2026-05-04T00:00:00Z",
		Intent:    "support",
		Mode:      "fts5",
		Query:     "row level security",
		TopPath:   "supabase/guides/database/postgres/row-level-security.md",
	})
	want := "2026-05-04T00:00:00Z  support       fts5      row level security  -> supabase/guides/database/postgres/row-level-security.md\n"
	if got != want {
		t.Fatalf("SearchTelemetryLogRow = %q, want %q", got, want)
	}

	got = SearchTelemetryLogRow(SearchTelemetryEntry{
		Timestamp: "2026-05-04T00:01:00Z",
		Query:     "ports",
	})
	want = "2026-05-04T00:01:00Z  unlabeled" + strings.Repeat(" ", 15) + "ports  -> —\n"
	if got != want {
		t.Fatalf("SearchTelemetryLogRow fallback = %q, want %q", got, want)
	}
}

func TestSearchTelemetryFixtureQueries(t *testing.T) {
	entries := []SearchTelemetryEntry{
		{
			Timestamp:    "2026-05-04T00:00:00Z",
			Query:        "row level security",
			Intent:       "support",
			SourceFilter: "supabase",
			Mode:         "fts5",
			TopPath:      "supabase/guides/database/postgres/row-level-security.md",
		},
		{
			Timestamp: "2026-05-04T00:01:00Z",
			Query:     "How do I configure workspace ports",
			Intent:    "support",
			Mode:      "hybrid",
			TopPath:   "nicos/local-dev/parallel-workspaces.md",
		},
		{
			Timestamp: "2026-05-04T00:02:00Z",
			Query:     "row level security",
			Intent:    "support",
			TopPath:   "duplicate.md",
		},
		{
			Timestamp: "bad",
			Query:     "bad timestamp",
			Intent:    "support",
			TopPath:   "bad.md",
		},
		{
			Timestamp: "2026-05-04T00:03:00Z",
			Query:     "wrong intent",
			Intent:    "research",
			TopPath:   "wrong.md",
		},
		{
			Timestamp: "2026-05-04T00:04:00Z",
			Query:     "missing top path",
			Intent:    "support",
		},
	}
	got := SearchTelemetryFixtureQueries(SearchTelemetryFixtureInput{
		Entries: entries,
		Intent:  "support",
		Since:   time.Date(2026, 5, 4, 0, 0, 30, 0, time.UTC),
		Exclude: map[string]bool{
			"row level security": true,
		},
	})
	if len(got) != 1 {
		t.Fatalf("fixture candidates = %+v, want one included candidate", got)
	}
	if got[0].Query != "How do I configure workspace ports" {
		t.Fatalf("query = %q, want workspace ports", got[0].Query)
	}
	if len(got[0].Expect) != 1 || got[0].Expect[0] != "nicos/local-dev/parallel-workspaces.md" {
		t.Fatalf("expect = %+v", got[0].Expect)
	}
	if got[0].Source != "" {
		t.Fatalf("source = %q, want empty source when telemetry has no source filter", got[0].Source)
	}
	if got[0].Note != "telemetry-candidate; verify expect before promoting; intent=support; mode=hybrid" {
		t.Fatalf("note = %q", got[0].Note)
	}
}

func TestQueryLogAppendFailedWarning(t *testing.T) {
	err := errors.New("permission denied")
	if got, want := QueryLogAppendFailedWarning(err), "query-log: append failed: permission denied\n"; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
}

func TestSearchQueryRequiredMessage(t *testing.T) {
	if got, want := SearchQueryRequiredMessage(), "search: query required\n"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestApplyVersionPolicyScoresFiltersAndSorts(t *testing.T) {
	hits := []Hit{
		{Path: "latest.md", Source: "react", SourceID: "react", VersionLane: "latest", Score: 10},
		{Path: "drop.md", Source: "other", SourceID: "other", VersionLane: "latest", Score: 1000},
		{Path: "workspace.md", Source: "react__v18", SourceID: "react__v18", VersionLane: "workspace-pinned", Score: 5},
		{Path: "already.md", Source: "react__v17", SourceID: "react__v17", VersionLane: "workspace-pinned", Score: 400, VersionScoreApplied: true},
	}
	got := ApplyVersionPolicy(hits, func(hit Hit) (Hit, bool) {
		if hit.Source == "other" {
			return hit, false
		}
		hit.SourceFamily = "react"
		return hit, true
	}, func(hit Hit) int {
		return VersionPolicyScore(hit, VersionPolicyScoreOptions{})
	})
	if len(got) != 3 {
		t.Fatalf("hits = %+v, want three matched hits", got)
	}
	if got[0].Path != "workspace.md" || got[0].Score != 455 || !got[0].VersionScoreApplied {
		t.Fatalf("top hit = %+v, want workspace boost", got[0])
	}
	if got[1].Path != "already.md" || got[1].Score != 400 {
		t.Fatalf("already-scored hit = %+v, want unchanged score", got[1])
	}
	if got[2].Path != "latest.md" || got[2].Score != 310 || got[2].SourceFamily != "react" {
		t.Fatalf("latest hit = %+v, want enriched latest boost", got[2])
	}
}

func TestVersionPolicyScoreWeights(t *testing.T) {
	if got := VersionPolicyScore(Hit{VersionLane: "workspace-pinned"}, VersionPolicyScoreOptions{Version: "18"}); got != 0 {
		t.Fatalf("hard version score = %d, want 0", got)
	}
	if got := VersionPolicyScore(Hit{VersionLane: "latest"}, VersionPolicyScoreOptions{PreferLatest: true}); got != 500 {
		t.Fatalf("prefer-latest latest score = %d, want 500", got)
	}
	if got := VersionPolicyScore(Hit{VersionLane: "workspace-pinned"}, VersionPolicyScoreOptions{PreferLatest: true}); got != 250 {
		t.Fatalf("prefer-latest workspace score = %d, want 250", got)
	}
	if got := VersionPolicyScore(Hit{SourceID: "react__v18", VersionLane: "latest"}, VersionPolicyScoreOptions{
		CwdPinnedSourceIDs: map[string]bool{"react__v18": true},
	}); got != 600 {
		t.Fatalf("cwd-pinned score = %d, want 600", got)
	}
	if got := VersionPolicyScore(Hit{VersionLane: "other-pinned"}, VersionPolicyScoreOptions{}); got != 50 {
		t.Fatalf("other-pinned score = %d, want 50", got)
	}
}

func TestVersionPolicyScoreFromState(t *testing.T) {
	if got := VersionPolicyScoreFromState(Hit{VersionLane: "latest"}, VersionPolicyState{LatestOnly: true}, nil); got != 0 {
		t.Fatalf("hard latest-only score = %d, want 0", got)
	}
	if got := VersionPolicyScoreFromState(Hit{VersionLane: "latest"}, VersionPolicyState{PreferLatest: true}, nil); got != 500 {
		t.Fatalf("prefer-latest score = %d, want 500", got)
	}
	if got := VersionPolicyScoreFromState(Hit{SourceID: "react__v18"}, VersionPolicyState{}, map[string]bool{"react__v18": true}); got != 600 {
		t.Fatalf("cwd-pinned score = %d, want 600", got)
	}
}

func TestNewHybridScoredPath(t *testing.T) {
	got := NewHybridScoredPath("docs/guide.md", 0.98)
	if got.Path != "docs/guide.md" || got.Score != 0.98 {
		t.Fatalf("hybrid scored path = %+v, want path and score preserved", got)
	}
}

type hybridFlatTopKTestCandidate struct {
	path  string
	score float32
}

func (candidate hybridFlatTopKTestCandidate) HybridPath() string { return candidate.path }

func (candidate hybridFlatTopKTestCandidate) HybridScore() float32 { return candidate.score }

func TestNewHybridFlatTopKCall(t *testing.T) {
	var (
		gotOutDir       string
		gotModel        string
		gotK            int
		gotDepthPenalty float32
	)
	topK := NewHybridFlatTopKCall(
		func(outDir, model string, _ []float32, k int, depthPenalty float32) ([]hybridFlatTopKTestCandidate, bool, error) {
			gotOutDir = outDir
			gotModel = model
			gotK = k
			gotDepthPenalty = depthPenalty
			return []hybridFlatTopKTestCandidate{{path: "docs/a.md", score: 0.8}}, true, nil
		},
	)

	got, ok, err := topK("docs-out", "text-embedding-3-small", []float32{1}, 7, 0.005)
	if err != nil {
		t.Fatalf("topK returned error: %v", err)
	}
	if !ok || gotOutDir != "docs-out" || gotModel != "text-embedding-3-small" || gotK != 7 || gotDepthPenalty != 0.005 {
		t.Fatalf("topK inputs = ok=%v out=%q model=%q k=%d depth=%v, want forwarded inputs", ok, gotOutDir, gotModel, gotK, gotDepthPenalty)
	}
	if len(got) != 1 || got[0].Path != "docs/a.md" || got[0].Score != 0.8 {
		t.Fatalf("hybrid scored candidates = %+v, want adapted path/score", got)
	}
}

func TestNewHybridFlatTopKCallNil(t *testing.T) {
	var topK HybridFlatTopKSource[hybridFlatTopKTestCandidate]
	if got := NewHybridFlatTopKCall(topK); got != nil {
		t.Fatalf("nil flat top-K adapter returned non-nil callback")
	}
}

func TestCosineSimilarity(t *testing.T) {
	if got := CosineSimilarity([]float32{1, 0}, []float32{1, 0}); got < 0.999 || got > 1.001 {
		t.Fatalf("cosine identical = %.6f, want 1", got)
	}
	if got := CosineSimilarity([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Fatalf("cosine orthogonal = %.6f, want 0", got)
	}
	if got := CosineSimilarity([]float32{1, 2}, []float32{1}); got != 0 {
		t.Fatalf("cosine dimension mismatch = %.6f, want 0", got)
	}
	if got := CosineSimilarity([]float32{0, 0}, []float32{1, 1}); got != 0 {
		t.Fatalf("cosine zero-norm = %.6f, want 0", got)
	}
}

func TestSplitEmbeddingTextChunks(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		chunkSize int
		want      []string
	}{
		{"empty", "", 100, []string{""}},
		{"shorter than size", "hello world", 100, []string{"hello world"}},
		{"exactly size", "hello", 5, []string{"hello"}},
		{"hard split, no boundary", "aaaaabbbbbccccc", 5, []string{"aaaaa", "bbbbb", "ccccc"}},
		{"break at paragraph", "first para text\n\nsecond para text", 16, []string{"first para text", "second para text"}},
		{"chunk size 0 returns single", "anything", 0, []string{"anything"}},
		{"chunk size negative returns single", "anything", -1, []string{"anything"}},
		{"break preferred over hard split when in last half", "abcdef ghij klmn\n\nopqrstuvwx", 24, []string{"abcdef ghij klmn", "opqrstuvwx"}},
	}
	for _, tc := range cases {
		got := SplitEmbeddingTextChunks(tc.text, tc.chunkSize)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %d chunks, want %d (%v vs %v)", tc.name, len(got), len(tc.want), got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: chunk[%d] = %q, want %q", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

func TestDefaultHybridDepthPenaltyPerSegment(t *testing.T) {
	if DefaultHybridDepthPenaltyPerSegment != 0.005 {
		t.Fatalf("default hybrid depth penalty = %.3f, want 0.005", DefaultHybridDepthPenaltyPerSegment)
	}
}

func TestHybridRankFusionScore(t *testing.T) {
	if DefaultHybridRRFK != 60 {
		t.Fatalf("default hybrid RRF K = %d, want 60", DefaultHybridRRFK)
	}
	if DefaultHybridBM25RankWeight != 0.7 {
		t.Fatalf("default hybrid BM25 weight = %.1f, want 0.7", DefaultHybridBM25RankWeight)
	}
	if DefaultHybridCosineRankWeight != 0.3 {
		t.Fatalf("default hybrid cosine weight = %.1f, want 0.3", DefaultHybridCosineRankWeight)
	}

	got := HybridRankFusionScore(1, 2)
	want := 0.7/61 + 0.3/62
	if got < want-0.000001 || got > want+0.000001 {
		t.Fatalf("hybrid rank fusion score = %.8f, want %.8f", got, want)
	}

	noVector := HybridRankFusionScore(1, 0)
	wantNoVector := 0.7 / 61
	if noVector < wantNoVector-0.000001 || noVector > wantNoVector+0.000001 {
		t.Fatalf("hybrid rank fusion score without vector = %.8f, want %.8f", noVector, wantNoVector)
	}
	if got := HybridRankFusionScore(0, 0); got != 0 {
		t.Fatalf("hybrid rank fusion score with invalid ranks = %.8f, want 0", got)
	}
}

func TestHybridFlatFallbackWarning(t *testing.T) {
	got := HybridFlatFallbackWarning(errors.New("flat missing"))
	want := "rerank-hybrid: flat embedding index unavailable: flat missing — using sqlite scan"
	if got.Message != want {
		t.Fatalf("warning = %q, want %q", got.Message, want)
	}
}

func TestNewHybridFlatFallbackWarningCall(t *testing.T) {
	var b strings.Builder
	warn := NewHybridFlatFallbackWarningCall(&b)
	if warn == nil {
		t.Fatalf("warning callback is nil")
	}
	warn(errors.New("flat missing"))
	want := "rerank-hybrid: flat embedding index unavailable: flat missing — using sqlite scan\n"
	if b.String() != want {
		t.Fatalf("warning output = %q, want %q", b.String(), want)
	}
}

func TestNewHybridFlatFallbackWarningCallNil(t *testing.T) {
	if got := NewHybridFlatFallbackWarningCall(nil); got != nil {
		t.Fatalf("nil writer warning callback returned non-nil callback")
	}
}

func TestMergeHybridCandidatesPreservesBM25AndAppendsEmbeddingOnly(t *testing.T) {
	bm25 := []Hit{
		{Path: "docs/a.md", Source: "docs", Score: 100, Title: "A"},
		{Path: "docs/a.md", Source: "docs", Score: 90, Title: "A duplicate"},
		{Path: "docs/b.md", Source: "docs", Score: 80, Title: "B"},
	}
	candidates := []HybridCandidate{
		{Path: "docs/b.md"},
		{Path: "other/c.md"},
		{Path: ""},
		{Path: "docs/d.md"},
	}
	got := MergeHybridCandidates(bm25, candidates, func(path string) string {
		return "title:" + path
	})
	if len(got) != 4 {
		t.Fatalf("merged hits = %+v, want 4 hits", got)
	}
	if got[0].Path != "docs/a.md" || got[0].Score != 100 || got[1].Path != "docs/b.md" || got[1].Score != 80 {
		t.Fatalf("BM25 order not preserved: %+v", got)
	}
	if got[2].Path != "other/c.md" || got[2].Source != "other" || got[2].Title != "title:other/c.md" || got[2].Score != 0 {
		t.Fatalf("first embedding-only hit = %+v", got[2])
	}
	if got[3].Path != "docs/d.md" || got[3].Source != "docs" || got[3].Title != "title:docs/d.md" {
		t.Fatalf("second embedding-only hit = %+v", got[3])
	}
}

func TestNewHybridTitleLoader(t *testing.T) {
	loader := NewHybridTitleLoader("docs-out", func(path string) string {
		return "title:" + path
	})
	if loader == nil {
		t.Fatalf("loader is nil")
	}
	if got := loader("docs/guide.md"); got != "title:docs-out/docs/guide.md" {
		t.Fatalf("title = %q, want joined path title", got)
	}
	if got := NewHybridTitleLoader("docs-out", nil); got != nil {
		t.Fatalf("nil extractTitle loader = %v, want nil", got)
	}
}

func TestRunHybridRetrievalFlatPathUsesHyDEAndMerges(t *testing.T) {
	var (
		gotModel  string
		gotAPIKey string
		gotQuery  string
		gotOutDir string
		gotK      int
	)

	got, err := RunHybridRetrieval(HybridRetrievalRunInput[*embeddingRerankTestIndex]{
		Query:        "rank docs",
		BM25Hits:     []Hit{{Path: "docs/a.md", Score: 100}},
		OutDir:       "docs-out",
		DefaultModel: "text-embedding-3-small",
		APIKey:       "test-key",
		APIKeyEnv:    "OPENAI_API_KEY",
		DefaultK:     2,
		UseHyDE:      true,
		GenerateHyDE: func(query string) (string, bool) {
			if query != "rank docs" {
				t.Fatalf("HyDE query = %q, want rank docs", query)
			}
			return "hypothetical doc", true
		},
		EmbedQuery: func(model, apiKey, query string) ([][]float32, error) {
			gotModel = model
			gotAPIKey = apiKey
			gotQuery = query
			return [][]float32{{1, 0}}, nil
		},
		FlatTopK: func(outDir, model string, queryVec []float32, k int, depthPenalty float32) ([]HybridScoredPath, bool, error) {
			gotOutDir = outDir
			gotK = k
			if model != "text-embedding-3-small" || len(queryVec) != 2 || depthPenalty != 0.005 {
				t.Fatalf("flat inputs = model=%q queryVec=%v depthPenalty=%.3f", model, queryVec, depthPenalty)
			}
			return []HybridScoredPath{
				{Path: "docs/a.md", Score: 0.99},
				{Path: "docs/b.md", Score: 0.98},
			}, true, nil
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			t.Fatalf("OpenIndex should not run when flat index is available")
			return nil, nil
		},
		DepthPenaltyPerSegment: 0.005,
		Title: func(path string) string {
			return "title:" + path
		},
	})
	if err != nil {
		t.Fatalf("RunHybridRetrieval returned error: %v", err)
	}
	if gotModel != "text-embedding-3-small" || gotAPIKey != "test-key" || gotQuery != "hypothetical doc" || gotOutDir != "docs-out" || gotK != 2 {
		t.Fatalf("callback inputs = model=%q apiKey=%q query=%q outDir=%q k=%d", gotModel, gotAPIKey, gotQuery, gotOutDir, gotK)
	}
	if len(got) != 2 || got[0].Path != "docs/a.md" || got[1].Path != "docs/b.md" || got[1].Title != "title:docs/b.md" {
		t.Fatalf("hits = %+v, want BM25 plus embedding-only flat candidate", got)
	}
}

func TestRunHybridRetrievalFallbackSortsVectors(t *testing.T) {
	index := &embeddingRerankTestIndex{}
	var warned string

	got, err := RunHybridRetrieval(HybridRetrievalRunInput[*embeddingRerankTestIndex]{
		Query:                  "rank docs",
		BM25Hits:               []Hit{{Path: "docs/a.md", Score: 100}},
		APIKey:                 "test-key",
		Model:                  "custom-embedding-model",
		K:                      2,
		DepthPenaltyPerSegment: 0.1,
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return [][]float32{{1}}, nil
		},
		FlatTopK: func(_ string, _ string, _ []float32, _ int, _ float32) ([]HybridScoredPath, bool, error) {
			return nil, false, errors.New("flat missing")
		},
		WarnFlatFallback: func(err error) {
			warned = err.Error()
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			return index, nil
		},
		LoadVectors: func(gotIndex *embeddingRerankTestIndex, model string) (map[string][]float32, error) {
			if gotIndex != index || model != "custom-embedding-model" {
				t.Fatalf("load inputs = index=%p model=%q", gotIndex, model)
			}
			return map[string][]float32{
				"docs/deep/c.md": {0.89},
				"docs/b.md":      {0.80},
				"z.md":           {0.95},
			}, nil
		},
		ScoreVector: func(_ []float32, docVec []float32) float32 {
			return docVec[0]
		},
	})
	if err != nil {
		t.Fatalf("RunHybridRetrieval returned error: %v", err)
	}
	if warned != "flat missing" {
		t.Fatalf("warning = %q, want flat missing", warned)
	}
	if len(got) != 3 || got[0].Path != "docs/a.md" || got[1].Path != "z.md" || got[2].Path != "docs/b.md" {
		t.Fatalf("hits = %+v, want BM25 then top fallback vectors", got)
	}
	if !index.closed {
		t.Fatalf("index was not closed")
	}
}

func TestRunHybridRetrievalRejectsMissingAPIKey(t *testing.T) {
	bm25 := []Hit{{Path: "docs/a.md"}}
	got, err := RunHybridRetrieval(HybridRetrievalRunInput[*embeddingRerankTestIndex]{
		BM25Hits:  bm25,
		APIKeyEnv: "OPENAI_API_KEY",
	})
	if err == nil || err.Error() != ProviderAPIKeyNotSetError("OPENAI_API_KEY").Error() {
		t.Fatalf("expected missing API-key error, got hits=%+v err=%v", got, err)
	}
	if len(got) != 1 || got[0].Path != "docs/a.md" {
		t.Fatalf("hits = %+v, want original BM25 hits", got)
	}
}

func TestRunHybridRetrievalRejectsMissingEmbedQuery(t *testing.T) {
	bm25 := []Hit{{Path: "docs/a.md"}}
	got, err := RunHybridRetrieval(HybridRetrievalRunInput[*embeddingRerankTestIndex]{
		BM25Hits: bm25,
		APIKey:   "test-key",
	})
	if err == nil || err.Error() != EmbeddingQueryCallMissingError().Error() {
		t.Fatalf("expected missing embed-query error %q, got hits=%+v err=%v", EmbeddingQueryCallMissingError().Error(), got, err)
	}
	if len(got) != 1 || got[0].Path != "docs/a.md" {
		t.Fatalf("hits = %+v, want original BM25 hits", got)
	}
}

func TestRunHybridRetrievalWrapsEmbedQueryError(t *testing.T) {
	bm25 := []Hit{{Path: "docs/a.md"}}
	embedErr := errors.New("provider failed")
	got, err := RunHybridRetrieval(HybridRetrievalRunInput[*embeddingRerankTestIndex]{
		BM25Hits: bm25,
		APIKey:   "test-key",
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return nil, embedErr
		},
	})
	if err == nil || err.Error() != EmbeddingQueryError(embedErr).Error() {
		t.Fatalf("expected embed-query error, got hits=%+v err=%v", got, err)
	}
	if !errors.Is(err, embedErr) {
		t.Fatalf("embed-query error does not wrap provider error: %v", err)
	}
	if len(got) != 1 || got[0].Path != "docs/a.md" {
		t.Fatalf("hits = %+v, want original BM25 hits", got)
	}
}

func TestRunHybridRetrievalRejectsBadVectorCount(t *testing.T) {
	bm25 := []Hit{{Path: "docs/a.md"}}
	got, err := RunHybridRetrieval(HybridRetrievalRunInput[*embeddingRerankTestIndex]{
		BM25Hits: bm25,
		APIKey:   "test-key",
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return nil, nil
		},
	})
	if err == nil || err.Error() != EmbeddingQueryVectorCountError(0).Error() {
		t.Fatalf("expected vector-count error %q, got hits=%+v err=%v", EmbeddingQueryVectorCountError(0).Error(), got, err)
	}
	if len(got) != 1 || got[0].Path != "docs/a.md" {
		t.Fatalf("hits = %+v, want original BM25 hits", got)
	}
}

func TestRunHybridRetrievalRejectsMissingOpenIndex(t *testing.T) {
	bm25 := []Hit{{Path: "docs/a.md"}}
	got, err := RunHybridRetrieval(HybridRetrievalRunInput[*embeddingRerankTestIndex]{
		BM25Hits: bm25,
		APIKey:   "test-key",
		K:        1,
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return [][]float32{{1}}, nil
		},
	})
	if err == nil || err.Error() != EmbeddingIndexOpenCallMissingError().Error() {
		t.Fatalf("expected missing open-index error %q, got hits=%+v err=%v", EmbeddingIndexOpenCallMissingError().Error(), got, err)
	}
	if len(got) != 1 || got[0].Path != "docs/a.md" {
		t.Fatalf("hits = %+v, want original BM25 hits", got)
	}
}

func TestRunHybridRetrievalWrapsOpenIndexError(t *testing.T) {
	bm25 := []Hit{{Path: "docs/a.md"}}
	openErr := errors.New("db unavailable")
	got, err := RunHybridRetrieval(HybridRetrievalRunInput[*embeddingRerankTestIndex]{
		BM25Hits: bm25,
		APIKey:   "test-key",
		K:        1,
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return [][]float32{{1}}, nil
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			return nil, openErr
		},
		LoadVectors: func(_ *embeddingRerankTestIndex, _ string) (map[string][]float32, error) {
			t.Fatalf("LoadVectors should not run after open-index error")
			return nil, nil
		},
		ScoreVector: func(_ []float32, _ []float32) float32 {
			t.Fatalf("ScoreVector should not run after open-index error")
			return 0
		},
	})
	if err == nil || err.Error() != EmbeddingIndexOpenError(openErr).Error() {
		t.Fatalf("expected open-index error, got hits=%+v err=%v", got, err)
	}
	if !errors.Is(err, openErr) {
		t.Fatalf("open-index error does not wrap opener error: %v", err)
	}
	if len(got) != 1 || got[0].Path != "docs/a.md" {
		t.Fatalf("hits = %+v, want original BM25 hits", got)
	}
}

func TestRunHybridRetrievalRejectsMissingLoadVectors(t *testing.T) {
	bm25 := []Hit{{Path: "docs/a.md"}}
	got, err := RunHybridRetrieval(HybridRetrievalRunInput[*embeddingRerankTestIndex]{
		BM25Hits: bm25,
		APIKey:   "test-key",
		K:        1,
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return [][]float32{{1}}, nil
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			t.Fatalf("OpenIndex should not run when LoadVectors is missing")
			return nil, nil
		},
	})
	if err == nil || err.Error() != EmbeddingLoadVectorsCallMissingError().Error() {
		t.Fatalf("expected missing load-vectors error %q, got hits=%+v err=%v", EmbeddingLoadVectorsCallMissingError().Error(), got, err)
	}
	if len(got) != 1 || got[0].Path != "docs/a.md" {
		t.Fatalf("hits = %+v, want original BM25 hits", got)
	}
}

func TestRunHybridRetrievalWrapsLoadVectorsError(t *testing.T) {
	bm25 := []Hit{{Path: "docs/a.md"}}
	index := &embeddingRerankTestIndex{}
	loadErr := errors.New("cache unavailable")
	got, err := RunHybridRetrieval(HybridRetrievalRunInput[*embeddingRerankTestIndex]{
		BM25Hits: bm25,
		APIKey:   "test-key",
		K:        1,
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return [][]float32{{1}}, nil
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			return index, nil
		},
		LoadVectors: func(_ *embeddingRerankTestIndex, _ string) (map[string][]float32, error) {
			return nil, loadErr
		},
		ScoreVector: func(_ []float32, _ []float32) float32 {
			t.Fatalf("ScoreVector should not run after load-vectors error")
			return 0
		},
	})
	if err == nil || err.Error() != EmbeddingLoadVectorsError(loadErr).Error() {
		t.Fatalf("expected load-vectors error, got hits=%+v err=%v", got, err)
	}
	if !errors.Is(err, loadErr) {
		t.Fatalf("load-vectors error does not wrap loader error: %v", err)
	}
	if len(got) != 1 || got[0].Path != "docs/a.md" {
		t.Fatalf("hits = %+v, want original BM25 hits", got)
	}
	if !index.closed {
		t.Fatalf("index was not closed")
	}
}

func TestRunHybridRetrievalRejectsMissingScoreVector(t *testing.T) {
	bm25 := []Hit{{Path: "docs/a.md"}}
	got, err := RunHybridRetrieval(HybridRetrievalRunInput[*embeddingRerankTestIndex]{
		BM25Hits: bm25,
		APIKey:   "test-key",
		K:        1,
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return [][]float32{{1}}, nil
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			t.Fatalf("OpenIndex should not run when ScoreVector is missing")
			return nil, nil
		},
		LoadVectors: func(_ *embeddingRerankTestIndex, _ string) (map[string][]float32, error) {
			t.Fatalf("LoadVectors should not run when ScoreVector is missing")
			return nil, nil
		},
	})
	if err == nil || err.Error() != EmbeddingScoreVectorCallMissingError().Error() {
		t.Fatalf("expected missing score-vector error %q, got hits=%+v err=%v", EmbeddingScoreVectorCallMissingError().Error(), got, err)
	}
	if len(got) != 1 || got[0].Path != "docs/a.md" {
		t.Fatalf("hits = %+v, want original BM25 hits", got)
	}
}

func TestRunHybridRetrievalReturnsCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	index := &embeddingRerankTestIndex{closeErr: closeErr}
	got, err := RunHybridRetrieval(HybridRetrievalRunInput[*embeddingRerankTestIndex]{
		BM25Hits: []Hit{{Path: "docs/a.md"}},
		APIKey:   "test-key",
		K:        1,
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return [][]float32{{1}}, nil
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			return index, nil
		},
		LoadVectors: func(_ *embeddingRerankTestIndex, _ string) (map[string][]float32, error) {
			return map[string][]float32{"docs/b.md": {1}}, nil
		},
		ScoreVector: func(_ []float32, docVec []float32) float32 {
			return docVec[0]
		},
	})
	if err == nil || err.Error() != EmbeddingIndexCloseError(closeErr).Error() {
		t.Fatalf("expected close error, got hits=%+v err=%v", got, err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("close error does not wrap index close error: %v", err)
	}
	if len(got) != 2 || got[0].Path != "docs/a.md" || got[1].Path != "docs/b.md" {
		t.Fatalf("hits = %+v, want merged hits preserved with close error", got)
	}
}

func TestLoadLLMRerankCandidates(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "guide.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ntitle: Guide\n---\n\nFirst line\nsecond line"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := LoadLLMRerankCandidates([]Hit{
		{Path: "docs/guide.md", Title: "Guide"},
		{Path: "docs/missing.md", Title: "Missing"},
	}, root, 16)
	if len(got) != 2 {
		t.Fatalf("candidates = %+v, want 2", got)
	}
	if got[0].Index != 0 || got[0].Path != "docs/guide.md" || got[0].Title != "Guide" {
		t.Fatalf("first candidate identity = %+v", got[0])
	}
	if got[0].Snippet != "First line secon" {
		t.Fatalf("first candidate snippet = %q, want trimmed body excerpt", got[0].Snippet)
	}
	if got[1].Index != 1 || got[1].Snippet != "" {
		t.Fatalf("missing-file candidate = %+v, want title/path only", got[1])
	}
}

func TestStripFrontmatter(t *testing.T) {
	cases := map[string]string{
		"":                             "",
		"no frontmatter\n\nbody":       "no frontmatter\n\nbody",
		"---\nfoo: bar\n---\nbody":     "body",
		"---\nfoo: bar\n---\n\n\nbody": "body",
		"---\nfoo: bar\n---":           "",
		"--- not really frontmatter":   "--- not really frontmatter",
		"---\nunclosed":                "---\nunclosed",
		"--- title: with --- in value\nbody\n---\ntrailer": "trailer",
	}
	for in, want := range cases {
		got := stripFrontmatter(in)
		if got != want {
			t.Errorf("stripFrontmatter(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildLLMRerankPromptAndDocumentText(t *testing.T) {
	candidates := []LLMRerankCandidate{
		{Index: 2, Path: "docs/guide.md", Title: "Guide", Snippet: "body text"},
		{Index: 3, Path: "docs/ref.md", Title: "Reference"},
	}
	prompt := BuildLLMRerankPrompt("how to test", candidates)
	for _, want := range []string{`Query: "how to test"`, `[2] title="Guide" path=docs/guide.md`, "snippet: body text", `"order"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
	if got := LLMRerankDocumentText(candidates[0]); got != "Guide\nbody text" {
		t.Fatalf("document text = %q, want title plus snippet", got)
	}
	if got := LLMRerankDocumentText(candidates[1]); got != "Reference" {
		t.Fatalf("document text without snippet = %q, want title", got)
	}
}

func TestResolveRerankProviderPolicy(t *testing.T) {
	providers := []RerankProviderPolicyProvider{
		{Name: "anthropic", PricingModels: []string{"claude-3-5-sonnet"}},
		{Name: "cohere", PricingModels: []string{"rerank-v3.5"}},
		{Name: "openai", OpenAICompat: true, PricingModels: []string{"gpt-4.1-mini", "gpt-4o-mini"}},
		{Name: "tei", PricingModels: []string{"BAAI/bge-reranker-v2-m3"}},
	}

	got, err := ResolveRerankProviderPolicy(RerankProviderPolicyInput{
		DefaultProvider: "openai",
		DefaultModel:    "gpt-4.1-mini",
		Providers:       providers,
	})
	if err != nil {
		t.Fatalf("ResolveRerankProviderPolicy returned error: %v", err)
	}
	if got.ProviderName != "openai" || got.Model != "gpt-4.1-mini" {
		t.Fatalf("result = %+v, want openai/gpt-4.1-mini", got)
	}

	got, err = ResolveRerankProviderPolicy(RerankProviderPolicyInput{
		RequestedProvider: "cohere",
		RequestedModel:    "rerank-v3.5",
		DefaultProvider:   "openai",
		DefaultModel:      "gpt-4.1-mini",
		Providers:         providers,
	})
	if err != nil {
		t.Fatalf("ResolveRerankProviderPolicy(cohere) returned error: %v", err)
	}
	if got.ProviderName != "cohere" || got.Model != "rerank-v3.5" {
		t.Fatalf("result = %+v, want cohere/rerank-v3.5", got)
	}
}

func TestNewRerankProviderPolicyProvider(t *testing.T) {
	models := []string{"gpt-4o-mini", "gpt-4.1-mini"}
	got := NewRerankProviderPolicyProvider("openai", true, models)
	if got.Name != "openai" || !got.OpenAICompat {
		t.Fatalf("provider identity = %+v, want openai compatible", got)
	}
	if len(got.PricingModels) != 2 || got.PricingModels[0] != "gpt-4.1-mini" || got.PricingModels[1] != "gpt-4o-mini" {
		t.Fatalf("pricing models = %v, want sorted copy", got.PricingModels)
	}
	models[0] = "mutated"
	if got.PricingModels[1] != "gpt-4o-mini" {
		t.Fatalf("pricing models mutated with input slice: %v", got.PricingModels)
	}
}

func TestNewRerankUsagePricing(t *testing.T) {
	got := NewRerankUsagePricing(0.15, 0.60)
	if got.InputPer1M != 0.15 || got.OutputPer1M != 0.60 {
		t.Fatalf("pricing = %+v, want input=0.15 output=0.60", got)
	}
}

func TestHyDEUsagePricingAndCents(t *testing.T) {
	cases := []struct {
		model     string
		inputPer  float64
		outputPer float64
		wantCents float64
	}{
		{
			model:     "gpt-4.1-mini",
			inputPer:  0.15,
			outputPer: 0.60,
			wantCents: (1000.0/1_000_000*0.15 + 500.0/1_000_000*0.60) * 100,
		},
		{
			model:     "gpt-some-other",
			inputPer:  0.15,
			outputPer: 0.60,
			wantCents: (1000.0/1_000_000*0.15 + 500.0/1_000_000*0.60) * 100,
		},
		{
			model:     "gpt-4o",
			inputPer:  2.50,
			outputPer: 10.00,
			wantCents: (1000.0/1_000_000*2.50 + 500.0/1_000_000*10.00) * 100,
		},
		{
			model:     "gpt-4.1-nano",
			inputPer:  0.10,
			outputPer: 0.40,
			wantCents: (1000.0/1_000_000*0.10 + 500.0/1_000_000*0.40) * 100,
		},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			pricing := HyDEUsagePricing(tc.model)
			if pricing.InputPer1M != tc.inputPer || pricing.OutputPer1M != tc.outputPer {
				t.Fatalf("pricing = %+v, want input=%v output=%v", pricing, tc.inputPer, tc.outputPer)
			}
			if got := EstimateHyDEUsageCents(tc.model, 1000, 500); got < tc.wantCents-0.000000001 || got > tc.wantCents+0.000000001 {
				t.Fatalf("cents = %v, want %v", got, tc.wantCents)
			}
			event := BuildHyDEUsageEvent(tc.model, 1000, 500)
			if event.ProviderName != "openai" || event.Model != tc.model || event.CallSite != DefaultHyDEUsageCallSite {
				t.Fatalf("HyDE event identity = %+v, want openai/model/default callsite", event)
			}
			if event.InputTokens != 1000 || event.OutputTokens != 500 {
				t.Fatalf("HyDE event tokens = %+v, want input=1000 output=500", event)
			}
			if event.Cents < tc.wantCents-0.000000001 || event.Cents > tc.wantCents+0.000000001 {
				t.Fatalf("HyDE event cents = %v, want %v", event.Cents, tc.wantCents)
			}
		})
	}
}

func TestEmbeddingUsagePricingAndEvent(t *testing.T) {
	pricingCases := []struct {
		model string
		want  float64
	}{
		{model: "text-embedding-3-small", want: 0.02},
		{model: "text-embedding-3-large", want: 0.13},
		{model: "text-embedding-ada-002", want: 0.10},
		{model: "unknown", want: 0},
	}
	for _, tc := range pricingCases {
		if got := EmbeddingUsagePricing(tc.model); got != tc.want {
			t.Fatalf("EmbeddingUsagePricing(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
	if !IsKnownEmbeddingModel(DefaultEmbeddingModel) {
		t.Fatalf("default embedding model %q should be known", DefaultEmbeddingModel)
	}
	if IsKnownEmbeddingModel("unknown") {
		t.Fatal("unknown embedding model should not be known")
	}
	if got, want := strings.Join(KnownEmbeddingModels(), ","), "text-embedding-3-large,text-embedding-3-small,text-embedding-ada-002"; got != want {
		t.Fatalf("known embedding models = %q, want %q", got, want)
	}
	if got := EstimateEmbeddingUsageUSD("text-embedding-3-large", 1000); got < 0.000129 || got > 0.000131 {
		t.Fatalf("embedding dollars = %.6f, want 0.00013", got)
	}
	if got, want := EmbeddingUnknownModelError("custom-embedding").Error(), `unknown model "custom-embedding" (known: [text-embedding-3-large text-embedding-3-small text-embedding-ada-002])`; got != want {
		t.Fatalf("unknown model error = %q, want %q", got, want)
	}
	if got, want := EmbeddingSourceNotFoundError("supabase", "/tmp/docs").Error(), `source "supabase" not found under /tmp/docs`; got != want {
		t.Fatalf("source not found error = %q, want %q", got, want)
	}
	if DefaultEmbeddingInputMaxChars != 16000 {
		t.Fatalf("embedding input max chars = %d, want 16000", DefaultEmbeddingInputMaxChars)
	}
	if DefaultOpenAIEmbeddingEndpoint != "https://api.openai.com/v1/embeddings" {
		t.Fatalf("embedding endpoint = %q, want OpenAI embeddings endpoint", DefaultOpenAIEmbeddingEndpoint)
	}
	if got := ClampEmbeddingInputChars(16003); got != 16000 {
		t.Fatalf("clamped embedding chars = %d, want 16000", got)
	}
	if got := ClampEmbeddingInputChars(-1); got != 0 {
		t.Fatalf("negative embedding chars = %d, want 0", got)
	}
	long := strings.Repeat("x", DefaultEmbeddingInputMaxChars+1)
	if got := len(TruncateEmbeddingInput(long)); got != DefaultEmbeddingInputMaxChars {
		t.Fatalf("truncated embedding input length = %d, want %d", got, DefaultEmbeddingInputMaxChars)
	}
	batch := []struct {
		title string
		body  string
	}{
		{title: "Title", body: "body"},
		{body: strings.Repeat("b", DefaultEmbeddingInputMaxChars+1)},
	}
	inputs := BuildTruncatedEmbeddingBatchInputs(batch, func(item struct {
		title string
		body  string
	}) string {
		return BuildEmbeddingInput(item.title, item.body)
	})
	if len(inputs) != 2 {
		t.Fatalf("batch input count = %d, want 2", len(inputs))
	}
	if inputs[0] != "Title\n\nbody" {
		t.Fatalf("first input = %q, want title-prefixed body", inputs[0])
	}
	if len(inputs[1]) != DefaultEmbeddingInputMaxChars {
		t.Fatalf("second input len = %d, want cap %d", len(inputs[1]), DefaultEmbeddingInputMaxChars)
	}
	inputTokens, flush := PlanEmbeddingBatchAppend(0, 0, DefaultEmbeddingInputMaxChars+1)
	if flush {
		t.Fatal("empty embedding batch append should not flush")
	}
	if inputTokens != EstimateEmbeddingBatchTokensFromChars(DefaultEmbeddingInputMaxChars) {
		t.Fatalf("planned input tokens = %d, want capped estimate", inputTokens)
	}
	inputTokens, flush = PlanEmbeddingBatchAppend(DefaultEmbeddingBatchMaxInputs, 0, DefaultEmbeddingBatchCharsPerToken)
	if !flush {
		t.Fatal("full embedding batch append should flush")
	}
	if inputTokens != 1 {
		t.Fatalf("full-batch planned input tokens = %d, want 1", inputTokens)
	}
	inputTokens, flush = PlanEmbeddingBatchAppend(1, DefaultEmbeddingBatchTokenCap, DefaultEmbeddingBatchCharsPerToken)
	if !flush {
		t.Fatal("token-capped embedding batch append should flush")
	}
	if inputTokens != 1 {
		t.Fatalf("token-capped planned input tokens = %d, want 1", inputTokens)
	}
	tokenLimitedItems := make([]int, 34)
	for i := range tokenLimitedItems {
		tokenLimitedItems[i] = DefaultEmbeddingInputMaxChars
	}
	tokenLimitedBatches := collectEmbeddingBatchItems(tokenLimitedItems)
	if len(tokenLimitedBatches) != 2 {
		t.Fatalf("token-limited batch count = %d, want 2", len(tokenLimitedBatches))
	}
	if len(tokenLimitedBatches[0]) != 33 || len(tokenLimitedBatches[1]) != 1 {
		t.Fatalf("token-limited batch sizes = %d,%d; want 33,1", len(tokenLimitedBatches[0]), len(tokenLimitedBatches[1]))
	}
	inputLimitedItems := make([]int, DefaultEmbeddingBatchMaxInputs+1)
	inputLimitedBatches := collectEmbeddingBatchItems(inputLimitedItems)
	if len(inputLimitedBatches) != 2 {
		t.Fatalf("input-limited batch count = %d, want 2", len(inputLimitedBatches))
	}
	if len(inputLimitedBatches[0]) != DefaultEmbeddingBatchMaxInputs || len(inputLimitedBatches[1]) != 1 {
		t.Fatalf("input-limited batch sizes = %d,%d; want %d,1", len(inputLimitedBatches[0]), len(inputLimitedBatches[1]), DefaultEmbeddingBatchMaxInputs)
	}
	mergedItems, mergedVecs, mergedDropped := MergeEmbeddingSplitResults(1, []string{"left"}, [][]float32{{1}}, 2, []string{"right"}, [][]float32{{2}}, 3)
	if !slices.Equal(mergedItems, []string{"left", "right"}) {
		t.Fatalf("merged items = %v, want left/right", mergedItems)
	}
	if len(mergedVecs) != 2 || len(mergedVecs[0]) != 1 || mergedVecs[0][0] != 1 || len(mergedVecs[1]) != 1 || mergedVecs[1][0] != 2 {
		t.Fatalf("merged vecs = %v, want concatenated vectors", mergedVecs)
	}
	if mergedDropped != 6 {
		t.Fatalf("merged dropped = %d, want 6", mergedDropped)
	}
	if DefaultEmbeddingPreflightCharsPerToken != 4 {
		t.Fatalf("embedding chars/token estimate = %d, want 4", DefaultEmbeddingPreflightCharsPerToken)
	}
	if got := EstimateEmbeddingTokensFromChars(16003); got != 4000 {
		t.Fatalf("embedding token estimate = %d, want 4000", got)
	}
	if got := EstimateEmbeddingTokensFromChars(-1); got != 0 {
		t.Fatalf("negative char estimate = %d, want 0", got)
	}
	if DefaultEmbeddingBatchTokenCap != 180000 {
		t.Fatalf("embedding batch token cap = %d, want 180000", DefaultEmbeddingBatchTokenCap)
	}
	if DefaultEmbeddingBatchMaxInputs != 2048 {
		t.Fatalf("embedding batch max inputs = %d, want 2048", DefaultEmbeddingBatchMaxInputs)
	}
	if DefaultEmbeddingBatchCharsPerToken != 3 {
		t.Fatalf("embedding batch chars/token estimate = %d, want 3", DefaultEmbeddingBatchCharsPerToken)
	}
	if got := EstimateEmbeddingBatchTokensFromChars(16003); got != 5334 {
		t.Fatalf("embedding batch token estimate = %d, want 5334", got)
	}
	if got := BuildEmbeddingInput("", "body"); got != "body" {
		t.Fatalf("embedding input without title = %q, want body", got)
	}
	if got := BuildEmbeddingInput("Title", "body"); got != "Title\n\nbody" {
		t.Fatalf("embedding input with title = %q, want title/body format", got)
	}

	got := BuildEmbeddingUsageEvent(EmbeddingUsageEventInput{
		ProviderName: "openai",
		Model:        "text-embedding-3-small",
		PromptTokens: 2000,
		TotalTokens:  3000,
	})
	if got.ProviderName != "openai" || got.Model != "text-embedding-3-small" || got.CallSite != DefaultEmbeddingUsageCallSite {
		t.Fatalf("event identity = %+v, want default callsite and provider/model", got)
	}
	if got.Tokens != 2000 {
		t.Fatalf("tokens = %d, want prompt_tokens", got.Tokens)
	}
	if got.Cents < 0.003999 || got.Cents > 0.004001 {
		t.Fatalf("cents = %.6f, want 0.004", got.Cents)
	}

	fallback := BuildEmbeddingUsageEvent(EmbeddingUsageEventInput{
		ProviderName: "openai",
		Model:        "text-embedding-3-large",
		CallSite:     "custom",
		TotalTokens:  1000,
	})
	if fallback.CallSite != "custom" || fallback.Tokens != 1000 {
		t.Fatalf("fallback event = %+v, want custom callsite and total_tokens", fallback)
	}
	if fallback.Cents < 0.012999 || fallback.Cents > 0.013001 {
		t.Fatalf("fallback cents = %.6f, want 0.013", fallback.Cents)
	}

	if got := EstimateEmbeddingUsageCents("text-embedding-3-small", -1); got != 0 {
		t.Fatalf("negative embedding cents = %.6f, want 0", got)
	}
}

func TestBuildHyDEPrompt(t *testing.T) {
	prompt := BuildHyDEPrompt("how do I configure row level security in supabase")
	checks := []string{
		"row level security",
		"hypothetical",
		"Output ONLY",
		"150 words",
		"Match the tone of authoritative technical reference",
	}
	for _, check := range checks {
		if !strings.Contains(prompt, check) {
			t.Fatalf("prompt missing %q: %s", check, prompt)
		}
	}
	if strings.Contains(prompt, "```") {
		t.Fatalf("prompt should not request markdown formatting: %s", prompt)
	}
}

func TestResolveHyDEModel(t *testing.T) {
	if got := ResolveHyDEModel("", "gpt-4.1-mini"); got != "gpt-4.1-mini" {
		t.Fatalf("empty requested model = %q, want default", got)
	}
	if got := ResolveHyDEModel("gpt-4o", "gpt-4.1-mini"); got != "gpt-4o" {
		t.Fatalf("requested model = %q, want requested", got)
	}
}

func TestHyDEFallbackWarnings(t *testing.T) {
	if got := HyDEMissingAPIKeyWarning("OPENAI_API_KEY"); got != "rerank-hyde: OPENAI_API_KEY not set — using raw query for embedding" {
		t.Fatalf("missing API-key warning = %q", got)
	}
	err := errors.New("openai status 500")
	if got := HyDEGenerationFallbackWarning(err); got != "rerank-hyde: openai status 500 — using raw query for embedding" {
		t.Fatalf("generation fallback warning = %q", got)
	}
}

func TestHyDEErrors(t *testing.T) {
	if err := HyDEProviderNotRegisteredError(); err == nil || err.Error() != "openai provider not registered (registry mis-init)" {
		t.Fatalf("HyDEProviderNotRegisteredError = %v", err)
	}

	statusErr := HyDEOpenAIStatusError(500, strings.Repeat("x", 210), 200)
	if statusErr == nil || statusErr.Error() != "openai status 500: "+strings.Repeat("x", 200)+"..." {
		t.Fatalf("HyDEOpenAIStatusError = %v", statusErr)
	}

	if err := HyDEProviderError("bad input"); err == nil || err.Error() != "openai error: bad input" {
		t.Fatalf("HyDEProviderError = %v", err)
	}
	if err := HyDEEmptyResponseError(); err == nil || err.Error() != "empty hyde response" {
		t.Fatalf("HyDEEmptyResponseError = %v", err)
	}
}

func TestNormalizeHyDEDocument(t *testing.T) {
	got, ok := NormalizeHyDEDocument("  canonical docs passage  \n")
	if !ok || got != "canonical docs passage" {
		t.Fatalf("NormalizeHyDEDocument = %q, %v; want trimmed document", got, ok)
	}
	if got, ok := NormalizeHyDEDocument(" \t\n "); ok || got != "" {
		t.Fatalf("empty NormalizeHyDEDocument = %q, %v; want empty false", got, ok)
	}
}

func TestHyDERetryBackoff(t *testing.T) {
	if got := HyDERetryBackoff(0); got != 0 {
		t.Fatalf("initial backoff = %v, want 0", got)
	}
	if got := HyDERetryBackoff(1); got != 2*time.Second {
		t.Fatalf("first retry backoff = %v, want 2s", got)
	}
	if got := HyDERetryBackoff(2); got != 4*time.Second {
		t.Fatalf("second retry backoff = %v, want 4s", got)
	}
}

func TestHyDERetryableStatus(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{status: http.StatusOK, want: false},
		{status: http.StatusBadRequest, want: false},
		{status: http.StatusUnauthorized, want: false},
		{status: http.StatusTooManyRequests, want: true},
		{status: http.StatusInternalServerError, want: true},
		{status: http.StatusBadGateway, want: true},
	}
	for _, tc := range cases {
		if got := HyDERetryableStatus(tc.status); got != tc.want {
			t.Fatalf("HyDERetryableStatus(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestEmbeddingRetryBackoff(t *testing.T) {
	if got := EmbeddingRetryBackoff(0); got != 0 {
		t.Fatalf("initial embedding backoff = %v, want 0", got)
	}
	if got := EmbeddingRetryBackoff(1); got != 2*time.Second {
		t.Fatalf("first embedding retry backoff = %v, want 2s", got)
	}
	if got := EmbeddingRetryBackoff(2); got != 4*time.Second {
		t.Fatalf("second embedding retry backoff = %v, want 4s", got)
	}
}

func TestDefaultEmbeddingMaxAttempts(t *testing.T) {
	if DefaultEmbeddingMaxAttempts != 3 {
		t.Fatalf("DefaultEmbeddingMaxAttempts = %d, want 3", DefaultEmbeddingMaxAttempts)
	}
}

func TestResetOpenAIEmbeddingRequestBody(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.test/v1/embeddings", nil)
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}

	ResetOpenAIEmbeddingRequestBody(req, []byte("first"))
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll first body returned error: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("first body = %q, want %q", got, "first")
	}
	if req.ContentLength != int64(len("first")) {
		t.Fatalf("first content length = %d, want %d", req.ContentLength, len("first"))
	}

	ResetOpenAIEmbeddingRequestBody(req, []byte("second"))
	got, err = io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll second body returned error: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("second body = %q, want %q", got, "second")
	}
	if req.ContentLength != int64(len("second")) {
		t.Fatalf("second content length = %d, want %d", req.ContentLength, len("second"))
	}
}

type trackingReadCloser struct {
	reader *strings.Reader
	closed bool
}

func (r *trackingReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestReadAndCloseOpenAIEmbeddingResponseBody(t *testing.T) {
	body := &trackingReadCloser{reader: strings.NewReader("payload")}
	resp := &http.Response{Body: body}

	got := ReadAndCloseOpenAIEmbeddingResponseBody(resp)
	if string(got) != "payload" {
		t.Fatalf("response body = %q, want payload", got)
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func TestEmbeddingRetryableStatus(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{status: http.StatusOK, want: false},
		{status: http.StatusBadRequest, want: false},
		{status: http.StatusUnauthorized, want: false},
		{status: http.StatusTooManyRequests, want: true},
		{status: http.StatusInternalServerError, want: true},
		{status: http.StatusBadGateway, want: true},
	}
	for _, tc := range cases {
		if got := EmbeddingRetryableStatus(tc.status); got != tc.want {
			t.Fatalf("EmbeddingRetryableStatus(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestEmbeddingSuccessStatus(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{status: http.StatusOK, want: true},
		{status: http.StatusCreated, want: false},
		{status: http.StatusBadRequest, want: false},
		{status: http.StatusTooManyRequests, want: false},
		{status: http.StatusInternalServerError, want: false},
	}
	for _, tc := range cases {
		if got := EmbeddingSuccessStatus(tc.status); got != tc.want {
			t.Fatalf("EmbeddingSuccessStatus(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestIsEmbeddingBatchTokenLimitError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "max tokens", err: errors.New("Requested 12000 tokens; max tokens per request is 8192"), want: true},
		{name: "maximum context", err: errors.New("maximum context length exceeded"), want: true},
		{name: "too many tokens", err: errors.New("too many tokens in request"), want: true},
		{name: "requested tokens", err: errors.New("requested 12000 input tokens"), want: true},
		{name: "unrelated", err: errors.New("connection reset by peer"), want: false},
	}
	for _, tc := range cases {
		if got := IsEmbeddingBatchTokenLimitError(tc.err); got != tc.want {
			t.Fatalf("%s: IsEmbeddingBatchTokenLimitError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

func TestEmbeddingBatchSplitPoint(t *testing.T) {
	cases := []struct {
		inputCount int
		wantMid    int
		wantOK     bool
	}{
		{inputCount: -1, wantMid: 0, wantOK: false},
		{inputCount: 0, wantMid: 0, wantOK: false},
		{inputCount: 1, wantMid: 0, wantOK: false},
		{inputCount: 2, wantMid: 1, wantOK: true},
		{inputCount: 3, wantMid: 1, wantOK: true},
		{inputCount: 4, wantMid: 2, wantOK: true},
	}
	for _, tc := range cases {
		gotMid, gotOK := EmbeddingBatchSplitPoint(tc.inputCount)
		if gotMid != tc.wantMid || gotOK != tc.wantOK {
			t.Fatalf("EmbeddingBatchSplitPoint(%d) = (%d, %v), want (%d, %v)", tc.inputCount, gotMid, gotOK, tc.wantMid, tc.wantOK)
		}
	}
}

func TestParseEmbeddingInputIndexError(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantIdx int
		wantOK  bool
	}{
		{name: "nil", err: nil, wantOK: false},
		{name: "match", err: errors.New("Invalid 'input[3]': input is too large"), wantIdx: 3, wantOK: true},
		{name: "zero", err: errors.New("Invalid 'input[0]'"), wantIdx: 0, wantOK: true},
		{name: "unrelated", err: errors.New("max tokens per request exceeded"), wantOK: false},
	}
	for _, tc := range cases {
		gotIdx, gotOK := ParseEmbeddingInputIndexError(tc.err)
		if gotIdx != tc.wantIdx || gotOK != tc.wantOK {
			t.Fatalf("%s: ParseEmbeddingInputIndexError(%v) = (%d, %v), want (%d, %v)", tc.name, tc.err, gotIdx, gotOK, tc.wantIdx, tc.wantOK)
		}
	}
}

func TestEmbeddingInputIndexInRange(t *testing.T) {
	cases := []struct {
		name       string
		index      int
		inputCount int
		want       bool
	}{
		{name: "negative index", index: -1, inputCount: 2, want: false},
		{name: "empty batch", index: 0, inputCount: 0, want: false},
		{name: "first", index: 0, inputCount: 2, want: true},
		{name: "last", index: 1, inputCount: 2, want: true},
		{name: "too large", index: 2, inputCount: 2, want: false},
	}
	for _, tc := range cases {
		if got := EmbeddingInputIndexInRange(tc.index, tc.inputCount); got != tc.want {
			t.Fatalf("%s: EmbeddingInputIndexInRange(%d, %d) = %v, want %v", tc.name, tc.index, tc.inputCount, got, tc.want)
		}
	}
}

func TestResolveRerankProviderPolicyRejectsUnsupportedProvider(t *testing.T) {
	got, err := ResolveRerankProviderPolicy(RerankProviderPolicyInput{
		RequestedProvider: "anthropic",
		RequestedModel:    "claude-3-5-sonnet",
		DefaultProvider:   "openai",
		DefaultModel:      "gpt-4.1-mini",
		Providers: []RerankProviderPolicyProvider{
			{Name: "anthropic", PricingModels: []string{"claude-3-5-sonnet"}},
			{Name: "openai", OpenAICompat: true, PricingModels: []string{"gpt-4.1-mini"}},
		},
	})
	if err == nil || err.Error() != RerankProviderUnsupportedError("anthropic").Error() {
		t.Fatalf("expected unsupported-provider error, got result=%+v err=%v", got, err)
	}
}

func TestResolveRerankProviderPolicyRejectsUnknownProviderAndModel(t *testing.T) {
	providers := []RerankProviderPolicyProvider{
		{Name: "cohere", PricingModels: []string{"rerank-v3.5"}},
		{Name: "openai", OpenAICompat: true, PricingModels: []string{"gpt-4.1-mini", "gpt-4o-mini"}},
	}

	got, err := ResolveRerankProviderPolicy(RerankProviderPolicyInput{
		RequestedProvider: "missing",
		DefaultProvider:   "openai",
		DefaultModel:      "gpt-4.1-mini",
		Providers:         providers,
	})
	if err == nil || err.Error() != RerankProviderUnknownError("missing", []string{"openai", "cohere"}).Error() {
		t.Fatalf("expected sorted unknown-provider error, got result=%+v err=%v", got, err)
	}

	got, err = ResolveRerankProviderPolicy(RerankProviderPolicyInput{
		RequestedProvider: "openai",
		RequestedModel:    "not-a-model",
		DefaultProvider:   "openai",
		DefaultModel:      "gpt-4.1-mini",
		Providers:         providers,
	})
	if err == nil || err.Error() != RerankProviderModelNotRegisteredError("not-a-model", "openai", []string{"gpt-4o-mini", "gpt-4.1-mini"}).Error() {
		t.Fatalf("expected sorted unknown-model error, got result=%+v err=%v", got, err)
	}

	if got, want := RerankProviderRegistryDisappearedError("openai").Error(), `rerank provider "openai" disappeared from registry`; got != want {
		t.Fatalf("RerankProviderRegistryDisappearedError() = %q, want %q", got, want)
	}
}

func TestResolveProviderAPIKey(t *testing.T) {
	if DefaultEmbeddingAPIKeyEnv != "OPENAI_API_KEY" {
		t.Fatalf("default embedding API-key env = %q, want OPENAI_API_KEY", DefaultEmbeddingAPIKeyEnv)
	}
	if DefaultEmbeddingModel != "text-embedding-3-small" {
		t.Fatalf("default embedding model = %q, want text-embedding-3-small", DefaultEmbeddingModel)
	}
	if DefaultRerankK != 10 {
		t.Fatalf("default rerank K = %d, want 10", DefaultRerankK)
	}
	if DefaultEmbeddingRerankCallSite != "docs-puller.search.embed_rerank" {
		t.Fatalf("default embedding-rerank call site = %q, want docs-puller.search.embed_rerank", DefaultEmbeddingRerankCallSite)
	}
	if DefaultHybridRetrievalCallSite != "docs-puller.search.hybrid_retrieval" {
		t.Fatalf("default hybrid-retrieval call site = %q, want docs-puller.search.hybrid_retrieval", DefaultHybridRetrievalCallSite)
	}
	if DefaultHyDEAPIKeyEnv != DefaultEmbeddingAPIKeyEnv {
		t.Fatalf("default HyDE API-key env = %q, want %s", DefaultHyDEAPIKeyEnv, DefaultEmbeddingAPIKeyEnv)
	}
	if DefaultHyDEUsageCallSite != "docs-puller.rerank_hyde" {
		t.Fatalf("default HyDE usage call site = %q, want docs-puller.rerank_hyde", DefaultHyDEUsageCallSite)
	}
	if DefaultRerankUsageCallSite != "docs-puller.rerank_llm" {
		t.Fatalf("default rerank usage call site = %q, want docs-puller.rerank_llm", DefaultRerankUsageCallSite)
	}
	if DefaultLLMRerankModel != "gpt-4.1-mini" {
		t.Fatalf("default LLM rerank model = %q, want gpt-4.1-mini", DefaultLLMRerankModel)
	}
	if DefaultRerankProvider != "openai" {
		t.Fatalf("default rerank provider = %q, want openai", DefaultRerankProvider)
	}
	if got := ProviderAPIKeyNotSetError(DefaultEmbeddingAPIKeyEnv).Error(); got != "OPENAI_API_KEY not set" {
		t.Fatalf("missing API-key error = %q, want OPENAI_API_KEY not set", got)
	}

	key, err := ResolveProviderAPIKey(ProviderAPIKeyInput{
		KeyEnv: DefaultEmbeddingAPIKeyEnv,
		Getenv: func(name string) string {
			if name != DefaultEmbeddingAPIKeyEnv {
				t.Fatalf("env lookup = %q, want %s", name, DefaultEmbeddingAPIKeyEnv)
			}
			return "test-key"
		},
	})
	if err != nil {
		t.Fatalf("ResolveProviderAPIKey returned error: %v", err)
	}
	if key != "test-key" {
		t.Fatalf("key = %q, want test-key", key)
	}

	key, err = ResolveProviderAPIKey(ProviderAPIKeyInput{})
	if err != nil || key != "" {
		t.Fatalf("keyless provider = key %q err %v, want empty nil", key, err)
	}

	key, err = ResolveProviderAPIKey(ProviderAPIKeyInput{KeyEnv: DefaultEmbeddingAPIKeyEnv})
	if err == nil || err.Error() != ProviderAPIKeyEnvLookupCallMissingError(DefaultEmbeddingAPIKeyEnv).Error() || key != "" {
		t.Fatalf("nil env lookup = key %q err %v, want lookup error", key, err)
	}

	key, err = ResolveProviderAPIKey(ProviderAPIKeyInput{
		KeyEnv: DefaultEmbeddingAPIKeyEnv,
		Getenv: func(string) string {
			return ""
		},
	})
	if err == nil || err.Error() != ProviderAPIKeyNotSetError(DefaultEmbeddingAPIKeyEnv).Error() || key != "" {
		t.Fatalf("missing key = key %q err %v, want not-set error", key, err)
	}
}

func TestDropEmbeddingInputAt(t *testing.T) {
	cases := []struct {
		name          string
		batch         []string
		index         int
		wantDropped   string
		wantRemaining []string
		wantOK        bool
	}{
		{
			name:          "first",
			batch:         []string{"a", "b", "c"},
			index:         0,
			wantDropped:   "a",
			wantRemaining: []string{"b", "c"},
			wantOK:        true,
		},
		{
			name:          "middle",
			batch:         []string{"a", "b", "c"},
			index:         1,
			wantDropped:   "b",
			wantRemaining: []string{"a", "c"},
			wantOK:        true,
		},
		{
			name:          "last",
			batch:         []string{"a", "b", "c"},
			index:         2,
			wantDropped:   "c",
			wantRemaining: []string{"a", "b"},
			wantOK:        true,
		},
		{
			name:          "out of range",
			batch:         []string{"a", "b"},
			index:         2,
			wantRemaining: []string{"a", "b"},
			wantOK:        false,
		},
		{
			name:          "empty",
			batch:         nil,
			index:         0,
			wantRemaining: nil,
			wantOK:        false,
		},
	}

	for _, tc := range cases {
		batch := append([]string(nil), tc.batch...)
		gotDropped, gotRemaining, gotOK := DropEmbeddingInputAt(batch, tc.index)
		if gotOK != tc.wantOK {
			t.Fatalf("%s: ok = %v, want %v", tc.name, gotOK, tc.wantOK)
		}
		if gotDropped != tc.wantDropped {
			t.Fatalf("%s: dropped = %q, want %q", tc.name, gotDropped, tc.wantDropped)
		}
		if !slices.Equal(gotRemaining, tc.wantRemaining) {
			t.Fatalf("%s: remaining = %v, want %v", tc.name, gotRemaining, tc.wantRemaining)
		}
	}

	batch := []string{"a", "b", "c"}
	dropped, remaining, ok := DropEmbeddingInputAt(batch, 1)
	if !ok {
		t.Fatal("expected middle drop to succeed")
	}
	if dropped != "b" || !slices.Equal(remaining, []string{"a", "c"}) {
		t.Fatalf("drop result = (%q, %v), want (b, [a c])", dropped, remaining)
	}
	if batch[2] != "" {
		t.Fatalf("dropped tail slot = %q, want zero value", batch[2])
	}

	batch = []string{"a", "b", "c"}
	dropped, remaining, ok = DropEmbeddingInputFromError(batch, errors.New("Invalid 'input[1]'"))
	if !ok {
		t.Fatal("expected error-index drop to succeed")
	}
	if dropped != "b" || !slices.Equal(remaining, []string{"a", "c"}) {
		t.Fatalf("error-index drop result = (%q, %v), want (b, [a c])", dropped, remaining)
	}
	dropped, remaining, ok = DropEmbeddingInputFromError([]string{"a"}, errors.New("not an index error"))
	if ok || dropped != "" || !slices.Equal(remaining, []string{"a"}) {
		t.Fatalf("non-index drop result = (%q, %v, %v), want zero/original/false", dropped, remaining, ok)
	}
}

func TestRunEmbeddingBatchWithFallbackSplitsAndDrops(t *testing.T) {
	var calls [][]string
	var droppedItems []string
	kept, vecs, dropped, err := RunEmbeddingBatchWithFallback(EmbeddingBatchWithFallbackInput[string]{
		Batch: []string{"left", "drop", "right"},
		Text: func(item string) string {
			return item
		},
		Embed: func(texts []string) ([][]float32, error) {
			calls = append(calls, append([]string(nil), texts...))
			switch {
			case slices.Equal(texts, []string{"left", "drop", "right"}):
				return nil, errors.New("requested 999999 input tokens")
			case slices.Equal(texts, []string{"left"}):
				return [][]float32{{1}}, nil
			case slices.Equal(texts, []string{"drop", "right"}):
				return nil, errors.New("Invalid 'input[0]'")
			case slices.Equal(texts, []string{"right"}):
				return [][]float32{{3}}, nil
			default:
				return nil, errors.New("unexpected batch")
			}
		},
		OnDrop: func(item string) {
			droppedItems = append(droppedItems, item)
		},
	})
	if err != nil {
		t.Fatalf("RunEmbeddingBatchWithFallback returned error: %v", err)
	}
	if !slices.Equal(kept, []string{"left", "right"}) {
		t.Fatalf("kept = %v, want [left right]", kept)
	}
	if len(vecs) != 2 || len(vecs[0]) != 1 || vecs[0][0] != 1 || len(vecs[1]) != 1 || vecs[1][0] != 3 {
		t.Fatalf("vecs = %v, want [[1] [3]]", vecs)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if !slices.Equal(droppedItems, []string{"drop"}) {
		t.Fatalf("dropped items = %v, want [drop]", droppedItems)
	}
	wantCalls := [][]string{
		{"left", "drop", "right"},
		{"left"},
		{"drop", "right"},
		{"right"},
	}
	if len(calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
	for i := range calls {
		if !slices.Equal(calls[i], wantCalls[i]) {
			t.Fatalf("call %d = %v, want %v", i, calls[i], wantCalls[i])
		}
	}
}

func TestRunEmbeddingBatchWithFallbackRejectsMissingCallbacks(t *testing.T) {
	kept, vecs, dropped, err := RunEmbeddingBatchWithFallback(EmbeddingBatchWithFallbackInput[string]{
		Batch: []string{"a"},
		Embed: func([]string) ([][]float32, error) {
			return nil, nil
		},
	})
	if err == nil || err.Error() != EmbeddingInputTextCallMissingError().Error() {
		t.Fatalf("missing text err = %v, want %s", err, EmbeddingInputTextCallMissingError().Error())
	}
	if kept != nil || vecs != nil || dropped != 0 {
		t.Fatalf("missing text result = (%v, %v, %d), want nil/nil/0", kept, vecs, dropped)
	}

	kept, vecs, dropped, err = RunEmbeddingBatchWithFallback(EmbeddingBatchWithFallbackInput[string]{
		Batch: []string{"a"},
		Text: func(item string) string {
			return item
		},
	})
	if err == nil || err.Error() != EmbeddingBatchCallMissingError().Error() {
		t.Fatalf("missing embed err = %v, want %s", err, EmbeddingBatchCallMissingError().Error())
	}
	if kept != nil || vecs != nil || dropped != 0 {
		t.Fatalf("missing embed result = (%v, %v, %d), want nil/nil/0", kept, vecs, dropped)
	}
}

func TestEmbeddingOversizeInputSkipMessage(t *testing.T) {
	got := EmbeddingOversizeInputSkipMessage("supabase/reference.md", 16001)
	want := "  oversize: supabase/reference.md — skipping (16001 chars exceeds per-input token cap)\n"
	if got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestEmbeddingOversizeChunkSkipMessage(t *testing.T) {
	got := EmbeddingOversizeChunkSkipMessage("supabase/reference.md", 7)
	want := "  oversize chunk: supabase/reference.md#7 — skipping\n"
	if got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestEmbeddingOversizeDocsWarning(t *testing.T) {
	got := EmbeddingOversizeDocsWarning(2)
	want := "warning: skipped 2 docs that exceeded the 8192-token per-input cap even after truncation\n"
	if got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if got := EmbeddingOversizeDocsWarning(0); got != "" {
		t.Fatalf("zero skipped message = %q, want empty", got)
	}
}

func TestEmbeddingOversizeChunksWarning(t *testing.T) {
	got := EmbeddingOversizeChunksWarning(3)
	want := "warning: skipped 3 chunks that exceeded the 8192-token per-input cap\n"
	if got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if got := EmbeddingOversizeChunksWarning(-1); got != "" {
		t.Fatalf("negative skipped message = %q, want empty", got)
	}
}

func TestEmbeddingNoDocsFoundMessage(t *testing.T) {
	if got, want := EmbeddingNoDocsFoundMessage(), "no docs found\n"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestEmbeddingUpToDateMessages(t *testing.T) {
	if got, want := EmbeddingUpToDateMessage(4, "text-embedding-3-small"), "up to date: 4 docs already embedded for model=text-embedding-3-small\n"; got != want {
		t.Fatalf("whole-doc message = %q, want %q", got, want)
	}
	if got, want := EmbeddingChunkUpToDateMessage(6, "text-embedding-3-small", 1500), "up to date: 6 docs already chunked-embedded for model=text-embedding-3-small chunk_size=1500\n"; got != want {
		t.Fatalf("chunk message = %q, want %q", got, want)
	}
}

func TestEmbeddingFlatIndexWrittenMessage(t *testing.T) {
	if got, want := EmbeddingFlatIndexWrittenMessage("text-embedding-3-small", 12, 1536), "wrote flat embedding index: model=text-embedding-3-small docs=12 dim=1536\n"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if got, want := EmbeddingFlatIndexMetadataInvalidError().Error(), "flat embedding metadata is stale or invalid"; got != want {
		t.Fatalf("metadata error = %q, want %q", got, want)
	}
	if got, want := EmbeddingFlatIndexVectorBytesError(12, 16).Error(), "flat embedding vector bytes=12, want 16"; got != want {
		t.Fatalf("vector bytes error = %q, want %q", got, want)
	}
	if got, want := EmbeddingFlatIndexVectorFileEmptyError("/tmp/docs/.embeddings/text-embedding-3-small.vec").Error(), "/tmp/docs/.embeddings/text-embedding-3-small.vec is empty"; got != want {
		t.Fatalf("empty vector file error = %q, want %q", got, want)
	}
	if got, want := EmbeddingFlatIndexVectorInvalidError("docs/a.md", 3, 8).Error(), "embedding docs/a.md has dim=3 blob_bytes=8"; got != want {
		t.Fatalf("invalid vector error = %q, want %q", got, want)
	}
	if got, want := EmbeddingFlatIndexMixedDimensionsError("text-embedding-3-small", 1536, 768).Error(), "mixed embedding dimensions for model text-embedding-3-small: 1536 and 768"; got != want {
		t.Fatalf("mixed dimensions error = %q, want %q", got, want)
	}
}

func TestEmbeddingLegacyMigrationSummaryMessage(t *testing.T) {
	if got, want := EmbeddingLegacyMigrationSummaryMessage(7, 11, 2, "/tmp/docs/embeddings.db"), "migrated legacy embeddings: docs=7 chunks=11 models=2 -> /tmp/docs/embeddings.db\n"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestEmbeddingLegacyNotFoundError(t *testing.T) {
	if got, want := EmbeddingLegacyNotFoundError("/tmp/docs/search.db").Error(), "legacy embeddings not found in /tmp/docs/search.db"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestEmbeddingWriteFlatOnlyErrors(t *testing.T) {
	if got, want := EmbeddingWriteFlatOnlySourceError().Error(), "--write-flat-only writes a model-wide sidecar; omit --source"; got != want {
		t.Fatalf("source error = %q, want %q", got, want)
	}
	if got, want := EmbeddingWriteFlatOnlyChunkSizeError().Error(), "--write-flat-only only supports whole-doc embeddings; omit --chunk-size"; got != want {
		t.Fatalf("chunk-size error = %q, want %q", got, want)
	}
	if got, want := EmbeddingWriteFlatOnlyMissingCacheError("text-embedding-3-small").Error(), "no cached embeddings found for model=text-embedding-3-small; run `docs-puller embed --model text-embedding-3-small` first"; got != want {
		t.Fatalf("missing-cache error = %q, want %q", got, want)
	}
	cause := errors.New("boom")
	if err := EmbeddingWriteFlatOnlyInspectCacheError(cause); err == nil || err.Error() != "inspect cached embeddings: boom" || !errors.Is(err, cause) {
		t.Fatalf("inspect-cache error = %v, want wrapped cause", err)
	}
	if err := EmbeddingFlatIndexWriteError(cause); err == nil || err.Error() != "write flat index: boom" || !errors.Is(err, cause) {
		t.Fatalf("flat-index write error = %v, want wrapped cause", err)
	}
	if err := EmbeddingFlatIndexWriteModelError("text-embedding-3-small", cause); err == nil || err.Error() != "write flat index for text-embedding-3-small: boom" || !errors.Is(err, cause) {
		t.Fatalf("model flat-index write error = %v, want wrapped cause", err)
	}
}

func TestEmbeddingCacheLoadError(t *testing.T) {
	cause := errors.New("boom")
	err := EmbeddingCacheLoadError(cause)
	if got, want := err.Error(), "load cache: boom"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error does not wrap cause: %v", err)
	}
}

func TestEmbeddingBatchErrors(t *testing.T) {
	cause := errors.New("boom")
	if err := EmbeddingBatchError(cause); err == nil || err.Error() != "embed batch: boom" || !errors.Is(err, cause) {
		t.Fatalf("embed-batch error = %v, want wrapped cause", err)
	}
	if err := EmbeddingStoreBatchError(cause); err == nil || err.Error() != "store batch: boom" || !errors.Is(err, cause) {
		t.Fatalf("store-batch error = %v, want wrapped cause", err)
	}
}

func TestEmbeddingDBMigrationErrors(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"busy timeout", EmbeddingDBBusyTimeoutError(cause), "set busy_timeout: boom"},
		{"journal mode", EmbeddingDBJournalModeError(cause), "set journal_mode=WAL: boom"},
		{"ensure schema", EmbeddingDBSchemaEnsureError(cause), "ensure embeddings schema: boom"},
		{"pk migration", EmbeddingDBPKMigrationError(cause), "migrate embeddings PK: boom"},
		{"read schema", EmbeddingSchemaReadError(cause), "read embeddings schema: boom"},
		{"begin migration", EmbeddingMigrationBeginError(cause), "begin migration tx: boom"},
		{"migration step", EmbeddingMigrationStepError("CREATE TABLE embeddings_new", cause), "migrate step \"CREATE TABLE embeddings_new\": boom"},
		{"legacy open", EmbeddingLegacyOpenError(cause), "open legacy search.db: boom"},
		{"legacy busy timeout", EmbeddingLegacyBusyTimeoutError(cause), "legacy busy_timeout: boom"},
	}
	for _, tc := range cases {
		if tc.err == nil || tc.err.Error() != tc.want || !errors.Is(tc.err, cause) {
			t.Fatalf("%s error = %v, want %q wrapping cause", tc.name, tc.err, tc.want)
		}
	}
}

func TestEmbeddingPlanMessages(t *testing.T) {
	if got, want := EmbeddingPlanMessage("text-embedding-3-small", 9, 2, 12345, 0.0123), "embed plan: model=text-embedding-3-small docs=9 skipped=2 est_tokens=12345 est_cost=$0.0123\n"; got != want {
		t.Fatalf("whole-doc plan = %q, want %q", got, want)
	}
	if got, want := EmbeddingChunkPlanMessage("text-embedding-3-small", 1500, 21, 3, 54321, 0.0543), "chunked embed plan: model=text-embedding-3-small chunk_size=1500 chunks=21 docs_skipped=3 est_tokens=54321 est_cost=$0.0543\n"; got != want {
		t.Fatalf("chunk plan = %q, want %q", got, want)
	}
}

func TestEmbeddingMaxCostExceededError(t *testing.T) {
	if got, want := EmbeddingMaxCostExceededError(0.12345, 0.1).Error(), "estimated cost $0.1235 > --max-cost $0.1000"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestEmbeddingProgressMessages(t *testing.T) {
	if got, want := EmbeddingProgressMessage(3, 4), "embedded 3/4 (75.0%)\n"; got != want {
		t.Fatalf("whole-doc progress = %q, want %q", got, want)
	}
	if got, want := EmbeddingChunkProgressMessage(5, 8), "embedded 5/8 chunks (62.5%)\n"; got != want {
		t.Fatalf("chunk progress = %q, want %q", got, want)
	}
	if got, want := EmbeddingProgressMessage(1, 0), "embedded 1/0 (0.0%)\n"; got != want {
		t.Fatalf("zero-total progress = %q, want %q", got, want)
	}
}

func TestEmbeddingSummaryMessages(t *testing.T) {
	if got, want := EmbeddingSummaryMessage(3, 1200*time.Millisecond, "text-embedding-3-small"), "embedded 3 docs in 1.2s (model=text-embedding-3-small)\n"; got != want {
		t.Fatalf("whole-doc summary = %q, want %q", got, want)
	}
	if got, want := EmbeddingChunkSummaryMessage(7, 1250*time.Millisecond, "text-embedding-3-small", 1500), "embedded 7 chunks in 1.25s (model=text-embedding-3-small chunk_size=1500)\n"; got != want {
		t.Fatalf("chunk summary = %q, want %q", got, want)
	}
}

func TestEmbeddingFlatIndexWriteWarning(t *testing.T) {
	if got, want := EmbeddingFlatIndexWriteWarning(errors.New("disk full")), "embedding-flat-index: write failed: disk full\n"; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
	if got := EmbeddingFlatIndexWriteWarning(nil); got != "" {
		t.Fatalf("nil error warning = %q, want empty", got)
	}
}

func TestNewEmbeddingOversizeInputDropCall(t *testing.T) {
	type item struct {
		path string
		text string
	}
	var b strings.Builder
	onDrop := NewEmbeddingOversizeInputDropCall(&b, func(i item) string {
		return i.path
	}, func(i item) string {
		return i.text
	})
	if onDrop == nil {
		t.Fatal("NewEmbeddingOversizeInputDropCall returned nil")
	}
	onDrop(item{path: "supabase/reference.md", text: "abcdef"})
	want := "  oversize: supabase/reference.md — skipping (6 chars exceeds per-input token cap)\n"
	if got := b.String(); got != want {
		t.Fatalf("drop message = %q, want %q", got, want)
	}
	if got := NewEmbeddingOversizeInputDropCall[item](nil, func(i item) string { return i.path }, func(i item) string { return i.text }); got != nil {
		t.Fatalf("nil writer returned non-nil callback")
	}
}

func TestNewEmbeddingOversizeChunkDropCall(t *testing.T) {
	type item struct {
		path     string
		chunkIdx int
	}
	var b strings.Builder
	onDrop := NewEmbeddingOversizeChunkDropCall(&b, func(i item) string {
		return i.path
	}, func(i item) int {
		return i.chunkIdx
	})
	if onDrop == nil {
		t.Fatal("NewEmbeddingOversizeChunkDropCall returned nil")
	}
	onDrop(item{path: "supabase/reference.md", chunkIdx: 7})
	want := "  oversize chunk: supabase/reference.md#7 — skipping\n"
	if got := b.String(); got != want {
		t.Fatalf("drop message = %q, want %q", got, want)
	}
	if got := NewEmbeddingOversizeChunkDropCall[item](nil, func(i item) string { return i.path }, func(i item) int { return i.chunkIdx }); got != nil {
		t.Fatalf("nil writer returned non-nil callback")
	}
}

func TestOrderEmbeddingVectorsByIndex(t *testing.T) {
	results := []EmbeddingVectorResult{
		{Index: 2, Vector: []float32{2}},
		{Index: 0, Vector: []float32{0}},
		{Index: 1, Vector: []float32{1}},
	}

	got := OrderEmbeddingVectorsByIndex(results)
	if len(got) != 3 {
		t.Fatalf("ordered vector count = %d, want 3", len(got))
	}
	for i, vector := range got {
		if len(vector) != 1 || vector[0] != float32(i) {
			t.Fatalf("ordered vector %d = %v, want [%d]", i, vector, i)
		}
	}
	if results[0].Index != 2 || results[1].Index != 0 || results[2].Index != 1 {
		t.Fatalf("input results mutated: %+v", results)
	}
}

func TestValidateEmbeddingVectorCount(t *testing.T) {
	if err := ValidateEmbeddingVectorCount(2, 2); err != nil {
		t.Fatalf("matching vector count returned error: %v", err)
	}
	err := ValidateEmbeddingVectorCount(1, 2)
	if err == nil || err.Error() != OpenAIEmbeddingVectorCountError(1, 2).Error() {
		t.Fatalf("mismatch error = %v, want stable provider count error", err)
	}
}

func TestEmbeddingStatusError(t *testing.T) {
	longBody := strings.Repeat("x", 505)
	retryErr := EmbeddingStatusError(http.StatusInternalServerError, longBody, true)
	if retryErr.Error() != "openai status 500: "+strings.Repeat("x", 200)+"..." {
		t.Fatalf("retryable status error = %q, want 200-char body preview", retryErr.Error())
	}

	terminalErr := EmbeddingStatusError(http.StatusBadRequest, longBody, false)
	if terminalErr.Error() != "openai status 400: "+strings.Repeat("x", 500)+"..." {
		t.Fatalf("terminal status error = %q, want 500-char body preview", terminalErr.Error())
	}
}

func TestOpenAIEmbeddingStatusDecision(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantRetry bool
		wantErr   string
	}{
		{
			name:   "success",
			status: http.StatusOK,
			body:   "ok",
		},
		{
			name:      "retryable",
			status:    http.StatusTooManyRequests,
			body:      "slow down",
			wantRetry: true,
			wantErr:   "openai status 429: slow down",
		},
		{
			name:    "terminal",
			status:  http.StatusBadRequest,
			body:    "bad input",
			wantErr: "openai status 400: bad input",
		},
	}

	for _, tc := range cases {
		gotRetry, gotErr := OpenAIEmbeddingStatusDecision(tc.status, []byte(tc.body))
		if gotRetry != tc.wantRetry {
			t.Fatalf("%s: retry = %v, want %v", tc.name, gotRetry, tc.wantRetry)
		}
		if tc.wantErr == "" {
			if gotErr != nil {
				t.Fatalf("%s: err = %v, want nil", tc.name, gotErr)
			}
			continue
		}
		if gotErr == nil || gotErr.Error() != tc.wantErr {
			t.Fatalf("%s: err = %v, want %q", tc.name, gotErr, tc.wantErr)
		}
	}
}

func TestEmbeddingProviderError(t *testing.T) {
	err := EmbeddingProviderError("bad input")
	if err == nil || err.Error() != "openai error: bad input" {
		t.Fatalf("provider error = %v, want stable embedding provider error", err)
	}
}

func TestBuildOpenAIEmbeddingRequestBody(t *testing.T) {
	body, err := BuildOpenAIEmbeddingRequestBody("text-embedding-3-small", []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("BuildOpenAIEmbeddingRequestBody returned error: %v", err)
	}
	var got struct {
		Input []string `json:"input"`
		Model string   `json:"model"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if got.Model != "text-embedding-3-small" || !slices.Equal(got.Input, []string{"alpha", "beta"}) {
		t.Fatalf("request body = %+v, want model plus inputs", got)
	}
}

func TestNewOpenAIEmbeddingRequest(t *testing.T) {
	req, err := NewOpenAIEmbeddingRequest("https://example.test/v1/embeddings", []byte(`{"ok":true}`))
	if err != nil {
		t.Fatalf("NewOpenAIEmbeddingRequest returned error: %v", err)
	}
	if req.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", req.Method)
	}
	if got := req.URL.String(); got != "https://example.test/v1/embeddings" {
		t.Fatalf("url = %q, want embeddings endpoint", got)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll request body returned error: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("body = %q, want JSON body", got)
	}
}

func TestApplyOpenAIEmbeddingRequestHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.test/v1/embeddings", nil)
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}
	ApplyOpenAIEmbeddingRequestHeaders(req, "test-key", "docs-test")
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want bearer key", got)
	}
	if got := req.Header.Get("User-Agent"); got != "docs-test" {
		t.Fatalf("User-Agent = %q, want docs-test", got)
	}
}

func TestPrepareOpenAIEmbeddingRequest(t *testing.T) {
	req, body, err := PrepareOpenAIEmbeddingRequest("text-embedding-3-small", []string{"alpha", "beta"}, "test-key", "docs-test")
	if err != nil {
		t.Fatalf("PrepareOpenAIEmbeddingRequest returned error: %v", err)
	}
	if req.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", req.Method)
	}
	if got := req.URL.String(); got != DefaultOpenAIEmbeddingEndpoint {
		t.Fatalf("url = %q, want default endpoint %q", got, DefaultOpenAIEmbeddingEndpoint)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want bearer key", got)
	}
	if got := req.Header.Get("User-Agent"); got != "docs-test" {
		t.Fatalf("User-Agent = %q, want docs-test", got)
	}

	gotBody, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll request body returned error: %v", err)
	}
	if string(gotBody) != string(body) {
		t.Fatalf("request body = %q, want returned body %q", gotBody, body)
	}
	var got struct {
		Input []string `json:"input"`
		Model string   `json:"model"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("returned body is not JSON: %v", err)
	}
	if got.Model != "text-embedding-3-small" || !slices.Equal(got.Input, []string{"alpha", "beta"}) {
		t.Fatalf("returned body = %+v, want model plus inputs", got)
	}
}

func TestParseOpenAIEmbeddingResponse(t *testing.T) {
	got, err := ParseOpenAIEmbeddingResponse([]byte(`{"data":[{"index":1,"embedding":[1.5,2.5]},{"index":0,"embedding":[0.5]}],"usage":{"prompt_tokens":11,"total_tokens":13}}`))
	if err != nil {
		t.Fatalf("ParseOpenAIEmbeddingResponse returned error: %v", err)
	}
	if len(got.Vectors) != 2 || got.Vectors[0].Index != 1 || got.Vectors[1].Index != 0 {
		t.Fatalf("vectors = %+v, want provider indexes preserved", got.Vectors)
	}
	if len(got.Vectors[0].Vector) != 2 || got.Vectors[0].Vector[0] != 1.5 || got.Vectors[0].Vector[1] != 2.5 {
		t.Fatalf("first vector = %v, want parsed float32 embedding", got.Vectors[0].Vector)
	}
	if !got.Usage.Present || got.Usage.PromptTokens != 11 || got.Usage.TotalTokens != 13 {
		t.Fatalf("usage = %+v, want prompt=11 total=13", got.Usage)
	}

	providerErr, err := ParseOpenAIEmbeddingResponse([]byte(`{"error":{"message":"bad input","type":"invalid_request_error"}}`))
	if err != nil {
		t.Fatalf("provider error response should parse without JSON error: %v", err)
	}
	if providerErr.ProviderError != "bad input" {
		t.Fatalf("provider error = %q, want bad input", providerErr.ProviderError)
	}

	raw := []byte(`{"data"`)
	var scratch any
	unmarshalErr := json.Unmarshal(raw, &scratch)
	if unmarshalErr == nil {
		t.Fatal("invalid JSON unexpectedly unmarshaled")
	}
	parsed, err := ParseOpenAIEmbeddingResponse(raw)
	if err == nil || err.Error() != OpenAIEmbeddingResponseParseError(unmarshalErr).Error() {
		t.Fatalf("parse error = %v, want stable response parse error", err)
	}
	if len(parsed.Vectors) != 0 || parsed.ProviderError != "" || parsed.Usage.Present {
		t.Fatalf("parsed response on error = %+v, want zero response", parsed)
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("parse error does not wrap JSON syntax error: %v", err)
	}
}

func TestBuildOpenAIEmbeddingSuccess(t *testing.T) {
	got, err := BuildOpenAIEmbeddingSuccess(OpenAIEmbeddingResponse{
		Vectors: []EmbeddingVectorResult{
			{Index: 1, Vector: []float32{1}},
			{Index: 0, Vector: []float32{0}},
		},
		Usage: EmbeddingResponseUsage{
			PromptTokens: 11,
			TotalTokens:  13,
			Present:      true,
		},
	}, "text-embedding-3-small", "")
	if err != nil {
		t.Fatalf("BuildOpenAIEmbeddingSuccess returned error: %v", err)
	}
	if len(got.Vectors) != 2 || len(got.Vectors[0]) != 1 || got.Vectors[0][0] != 0 || got.Vectors[1][0] != 1 {
		t.Fatalf("vectors = %v, want ordered vectors", got.Vectors)
	}
	if !got.UsageEventPresent {
		t.Fatalf("usage event not present")
	}
	if got.UsageEvent.ProviderName != "openai" || got.UsageEvent.Model != "text-embedding-3-small" || got.UsageEvent.CallSite != DefaultEmbeddingUsageCallSite || got.UsageEvent.Tokens != 11 {
		t.Fatalf("usage event = %+v, want default openai embedding event", got.UsageEvent)
	}

	providerErr, err := BuildOpenAIEmbeddingSuccess(OpenAIEmbeddingResponse{ProviderError: "bad input"}, "text-embedding-3-small", "")
	if err == nil || err.Error() != "openai error: bad input" {
		t.Fatalf("provider error success = %+v err=%v, want provider error", providerErr, err)
	}

	noUsage, err := BuildOpenAIEmbeddingSuccess(OpenAIEmbeddingResponse{
		Vectors: []EmbeddingVectorResult{{Index: 0, Vector: []float32{1}}},
	}, "text-embedding-3-small", "custom")
	if err != nil {
		t.Fatalf("BuildOpenAIEmbeddingSuccess without usage returned error: %v", err)
	}
	if noUsage.UsageEventPresent {
		t.Fatalf("usage event present for response without usage: %+v", noUsage.UsageEvent)
	}
}

func TestDecodeOpenAIEmbeddingSuccessResponse(t *testing.T) {
	got, err := DecodeOpenAIEmbeddingSuccessResponse([]byte(`{"data":[{"index":1,"embedding":[1]},{"index":0,"embedding":[0]}],"usage":{"total_tokens":13}}`), "text-embedding-3-small", "custom")
	if err != nil {
		t.Fatalf("DecodeOpenAIEmbeddingSuccessResponse returned error: %v", err)
	}
	if len(got.Vectors) != 2 || len(got.Vectors[0]) != 1 || got.Vectors[0][0] != 0 || got.Vectors[1][0] != 1 {
		t.Fatalf("vectors = %v, want ordered vectors", got.Vectors)
	}
	if !got.UsageEventPresent || got.UsageEvent.CallSite != "custom" || got.UsageEvent.Tokens != 13 {
		t.Fatalf("usage event = %+v present=%v, want custom total-token usage", got.UsageEvent, got.UsageEventPresent)
	}

	providerErr, err := DecodeOpenAIEmbeddingSuccessResponse([]byte(`{"error":{"message":"bad input"}}`), "text-embedding-3-small", "")
	if err == nil || err.Error() != "openai error: bad input" {
		t.Fatalf("provider error result = %+v err=%v, want stable provider error", providerErr, err)
	}
}

func TestProcessOpenAIEmbeddingHTTPResponse(t *testing.T) {
	success, retry, err := ProcessOpenAIEmbeddingHTTPResponse(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[1]}],"usage":{"prompt_tokens":11}}`)),
	}, "text-embedding-3-small", "custom")
	if err != nil {
		t.Fatalf("ProcessOpenAIEmbeddingHTTPResponse success returned error: %v", err)
	}
	if retry {
		t.Fatalf("success retry = true, want false")
	}
	if len(success.Vectors) != 1 || len(success.Vectors[0]) != 1 || success.Vectors[0][0] != 1 {
		t.Fatalf("success vectors = %v, want decoded vector", success.Vectors)
	}
	if !success.UsageEventPresent || success.UsageEvent.CallSite != "custom" || success.UsageEvent.Tokens != 11 {
		t.Fatalf("success usage = %+v present=%v, want decoded usage event", success.UsageEvent, success.UsageEventPresent)
	}

	retrySuccess, retry, err := ProcessOpenAIEmbeddingHTTPResponse(&http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader("slow down")),
	}, "text-embedding-3-small", "")
	if !retry || err == nil || err.Error() != "openai status 429: slow down" {
		t.Fatalf("retry response = success %+v retry=%v err=%v, want retryable status error", retrySuccess, retry, err)
	}

	terminalSuccess, retry, err := ProcessOpenAIEmbeddingHTTPResponse(&http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader("bad input")),
	}, "text-embedding-3-small", "")
	if retry || err == nil || err.Error() != "openai status 400: bad input" {
		t.Fatalf("terminal response = success %+v retry=%v err=%v, want terminal status error", terminalSuccess, retry, err)
	}
}

func TestRunOpenAIEmbeddingHTTPAttempt(t *testing.T) {
	req, err := NewOpenAIEmbeddingRequest("https://example.test/v1/embeddings", []byte(`{"old":true}`))
	if err != nil {
		t.Fatalf("NewOpenAIEmbeddingRequest returned error: %v", err)
	}

	success, retry, err := RunOpenAIEmbeddingHTTPAttempt(req, []byte(`{"input":["ok"]}`), func(req *http.Request) (*http.Response, error) {
		gotBody, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("ReadAll request body returned error: %v", err)
		}
		if string(gotBody) != `{"input":["ok"]}` {
			t.Fatalf("attempt body = %q, want reset request body", gotBody)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[1]}],"usage":{"total_tokens":13}}`)),
		}, nil
	}, "text-embedding-3-small", "custom")
	if err != nil {
		t.Fatalf("RunOpenAIEmbeddingHTTPAttempt success returned error: %v", err)
	}
	if retry {
		t.Fatalf("success retry = true, want false")
	}
	if len(success.Vectors) != 1 || len(success.Vectors[0]) != 1 || success.Vectors[0][0] != 1 {
		t.Fatalf("success vectors = %v, want decoded vector", success.Vectors)
	}
	if !success.UsageEventPresent || success.UsageEvent.CallSite != "custom" || success.UsageEvent.Tokens != 13 {
		t.Fatalf("success usage = %+v present=%v, want decoded usage event", success.UsageEvent, success.UsageEventPresent)
	}

	doErr := errors.New("network down")
	networkSuccess, retry, err := RunOpenAIEmbeddingHTTPAttempt(req, []byte(`{"input":["retry"]}`), func(*http.Request) (*http.Response, error) {
		return nil, doErr
	}, "text-embedding-3-small", "")
	if !retry || !errors.Is(err, doErr) {
		t.Fatalf("network response = success %+v retry=%v err=%v, want retryable do error", networkSuccess, retry, err)
	}

	retrySuccess, retry, err := RunOpenAIEmbeddingHTTPAttempt(req, []byte(`{"input":["retry"]}`), func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader("slow down")),
		}, nil
	}, "text-embedding-3-small", "")
	if !retry || err == nil || err.Error() != "openai status 429: slow down" {
		t.Fatalf("retry response = success %+v retry=%v err=%v, want retryable status error", retrySuccess, retry, err)
	}
}

func TestRunOpenAIEmbeddingHTTPAttemptUsesDefaultTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotBody, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("ReadAll request body returned error: %v", err)
		}
		if string(gotBody) != `{"input":["ok"]}` {
			t.Fatalf("attempt body = %q, want reset request body", gotBody)
		}
		w.Header().Set("Content-Type", "application/json")
		written, err := w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
		if err != nil {
			t.Fatalf("Write response returned error: %v", err)
		}
		if written == 0 {
			t.Fatal("Write response wrote 0 bytes")
		}
	}))
	defer srv.Close()

	req, err := NewOpenAIEmbeddingRequest(srv.URL, []byte(`{"old":true}`))
	if err != nil {
		t.Fatalf("NewOpenAIEmbeddingRequest returned error: %v", err)
	}
	success, retry, err := RunOpenAIEmbeddingHTTPAttempt(req, []byte(`{"input":["ok"]}`), nil, "text-embedding-3-small", "custom")
	if err != nil {
		t.Fatalf("RunOpenAIEmbeddingHTTPAttempt returned error: %v", err)
	}
	if retry {
		t.Fatalf("retry = true, want false")
	}
	if len(success.Vectors) != 1 || len(success.Vectors[0]) != 1 || success.Vectors[0][0] != 1 {
		t.Fatalf("success vectors = %v, want decoded vector", success.Vectors)
	}
}

func TestRunOpenAIEmbeddingHTTPAttemptsRetriesThenSucceeds(t *testing.T) {
	req, err := NewOpenAIEmbeddingRequest("https://example.test/v1/embeddings", []byte(`{"old":true}`))
	if err != nil {
		t.Fatalf("NewOpenAIEmbeddingRequest returned error: %v", err)
	}

	var sleeps []time.Duration
	attempts := 0
	success, err := RunOpenAIEmbeddingHTTPAttempts(req, []byte(`{"input":["ok"]}`), func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader("slow down")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[1]}],"usage":{"total_tokens":13}}`)),
		}, nil
	}, "text-embedding-3-small", "custom", func(delay time.Duration) {
		sleeps = append(sleeps, delay)
	})
	if err != nil {
		t.Fatalf("RunOpenAIEmbeddingHTTPAttempts returned error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if !slices.Equal(sleeps, []time.Duration{2 * time.Second}) {
		t.Fatalf("sleeps = %v, want [2s]", sleeps)
	}
	if len(success.Vectors) != 1 || len(success.Vectors[0]) != 1 || success.Vectors[0][0] != 1 {
		t.Fatalf("success vectors = %v, want decoded vector", success.Vectors)
	}
	if !success.UsageEventPresent || success.UsageEvent.CallSite != "custom" || success.UsageEvent.Tokens != 13 {
		t.Fatalf("success usage = %+v present=%v, want decoded usage event", success.UsageEvent, success.UsageEventPresent)
	}
}

func TestRunOpenAIEmbeddingHTTPAttemptsExhaustsRetryErrors(t *testing.T) {
	req, err := NewOpenAIEmbeddingRequest("https://example.test/v1/embeddings", []byte(`{"old":true}`))
	if err != nil {
		t.Fatalf("NewOpenAIEmbeddingRequest returned error: %v", err)
	}

	doErr := errors.New("network down")
	var sleeps []time.Duration
	attempts := 0
	success, err := RunOpenAIEmbeddingHTTPAttempts(req, []byte(`{"input":["retry"]}`), func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, doErr
	}, "text-embedding-3-small", "", func(delay time.Duration) {
		sleeps = append(sleeps, delay)
	})
	if !errors.Is(err, doErr) {
		t.Fatalf("success = %+v err=%v, want last retry error", success, err)
	}
	if attempts != DefaultEmbeddingMaxAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, DefaultEmbeddingMaxAttempts)
	}
	if !slices.Equal(sleeps, []time.Duration{2 * time.Second, 4 * time.Second}) {
		t.Fatalf("sleeps = %v, want [2s 4s]", sleeps)
	}
}

func TestRunOpenAIEmbeddingHTTPAttemptsStopsOnTerminalStatus(t *testing.T) {
	req, err := NewOpenAIEmbeddingRequest("https://example.test/v1/embeddings", []byte(`{"old":true}`))
	if err != nil {
		t.Fatalf("NewOpenAIEmbeddingRequest returned error: %v", err)
	}

	var sleeps []time.Duration
	attempts := 0
	success, err := RunOpenAIEmbeddingHTTPAttempts(req, []byte(`{"input":["bad"]}`), func(*http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader("bad input")),
		}, nil
	}, "text-embedding-3-small", "", func(delay time.Duration) {
		sleeps = append(sleeps, delay)
	})
	if err == nil || err.Error() != "openai status 400: bad input" {
		t.Fatalf("success = %+v err=%v, want terminal status error", success, err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if len(sleeps) != 0 {
		t.Fatalf("sleeps = %v, want none", sleeps)
	}
}

func TestRunOpenAIEmbeddingHTTP(t *testing.T) {
	var sleeps []time.Duration
	attempts := 0
	success, err := RunOpenAIEmbeddingHTTP("text-embedding-3-small", []string{"alpha"}, "test-key", "docs-test", func(req *http.Request) (*http.Response, error) {
		attempts++
		if req.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", req.Method)
		}
		if got := req.URL.String(); got != DefaultOpenAIEmbeddingEndpoint {
			t.Fatalf("url = %q, want default endpoint %q", got, DefaultOpenAIEmbeddingEndpoint)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q, want bearer key", got)
		}
		if got := req.Header.Get("User-Agent"); got != "docs-test" {
			t.Fatalf("User-Agent = %q, want docs-test", got)
		}
		gotBody, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("ReadAll request body returned error: %v", err)
		}
		if !strings.Contains(string(gotBody), `"alpha"`) || !strings.Contains(string(gotBody), `"text-embedding-3-small"`) {
			t.Fatalf("request body = %q, want input plus model", gotBody)
		}
		if attempts == 1 {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader("slow down")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[1]}],"usage":{"total_tokens":13}}`)),
		}, nil
	}, "custom", func(delay time.Duration) {
		sleeps = append(sleeps, delay)
	})
	if err != nil {
		t.Fatalf("RunOpenAIEmbeddingHTTP returned error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if !slices.Equal(sleeps, []time.Duration{2 * time.Second}) {
		t.Fatalf("sleeps = %v, want [2s]", sleeps)
	}
	if len(success.Vectors) != 1 || len(success.Vectors[0]) != 1 || success.Vectors[0][0] != 1 {
		t.Fatalf("success vectors = %v, want decoded vector", success.Vectors)
	}
	if !success.UsageEventPresent || success.UsageEvent.CallSite != "custom" || success.UsageEvent.Tokens != 13 {
		t.Fatalf("success usage = %+v present=%v, want decoded usage event", success.UsageEvent, success.UsageEventPresent)
	}
}

func TestRunOpenAIEmbeddingVectorsRecordsUsage(t *testing.T) {
	var recorded EmbeddingUsageEvent
	vectors, err := RunOpenAIEmbeddingVectors("text-embedding-3-small", []string{"alpha"}, "test-key", "docs-test", func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[1,2]}],"usage":{"prompt_tokens":13}}`)),
		}, nil
	}, "custom", nil, func(event EmbeddingUsageEvent) {
		recorded = event
	})
	if err != nil {
		t.Fatalf("RunOpenAIEmbeddingVectors returned error: %v", err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 2 || vectors[0][0] != 1 || vectors[0][1] != 2 {
		t.Fatalf("vectors = %v, want decoded vectors", vectors)
	}
	if recorded.ProviderName != "openai" || recorded.Model != "text-embedding-3-small" || recorded.CallSite != "custom" || recorded.Tokens != 13 {
		t.Fatalf("recorded event = %+v, want usage event", recorded)
	}
}

func TestRunOpenAIEmbeddingValidatedVectorsRejectsBadVectorCount(t *testing.T) {
	vectors, err := RunOpenAIEmbeddingValidatedVectors("text-embedding-3-small", []string{"alpha"}, "test-key", "docs-test", func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
		}, nil
	}, "custom", nil, nil)
	if err == nil {
		t.Fatal("RunOpenAIEmbeddingValidatedVectors returned nil error, want vector-count error")
	}
	if vectors != nil {
		t.Fatalf("vectors = %v, want nil on vector-count error", vectors)
	}
	if err.Error() != OpenAIEmbeddingVectorCountError(0, 1).Error() {
		t.Fatalf("error = %v, want vector-count error", err)
	}
}

func TestRunOpenAIEmbeddingValidatedVectorsWithUsageLogRecordsUsage(t *testing.T) {
	var (
		gotProvider     string
		gotModel        string
		gotCallSite     string
		gotInputTokens  int
		gotOutputTokens int
		gotCents        float64
	)
	vectors, err := RunOpenAIEmbeddingValidatedVectorsWithUsageLog("text-embedding-3-small", []string{"alpha"}, "test-key", "docs-test", func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[1,2]}],"usage":{"total_tokens":13}}`)),
		}, nil
	}, "custom", nil, func(providerName, model, callSite string, inputTokens, outputTokens int, cents float64) {
		gotProvider = providerName
		gotModel = model
		gotCallSite = callSite
		gotInputTokens = inputTokens
		gotOutputTokens = outputTokens
		gotCents = cents
	})
	if err != nil {
		t.Fatalf("RunOpenAIEmbeddingValidatedVectorsWithUsageLog returned error: %v", err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 2 || vectors[0][0] != 1 || vectors[0][1] != 2 {
		t.Fatalf("vectors = %v, want decoded vectors", vectors)
	}
	if gotProvider != "openai" || gotModel != "text-embedding-3-small" || gotCallSite != "custom" || gotInputTokens != 13 || gotOutputTokens != 0 || gotCents <= 0 {
		t.Fatalf("usage log = provider=%q model=%q callSite=%q input=%d output=%d cents=%f", gotProvider, gotModel, gotCallSite, gotInputTokens, gotOutputTokens, gotCents)
	}
}

func TestNewOpenAIEmbeddingBatchCall(t *testing.T) {
	var (
		captured    openAIEmbeddingRequest
		gotCallSite string
		gotTokens   int
	)
	embed := NewOpenAIEmbeddingBatchCall(OpenAIEmbeddingBatchCallInput{
		Model:     "text-embedding-3-small",
		APIKey:    "test-key",
		UserAgent: "docs-test",
		Do: func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
				t.Fatalf("Authorization = %q, want bearer key", got)
			}
			if got := req.Header.Get("User-Agent"); got != "docs-test" {
				t.Fatalf("User-Agent = %q, want docs-test", got)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("ReadAll request body returned error: %v", err)
			}
			if err := json.Unmarshal(body, &captured); err != nil {
				t.Fatalf("Unmarshal request body returned error: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":1,"embedding":[2]},{"index":0,"embedding":[1]}],"usage":{"total_tokens":19}}`)),
			}, nil
		},
		CallSite: "docs-puller.embed",
		Record: func(_providerName, _model, callSite string, inputTokens, _outputTokens int, _cents float64) {
			gotCallSite = callSite
			gotTokens = inputTokens
		},
	})
	vectors, err := embed([]string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("batch embed returned error: %v", err)
	}
	if captured.Model != "text-embedding-3-small" || !slices.Equal(captured.Input, []string{"alpha", "beta"}) {
		t.Fatalf("captured request = %+v, want two batch inputs", captured)
	}
	if len(vectors) != 2 || len(vectors[0]) != 1 || vectors[0][0] != 1 || len(vectors[1]) != 1 || vectors[1][0] != 2 {
		t.Fatalf("vectors = %v, want ordered batch vectors", vectors)
	}
	if gotCallSite != "docs-puller.embed" || gotTokens != 19 {
		t.Fatalf("usage log = callSite=%q tokens=%d, want batch usage", gotCallSite, gotTokens)
	}
}

func TestRunOpenAIEmbeddingBatchWithFallback(t *testing.T) {
	var captured openAIEmbeddingRequest
	kept, vecs, dropped, err := RunOpenAIEmbeddingBatchWithFallback(OpenAIEmbeddingBatchWithFallbackInput[string]{
		OpenAI: OpenAIEmbeddingBatchCallInput{
			Model:     "text-embedding-3-small",
			APIKey:    "test-key",
			UserAgent: "docs-test",
			Do: func(req *http.Request) (*http.Response, error) {
				if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
					t.Fatalf("Authorization = %q, want bearer key", got)
				}
				if got := req.Header.Get("User-Agent"); got != "docs-test" {
					t.Fatalf("User-Agent = %q, want docs-test", got)
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll request body returned error: %v", err)
				}
				if err := json.Unmarshal(body, &captured); err != nil {
					t.Fatalf("Unmarshal request body returned error: %v", err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[1,2]}]}`)),
				}, nil
			},
			CallSite: "docs-puller.embed",
		},
		Batch: []string{"alpha"},
		Text: func(item string) string {
			return item
		},
	})
	if err != nil {
		t.Fatalf("RunOpenAIEmbeddingBatchWithFallback returned error: %v", err)
	}
	if dropped != 0 || !slices.Equal(kept, []string{"alpha"}) {
		t.Fatalf("kept=%v dropped=%d, want original batch and no drops", kept, dropped)
	}
	if captured.Model != "text-embedding-3-small" || !slices.Equal(captured.Input, []string{"alpha"}) {
		t.Fatalf("captured request = %+v, want mapped text input", captured)
	}
	if len(vecs) != 1 || len(vecs[0]) != 2 || vecs[0][0] != 1 || vecs[0][1] != 2 {
		t.Fatalf("vectors = %v, want decoded vector", vecs)
	}
}

func TestNewOpenAIEmbeddingQueryCall(t *testing.T) {
	var (
		captured    openAIEmbeddingRequest
		gotCallSite string
		gotTokens   int
	)
	embedQuery := NewOpenAIEmbeddingQueryCall(OpenAIEmbeddingQueryCallInput{
		UserAgent: "docs-test",
		Do: func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
				t.Fatalf("Authorization = %q, want bearer key", got)
			}
			if got := req.Header.Get("User-Agent"); got != "docs-test" {
				t.Fatalf("User-Agent = %q, want docs-test", got)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("ReadAll request body returned error: %v", err)
			}
			if err := json.Unmarshal(body, &captured); err != nil {
				t.Fatalf("Unmarshal request body returned error: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[3,4]}],"usage":{"total_tokens":23}}`)),
			}, nil
		},
		CallSite: "docs-puller.search.embed_rerank",
		Record: func(_providerName, _model, callSite string, inputTokens, _outputTokens int, _cents float64) {
			gotCallSite = callSite
			gotTokens = inputTokens
		},
	})
	vectors, err := embedQuery("text-embedding-3-small", "test-key", "search query")
	if err != nil {
		t.Fatalf("query embed returned error: %v", err)
	}
	if captured.Model != "text-embedding-3-small" || !slices.Equal(captured.Input, []string{"search query"}) {
		t.Fatalf("captured request = %+v, want one query input", captured)
	}
	if len(vectors) != 1 || len(vectors[0]) != 2 || vectors[0][0] != 3 || vectors[0][1] != 4 {
		t.Fatalf("vectors = %v, want query vector", vectors)
	}
	if gotCallSite != "docs-puller.search.embed_rerank" || gotTokens != 23 {
		t.Fatalf("usage log = callSite=%q tokens=%d, want query usage", gotCallSite, gotTokens)
	}
}

func TestRunOpenAIEmbeddingTextWithUsageLog(t *testing.T) {
	var (
		captured    openAIEmbeddingRequest
		gotCallSite string
		gotTokens   int
	)
	vectors, err := RunOpenAIEmbeddingTextWithUsageLog("text-embedding-3-small", "search query", "test-key", "docs-test", func(req *http.Request) (*http.Response, error) {
		if got := req.Method; got != http.MethodPost {
			t.Fatalf("method = %q, want POST", got)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q, want bearer key", got)
		}
		if got := req.Header.Get("User-Agent"); got != "docs-test" {
			t.Fatalf("User-Agent = %q, want docs-test", got)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("ReadAll request body returned error: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("Unmarshal request body returned error: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[3,4]}],"usage":{"total_tokens":17}}`)),
		}, nil
	}, "docs-puller.search.embed_rerank", func(_providerName, _model, callSite string, inputTokens, _outputTokens int, _cents float64) {
		gotCallSite = callSite
		gotTokens = inputTokens
	})
	if err != nil {
		t.Fatalf("RunOpenAIEmbeddingTextWithUsageLog returned error: %v", err)
	}
	if captured.Model != "text-embedding-3-small" || !slices.Equal(captured.Input, []string{"search query"}) {
		t.Fatalf("captured request = %+v, want one query input", captured)
	}
	if len(vectors) != 1 || len(vectors[0]) != 2 || vectors[0][0] != 3 || vectors[0][1] != 4 {
		t.Fatalf("vectors = %v, want decoded query vector", vectors)
	}
	if gotCallSite != "docs-puller.search.embed_rerank" || gotTokens != 17 {
		t.Fatalf("usage log = callSite=%q tokens=%d, want query usage", gotCallSite, gotTokens)
	}
}

func TestEmitOpenAIEmbeddingUsageEvent(t *testing.T) {
	var recorded EmbeddingUsageEvent
	got := EmitOpenAIEmbeddingUsageEvent(OpenAIEmbeddingSuccess{
		UsageEvent: EmbeddingUsageEvent{
			ProviderName: "openai",
			Model:        "text-embedding-3-small",
			CallSite:     "custom",
			Tokens:       13,
			Cents:        0.01,
		},
		UsageEventPresent: true,
	}, func(event EmbeddingUsageEvent) {
		recorded = event
	})
	if !got {
		t.Fatalf("EmitOpenAIEmbeddingUsageEvent = false, want true")
	}
	if recorded.ProviderName != "openai" || recorded.Model != "text-embedding-3-small" || recorded.CallSite != "custom" || recorded.Tokens != 13 {
		t.Fatalf("recorded event = %+v, want success usage event", recorded)
	}

	if EmitOpenAIEmbeddingUsageEvent(OpenAIEmbeddingSuccess{UsageEventPresent: true}, nil) {
		t.Fatalf("nil recorder returned true")
	}
	if EmitOpenAIEmbeddingUsageEvent(OpenAIEmbeddingSuccess{}, func(EmbeddingUsageEvent) {
		t.Fatal("recorder should not be called without usage")
	}) {
		t.Fatalf("missing usage returned true")
	}
}

func TestAdaptEmbeddingUsageLogRecorder(t *testing.T) {
	var (
		gotProvider     string
		gotModel        string
		gotCallSite     string
		gotInputTokens  int
		gotOutputTokens int
		gotCents        float64
	)
	recorder := AdaptEmbeddingUsageLogRecorder(func(providerName, model, callSite string, inputTokens, outputTokens int, cents float64) {
		gotProvider = providerName
		gotModel = model
		gotCallSite = callSite
		gotInputTokens = inputTokens
		gotOutputTokens = outputTokens
		gotCents = cents
	})
	if recorder == nil {
		t.Fatalf("AdaptEmbeddingUsageLogRecorder returned nil recorder")
	}
	recorder(EmbeddingUsageEvent{
		ProviderName: "openai",
		Model:        "text-embedding-3-small",
		CallSite:     "custom",
		Tokens:       13,
		Cents:        0.01,
	})
	if gotProvider != "openai" || gotModel != "text-embedding-3-small" || gotCallSite != "custom" || gotInputTokens != 13 || gotOutputTokens != 0 || gotCents != 0.01 {
		t.Fatalf("usage log record = provider=%q model=%q callSite=%q input=%d output=%d cents=%.2f", gotProvider, gotModel, gotCallSite, gotInputTokens, gotOutputTokens, gotCents)
	}
	if AdaptEmbeddingUsageLogRecorder(nil) != nil {
		t.Fatalf("nil usage log recorder should adapt to nil embedding recorder")
	}
}

func collectEmbeddingBatchItems(items []int) [][]int {
	var batches [][]int
	for batch := range ChunkEmbeddingBatchItems(items, func(item int) int {
		return item
	}) {
		batches = append(batches, batch)
	}
	return batches
}

func TestBuildRerankUsageEvent(t *testing.T) {
	got := BuildRerankUsageEvent(RerankUsageEventInput{
		ProviderName: "openai",
		Model:        "gpt-4.1-mini",
		InputTokens:  1000,
		OutputTokens: 250,
		ProviderPricing: RerankUsagePricing{
			InputPer1M:  0.15,
			OutputPer1M: 0.60,
		},
	})
	if got.ProviderName != "openai" || got.Model != "gpt-4.1-mini" || got.CallSite != DefaultRerankUsageCallSite {
		t.Fatalf("event identity = %+v, want default callsite and provider/model", got)
	}
	if got.InputTokens != 1000 || got.OutputTokens != 250 {
		t.Fatalf("tokens = %+v, want input=1000 output=250", got)
	}
	if got.Cents < 0.029999 || got.Cents > 0.030001 {
		t.Fatalf("cents = %.6f, want 0.03", got.Cents)
	}

	custom := BuildRerankUsageEvent(RerankUsageEventInput{
		ProviderName: "cohere",
		Model:        "rerank-v3.5",
		CallSite:     "custom",
		InputTokens:  -1,
		ProviderPricing: RerankUsagePricing{
			InputPer1M: 2,
		},
	})
	if custom.CallSite != "custom" {
		t.Fatalf("custom callsite = %q, want custom", custom.CallSite)
	}
	if custom.Cents != 0 {
		t.Fatalf("negative token estimate should clamp to zero cents, got %.6f", custom.Cents)
	}
}

func TestEmitRerankUsageEvent(t *testing.T) {
	var recorded RerankUsageEvent
	got := EmitRerankUsageEvent(RerankUsageEventInput{
		ProviderName: "openai",
		Model:        "gpt-4.1-mini",
		InputTokens:  1000,
		ProviderPricing: RerankUsagePricing{
			InputPer1M: 0.15,
		},
	}, func(event RerankUsageEvent) {
		recorded = event
	})
	if got != recorded {
		t.Fatalf("recorded event = %+v, want %+v", recorded, got)
	}
	if got.CallSite != DefaultRerankUsageCallSite || got.Cents < 0.014999 || got.Cents > 0.015001 {
		t.Fatalf("event = %+v, want default callsite and estimated cents", got)
	}

	unrecorded := EmitRerankUsageEvent(RerankUsageEventInput{
		ProviderName: "tei",
		Model:        "BAAI/bge-reranker-v2-m3",
	}, nil)
	if unrecorded.ProviderName != "tei" || unrecorded.Model != "BAAI/bge-reranker-v2-m3" {
		t.Fatalf("nil recorder event = %+v, want stable event", unrecorded)
	}
}

func TestRunLLMRerankChatPathBuildsPromptAndAppliesOrder(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "guide.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# Guide\n\nbody text"), 0o644); err != nil {
		t.Fatal(err)
	}

	chatPrompt := ""
	got, err := RunLLMRerank(LLMRerankRunInput{
		Query:        "rank docs",
		OutDir:       root,
		ProviderName: "openai",
		Hits: []Hit{
			{Path: "docs/missing.md", Title: "Missing"},
			{Path: "docs/guide.md", Title: "Guide"},
		},
		Chat: func(prompt string) ([]int, error) {
			chatPrompt = prompt
			return []int{1, 0}, nil
		},
	})
	if err != nil {
		t.Fatalf("RunLLMRerank returned error: %v", err)
	}
	if len(got) != 2 || got[0].Path != "docs/guide.md" || got[1].Path != "docs/missing.md" {
		t.Fatalf("hits = %+v, want chat order applied", got)
	}
	if !strings.Contains(chatPrompt, `Query: "rank docs"`) || !strings.Contains(chatPrompt, `path=docs/guide.md`) || !strings.Contains(chatPrompt, "body text") {
		t.Fatalf("prompt missing expected query/candidate text: %s", chatPrompt)
	}
}

func TestRunLLMRerankCoherePathUsesCandidates(t *testing.T) {
	var gotCandidates []LLMRerankCandidate
	got, err := RunLLMRerank(LLMRerankRunInput{
		Query:        "rank docs",
		ProviderName: "cohere",
		Hits: []Hit{
			{Path: "a.md", Title: "A"},
			{Path: "b.md", Title: "B"},
		},
		Cohere: func(candidates []LLMRerankCandidate) ([]int, error) {
			gotCandidates = candidates
			return []int{1, 0}, nil
		},
	})
	if err != nil {
		t.Fatalf("RunLLMRerank returned error: %v", err)
	}
	if len(got) != 2 || got[0].Path != "b.md" || got[1].Path != "a.md" {
		t.Fatalf("hits = %+v, want cohere order applied", got)
	}
	if len(gotCandidates) != 2 || gotCandidates[0].Index != 0 || gotCandidates[0].Title != "A" {
		t.Fatalf("candidates = %+v, want runtime candidates", gotCandidates)
	}
}

func TestRunLLMRerankMissingProviderCall(t *testing.T) {
	for _, tt := range []struct {
		name         string
		providerName string
		wantErr      error
	}{
		{name: "cohere", providerName: "cohere", wantErr: CohereRerankCallMissingError()},
		{name: "tei", providerName: "tei", wantErr: TEIRerankCallMissingError()},
		{name: "llm", providerName: "openai", wantErr: LLMRerankCallMissingError()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RunLLMRerank(LLMRerankRunInput{
				ProviderName: tt.providerName,
				Hits:         []Hit{{Path: "a.md"}},
			})
			if err == nil || err.Error() != tt.wantErr.Error() {
				t.Fatalf("expected nil-call error %q, got hits=%+v err=%v", tt.wantErr.Error(), got, err)
			}
		})
	}
}

func TestRunLLMRerankWrapsProviderCallErrors(t *testing.T) {
	callErr := errors.New("provider unavailable")
	for _, tt := range []struct {
		name      string
		input     LLMRerankRunInput
		wantError func(error) error
	}{
		{
			name: "cohere",
			input: LLMRerankRunInput{
				ProviderName: "cohere",
				Cohere: func(_ []LLMRerankCandidate) ([]int, error) {
					return nil, callErr
				},
			},
			wantError: CohereRerankCallError,
		},
		{
			name: "tei",
			input: LLMRerankRunInput{
				ProviderName: "tei",
				TEI: func(_ []LLMRerankCandidate) ([]int, error) {
					return nil, callErr
				},
			},
			wantError: TEIRerankCallError,
		},
		{
			name: "llm",
			input: LLMRerankRunInput{
				ProviderName: "openai",
				Chat: func(_ string) ([]int, error) {
					return nil, callErr
				},
			},
			wantError: LLMRerankCallError,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.input.Hits = []Hit{{Path: "a.md"}}
			got, err := RunLLMRerank(tt.input)
			if err == nil || err.Error() != tt.wantError(callErr).Error() {
				t.Fatalf("expected provider call error, got hits=%+v err=%v", got, err)
			}
			if !errors.Is(err, callErr) {
				t.Fatalf("provider call error does not wrap callback error: %v", err)
			}
			if got != nil {
				t.Fatalf("hits = %+v, want nil on provider call error", got)
			}
		})
	}
}

type embeddingRerankTestIndex struct {
	closed   bool
	closeErr error
}

func (i *embeddingRerankTestIndex) Close() error {
	i.closed = true
	return i.closeErr
}

func TestNewReadOnlyEmbeddingIndexOpenCall(t *testing.T) {
	index := &embeddingRerankTestIndex{}
	var (
		gotOutDir   string
		gotReadOnly bool
	)
	openIndex := NewReadOnlyEmbeddingIndexOpenCall(func(outDir string, readOnly bool) (*embeddingRerankTestIndex, error) {
		gotOutDir = outDir
		gotReadOnly = readOnly
		return index, nil
	})
	got, err := openIndex("docs-out")
	if err != nil {
		t.Fatalf("openIndex returned error: %v", err)
	}
	if got != index || gotOutDir != "docs-out" || !gotReadOnly {
		t.Fatalf("open result = index=%p outDir=%q readOnly=%v, want index/read-only docs-out", got, gotOutDir, gotReadOnly)
	}
}

func TestNewHybridVectorLoadCall(t *testing.T) {
	index := &embeddingRerankTestIndex{}
	var (
		gotIndex *embeddingRerankTestIndex
		gotModel string
	)
	loadVectors := NewHybridVectorLoadCall(func(index *embeddingRerankTestIndex, model string) (map[string][]float32, error) {
		gotIndex = index
		gotModel = model
		return map[string][]float32{"docs/a.md": {0.75}}, nil
	})

	got, err := loadVectors(index, "text-embedding-3-small")
	if err != nil {
		t.Fatalf("loadVectors returned error: %v", err)
	}
	if gotIndex != index || gotModel != "text-embedding-3-small" || got["docs/a.md"][0] != 0.75 {
		t.Fatalf("load result = index=%p model=%q vectors=%v, want input index/model and vector", gotIndex, gotModel, got)
	}
}

func TestNewHybridVectorLoadCallNil(t *testing.T) {
	var load HybridVectorLoadCall[*embeddingRerankTestIndex]
	if got := NewHybridVectorLoadCall(load); got != nil {
		t.Fatalf("nil load adapter returned non-nil callback")
	}
}

func TestNewEmbeddingRerankWholeCall(t *testing.T) {
	index := &embeddingRerankTestIndex{}
	var (
		gotIndex *embeddingRerankTestIndex
		gotVec   []float32
	)
	rerank := NewEmbeddingRerankWholeCall(func(index *embeddingRerankTestIndex, hits []Hit, queryVec []float32) ([]Hit, error) {
		gotIndex = index
		gotVec = queryVec
		return []Hit{{Path: hits[1].Path}, {Path: hits[0].Path}}, nil
	})

	got, err := rerank(index, []Hit{{Path: "docs/a.md"}, {Path: "docs/b.md"}}, []float32{0.25})
	if err != nil {
		t.Fatalf("rerank returned error: %v", err)
	}
	if gotIndex != index || len(gotVec) != 1 || gotVec[0] != 0.25 || len(got) != 2 || got[0].Path != "docs/b.md" {
		t.Fatalf("rerank result = index=%p vec=%v hits=%+v, want forwarded index/vector and reordered hits", gotIndex, gotVec, got)
	}
}

func TestNewEmbeddingRerankWholeCallNil(t *testing.T) {
	var rerank EmbeddingRerankWholeCall[*embeddingRerankTestIndex]
	if got := NewEmbeddingRerankWholeCall(rerank); got != nil {
		t.Fatalf("nil whole-doc rerank adapter returned non-nil callback")
	}
}

func TestNewEmbeddingRerankChunkedCall(t *testing.T) {
	index := &embeddingRerankTestIndex{}
	var (
		gotIndex     *embeddingRerankTestIndex
		gotVec       []float32
		gotModel     string
		gotChunkSize int
	)
	rerank := NewEmbeddingRerankChunkedCall(func(index *embeddingRerankTestIndex, hits []Hit, queryVec []float32, model string, chunkSize int) ([]Hit, error) {
		gotIndex = index
		gotVec = queryVec
		gotModel = model
		gotChunkSize = chunkSize
		return []Hit{{Path: hits[1].Path}, {Path: hits[0].Path}}, nil
	})

	got, err := rerank(index, []Hit{{Path: "docs/a.md"}, {Path: "docs/b.md"}}, []float32{0.5}, "text-embedding-3-small", 900)
	if err != nil {
		t.Fatalf("rerank returned error: %v", err)
	}
	if gotIndex != index || len(gotVec) != 1 || gotVec[0] != 0.5 || gotModel != "text-embedding-3-small" || gotChunkSize != 900 || len(got) != 2 || got[0].Path != "docs/b.md" {
		t.Fatalf("rerank result = index=%p vec=%v model=%q chunk=%d hits=%+v, want forwarded inputs and reordered hits", gotIndex, gotVec, gotModel, gotChunkSize, got)
	}
}

func TestNewEmbeddingRerankChunkedCallNil(t *testing.T) {
	var rerank EmbeddingRerankChunkedCall[*embeddingRerankTestIndex]
	if got := NewEmbeddingRerankChunkedCall(rerank); got != nil {
		t.Fatalf("nil chunked rerank adapter returned non-nil callback")
	}
}

func TestRunEmbeddingRerankWholeDoc(t *testing.T) {
	index := &embeddingRerankTestIndex{}
	var (
		gotModel  string
		gotAPIKey string
		gotQuery  string
		gotOutDir string
	)

	got, err := RunEmbeddingRerank(EmbeddingRerankRunInput[*embeddingRerankTestIndex]{
		Query:        "rank docs",
		Hits:         []Hit{{Path: "a.md"}, {Path: "b.md"}},
		OutDir:       "docs-out",
		DefaultModel: "text-embedding-3-small",
		APIKey:       "test-key",
		APIKeyEnv:    "OPENAI_API_KEY",
		EmbedQuery: func(model, apiKey, query string) ([][]float32, error) {
			gotModel = model
			gotAPIKey = apiKey
			gotQuery = query
			return [][]float32{{1, 0}}, nil
		},
		OpenIndex: func(outDir string) (*embeddingRerankTestIndex, error) {
			gotOutDir = outDir
			return index, nil
		},
		RerankWhole: func(gotIndex *embeddingRerankTestIndex, hits []Hit, queryVec []float32) ([]Hit, error) {
			if gotIndex != index {
				t.Fatalf("rerank index = %p, want %p", gotIndex, index)
			}
			if len(queryVec) != 2 || queryVec[0] != 1 || queryVec[1] != 0 {
				t.Fatalf("queryVec = %v, want [1 0]", queryVec)
			}
			return []Hit{hits[1], hits[0]}, nil
		},
	})
	if err != nil {
		t.Fatalf("RunEmbeddingRerank returned error: %v", err)
	}
	if gotModel != "text-embedding-3-small" || gotAPIKey != "test-key" || gotQuery != "rank docs" || gotOutDir != "docs-out" {
		t.Fatalf("callback inputs = model=%q apiKey=%q query=%q outDir=%q", gotModel, gotAPIKey, gotQuery, gotOutDir)
	}
	if len(got) != 2 || got[0].Path != "b.md" || got[1].Path != "a.md" {
		t.Fatalf("hits = %+v, want whole-doc order applied", got)
	}
	if !index.closed {
		t.Fatalf("index was not closed")
	}
}

func TestRunEmbeddingRerankChunked(t *testing.T) {
	index := &embeddingRerankTestIndex{}
	var gotEmbedModel string
	var gotChunkModel string
	var gotChunkSize int

	got, err := RunEmbeddingRerank(EmbeddingRerankRunInput[*embeddingRerankTestIndex]{
		Query:     "rank docs",
		Hits:      []Hit{{Path: "a.md"}},
		Model:     "custom-embedding-model",
		APIKey:    "test-key",
		ChunkSize: 1500,
		EmbedQuery: func(model, _ string, _ string) ([][]float32, error) {
			gotEmbedModel = model
			return [][]float32{{1, 0}}, nil
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			return index, nil
		},
		RerankChunked: func(gotIndex *embeddingRerankTestIndex, hits []Hit, _ []float32, model string, chunkSize int) ([]Hit, error) {
			if gotIndex != index {
				t.Fatalf("rerank index = %p, want %p", gotIndex, index)
			}
			gotChunkSize = chunkSize
			gotChunkModel = model
			return hits, nil
		},
	})
	if err != nil {
		t.Fatalf("RunEmbeddingRerank returned error: %v", err)
	}
	if len(got) != 1 || got[0].Path != "a.md" {
		t.Fatalf("hits = %+v, want original chunked hits", got)
	}
	if gotEmbedModel != "custom-embedding-model" || gotChunkModel != "custom-embedding-model" || gotChunkSize != 1500 {
		t.Fatalf("chunked inputs = embedModel=%q chunkModel=%q chunkSize=%d", gotEmbedModel, gotChunkModel, gotChunkSize)
	}
	if !index.closed {
		t.Fatalf("index was not closed")
	}
}

func TestRunEmbeddingRerankRejectsMissingAPIKey(t *testing.T) {
	got, err := RunEmbeddingRerank(EmbeddingRerankRunInput[*embeddingRerankTestIndex]{
		APIKeyEnv: "OPENAI_API_KEY",
	})
	if err == nil || err.Error() != ProviderAPIKeyNotSetError("OPENAI_API_KEY").Error() {
		t.Fatalf("expected missing API-key error, got hits=%+v err=%v", got, err)
	}
}

func TestRunEmbeddingRerankRejectsMissingEmbedQuery(t *testing.T) {
	got, err := RunEmbeddingRerank(EmbeddingRerankRunInput[*embeddingRerankTestIndex]{
		APIKey: "test-key",
	})
	if err == nil || err.Error() != EmbeddingQueryCallMissingError().Error() {
		t.Fatalf("expected missing embed-query error %q, got hits=%+v err=%v", EmbeddingQueryCallMissingError().Error(), got, err)
	}
}

func TestRunEmbeddingRerankWrapsEmbedQueryError(t *testing.T) {
	embedErr := errors.New("provider failed")
	got, err := RunEmbeddingRerank(EmbeddingRerankRunInput[*embeddingRerankTestIndex]{
		APIKey: "test-key",
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return nil, embedErr
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			t.Fatalf("OpenIndex should not run after embed-query error")
			return nil, nil
		},
	})
	if err == nil || err.Error() != EmbeddingQueryError(embedErr).Error() {
		t.Fatalf("expected embed-query error, got hits=%+v err=%v", got, err)
	}
	if !errors.Is(err, embedErr) {
		t.Fatalf("embed-query error does not wrap provider error: %v", err)
	}
	if got != nil {
		t.Fatalf("hits = %+v, want nil on embed-query error", got)
	}
}

func TestRunEmbeddingRerankRejectsMissingOpenIndex(t *testing.T) {
	got, err := RunEmbeddingRerank(EmbeddingRerankRunInput[*embeddingRerankTestIndex]{
		APIKey: "test-key",
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return [][]float32{{1, 0}}, nil
		},
	})
	if err == nil || err.Error() != EmbeddingIndexOpenCallMissingError().Error() {
		t.Fatalf("expected missing open-index error %q, got hits=%+v err=%v", EmbeddingIndexOpenCallMissingError().Error(), got, err)
	}
}

func TestRunEmbeddingRerankWrapsOpenIndexError(t *testing.T) {
	openErr := errors.New("db unavailable")
	got, err := RunEmbeddingRerank(EmbeddingRerankRunInput[*embeddingRerankTestIndex]{
		APIKey: "test-key",
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return [][]float32{{1, 0}}, nil
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			return nil, openErr
		},
		RerankWhole: func(_ *embeddingRerankTestIndex, hits []Hit, _ []float32) ([]Hit, error) {
			t.Fatalf("RerankWhole should not run after open-index error")
			return hits, nil
		},
	})
	if err == nil || err.Error() != EmbeddingIndexOpenError(openErr).Error() {
		t.Fatalf("expected open-index error, got hits=%+v err=%v", got, err)
	}
	if !errors.Is(err, openErr) {
		t.Fatalf("open-index error does not wrap opener error: %v", err)
	}
	if got != nil {
		t.Fatalf("hits = %+v, want nil on open-index error", got)
	}
}

func TestRunEmbeddingRerankRejectsMissingChunkedRerank(t *testing.T) {
	index := &embeddingRerankTestIndex{}
	got, err := RunEmbeddingRerank(EmbeddingRerankRunInput[*embeddingRerankTestIndex]{
		Query:     "rank docs",
		APIKey:    "test-key",
		ChunkSize: 1000,
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return [][]float32{{1, 0}}, nil
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			return index, nil
		},
	})
	if err == nil || err.Error() != EmbeddingRerankChunkedCallMissingError().Error() {
		t.Fatalf("expected missing chunked-rerank error %q, got hits=%+v err=%v", EmbeddingRerankChunkedCallMissingError().Error(), got, err)
	}
	if !index.closed {
		t.Fatalf("index was not closed")
	}
}

func TestRunEmbeddingRerankRejectsMissingWholeDocRerank(t *testing.T) {
	index := &embeddingRerankTestIndex{}
	got, err := RunEmbeddingRerank(EmbeddingRerankRunInput[*embeddingRerankTestIndex]{
		Query:  "rank docs",
		APIKey: "test-key",
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return [][]float32{{1, 0}}, nil
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			return index, nil
		},
	})
	if err == nil || err.Error() != EmbeddingRerankWholeCallMissingError().Error() {
		t.Fatalf("expected missing whole-doc-rerank error %q, got hits=%+v err=%v", EmbeddingRerankWholeCallMissingError().Error(), got, err)
	}
	if !index.closed {
		t.Fatalf("index was not closed")
	}
}

func TestRunEmbeddingRerankReturnsCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	index := &embeddingRerankTestIndex{closeErr: closeErr}
	got, err := RunEmbeddingRerank(EmbeddingRerankRunInput[*embeddingRerankTestIndex]{
		Query:  "rank docs",
		Hits:   []Hit{{Path: "a.md"}},
		APIKey: "test-key",
		EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
			return [][]float32{{1, 0}}, nil
		},
		OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
			return index, nil
		},
		RerankWhole: func(_ *embeddingRerankTestIndex, hits []Hit, _ []float32) ([]Hit, error) {
			return hits, nil
		},
	})
	if err == nil || err.Error() != EmbeddingIndexCloseError(closeErr).Error() {
		t.Fatalf("expected close error, got hits=%+v err=%v", got, err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("close error does not wrap index close error: %v", err)
	}
	if len(got) != 1 || got[0].Path != "a.md" {
		t.Fatalf("hits = %+v, want reranked hits preserved with close error", got)
	}
	if !index.closed {
		t.Fatalf("index was not closed")
	}
}

func TestRunEmbeddingRerankRejectsBadVectorCount(t *testing.T) {
	for name, vecs := range map[string][][]float32{
		"none": {},
		"many": {{1, 0}, {0, 1}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RunEmbeddingRerank(EmbeddingRerankRunInput[*embeddingRerankTestIndex]{
				Query:  "rank docs",
				APIKey: "test-key",
				EmbedQuery: func(_ string, _ string, _ string) ([][]float32, error) {
					return vecs, nil
				},
				OpenIndex: func(_ string) (*embeddingRerankTestIndex, error) {
					t.Fatalf("OpenIndex should not run after bad vector count")
					return nil, nil
				},
			})
			if err == nil || err.Error() != EmbeddingQueryVectorCountError(len(vecs)).Error() {
				t.Fatalf("expected vector-count error %q, got hits=%+v err=%v", EmbeddingQueryVectorCountError(len(vecs)).Error(), got, err)
			}
		})
	}
}

func TestParseChatCompletionRankResponse(t *testing.T) {
	got, err := ParseChatCompletionRankResponse([]byte(`{"choices":[{"message":{"content":"{\"order\":[1,0]}"}}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`))
	if err != nil {
		t.Fatalf("ParseChatCompletionRankResponse returned error: %v", err)
	}
	if len(got.Order) != 2 || got.Order[0] != 1 || got.Order[1] != 0 {
		t.Fatalf("order = %v, want [1 0]", got.Order)
	}
	if !got.Usage.Present || got.Usage.PromptTokens != 11 || got.Usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v, want prompt=11 completion=3", got.Usage)
	}

	providerErr, err := ParseChatCompletionRankResponse([]byte(`{"error":{"message":"bad key"}}`))
	if err != nil {
		t.Fatalf("provider error response should parse without JSON error: %v", err)
	}
	if providerErr.ProviderError != "bad key" {
		t.Fatalf("provider error = %q, want bad key", providerErr.ProviderError)
	}
}

func TestParseHyDEChatResponse(t *testing.T) {
	got, err := ParseHyDEChatResponse([]byte(`{"choices":[{"message":{"content":" canonical passage "}}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`))
	if err != nil {
		t.Fatalf("ParseHyDEChatResponse returned error: %v", err)
	}
	if got.Document != " canonical passage " {
		t.Fatalf("document = %q, want raw provider content", got.Document)
	}
	if !got.Usage.Present || got.Usage.PromptTokens != 11 || got.Usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v, want prompt=11 completion=3", got.Usage)
	}

	providerErr, err := ParseHyDEChatResponse([]byte(`{"error":{"message":"bad key"}}`))
	if err != nil {
		t.Fatalf("provider error response should parse without JSON error: %v", err)
	}
	if providerErr.ProviderError != "bad key" {
		t.Fatalf("provider error = %q, want bad key", providerErr.ProviderError)
	}

	empty, err := ParseHyDEChatResponse([]byte(`{"choices":[]}`))
	if err == nil || err.Error() != ChatCompletionNoChoicesError().Error() {
		t.Fatalf("empty choices error = %v, want no choices", err)
	}
	if empty != (HyDEChatResponse{}) {
		t.Fatalf("empty choices response = %+v, want zero value", empty)
	}

	invalid, err := ParseHyDEChatResponse([]byte(`[`))
	if err == nil || !strings.HasPrefix(err.Error(), "parse response: ") {
		t.Fatalf("parse error = %v, want parse response wrapper", err)
	}
	if invalid != (HyDEChatResponse{}) {
		t.Fatalf("invalid response = %+v, want zero value", invalid)
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("parse error does not wrap JSON syntax error: %v", err)
	}
}

func TestRerankParsersWrapResponseParseErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func([]byte) error
	}{
		{
			name: "chat",
			call: func(raw []byte) error {
				_, err := ParseChatCompletionRankResponse(raw)
				return err
			},
		},
		{
			name: "cohere",
			call: func(raw []byte) error {
				_, _, err := ParseCohereRankResponse(raw)
				return err
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call([]byte(`[`))
			if err == nil || !strings.HasPrefix(err.Error(), "parse response: ") {
				t.Fatalf("parse error = %v, want parse response wrapper", err)
			}
			var syntaxErr *json.SyntaxError
			if !errors.As(err, &syntaxErr) {
				t.Fatalf("parse error does not wrap JSON syntax error: %v", err)
			}
		})
	}
}

func TestRerankParsersFormatEmptyResponseErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func() error
		want error
	}{
		{
			name: "chat",
			call: func() error {
				_, err := ParseChatCompletionRankResponse([]byte(`{"choices":[]}`))
				return err
			},
			want: ChatCompletionNoChoicesError(),
		},
		{
			name: "cohere",
			call: func() error {
				_, _, err := ParseCohereRankResponse([]byte(`{"results":[]}`))
				return err
			},
			want: RerankNoResultsError("cohere"),
		},
		{
			name: "tei",
			call: func() error {
				_, err := ParseTEIRankResponse([]byte(`[]`))
				return err
			},
			want: RerankNoResultsError("tei"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil || err.Error() != tt.want.Error() {
				t.Fatalf("empty response error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestParseChatCompletionRankResponseWrapsOrderParseError(t *testing.T) {
	content := "{" + strings.Repeat("x", 240)
	raw, err := json.Marshal(struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}{
		Choices: []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}{
			{
				Message: struct {
					Content string `json:"content"`
				}{Content: content},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal chat response: %v", err)
	}

	got, err := ParseChatCompletionRankResponse(raw)
	if err == nil || !strings.HasPrefix(err.Error(), "parse order: ") {
		t.Fatalf("parse order error = %v, want parse order wrapper", err)
	}
	if !strings.Contains(err.Error(), "(raw: "+truncateForError(content, 200)+")") {
		t.Fatalf("parse order error = %v, want capped raw preview", err)
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("parse order error does not wrap JSON syntax error: %v", err)
	}
	if got.Order != nil {
		t.Fatalf("order = %v, want nil", got.Order)
	}
}

func TestProviderRankersRejectMissingHTTPClient(t *testing.T) {
	for _, tt := range []struct {
		name         string
		call         func() ([]int, error)
		providerName string
	}{
		{
			name: "chat",
			call: func() ([]int, error) {
				return CallChatCompletionRanker(ChatCompletionRankerInput{ProviderName: "openai"})
			},
			providerName: "openai",
		},
		{
			name: "cohere",
			call: func() ([]int, error) {
				return CallCohereRanker(CohereRankerInput{})
			},
			providerName: "cohere",
		},
		{
			name: "tei",
			call: func() ([]int, error) {
				return CallTEIRanker(TEIRankerInput{})
			},
			providerName: "tei",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			order, err := tt.call()
			if err == nil || err.Error() != RerankHTTPClientMissingError(tt.providerName).Error() {
				t.Fatalf("expected missing HTTP client error, got order=%v err=%v", order, err)
			}
			if order != nil {
				t.Fatalf("order = %v, want nil", order)
			}
		})
	}
}

type rankerRoundTrip func(*http.Request) (*http.Response, error)

func (f rankerRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type rankerErrorBody struct {
	data     []byte
	read     bool
	readErr  error
	closeErr error
}

func (b *rankerErrorBody) Read(p []byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	if b.read {
		return 0, io.EOF
	}
	b.read = true
	return copy(p, b.data), nil
}

func (b *rankerErrorBody) Close() error {
	return b.closeErr
}

func TestProviderRankersWrapResponseBodyErrors(t *testing.T) {
	for _, tt := range []struct {
		name         string
		providerName string
		call         func(*http.Client, func(time.Duration)) ([]int, error)
	}{
		{
			name:         "chat",
			providerName: "openai",
			call: func(client *http.Client, sleep func(time.Duration)) ([]int, error) {
				return CallChatCompletionRanker(ChatCompletionRankerInput{
					ProviderName: "openai",
					BaseURL:      "http://ranker.test",
					HTTPClient:   client,
					Sleep:        sleep,
				})
			},
		},
		{
			name:         "cohere",
			providerName: "cohere",
			call: func(client *http.Client, sleep func(time.Duration)) ([]int, error) {
				return CallCohereRanker(CohereRankerInput{
					BaseURL:    "http://ranker.test",
					HTTPClient: client,
					Sleep:      sleep,
				})
			},
		},
		{
			name:         "tei",
			providerName: "tei",
			call: func(client *http.Client, sleep func(time.Duration)) ([]int, error) {
				return CallTEIRanker(TEIRankerInput{
					BaseURL:    "http://ranker.test",
					HTTPClient: client,
					Sleep:      sleep,
				})
			},
		},
	} {
		t.Run(tt.name+"/read", func(t *testing.T) {
			readErr := errors.New("read unavailable")
			client := rankerErrorClient(func() io.ReadCloser {
				return &rankerErrorBody{readErr: readErr}
			})
			order, err := tt.call(client, func(time.Duration) {})
			if err == nil || err.Error() != RerankReadResponseError(tt.providerName, readErr).Error() {
				t.Fatalf("expected read response error, got order=%v err=%v", order, err)
			}
			if !errors.Is(err, readErr) {
				t.Fatalf("read response error does not wrap body error: %v", err)
			}
		})
		t.Run(tt.name+"/close", func(t *testing.T) {
			closeErr := errors.New("close unavailable")
			client := rankerErrorClient(func() io.ReadCloser {
				return &rankerErrorBody{
					data:     []byte(`{"results":[]}`),
					closeErr: closeErr,
				}
			})
			order, err := tt.call(client, func(time.Duration) {})
			if err == nil || err.Error() != RerankCloseResponseError(tt.providerName, closeErr).Error() {
				t.Fatalf("expected close response error, got order=%v err=%v", order, err)
			}
			if !errors.Is(err, closeErr) {
				t.Fatalf("close response error does not wrap body error: %v", err)
			}
		})
	}
}

func TestProviderRankersFormatStatusErrors(t *testing.T) {
	longBody := strings.Repeat("x", 600)
	for _, tt := range []struct {
		name         string
		providerName string
		call         func(*http.Client, func(time.Duration)) ([]int, error)
	}{
		{
			name:         "chat",
			providerName: "openai",
			call: func(client *http.Client, sleep func(time.Duration)) ([]int, error) {
				return CallChatCompletionRanker(ChatCompletionRankerInput{
					ProviderName: "openai",
					BaseURL:      "http://ranker.test",
					HTTPClient:   client,
					Sleep:        sleep,
				})
			},
		},
		{
			name:         "cohere",
			providerName: "cohere",
			call: func(client *http.Client, sleep func(time.Duration)) ([]int, error) {
				return CallCohereRanker(CohereRankerInput{
					BaseURL:    "http://ranker.test",
					HTTPClient: client,
					Sleep:      sleep,
				})
			},
		},
		{
			name:         "tei",
			providerName: "tei",
			call: func(client *http.Client, sleep func(time.Duration)) ([]int, error) {
				return CallTEIRanker(TEIRankerInput{
					BaseURL:    "http://ranker.test",
					HTTPClient: client,
					Sleep:      sleep,
				})
			},
		},
	} {
		t.Run(tt.name+"/retryable", func(t *testing.T) {
			order, err := tt.call(rankerStatusClient(http.StatusInternalServerError, longBody), func(time.Duration) {})
			if err == nil || err.Error() != RerankStatusError(tt.providerName, http.StatusInternalServerError, longBody, true).Error() {
				t.Fatalf("expected retryable status error, got order=%v err=%v", order, err)
			}
		})
		t.Run(tt.name+"/terminal", func(t *testing.T) {
			order, err := tt.call(rankerStatusClient(http.StatusBadRequest, longBody), func(time.Duration) {})
			if err == nil || err.Error() != RerankStatusError(tt.providerName, http.StatusBadRequest, longBody, false).Error() {
				t.Fatalf("expected terminal status error, got order=%v err=%v", order, err)
			}
		})
	}
}

func TestProviderRankersFormatProviderMessages(t *testing.T) {
	for _, tt := range []struct {
		name         string
		providerName string
		call         func(*http.Client) ([]int, error)
		body         string
	}{
		{
			name:         "chat",
			providerName: "openai",
			call: func(client *http.Client) ([]int, error) {
				return CallChatCompletionRanker(ChatCompletionRankerInput{
					ProviderName: "openai",
					BaseURL:      "http://ranker.test",
					HTTPClient:   client,
					Sleep:        func(time.Duration) {},
				})
			},
			body: `{"error":{"message":"bad key"}}`,
		},
		{
			name:         "cohere",
			providerName: "cohere",
			call: func(client *http.Client) ([]int, error) {
				return CallCohereRanker(CohereRankerInput{
					BaseURL:    "http://ranker.test",
					HTTPClient: client,
					Sleep:      func(time.Duration) {},
				})
			},
			body: `{"message":"rate limited"}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			order, err := tt.call(rankerStatusClient(http.StatusOK, tt.body))
			wantMessage := "bad key"
			if tt.name == "cohere" {
				wantMessage = "rate limited"
			}
			if err == nil || err.Error() != RerankProviderMessageError(tt.providerName, wantMessage).Error() {
				t.Fatalf("expected provider-message error, got order=%v err=%v", order, err)
			}
			if order != nil {
				t.Fatalf("order = %v, want nil", order)
			}
		})
	}
}

func rankerErrorClient(body func() io.ReadCloser) *http.Client {
	return &http.Client{
		Transport: rankerRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       body(),
			}, nil
		}),
	}
}

func rankerStatusClient(status int, body string) *http.Client {
	return &http.Client{
		Transport: rankerRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}
}

func TestCallChatCompletionRankerPostsAndRecordsUsage(t *testing.T) {
	var gotReq struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		ResponseFormat *struct {
			Type string `json:"type"`
		} `json:"response_format,omitempty"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q, want bearer key", got)
		}
		if got := r.Header.Get("User-Agent"); got != "docs-test" {
			t.Fatalf("User-Agent = %q, want docs-test", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		written, err := w.Write([]byte(`{"choices":[{"message":{"content":"{\"order\":[2,0,1]}"}}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`))
		if err != nil {
			t.Fatalf("write response: %v", err)
		}
		if written == 0 {
			t.Fatal("write response wrote 0 bytes")
		}
	}))
	defer server.Close()

	var usage LLMRankUsage
	order, err := CallChatCompletionRanker(ChatCompletionRankerInput{
		ProviderName: "openai",
		BaseURL:      server.URL,
		Model:        "gpt-test",
		APIKey:       "test-key",
		Prompt:       "rank these",
		UserAgent:    "docs-test",
		HTTPClient:   server.Client(),
		Sleep: func(d time.Duration) {
			t.Fatalf("unexpected retry sleep: %s", d)
		},
		RecordUsage: func(u LLMRankUsage) {
			usage = u
		},
	})
	if err != nil {
		t.Fatalf("CallChatCompletionRanker returned error: %v", err)
	}
	if len(order) != 3 || order[0] != 2 || order[1] != 0 || order[2] != 1 {
		t.Fatalf("order = %v, want [2 0 1]", order)
	}
	if gotReq.Model != "gpt-test" || len(gotReq.Messages) != 2 || gotReq.Messages[1].Content != "rank these" {
		t.Fatalf("request = %+v, want model and system/user messages", gotReq)
	}
	if gotReq.ResponseFormat == nil || gotReq.ResponseFormat.Type != "json_object" {
		t.Fatalf("response_format = %+v, want json_object", gotReq.ResponseFormat)
	}
	if !usage.Present || usage.PromptTokens != 7 || usage.CompletionTokens != 2 {
		t.Fatalf("usage = %+v, want prompt=7 completion=2", usage)
	}
}

func TestCallChatCompletionRankerRetriesTransientStatus(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, "try again", http.StatusInternalServerError)
			return
		}
		written, err := w.Write([]byte(`{"choices":[{"message":{"content":"{\"order\":[0]}"}}]}`))
		if err != nil {
			t.Fatalf("write response: %v", err)
		}
		if written == 0 {
			t.Fatal("write response wrote 0 bytes")
		}
	}))
	defer server.Close()

	var slept []time.Duration
	order, err := CallChatCompletionRanker(ChatCompletionRankerInput{
		ProviderName: "openai",
		BaseURL:      server.URL,
		Model:        "gpt-test",
		APIKey:       "test-key",
		Prompt:       "rank one",
		UserAgent:    "docs-test",
		HTTPClient:   server.Client(),
		Sleep: func(d time.Duration) {
			slept = append(slept, d)
		},
	})
	if err != nil {
		t.Fatalf("CallChatCompletionRanker returned error: %v", err)
	}
	if len(order) != 1 || order[0] != 0 {
		t.Fatalf("order = %v, want [0]", order)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want retry once", attempts)
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Fatalf("sleep calls = %v, want [2s]", slept)
	}
}

func TestCallCohereRankerPostsAndRecordsUsage(t *testing.T) {
	var gotReq struct {
		Model     string   `json:"model"`
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
		TopN      int      `json:"top_n,omitempty"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Fatalf("path = %q, want /rerank", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cohere-key" {
			t.Fatalf("Authorization = %q, want bearer key", got)
		}
		if got := r.Header.Get("User-Agent"); got != "docs-test" {
			t.Fatalf("User-Agent = %q, want docs-test", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if err := json.NewEncoder(w).Encode(struct {
			Results []struct {
				Index int `json:"index"`
			} `json:"results"`
		}{
			Results: []struct {
				Index int `json:"index"`
			}{
				{Index: 1},
				{Index: 0},
			},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	beforeAttempts := 0
	usageRecorded := false
	order, err := CallCohereRanker(CohereRankerInput{
		BaseURL:    server.URL,
		Model:      "rerank-test",
		APIKey:     "cohere-key",
		Query:      "rank docs",
		UserAgent:  "docs-test",
		HTTPClient: server.Client(),
		Candidates: []LLMRerankCandidate{
			{Index: 0, Title: "Alpha", Snippet: "first doc"},
			{Index: 1, Title: "Bravo"},
		},
		Sleep: func(d time.Duration) {
			t.Fatalf("unexpected retry sleep: %s", d)
		},
		BeforeAttempt: func() {
			beforeAttempts++
		},
		RecordUsage: func() {
			usageRecorded = true
		},
	})
	if err != nil {
		t.Fatalf("CallCohereRanker returned error: %v", err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 0 {
		t.Fatalf("order = %v, want [1 0]", order)
	}
	if gotReq.Model != "rerank-test" || gotReq.Query != "rank docs" || gotReq.TopN != 2 {
		t.Fatalf("request = %+v, want model/query/top_n", gotReq)
	}
	if len(gotReq.Documents) != 2 || gotReq.Documents[0] != "Alpha\nfirst doc" || gotReq.Documents[1] != "Bravo" {
		t.Fatalf("documents = %#v, want title+snippet request text", gotReq.Documents)
	}
	if beforeAttempts != 1 {
		t.Fatalf("beforeAttempts = %d, want 1", beforeAttempts)
	}
	if !usageRecorded {
		t.Fatal("usage hook was not called")
	}
}

func TestCallCohereRankerRetriesRateLimitWithRetryAfter(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "3")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		if err := json.NewEncoder(w).Encode(struct {
			Results []struct {
				Index int `json:"index"`
			} `json:"results"`
		}{
			Results: []struct {
				Index int `json:"index"`
			}{{Index: 0}},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	var slept []time.Duration
	order, err := CallCohereRanker(CohereRankerInput{
		BaseURL:    server.URL,
		Model:      "rerank-test",
		APIKey:     "cohere-key",
		Query:      "rank docs",
		UserAgent:  "docs-test",
		HTTPClient: server.Client(),
		Candidates: []LLMRerankCandidate{{Index: 0, Title: "Alpha"}},
		Sleep: func(d time.Duration) {
			slept = append(slept, d)
		},
		RetryAfter: func(h http.Header) time.Duration {
			if h.Get("Retry-After") != "3" {
				t.Fatalf("Retry-After = %q, want 3", h.Get("Retry-After"))
			}
			return 3 * time.Second
		},
	})
	if err != nil {
		t.Fatalf("CallCohereRanker returned error: %v", err)
	}
	if len(order) != 1 || order[0] != 0 {
		t.Fatalf("order = %v, want [0]", order)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want retry once", attempts)
	}
	if len(slept) != 2 || slept[0] != 3*time.Second || slept[1] != 2*time.Second {
		t.Fatalf("sleep calls = %v, want [3s 2s]", slept)
	}
}

func TestCallCohereRankerReturnsRateLimitRetryError(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "3")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer server.Close()

	order, err := CallCohereRanker(CohereRankerInput{
		BaseURL:    server.URL,
		Model:      "rerank-test",
		APIKey:     "cohere-key",
		Query:      "rank docs",
		UserAgent:  "docs-test",
		HTTPClient: server.Client(),
		Candidates: []LLMRerankCandidate{{Index: 0, Title: "Alpha"}},
		Sleep:      func(time.Duration) {},
		RetryAfter: func(h http.Header) time.Duration {
			if h.Get("Retry-After") != "3" {
				t.Fatalf("Retry-After = %q, want 3", h.Get("Retry-After"))
			}
			return 3 * time.Second
		},
	})
	if err == nil || err.Error() != CohereRateLimitRetryError(3*time.Second).Error() {
		t.Fatalf("CallCohereRanker error = %v, want %v", err, CohereRateLimitRetryError(3*time.Second))
	}
	if order != nil {
		t.Fatalf("order = %v, want nil", order)
	}
	if attempts != 5 {
		t.Fatalf("attempts = %d, want 5", attempts)
	}
}

func TestCallTEIRankerPostsAndRecordsUsage(t *testing.T) {
	var gotReq struct {
		Query     string   `json:"query"`
		Texts     []string `json:"texts"`
		RawScores bool     `json:"raw_scores,omitempty"`
		Truncate  bool     `json:"truncate,omitempty"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Fatalf("path = %q, want /rerank", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want empty", got)
		}
		if got := r.Header.Get("User-Agent"); got != "docs-test" {
			t.Fatalf("User-Agent = %q, want docs-test", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if err := json.NewEncoder(w).Encode([]struct {
			Index int `json:"index"`
		}{
			{Index: 1},
			{Index: 0},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	usageDocs := 0
	order, err := CallTEIRanker(TEIRankerInput{
		BaseURL:      server.URL,
		DefaultModel: "reranker-test",
		Query:        "rank docs",
		UserAgent:    "docs-test",
		HTTPClient:   server.Client(),
		Candidates: []LLMRerankCandidate{
			{Index: 0, Title: "Alpha", Snippet: "first doc"},
			{Index: 1, Title: "Bravo"},
		},
		Sleep: func(d time.Duration) {
			t.Fatalf("unexpected retry sleep: %s", d)
		},
		RecordUsage: func(documentCount int) {
			usageDocs = documentCount
		},
	})
	if err != nil {
		t.Fatalf("CallTEIRanker returned error: %v", err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 0 {
		t.Fatalf("order = %v, want [1 0]", order)
	}
	if gotReq.Query != "rank docs" || !gotReq.Truncate {
		t.Fatalf("request = %+v, want query and truncate=true", gotReq)
	}
	if len(gotReq.Texts) != 2 || gotReq.Texts[0] != "Alpha\nfirst doc" || gotReq.Texts[1] != "Bravo" {
		t.Fatalf("texts = %#v, want title+snippet request text", gotReq.Texts)
	}
	if usageDocs != 2 {
		t.Fatalf("usageDocs = %d, want 2", usageDocs)
	}
}

func TestCallTEIRankerRetriesTransientStatus(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		if err := json.NewEncoder(w).Encode([]struct {
			Index int `json:"index"`
		}{{Index: 0}}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	var slept []time.Duration
	order, err := CallTEIRanker(TEIRankerInput{
		BaseURL:      server.URL,
		DefaultModel: "reranker-test",
		Query:        "rank docs",
		UserAgent:    "docs-test",
		HTTPClient:   server.Client(),
		Candidates:   []LLMRerankCandidate{{Index: 0, Title: "Alpha"}},
		Sleep: func(d time.Duration) {
			slept = append(slept, d)
		},
	})
	if err != nil {
		t.Fatalf("CallTEIRanker returned error: %v", err)
	}
	if len(order) != 1 || order[0] != 0 {
		t.Fatalf("order = %v, want [0]", order)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want retry once", attempts)
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Fatalf("sleep calls = %v, want [2s]", slept)
	}
}

func TestCallTEIRankerReturnsNotReachableError(t *testing.T) {
	transportErr := errors.New("connection refused")
	attempts := 0
	client := &http.Client{
		Transport: rankerRoundTrip(func(*http.Request) (*http.Response, error) {
			attempts++
			return nil, transportErr
		}),
	}

	order, err := CallTEIRanker(TEIRankerInput{
		BaseURL:      "http://127.0.0.1:8080",
		DefaultModel: "reranker-test",
		Query:        "rank docs",
		UserAgent:    "docs-test",
		HTTPClient:   client,
		Candidates:   []LLMRerankCandidate{{Index: 0, Title: "Alpha"}},
		Sleep:        func(time.Duration) {},
	})
	wantPrefix := "tei not reachable at http://127.0.0.1:8080 (start it with: text-embeddings-router --model-id reranker-test --port 8080): "
	if err == nil || !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Fatalf("CallTEIRanker error = %v, want prefix %q", err, wantPrefix)
	}
	if !errors.Is(err, transportErr) {
		t.Fatalf("not-reachable error does not wrap transport error: %v", err)
	}
	if order != nil {
		t.Fatalf("order = %v, want nil", order)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestParseCohereRankResponse(t *testing.T) {
	order, providerMessage, err := ParseCohereRankResponse([]byte(`{"results":[{"index":2,"relevance_score":0.9},{"index":0,"relevance_score":0.2}]}`))
	if err != nil {
		t.Fatalf("ParseCohereRankResponse returned error: %v", err)
	}
	if providerMessage != "" {
		t.Fatalf("provider message = %q, want empty", providerMessage)
	}
	if len(order) != 2 || order[0] != 2 || order[1] != 0 {
		t.Fatalf("order = %v, want [2 0]", order)
	}

	order, providerMessage, err = ParseCohereRankResponse([]byte(`{"message":"rate limited"}`))
	if err != nil {
		t.Fatalf("cohere provider message should not be a parse error: %v", err)
	}
	if len(order) != 0 {
		t.Fatalf("order = %v, want empty when provider returns message", order)
	}
	if providerMessage != "rate limited" {
		t.Fatalf("provider message = %q, want rate limited", providerMessage)
	}
}

func TestParseTEIRankResponse(t *testing.T) {
	order, err := ParseTEIRankResponse([]byte(`[{"index":3,"score":0.8},{"index":1,"score":0.4}]`))
	if err != nil {
		t.Fatalf("ParseTEIRankResponse returned error: %v", err)
	}
	if len(order) != 2 || order[0] != 3 || order[1] != 1 {
		t.Fatalf("order = %v, want [3 1]", order)
	}

	empty, err := ParseTEIRankResponse([]byte(`[]`))
	if err == nil {
		t.Fatalf("ParseTEIRankResponse should reject empty results, got %v", empty)
	}
}

func TestParseTEIRankResponseWrapsBodyPreviewParseError(t *testing.T) {
	raw := []byte("[" + strings.Repeat("x", 240))
	order, err := ParseTEIRankResponse(raw)
	if err == nil || !strings.HasPrefix(err.Error(), "parse response: ") {
		t.Fatalf("parse error = %v, want parse response wrapper", err)
	}
	if !strings.Contains(err.Error(), "(body: "+truncateForError(string(raw), 200)+")") {
		t.Fatalf("parse error = %v, want capped body preview", err)
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("parse error does not wrap JSON syntax error: %v", err)
	}
	if order != nil {
		t.Fatalf("order = %v, want nil", order)
	}
}

func TestOrderRerankCandidatesSortsByScoreAndBaseRank(t *testing.T) {
	got := OrderRerankCandidates([]RerankOrderCandidate{
		{Hit: Hit{Path: "third.md"}, Score: 0.10, BaseRank: 3},
		{Hit: Hit{Path: "first.md"}, Score: 0.30, BaseRank: 2},
		{Hit: Hit{Path: "second.md"}, Score: 0.30, BaseRank: 1},
	})
	if len(got) != 3 {
		t.Fatalf("hits = %+v, want 3", got)
	}
	if got[0].Path != "second.md" || got[1].Path != "first.md" || got[2].Path != "third.md" {
		t.Fatalf("ordered hits = %+v, want score desc with base-rank tie-break", got)
	}
}

func TestFilterByVersionPolicy(t *testing.T) {
	hits := []Hit{
		{Path: "first.md", Source: "docs", Score: 10},
		{Path: "second.md", Source: "docs", Score: 20},
		{Path: "third.md", Source: "docs", Score: 30},
	}
	got := FilterByVersionPolicy(hits, VersionPolicyOptions{Active: true, Version: "1.0"}, func(hit Hit) (Hit, bool) {
		if hit.Path == "second.md" {
			return hit, false
		}
		hit.SourceID = "docs__v1"
		return hit, true
	})
	if len(got) != 2 {
		t.Fatalf("hits = %+v, want two matching hits", got)
	}
	if got[0].Path != "first.md" || got[1].Path != "third.md" {
		t.Fatalf("hit order = %+v, want original order of survivors", got)
	}
	if got[0].SourceID != "docs__v1" || got[1].SourceID != "docs__v1" {
		t.Fatalf("version metadata not preserved from matcher: %+v", got)
	}
}

func TestFilterByVersionPolicyNoopsForSoftPolicy(t *testing.T) {
	hits := []Hit{{Path: "a.md"}, {Path: "b.md"}}
	called := false
	got := FilterByVersionPolicy(hits, VersionPolicyOptions{Active: true}, func(hit Hit) (Hit, bool) {
		called = true
		return hit, false
	})
	if called {
		t.Fatal("matcher should not run for soft version policy")
	}
	if len(got) != 2 || got[0].Path != "a.md" || got[1].Path != "b.md" {
		t.Fatalf("hits = %+v, want unchanged soft-policy hits", got)
	}
}

func TestRunPostRetrievalProcessingAppliesRuntimeProfileAndSourceHygiene(t *testing.T) {
	got := RunPostRetrievalProcessing(PostRetrievalProcessing{
		Hits: []Hit{
			{Path: "docs/generated.md", Source: "docs", Score: 90},
			{Path: "docs/guide.md", Source: "docs", Score: 80},
			{Path: "other/guide.md", Source: "other", Score: 70},
		},
		Mode: "fts5",
		Options: PostRetrievalOptions{
			ProfileStrict: true,
			UserLimit:     1,
		},
		Profile: profileMatcherFunc(func(source, relPath string) (bool, bool) {
			return source == "docs", relPath == "guide.md"
		}),
		SourceHygienePenalty: func(hit Hit) int {
			if strings.Contains(hit.Path, "generated") {
				return 10
			}
			return 0
		},
	})
	if got.Mode != "fts5" {
		t.Fatalf("mode = %q, want fts5", got.Mode)
	}
	if len(got.Hits) != 1 {
		t.Fatalf("hits = %+v, want one strict/profile/hygiene survivor", got.Hits)
	}
	if got.Hits[0].Path != "docs/guide.md" || !got.Hits[0].InProfile || !got.Hits[0].InProfileSub {
		t.Fatalf("unexpected survivor: %+v", got.Hits[0])
	}
}

func TestRunPostRetrievalProcessingSkipsRerankWhenBM25Confident(t *testing.T) {
	called := false
	got := RunPostRetrievalProcessing(PostRetrievalProcessing{
		Hits: []Hit{
			{Path: "top.md", Score: 100},
			{Path: "second.md", Score: 80},
		},
		Mode: "fts5",
		Options: PostRetrievalOptions{
			Rerank:     true,
			RerankGate: 0.10,
		},
		EmbeddingRerank: func([]Hit) ([]Hit, error) {
			called = true
			return nil, errors.New("rerank should be gated")
		},
	})
	if called {
		t.Fatal("rerank should not run when BM25 is confident")
	}
	if got.Mode != "fts5" {
		t.Fatalf("mode = %q, want fts5", got.Mode)
	}
	if len(got.Hits) != 2 || got.Hits[0].Path != "top.md" {
		t.Fatalf("unexpected hits: %+v", got.Hits)
	}
}

func TestParseLLMRankOrder(t *testing.T) {
	got, err := ParseLLMRankOrder(`{"order":[2,0,1]}`)
	if err != nil {
		t.Fatalf("ParseLLMRankOrder returned error: %v", err)
	}
	if len(got) != 3 || got[0] != 2 || got[1] != 0 || got[2] != 1 {
		t.Fatalf("order = %v, want [2 0 1]", got)
	}

	bad, err := ParseLLMRankOrder(`not json`)
	if err == nil {
		t.Fatalf("ParseLLMRankOrder should reject malformed JSON, got %v", bad)
	}
}

func TestApplyLLMRankOrder(t *testing.T) {
	mk := func(paths ...string) []Hit {
		hits := make([]Hit, len(paths))
		for i, path := range paths {
			hits[i] = Hit{Path: path}
		}
		return hits
	}
	cases := []struct {
		name  string
		hits  []Hit
		order []int
		want  []string
	}{
		{"empty order returns input", mk("a", "b", "c"), nil, []string{"a", "b", "c"}},
		{"full reorder", mk("a", "b", "c"), []int{2, 0, 1}, []string{"c", "a", "b"}},
		{"partial order keeps tail in BM25 order", mk("a", "b", "c"), []int{2}, []string{"c", "a", "b"}},
		{"out-of-bounds indices ignored", mk("a", "b", "c"), []int{99, 1, -1, 0}, []string{"b", "a", "c"}},
		{"duplicate indices dedup", mk("a", "b", "c"), []int{1, 1, 1, 0}, []string{"b", "a", "c"}},
	}
	for _, c := range cases {
		got := ApplyLLMRankOrder(c.hits, c.order)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %d hits, want %d", c.name, len(got), len(c.want))
			continue
		}
		for i := range got {
			if got[i].Path != c.want[i] {
				t.Errorf("%s: rank %d = %q, want %q", c.name, i, got[i].Path, c.want[i])
			}
		}
	}
}

func TestRelPathInSource(t *testing.T) {
	cases := []struct {
		path, source, want string
	}{
		{"supabase/guides/auth.md", "supabase", "guides/auth.md"},
		{"microsoft-learn/azure/cli/login.md", "microsoft-learn", "azure/cli/login.md"},
		{"already/relative.md", "wrong-source", "already/relative.md"},
	}
	for _, c := range cases {
		if got := RelPathInSource(c.path, c.source); got != c.want {
			t.Errorf("RelPathInSource(%q,%q)=%q, want %q", c.path, c.source, got, c.want)
		}
	}
}

func TestHitsToCoreDropsRuntimeOnlyFields(t *testing.T) {
	got := HitsToCore([]Hit{{
		Path:                "demo/guide.md",
		Source:              "demo",
		SourceFamily:        "demo-family",
		SourceID:            "demo__v1",
		DocsVersion:         "1",
		VersionLane:         "workspace",
		PinScope:            "platform",
		Title:               "Guide",
		URL:                 "https://example.com",
		Score:               42,
		InProfile:           true,
		InProfileSub:        true,
		VersionScoreApplied: true,
		Snippets:            []Snippet{{Line: 7, Text: "needle"}},
	}})
	if len(got) != 1 {
		t.Fatalf("got %d hits, want 1", len(got))
	}
	if got[0].Path != "demo/guide.md" || got[0].Source != "demo" || got[0].SourceFamily != "demo-family" || got[0].SourceID != "demo__v1" {
		t.Fatalf("unexpected identity fields: %+v", got[0])
	}
	if got[0].DocsVersion != "1" || got[0].Title != "Guide" || got[0].URL != "https://example.com" || got[0].Score != 42 {
		t.Fatalf("unexpected public fields: %+v", got[0])
	}
	if len(got[0].Snippets) != 1 || got[0].Snippets[0].Line != 7 || got[0].Snippets[0].Text != "needle" {
		t.Fatalf("unexpected snippets: %+v", got[0].Snippets)
	}
}
