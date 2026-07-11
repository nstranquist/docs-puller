package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression: two manifest entries (distinct URLs) can point at the same
// on-disk path — e.g. a page pulled both as .../messages and
// .../messages.md. loadManifestMaps used last-write-wins over randomized
// map iteration, so `docs list` last_pull and the URL column flipped
// between runs (observed live 2026-07-11 on the anthropic source: the two
// newest-fetch entries were exactly the duplicated paths, so last_pull
// nondeterministically moved backward from 07-01 to 06-29). The collision
// must resolve deterministically: newest fetched_at wins, ties break to
// the lexicographically smaller URL.
func TestLoadManifestMapsDuplicatePathDeterministic(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "anthropic")
	m := newManifest()
	m.Entries["https://example.com/docs/api/messages"] = result{
		URL:       "https://example.com/docs/api/messages",
		Source:    "anthropic",
		Path:      "anthropic/api/messages.md",
		FetchedAt: "2026-07-01T00:26:55Z",
	}
	m.Entries["https://example.com/docs/api/messages.md"] = result{
		URL:       "https://example.com/docs/api/messages.md",
		Source:    "anthropic",
		Path:      "anthropic/api/messages.md",
		FetchedAt: "2026-06-29T22:09:33Z",
	}
	// A non-colliding older sibling so the maps have unrelated content too.
	m.Entries["https://example.com/docs/other"] = result{
		URL:       "https://example.com/docs/other",
		Source:    "anthropic",
		Path:      "anthropic/other.md",
		FetchedAt: "2026-05-28T22:49:55Z",
	}
	if err := writeManifestAtomic(srcDir, m); err != nil {
		t.Fatal(err)
	}

	// Go map iteration order is randomized per run; loop enough times that
	// the pre-fix last-write-wins behavior would flip with overwhelming
	// probability (each rebuild is an independent coin flip).
	for i := 0; i < 40; i++ {
		urlByPath, fetchedByPath := loadManifestMaps(srcDir, "anthropic")
		if got := fetchedByPath["api/messages.md"]; got != "2026-07-01T00:26:55Z" {
			t.Fatalf("iteration %d: fetched_at = %q, want newest 2026-07-01T00:26:55Z", i, got)
		}
		if got := urlByPath["api/messages.md"]; got != "https://example.com/docs/api/messages" {
			t.Fatalf("iteration %d: url = %q, want the newest-fetch URL", i, got)
		}
		if got := fetchedByPath["other.md"]; got != "2026-05-28T22:49:55Z" {
			t.Fatalf("iteration %d: unrelated entry disturbed: %q", i, got)
		}
	}
}

// Equal fetched_at values must still resolve deterministically (smaller URL
// wins) so downstream listings never depend on map order.
func TestLoadManifestMapsDuplicatePathTieBreak(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	m := newManifest()
	m.Entries["https://example.com/b"] = result{
		URL: "https://example.com/b", Source: "src", Path: "src/page.md", FetchedAt: "2026-07-01T00:00:00Z",
	}
	m.Entries["https://example.com/a"] = result{
		URL: "https://example.com/a", Source: "src", Path: "src/page.md", FetchedAt: "2026-07-01T00:00:00Z",
	}
	if err := writeManifestAtomic(srcDir, m); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		urlByPath, _ := loadManifestMaps(srcDir, "src")
		if got := urlByPath["page.md"]; got != "https://example.com/a" {
			t.Fatalf("iteration %d: url = %q, want deterministic tie-break to https://example.com/a", i, got)
		}
	}
}

// dedupeManifestPaths keeps exactly one entry per on-disk path — newest
// fetched_at wins, ties break to the smaller URL — and leaves everything
// else alone.
func TestDedupeManifestPaths(t *testing.T) {
	m := newManifest()
	m.Entries["https://example.com/a/messages"] = result{
		URL: "https://example.com/a/messages", Path: "src/a/messages.md", FetchedAt: "2026-07-01T00:00:00Z",
	}
	m.Entries["https://example.com/a/messages.md"] = result{
		URL: "https://example.com/a/messages.md", Path: "src/a/messages.md", FetchedAt: "2026-06-29T00:00:00Z",
	}
	m.Entries["https://example.com/tie-b"] = result{
		URL: "https://example.com/tie-b", Path: "src/tie.md", FetchedAt: "2026-06-01T00:00:00Z",
	}
	m.Entries["https://example.com/tie-a"] = result{
		URL: "https://example.com/tie-a", Path: "src/tie.md", FetchedAt: "2026-06-01T00:00:00Z",
	}
	m.Entries["https://example.com/solo"] = result{
		URL: "https://example.com/solo", Path: "src/solo.md", FetchedAt: "2026-05-01T00:00:00Z",
	}
	m.Entries["https://example.com/no-path"] = result{
		URL: "https://example.com/no-path", FetchedAt: "2026-05-01T00:00:00Z",
	}

	removed := dedupeManifestPaths(&m)
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if _, ok := m.Entries["https://example.com/a/messages"]; !ok {
		t.Error("newest-fetch URL for messages.md must survive")
	}
	if _, ok := m.Entries["https://example.com/a/messages.md"]; ok {
		t.Error("older URL variant must be pruned")
	}
	if _, ok := m.Entries["https://example.com/tie-a"]; !ok {
		t.Error("tie must break to the lexicographically smaller URL")
	}
	if _, ok := m.Entries["https://example.com/tie-b"]; ok {
		t.Error("tie loser must be pruned")
	}
	if _, ok := m.Entries["https://example.com/solo"]; !ok {
		t.Error("unique-path entry must survive")
	}
	if _, ok := m.Entries["https://example.com/no-path"]; !ok {
		t.Error("path-less entry must never be touched")
	}
	if again := dedupeManifestPaths(&m); again != 0 {
		t.Errorf("dedupe must be idempotent, second pass removed %d", again)
	}
}

// writeManifests prunes stale duplicate-path entries on every write, so a
// re-pull of a source heals manifests that accumulated URL variants.
func TestWriteManifestsPrunesDuplicatePaths(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	seed := newManifest()
	seed.Entries["https://example.com/page.md"] = result{
		URL: "https://example.com/page.md", Source: "src", Path: "src/page.md", FetchedAt: "2026-06-29T00:00:00Z",
	}
	if err := writeManifestAtomic(srcDir, seed); err != nil {
		t.Fatal(err)
	}

	fresh := []result{{
		URL: "https://example.com/page", Source: "src", Path: "src/page.md", FetchedAt: "2026-07-01T00:00:00Z",
	}}
	if err := writeManifests(dir, fresh, false, nil); err != nil {
		t.Fatal(err)
	}

	m, err := loadOrMigrateManifest(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("entries = %d, want 1 after dedupe-on-write", len(m.Entries))
	}
	if _, ok := m.Entries["https://example.com/page"]; !ok {
		t.Error("the fresh (newest) URL must be the surviving entry")
	}
}

// Sanity: writeManifestAtomic must not leave temp files behind on success.
func TestWriteManifestAtomicNoTempDebris(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	if err := writeManifestAtomic(srcDir, newManifest()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != manifestFile {
			t.Fatalf("unexpected file in srcDir after atomic write: %s", e.Name())
		}
	}
}
