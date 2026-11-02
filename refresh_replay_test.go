package main

import (
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
		{Mode: "llms-txt", Args: []string{"pull", "--llms-txt", "u"}, Sources: []string{"cc"}, StartedAt: "new"},
		{Mode: "from-file", Args: []string{"pull", "--from", "fixture.txt"}, Sources: []string{"cc"}, StartedAt: "old"},
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
