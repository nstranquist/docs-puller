package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nstranquist/docs-puller/internal/sourcehygiene"
	"github.com/nstranquist/docs-puller/searchruntime"
)

func writeDoc(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSearchScoresAndRanks(t *testing.T) {
	out := t.TempDir()
	srcA := filepath.Join(out, "a")
	srcB := filepath.Join(out, "b")

	// 3 hits in body, no title match. Score = 3.
	writeDoc(t, srcA, "doc1.md",
		"# Other Title\n\nfoo appears once. foo again. and foo.\n")
	// 1 body hit + title match. Score = title boost (5) + body hit on title
	// line (1) + body hit on real body line (1) = 7.
	writeDoc(t, srcA, "doc2.md",
		"# About foo\n\nThis page mentions foo once.\n")
	// no hits.
	writeDoc(t, srcB, "doc3.md", "# Unrelated\n\nnothing relevant\n")
	// _INDEX.md must be ignored even if it contains the query.
	writeDoc(t, srcA, "_INDEX.md", "# Index\n\nfoo foo foo\n")

	hits, scanned := runSearch("foo", searchOpts{out: out, limit: 10})

	if scanned != 3 {
		t.Errorf("scanned = %d, want 3 (excluding _INDEX.md)", scanned)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if !filepath.HasPrefix(hits[0].Path, "a/doc2") {
		t.Errorf("rank 0 path = %q, want a/doc2.md", hits[0].Path)
	}
	if hits[0].Score != 7 {
		t.Errorf("rank 0 score = %d, want 7 (title boost 5 + 2 body hits)", hits[0].Score)
	}
	if hits[1].Score != 3 {
		t.Errorf("rank 1 score = %d, want 3", hits[1].Score)
	}
	if hits[0].Title != "About foo" {
		t.Errorf("title = %q, want About foo", hits[0].Title)
	}
}

func TestSearchFrontmatterTitle(t *testing.T) {
	// Source-mode pulls (e.g. Supabase MDX) have YAML frontmatter, no body H1.
	out := t.TempDir()
	body := "---\ntitle: 'Row Level Security'\nid: 'rls'\n---\n\nbody about rls\n"
	writeDoc(t, filepath.Join(out, "supabase"), "guides/rls.md", body)

	hits, _ := runSearch("rls", searchOpts{out: out, limit: 10})
	if len(hits) != 1 {
		t.Fatalf("got %d hits", len(hits))
	}
	if hits[0].Title != "Row Level Security" {
		t.Errorf("title = %q, want Row Level Security", hits[0].Title)
	}
}

func TestSplitSearchArgs(t *testing.T) {
	cases := []struct {
		in    []string
		flags []string
		query []string
	}{
		{[]string{"foo", "--limit", "3", "--json"}, []string{"--limit", "3", "--json"}, []string{"foo"}},
		{[]string{"--limit", "3", "foo", "bar"}, []string{"--limit", "3"}, []string{"foo", "bar"}},
		{[]string{"--json", "row level", "security"}, []string{"--json"}, []string{"row level", "security"}},
		{[]string{"foo"}, nil, []string{"foo"}},
	}
	for _, c := range cases {
		gotF, gotQ := splitSearchArgs(c.in)
		if !equalSlices(gotF, c.flags) || !equalSlices(gotQ, c.query) {
			t.Errorf("splitSearchArgs(%v) = (%v, %v), want (%v, %v)",
				c.in, gotF, gotQ, c.flags, c.query)
		}
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSearchSourceFilter(t *testing.T) {
	out := t.TempDir()
	writeDoc(t, filepath.Join(out, "a"), "doc.md", "# x\n\nfoo\n")
	writeDoc(t, filepath.Join(out, "b"), "doc.md", "# x\n\nfoo\n")

	hits, _ := runSearch("foo", searchOpts{out: out, source: "a", limit: 10})
	if len(hits) != 1 || hits[0].Source != "a" {
		t.Errorf("source filter failed: %+v", hits)
	}
}

func TestRunDispatchSearchMatchesLegacyTupleWrapper(t *testing.T) {
	out := t.TempDir()
	writeDoc(t, filepath.Join(out, "demo"), "doc.md", "# Demo\n\nneedle appears here\n")
	opts := searchOpts{out: out, limit: 5, useScan: true}

	legacyHits, legacyScanned, legacyMode := dispatchSearch("needle", opts, nil)
	result := runDispatchSearch(dispatchSearchRequest{Query: "needle", Opts: opts})

	if result.Mode != legacyMode || result.Scanned != legacyScanned {
		t.Fatalf("structured result = mode %q scanned %d, legacy = mode %q scanned %d", result.Mode, result.Scanned, legacyMode, legacyScanned)
	}
	if len(result.Hits) != len(legacyHits) {
		t.Fatalf("structured hits = %d, legacy hits = %d", len(result.Hits), len(legacyHits))
	}
	if len(result.Hits) == 0 || result.Hits[0].Path != legacyHits[0].Path {
		t.Fatalf("structured hits = %+v, legacy hits = %+v", result.Hits, legacyHits)
	}
}

func TestSearchSnippets(t *testing.T) {
	out := t.TempDir()
	body := "# Page\n\nfirst line foo here\nsecond foo line\nthird foo too\nfourth foo, ignored\n"
	writeDoc(t, filepath.Join(out, "a"), "doc.md", body)

	hits, _ := runSearch("foo", searchOpts{out: out, limit: 10})
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if len(hits[0].Snippets) != 3 {
		t.Errorf("got %d snippets, want 3 (capped)", len(hits[0].Snippets))
	}
	if hits[0].Snippets[0].Line != 3 {
		t.Errorf("first snippet line = %d, want 3", hits[0].Snippets[0].Line)
	}
}

func TestRunSearchPassesSnippetOverridesToScanEngine(t *testing.T) {
	out := t.TempDir()
	body := "# Page\n\nfirst line foo appears in a deliberately long sentence\nsecond foo line with more text\n"
	writeDoc(t, filepath.Join(out, "a"), "doc.md", body)

	hits, _ := runSearch("foo", searchOpts{out: out, limit: 10, maxSnippets: 1, snippetLen: 24})
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if got := len(hits[0].Snippets); got != 1 {
		t.Fatalf("snippet count = %d, want 1: %+v", got, hits[0].Snippets)
	}
	if got := len(hits[0].Snippets[0].Text); got > 24 {
		t.Fatalf("snippet length = %d, want <= 24: %q", got, hits[0].Snippets[0].Text)
	}
}

func TestSearchURLFromManifest(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "x")
	writeDoc(t, src, "guides/foo.md", "# Foo\n\nfoo body\n")
	m := newManifest()
	m.Entries["https://example.com/foo"] = result{
		URL: "https://example.com/foo", Source: "x",
		Path: "x/guides/foo.md", FetchedAt: "now",
	}
	if err := writeManifestAtomic(src, m); err != nil {
		t.Fatal(err)
	}

	hits, _ := runSearch("foo", searchOpts{out: out, limit: 10})
	if len(hits) != 1 {
		t.Fatalf("got %d hits", len(hits))
	}
	if hits[0].URL != "https://example.com/foo" {
		t.Errorf("URL = %q, want from manifest", hits[0].URL)
	}
}

func TestDedupeSnippetsAcross(t *testing.T) {
	in := []searchHit{
		{Path: "a", Snippets: []searchSnippet{{Line: 1, Text: "shared line"}, {Line: 2, Text: "unique to a"}}},
		{Path: "b", Snippets: []searchSnippet{{Line: 5, Text: "shared line"}, {Line: 6, Text: "unique to b"}}},
		{Path: "c", Snippets: []searchSnippet{{Line: 9, Text: "shared line"}, {Line: 10, Text: "unique to a"}}},
		{Path: "d", Snippets: []searchSnippet{{Line: 1, Text: "  shared line  "}}},
	}
	got := searchruntime.DedupeSnippetsAcross(in)
	if len(got[0].Snippets) != 2 {
		t.Errorf("hit 0: snippets = %d, want 2 (both unique on first sight)", len(got[0].Snippets))
	}
	if len(got[1].Snippets) != 1 || got[1].Snippets[0].Text != "unique to b" {
		t.Errorf("hit 1: snippets = %v, want only [unique to b]", got[1].Snippets)
	}
	// "unique to a" first appeared in hit 0, so hit 2 sees both its snippets
	// dedupe away even though line numbers differ.
	if len(got[2].Snippets) != 0 {
		t.Errorf("hit 2: snippets = %v, want empty (all dups)", got[2].Snippets)
	}
	// Whitespace-only differences must dedupe (we trim before comparing).
	if len(got[3].Snippets) != 0 {
		t.Errorf("hit 3: snippets = %v, want empty (whitespace-trimmed dup)", got[3].Snippets)
	}
}

func TestEmitSearchTextDedupeSnippetsAcrossRows(t *testing.T) {
	hits := []searchHit{
		{
			Path:  "supabase/reference/cli/supabase-db-dump.md",
			Title: "supabase db dump",
			URL:   "https://example.com/dump",
			Score: 345,
			Snippets: []searchSnippet{
				{Line: 11, Text: "shared cli overview"},
				{Line: 950, Text: "supabase db dump"},
			},
		},
		{
			Path:  "supabase/reference/cli/supabase-db-diff.md",
			Title: "supabase db diff",
			URL:   "https://example.com/diff",
			Score: 334,
			Snippets: []searchSnippet{
				{Line: 11, Text: "shared cli overview"},
				{Line: 950, Text: "supabase db dump"},
			},
		},
	}

	out := captureStdout(t, func() {
		emitSearchText("supabase db dump", 2, hits, "fts5", searchOpts{})
	})

	for _, want := range []string{"supabase-db-dump.md", "supabase-db-diff.md", "supabase db diff"} {
		if !strings.Contains(out, want) {
			t.Fatalf("emitted text missing %q:\n%s", want, out)
		}
	}
	if got := strings.Count(out, "shared cli overview"); got != 1 {
		t.Fatalf("shared snippet count = %d, want 1:\n%s", got, out)
	}
	if got := strings.Count(out, "L950: supabase db dump"); got != 1 {
		t.Fatalf("duplicate command snippet count = %d, want 1:\n%s", got, out)
	}
}

func TestEmitSearchTextKeepsRowsWithEmptySnippets(t *testing.T) {
	hits := []searchHit{
		{Path: "supabase/reference/cli/supabase-db-diff.md", Title: "supabase db diff", URL: "https://example.com/diff", Score: 334},
	}

	out := captureStdout(t, func() {
		emitSearchText("supabase db dump", 1, hits, "fts5", searchOpts{})
	})

	for _, want := range []string{"supabase-db-diff.md", "supabase db diff", "https://example.com/diff"} {
		if !strings.Contains(out, want) {
			t.Fatalf("emitted text missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "L0:") {
		t.Fatalf("empty snippet row should not print fake snippet lines:\n%s", out)
	}
}

func TestEmitSearchTextProfileBadges(t *testing.T) {
	hits := []searchHit{
		{Path: "platform/doc.md", Title: "Platform", Score: 20, InProfile: true},
		{Path: "platform/sub/doc.md", Title: "Sub", Score: 19, InProfile: true, InProfileSub: true},
	}

	out := captureStdout(t, func() {
		emitSearchText("platform", 2, hits, "fts5", searchOpts{resolvedProfile: "nicos", profileReason: "out-pin"})
	})

	for _, want := range []string{"profile=nicos (out-pin; boost)", "[profile] platform/doc.md", "[profile*] platform/sub/doc.md"} {
		if !strings.Contains(out, want) {
			t.Fatalf("emitted text missing %q:\n%s", want, out)
		}
	}
}

func TestRunDispatchSearchAppliesCompactOutput(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "demo")
	writeDoc(t, src, "doc.md", "# Demo\n\nalpha beta gamma\nalpha beta delta\n")
	m := newManifest()
	m.Entries["https://example.com/demo"] = result{
		URL: "https://example.com/demo", Source: "demo",
		Path: "demo/doc.md", FetchedAt: "now",
	}
	if err := writeManifestAtomic(src, m); err != nil {
		t.Fatal(err)
	}

	got := runDispatchSearch(dispatchSearchRequest{
		Query: "alpha",
		Opts:  searchOpts{out: out, limit: 5, useScan: true, compact: true, maxSnippets: 1, snippetLen: 80},
	})
	if len(got.Hits) != 1 {
		t.Fatalf("hits = %+v, want one hit", got.Hits)
	}
	if got.Hits[0].URL != "" {
		t.Fatalf("compact URL = %q, want empty", got.Hits[0].URL)
	}
	if len(got.Hits[0].Snippets) != 1 {
		t.Fatalf("compact snippets = %d, want 1: %+v", len(got.Hits[0].Snippets), got.Hits[0].Snippets)
	}
}

func TestRunDispatchSearchCollapsesReactNativeVersionEquivalentStems(t *testing.T) {
	out := writeReactNativeDebuggingFixture(t)

	got := runDispatchSearch(dispatchSearchRequest{
		Query: "react native debugging",
		Opts: searchOpts{
			out:             out,
			source:          "react-native",
			requestedSource: "react-native",
			limit:           5,
		},
	})
	if len(got.Hits) != 5 {
		t.Fatalf("hits = %d, want 5 unique React Native debugging pages: %#v", len(got.Hits), got.Hits)
	}

	stems := map[string]bool{}
	for _, hit := range got.Hits {
		stem, ok := reactNativeVersionEquivalentStem(hit)
		if !ok {
			t.Fatalf("hit missing React Native version stem: %#v", hit)
		}
		if stems[stem] {
			t.Fatalf("duplicate version-equivalent stem %q survived: %#v", stem, got.Hits)
		}
		stems[stem] = true
	}
	for _, want := range []string{
		"react-native/docs/debugging.md",
		"react-native/docs/debugging-native-code.md",
		"react-native/docs/debugging-release-builds.md",
		"react-native/docs/other-debugging-methods.md",
		"react-native/docs/react-native-devtools.md",
	} {
		if !stems[want] {
			t.Fatalf("collapsed top 5 missing %s; stems=%v hits=%#v", want, stems, got.Hits)
		}
	}
}

func TestRunDispatchSearchAllVersionsKeepsReactNativeMirrors(t *testing.T) {
	opts := searchOpts{source: "react-native", requestedSource: "react-native", limit: 5}
	if !shouldCollapseVersionEquivalentStems("react native debugging", opts) {
		t.Fatal("default React Native search should collapse version-equivalent stems")
	}
	opts.allVersions = true
	if shouldCollapseVersionEquivalentStems("react native debugging", opts) {
		t.Fatal("--all-versions should disable version-equivalent stem collapse")
	}
}

func TestReactNativeVersionEquivalentStemPreservesLegacyTopicPath(t *testing.T) {
	canonical := searchHit{Path: "react-native/docs/legacy/native-modules-android.md", Source: "react-native"}
	got, ok := reactNativeVersionEquivalentStem(canonical)
	if !ok {
		t.Fatalf("reactNativeVersionEquivalentStem(%q) did not match React Native", canonical.Path)
	}
	if got != canonical.Path {
		t.Fatalf("canonical legacy topic stem = %q, want %q", got, canonical.Path)
	}

	archived := searchHit{Path: "react-native/docs/0.77/legacy/native-modules-android.md", Source: "react-native"}
	got, ok = reactNativeVersionEquivalentStem(archived)
	if !ok {
		t.Fatalf("reactNativeVersionEquivalentStem(%q) did not match React Native", archived.Path)
	}
	if got != canonical.Path {
		t.Fatalf("archived legacy topic stem = %q, want %q", got, canonical.Path)
	}
}

func TestEmitSearchJSONCompactSuppressesRedundantMetadata(t *testing.T) {
	hits := []searchHit{{
		Path:         "react-native/docs/debugging.md",
		Source:       "react-native",
		SourceFamily: "react-native",
		SourceID:     "react-native",
		DocsVersion:  "0.83",
		VersionLane:  versionLaneLatest,
		Title:        "Debugging Basics · React Native",
		Score:        558,
		InProfile:    true,
		Snippets:     []searchSnippet{{Line: 5, Text: "React Native debugging"}},
	}}

	out := captureStdout(t, func() {
		emitSearchJSON("react native debugging", 10, hits, "fts5", searchOpts{
			compact:         true,
			requestedSource: "react-native",
			resolvedProfile: "nicos",
			profileReason:   "cwd-nicos",
			versionPolicy:   &versionSearchPolicy{cwdScope: "tools-monorepo"},
		})
	})

	for _, notWant := range []string{`"mode"`, `"profile_reason"`, `"profile_mode"`, `"pin_context"`, `"source_family"`, `"source_id"`, `"in_profile"`} {
		if strings.Contains(out, notWant) {
			t.Fatalf("compact JSON leaked %s:\n%s", notWant, out)
		}
	}
	for _, want := range []string{`"docs_version": "0.83"`, `"version_lane": "latest"`, `"snippets"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("compact JSON missing %s:\n%s", want, out)
		}
	}
}

func TestEmitSearchJSONCompactExplainKeepsDebugMetadata(t *testing.T) {
	hits := []searchHit{{
		Path:         "react-native/docs/debugging.md",
		Source:       "react-native",
		SourceFamily: "react-native",
		SourceID:     "react-native",
		DocsVersion:  "0.83",
		VersionLane:  versionLaneLatest,
		Title:        "Debugging Basics · React Native",
		Score:        558,
		InProfile:    true,
	}}

	out := captureStdout(t, func() {
		emitSearchJSON("react native debugging", 10, hits, "fts5", searchOpts{
			compact:         true,
			explain:         true,
			requestedSource: "react-native",
			resolvedProfile: "nicos",
			profileReason:   "cwd-nicos",
			versionPolicy:   &versionSearchPolicy{cwdScope: "tools-monorepo"},
		})
	})

	for _, want := range []string{`"mode": "fts5"`, `"profile_reason": "cwd-nicos"`, `"profile_mode": "boost"`, `"pin_context": "tools-monorepo"`, `"source_family": "react-native"`, `"source_id": "react-native"`, `"in_profile": true`} {
		if !strings.Contains(out, want) {
			t.Fatalf("compact --explain JSON missing %s:\n%s", want, out)
		}
	}
}

func writeReactNativeDebuggingFixture(t *testing.T) string {
	t.Helper()
	out := t.TempDir()
	src := filepath.Join(out, "react-native")
	debuggingBody := "# Debugging Basics · React Native\n\nReact Native debugging debugging debugging. Dev Menu, LogBox, and React Native DevTools help debug JavaScript code.\n"
	for _, dir := range []string{"docs", "docs/0.77", "docs/0.78", "docs/0.79", "docs/0.80", "docs/0.81", "docs/0.82", "docs/0.83", "docs/0.84", "docs/next"} {
		writeFTSDoc(t, src, filepath.Join(dir, "debugging.md"), debuggingBody)
	}
	writeFTSDoc(t, src, "docs/debugging-native-code.md", "# Debugging Native Code · React Native\n\nReact Native debugging for native code in Xcode and Android Studio.\n")
	writeFTSDoc(t, src, "docs/debugging-release-builds.md", "# Debugging Release Builds · React Native\n\nReact Native debugging release builds and production diagnostics.\n")
	writeFTSDoc(t, src, "docs/other-debugging-methods.md", "# Other Debugging Methods · React Native\n\nReact Native debugging methods for JavaScript runtimes and legacy setups.\n")
	writeFTSDoc(t, src, "docs/react-native-devtools.md", "# React Native DevTools · React Native\n\nReact Native debugging with DevTools, console logs, components, and profiler.\n")

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestApplySearchSourceHygieneDownranksGeneratedWhenDurableHitsExist(t *testing.T) {
	hits := []searchHit{
		{Path: "kb/generated/local-ai/human-command-prompts/exports/codex/2026-04-part-14.md", Score: 500},
		{Path: "kb/local-ai/human-command-prompts/ingested/2026-04/replay.md", Score: 490},
		{Path: "kb/repos/ingested/supermemory.md", Score: 120},
		{Path: "anthropic/api/typescript/beta.md", Score: 100},
	}
	got := searchruntime.ApplySourceHygiene(hits, 2, func(hit searchHit) int {
		return sourcehygiene.Penalty(hit.Path, hit.URL)
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

func TestBM25IsConfident(t *testing.T) {
	hits := []searchHit{{Score: 100}, {Score: 80}}
	if !searchruntime.BM25IsConfident(hits, 0.10) {
		t.Fatal("20% top-1 gap should satisfy 10% rerank gate")
	}
	if searchruntime.BM25IsConfident(hits, 0.25) {
		t.Fatal("20% top-1 gap should not satisfy 25% rerank gate")
	}
	if searchruntime.BM25IsConfident(hits, 0) {
		t.Fatal("gate=0 should disable confidence gating")
	}
	if searchruntime.BM25IsConfident(hits[:1], 0.10) {
		t.Fatal("single hit has no top-2 comparison and should not gate")
	}
}

func TestNaturalLanguageSourceKeywords(t *testing.T) {
	goSources := sourcesFromQueryTokens(tokenizeForFTS("how do I use Go modules"))
	if !goSources["go"] {
		t.Fatalf("Go language query should infer go source, got %v", goSources)
	}
	obsidianSources := sourcesFromQueryTokens(tokenizeForFTS("how do I write an Obsidian plugin manifest"))
	if !obsidianSources["obsidian-app-docs"] {
		t.Fatalf("Obsidian plugin query should infer obsidian-app-docs source, got %v", obsidianSources)
	}
	rnSources := sourcesFromQueryTokens(tokenizeForFTS("how do I tune a React Native FlatList"))
	if !rnSources["react-native"] {
		t.Fatalf("React Native phrase should infer react-native source, got %v", rnSources)
	}
	xaiSources := sourcesFromQueryTokens(tokenizeForFTS("how do I call the Grok API"))
	if !xaiSources["xai"] {
		t.Fatalf("Grok query should infer xai source, got %v", xaiSources)
	}
	aiSDKSources := sourcesFromQueryTokens(tokenizeForFTS("how do I stream text with the Vercel AI SDK"))
	if !aiSDKSources["ai-sdk"] {
		t.Fatalf("AI SDK phrase should infer ai-sdk source, got %v", aiSDKSources)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return string(data)
}
