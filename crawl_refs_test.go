package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedRefsCases writes a minimal _cases tree: two real slugs (one with a
// manifest) plus an underscore-prefixed dir that must be ignored.
func seedRefsCases(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, root, "warp/00-manifest.json", `{"source":{"slug":"warp","repo_url":"git@github.com:warpdotdev/warp.git","primary_language":"rust","latest_commit_subject":"Keep maximized state"}}`)
	writeTestFile(t, root, "warp/01-repo-structure.md", "# Warp repo structure\nAmbient agents live here.\n")
	writeTestFile(t, root, "warp/03-code-patterns.md", "# Warp code patterns\nevent-sourced resume pattern.\n")
	writeTestFile(t, root, "warp/08-adoption-assessment.md", "# Adoption\n\n> ⚠ STUB. The agent should fill this in.\n")
	writeTestFile(t, root, "archon/01-repo-structure.md", "# Archon\nDAG workflow engine.\n")
	// must be ignored: underscore-prefixed control dir + a non-md file
	writeTestFile(t, root, "_archive/old/01-repo-structure.md", "# archived\n")
	writeTestFile(t, root, "archon/notes.txt", "not markdown\n")
	return root
}

func TestCollectRefsDissectionDocs(t *testing.T) {
	root := seedRefsCases(t)

	docs, cases, err := collectRefsDissectionDocs(root, "2026-06-08T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatalf("cases = %d, want 2 (%+v)", len(cases), cases)
	}
	// cases sorted: archon, warp
	if cases[0].Slug != "archon" || cases[1].Slug != "warp" {
		t.Fatalf("slugs = %q,%q want archon,warp", cases[0].Slug, cases[1].Slug)
	}

	body := docsByRel(docs)
	for _, rel := range []string{"README.md", "warp/01-repo-structure.md", "warp/03-code-patterns.md", "archon/01-repo-structure.md"} {
		if _, ok := body[rel]; !ok {
			t.Fatalf("missing expected doc %s; have %v", rel, keysOf(body))
		}
	}
	// 00-manifest.json is JSON, never ingested as a doc.
	if _, ok := body["warp/00-manifest.json"]; ok {
		t.Fatal("00-manifest.json must not be ingested as a doc")
	}
	if _, ok := body["warp/08-adoption-assessment.md"]; ok {
		t.Fatal("stub narrative must not be ingested as retrieval evidence")
	}
	// underscore-prefixed slug dir ignored.
	for rel := range body {
		if strings.HasPrefix(rel, "_archive/") {
			t.Fatalf("underscore-prefixed dir leaked into corpus: %s", rel)
		}
	}
	// README surfaces manifest enrichment.
	readme := body["README.md"]
	for _, want := range []string{"warp", "lang `rust`", "Keep maximized state", "Ref Dissection Cases", "refs-dissections"} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README missing %q:\n%s", want, readme)
		}
	}
}

func TestCollectRefsDissectionDocsFailClosed(t *testing.T) {
	// Missing root → error (no destructive replace).
	if _, _, err := collectRefsDissectionDocs(filepath.Join(t.TempDir(), "nope"), "x"); err == nil {
		t.Fatal("missing cases root must fail closed")
	}
	// Present root with zero markdown → error.
	empty := t.TempDir()
	writeTestFile(t, empty, "slug/00-manifest.json", `{}`)
	writeTestFile(t, empty, "slug/readme.txt", "no md here\n")
	if _, _, err := collectRefsDissectionDocs(empty, "x"); err == nil {
		t.Fatal("zero-markdown tree must fail closed (would otherwise wipe a good source)")
	}
	stubOnly := t.TempDir()
	writeTestFile(t, stubOnly, "slug/03-code-patterns.md", "# Patterns\n\n> ⚠ STUB. The agent should fill this in.\n")
	if _, _, err := collectRefsDissectionDocs(stubOnly, "x"); err == nil {
		t.Fatal("stub-only tree must fail closed and preserve the last good source")
	}
}

// TestCrawlRefsReplaceSource exercises the real write path: collect → replace →
// assert the source dir + manifest landed under out.
func TestCrawlRefsReplaceSource(t *testing.T) {
	root := seedRefsCases(t)
	out := t.TempDir()

	docs, _, err := collectRefsDissectionDocs(root, "2026-06-08T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	n, err := replaceCrawlSource(out, "refs-dissections", "refs-crawl", docs, []string{"crawl-refs"})
	if err != nil {
		t.Fatal(err)
	}
	if n < 4 {
		t.Fatalf("copied = %d, want >= 4", n)
	}
	for _, rel := range []string{
		"refs-dissections/README.md",
		"refs-dissections/warp/03-code-patterns.md",
		"refs-dissections/archon/01-repo-structure.md",
	} {
		if _, err := os.Stat(filepath.Join(out, rel)); err != nil {
			t.Fatalf("expected written file %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "refs-dissections", "manifest.json")); err != nil {
		t.Fatalf("expected per-source manifest: %v", err)
	}
}

func TestReplaceCrawlSourceRefreshesFTSAsCompleteSource(t *testing.T) {
	out := t.TempDir()
	first := []crawlDoc{
		{Rel: "one.md", URL: "refs-crawl://one", Content: []byte("first adoption pattern")},
		{Rel: "two.md", URL: "refs-crawl://two", Content: []byte("obsolete adoption pattern")},
	}
	if _, err := replaceCrawlSource(out, "refs-dissections", "refs-crawl", first, []string{"crawl-refs"}); err != nil {
		t.Fatal(err)
	}
	second := []crawlDoc{
		{Rel: "one.md", URL: "refs-crawl://one", Content: []byte("updated adoption pattern")},
	}
	if _, err := replaceCrawlSource(out, "refs-dissections", "refs-crawl", second, []string{"crawl-refs"}); err != nil {
		t.Fatal(err)
	}

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	got, err := idx.totalDocs()
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("indexed docs = %d, want 1 after complete-source replacement", got)
	}
	if _, err := os.Stat(filepath.Join(out, "refs-dissections", "two.md")); !os.IsNotExist(err) {
		t.Fatalf("removed crawl doc still exists: %v", err)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
