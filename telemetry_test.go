package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSearchQueryLogRoundtripAndFixtureExport(t *testing.T) {
	out := t.TempDir()
	entry := queryLogEntry{
		Timestamp:    "2026-05-03T00:00:00Z",
		Query:        "how do I enable row level security",
		Intent:       "support",
		SourceFilter: "supabase",
		Mode:         "fts5",
		Limit:        10,
		ResultCount:  1,
		TopPath:      "supabase/guides/database/postgres/row-level-security.md",
		TopSource:    "supabase",
	}
	if err := appendSearchQueryLog(out, entry); err != nil {
		t.Fatal(err)
	}
	got, err := readSearchQueryLog(out, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Query != entry.Query {
		t.Fatalf("query log = %+v, want one entry", got)
	}
	fixture := fixtureFromTelemetry(got, "support", time.Time{}, nil)
	if len(fixture.Queries) != 1 {
		t.Fatalf("fixture queries = %d, want 1", len(fixture.Queries))
	}
	q := fixture.Queries[0]
	if q.Q != entry.Query || q.Source != "supabase" || len(q.Expect) != 1 || q.Expect[0] != entry.TopPath {
		t.Fatalf("fixture query = %+v", q)
	}
}

func TestNewSearchQueryLogEntryUsesRuntimeOutputMetadata(t *testing.T) {
	got := newSearchQueryLogEntry("react hooks", 42, []searchHit{{
		Path:         "react/reference/react/hooks.md",
		Source:       "react",
		SourceFamily: "react",
	}}, "fts5", searchOpts{
		source:          "react",
		queryIntent:     "support",
		limit:           5,
		resolvedProfile: "acme",
		versionPolicy: &versionSearchPolicy{
			version:  "18",
			cwdScope: "workspace",
		},
		rerankLLM: true,
	})
	if got.Query != "react hooks" || got.Intent != "support" || got.SourceFilter != "react" {
		t.Fatalf("query metadata = %+v", got)
	}
	if got.Profile != "acme" || got.Version != "18" || got.PinContext != "workspace" {
		t.Fatalf("output metadata = %+v, want profile/version/pin context", got)
	}
	if got.Mode != "fts5" || got.Scanned != 42 || got.Limit != 5 || got.ResultCount != 1 || !got.RerankLLM {
		t.Fatalf("runtime metadata = %+v", got)
	}
	if got.TopPath != "react/reference/react/hooks.md" || got.TopSource != "react" || got.TopSourceFamily != "react" {
		t.Fatalf("top hit metadata = %+v", got)
	}
}

func TestEvalSuiteFixturePaths(t *testing.T) {
	dir := t.TempDir()
	// Valid fixtures (have a `queries:` key with a list value, even if empty)
	for _, name := range []string{"fixture.yaml", "natural-language.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name),
			[]byte("queries:\n  - q: hello\n    expect: [a/b.md]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Non-fixture files that must be filtered out:
	// - notes.txt: not .yaml extension, glob already excludes
	// - notes.yaml: yaml extension but no `queries:` key (must be skipped via YAML safety)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"),
		[]byte("queries: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.yaml"),
		[]byte("notes: hi\n# no queries key here, this is just an arbitrary YAML\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	paths, err := evalSuiteFixturePaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 ||
		filepath.Base(paths[0]) != "fixture.yaml" ||
		filepath.Base(paths[1]) != "natural-language.yaml" {
		t.Fatalf("fixture paths = %v (notes.yaml should be filtered by YAML safety check)", paths)
	}
}

// TestFixtureFromTelemetryRespectsExclude regression-guards the
// --exclude-fixture flag added 2026-05-04: queries already present in a
// known fixture must be skipped from the telemetry-derived sample so that
// the production-telemetry fixture isn't dominated by eval-driven duplicates
// (queries.jsonl is currently dominated by eval re-runs of the fixture).
func TestFixtureFromTelemetryRespectsExclude(t *testing.T) {
	entries := []queryLogEntry{
		{
			Timestamp: "2026-05-04T00:00:00Z",
			Query:     "row level security",
			TopPath:   "supabase/guides/database/postgres/row-level-security.md",
		},
		{
			Timestamp: "2026-05-04T00:01:00Z",
			Query:     "How do I configure my workspace ports",
			TopPath:   "team-kb/local-dev/parallel-workspaces.md",
		},
	}
	// Exclude the first query (mimicking it being already in fixture.yaml).
	exclude := map[string]bool{
		"row level security": true,
	}
	fixture := fixtureFromTelemetry(entries, "", time.Time{}, exclude)
	if len(fixture.Queries) != 1 {
		t.Fatalf("fixture queries = %d, want 1 (the excluded one must be dropped)", len(fixture.Queries))
	}
	if fixture.Queries[0].Q != "How do I configure my workspace ports" {
		t.Errorf("kept wrong query: %q", fixture.Queries[0].Q)
	}
	// Sanity: case + trim insensitivity. Same input but with whitespace + case
	// variation must still be excluded.
	exclude2 := map[string]bool{
		"  ROW LEVEL SECURITY  ": true,
	}
	// Build the lookup key the way telemetry.go does (lowercase + trim) to
	// confirm the exclude map's keys must come pre-normalized. We DON'T
	// re-normalize inside fixtureFromTelemetry — the caller (cmdTelemetryFixture)
	// owns normalization. So this should NOT exclude.
	fixture2 := fixtureFromTelemetry(entries, "", time.Time{}, exclude2)
	if len(fixture2.Queries) != 2 {
		t.Errorf("fixture queries = %d, want 2 (exclude with un-normalized key must miss)", len(fixture2.Queries))
	}
}

func TestEvalSuiteHasErrors(t *testing.T) {
	if !evalSuiteHasErrors([]evalSuiteRow{{Fixture: "bad.yaml", Error: "parse failed"}}) {
		t.Fatalf("expected eval-suite error detection")
	}
	if evalSuiteHasErrors([]evalSuiteRow{{Fixture: "fixture.yaml"}}) {
		t.Fatalf("unexpected eval-suite error detection")
	}
}
