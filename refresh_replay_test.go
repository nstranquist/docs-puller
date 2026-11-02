package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFilterURLsCountedReportsCapDrop(t *testing.T) {
	in := []string{
		"https://example.com/docs/a",
		"https://example.com/blog/x",
		"https://example.com/docs/b",
		"https://example.com/docs/c",
	}
	kept, matched := filterURLsCounted(in, "https://example.com/docs/", 2)
	if len(kept) != 2 || matched != 3 {
		t.Fatalf("kept=%d matched=%d, want 2 and 3", len(kept), matched)
	}
	kept, matched = filterURLsCounted(in, "https://example.com/docs/", 0)
	if len(kept) != 3 || matched != 3 {
		t.Fatalf("uncapped: kept=%d matched=%d, want 3 and 3", len(kept), matched)
	}
	if _, dropped := applyURLCap(in, "https://example.com/docs/", 2); dropped != 1 {
		t.Fatalf("applyURLCap dropped=%d, want 1", dropped)
	}
	if _, dropped := applyURLCap(in, "https://example.com/docs/", 5); dropped != 0 {
		t.Fatalf("applyURLCap under cap dropped=%d, want 0", dropped)
	}
}

func TestReplayArgsDropsMaxAndOut(t *testing.T) {
	got := replayArgs([]string{
		"pull", "--llms-txt", "https://x/llms.txt", "--filter", "https://x/en/",
		"--max", "200", "--out", "/old", "--max=5", "-out=/old2", "--concurrency", "4",
	}, "/new")
	want := []string{"pull", "--llms-txt", "https://x/llms.txt", "--filter", "https://x/en/", "--concurrency", "4", "--out", "/new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestLatestReplayableIngestPicksNewestPullForSource(t *testing.T) {
	entries := []logEntry{ // newest first
		{Mode: "pull-url", Args: []string{"pull-url", "https://x/one"}, Sources: []string{"cc"}},
		{Mode: "from-file", Args: []string{"pull", "--from", "inputs/cc-fixture.txt"}, Sources: []string{"cc"}, StartedAt: "fixture"},
		{Mode: "llms-txt", Args: []string{"pull", "--llms-txt", "u"}, Sources: []string{"cc"}, StartedAt: "new"},
		{Mode: "from-file", Args: []string{"pull", "--from", "/abs/fixture.txt"}, Sources: []string{"cc"}, StartedAt: "old"},
		{Mode: "sitemap", Args: []string{"pull", "--sitemap", "s"}, Sources: []string{"other"}},
	}
	e, ok := latestReplayableIngest(entries, "cc")
	if !ok || e.StartedAt != "new" {
		t.Fatalf("got %+v ok=%v, want the newest llms-txt pull", e, ok)
	}
	if _, ok := latestReplayableIngest(entries, "missing"); ok {
		t.Fatal("unknown source must not resolve")
	}
}

func TestCollectCappedSourcesUsesNewestEntryPerSource(t *testing.T) {
	entries := []logEntry{ // newest first
		{Sources: []string{"healed"}, StartedAt: "t3"},
		{Sources: []string{"partial"}, Capped: 28, CapHit: true, StartedAt: "t2"},
		{Sources: []string{"docc"}, CapHit: true, StartedAt: "t2"},
		{Sources: []string{"healed"}, Capped: 9, CapHit: true, StartedAt: "t1"},
	}
	got := collectCappedSources(entries)
	want := []statusCappedSource{{Name: "partial", Capped: 28, PulledAt: "t2"}, {Name: "docc", PulledAt: "t2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	warnings := buildStatusWarningsWithOptions(docsStatus{SourceCount: 1, CappedSources: got,
		FTS: statusFTS{Exists: true, Ready: true, MatchesCorpus: true}}, false, false)
	joined := strings.Join(warnings, "\n")
	for _, needle := range []string{"partial is a partial mirror", "dropped 28", "an unknown number of", "refresh --source docc"} {
		if !strings.Contains(joined, needle) {
			t.Errorf("warnings missing %q:\n%s", needle, joined)
		}
	}
}

func TestLatestIngestForSourceFindsNonReplayableOwner(t *testing.T) {
	entries := []logEntry{
		{Mode: "pull-article", Args: []string{"pull-article", "https://x/a"}, Sources: []string{"articles"}},
	}
	if _, ok := latestReplayableIngest(entries, "articles"); ok {
		t.Fatal("pull-article must not be replayable")
	}
	e, ok := latestIngestForSource(entries, "articles")
	if !ok || firstArg(e.Args) != "pull-article" {
		t.Fatalf("got %+v ok=%v, want the pull-article entry", e, ok)
	}
	if got := otherSources([]string{"github", "gh-cli"}, "github"); !reflect.DeepEqual(got, []string{"gh-cli"}) {
		t.Fatalf("otherSources = %v", got)
	}
}

func TestReplaySkipsSpilloverSources(t *testing.T) {
	entries := []logEntry{ // newest first
		{Mode: "from-file", StartedAt: "t3", Sources: []string{"github", "temporal"},
			Args: []string{"pull", "--from", "https://docs.temporal.io/llms.txt", "--max", "150"}},
		{Mode: "from-file", StartedAt: "t2", Sources: []string{"github", "nextjs"},
			Args: []string{"pull", "--from", "https://nextjs.org/docs/llms.txt"}},
		{Mode: "github-repo", StartedAt: "t1", Sources: []string{"vendor-docs", "github"},
			Args: []string{"pull", "--github-repo", "o/r", "--name", "vendor-docs"}},
	}
	if e, ok := latestReplayableIngest(entries, "temporal"); !ok || e.StartedAt != "t3" {
		t.Fatalf("temporal: got %+v ok=%v, want t3", e, ok)
	}
	if e, ok := latestReplayableIngest(entries, "nextjs"); !ok || e.StartedAt != "t2" {
		t.Fatalf("nextjs (routed host): got %+v ok=%v, want t2", e, ok)
	}
	if e, ok := latestReplayableIngest(entries, "vendor-docs"); !ok || e.StartedAt != "t1" {
		t.Fatalf("--name owner: got %+v ok=%v, want t1", e, ok)
	}
	if e, ok := latestReplayableIngest(entries, "github"); ok {
		t.Fatalf("github is spillover only, got owner %+v", e)
	}
}

func TestFlagValueForms(t *testing.T) {
	args := []string{"pull", "--from", "a", "-name=b", "--max", "3"}
	if flagValue(args, "from") != "a" || flagValue(args, "name") != "b" || flagValue(args, "max") != "3" || flagValue(args, "out") != "" {
		t.Fatalf("flagValue mismatch on %v", args)
	}
}

func TestReplayPrefersDiscoveryOverNewerURLList(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "bun-fixture.txt")
	if err := os.WriteFile(list, []byte("https://bun.com/docs/a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []logEntry{ // newest first
		{Mode: "from-file", StartedAt: "list", Sources: []string{"bun"}, Args: []string{"pull", "--from", list}},
		{Mode: "from-file", StartedAt: "gone", Sources: []string{"bun"}, Args: []string{"pull", "--from", filepath.Join(dir, "missing.txt")}},
		{Mode: "from-file", StartedAt: "llms", Sources: []string{"bun"}, Args: []string{"pull", "--from", "https://bun.com/llms.txt", "--max", "150"}},
	}
	if e, ok := latestReplayableIngest(entries, "bun"); !ok || e.StartedAt != "llms" {
		t.Fatalf("got %+v ok=%v, want the llms.txt discovery recipe", e, ok)
	}
	// With no discovery recipe, the newest existing list wins; a missing one never does.
	if e, ok := latestReplayableIngest(entries[:2], "bun"); !ok || e.StartedAt != "list" {
		t.Fatalf("fallback: got %+v ok=%v, want the existing list", e, ok)
	}
	if _, ok := latestReplayableIngest(entries[1:2], "bun"); ok {
		t.Fatal("a recipe whose input file is gone must not replay")
	}
}
