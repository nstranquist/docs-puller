package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactConn(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"postgres://user:secret@host:5432/db", "postgres://user:***@host:5432/db"},
		{"postgresql://nico:hunter2@db.example.com/app?sslmode=require",
			"postgresql://nico:***@db.example.com/app?sslmode=require"},
		{"tcp://default:s3kret@clickhouse.local:9000/main", "tcp://default:***@clickhouse.local:9000/main"},
		{"postgres://user@host/db", "postgres://user@host/db"}, // no password to redact
		{"not-a-url", "***"},
		{"", "***"},
	}
	for _, c := range cases {
		got := redactConn(c.in)
		if got != c.want {
			t.Errorf("redactConn(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, "hunter2") || strings.Contains(got, "s3kret") {
			t.Errorf("redactConn(%q) leaked secret in output: %q", c.in, got)
		}
	}
}

func TestNowStampShape(t *testing.T) {
	s := nowStamp()
	// Format is YYYYMMDDTHHMMSSZ — 16 chars including T and Z separators.
	if len(s) != 16 || s[8] != 'T' || s[15] != 'Z' {
		t.Errorf("nowStamp() = %q, want a 16-char YYYYMMDDTHHMMSSZ", s)
	}
}

// TestStageAndExtract_DoesNotClobberSchemaTree is the regression test for
// the wave-3 stageAndExtract bug. Pre-fix, the function wrote to _schema/
// then renamed it to _live/, which silently moved any pre-existing
// migration docs into _live/. The fix is to write directly into _live/
// via the threaded outPrefix on extractSQLSchemaInto.
func TestStageAndExtract_DoesNotClobberSchemaTree(t *testing.T) {
	out := t.TempDir()

	// Seed a "prior extract sql-schema run": one doc under _schema/migrations/.
	source := "test"
	srcOut := filepath.Join(out, source)
	priorPath := filepath.Join(srcOut, "_schema", "migrations", "20260101_init.md")
	if err := os.MkdirAll(filepath.Dir(priorPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(priorPath, []byte("# pre-existing migration doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	staleLiveMigration := filepath.Join(srcOut, "_live", "migrations", "stale_snap.md")
	if err := os.MkdirAll(filepath.Dir(staleLiveMigration), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleLiveMigration, []byte("# Stale Live\n\nstale marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	staleLiveTable := filepath.Join(srcOut, "_live", "tables", "public.old_t.md")
	if err := os.MkdirAll(filepath.Dir(staleLiveTable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleLiveTable, []byte("# Table: public.old_t\n\nstale marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mf := newManifest()
	mf.Entries["file://schema"] = result{URL: "file://schema", Source: source, Path: source + "/_schema/migrations/20260101_init.md", FetchedAt: "2026-01-01T00:00:00Z"}
	mf.Entries["file://stale-live"] = result{URL: "file://stale-live", Source: source, Path: source + "/_live/migrations/stale_snap.md", FetchedAt: "2026-01-01T00:00:00Z"}
	mf.Entries["extract-table://test/public.old_t"] = result{URL: "extract-table://test/public.old_t", Source: source, Path: source + "/_live/tables/public.old_t.md", FetchedAt: "2026-01-01T00:00:00Z"}
	if err := writeManifestAtomic(srcOut, mf); err != nil {
		t.Fatal(err)
	}
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.rebuild(out); err != nil {
		idx.close()
		t.Fatal(err)
	}
	idx.close()
	if hits, _, mode := dispatchSearch("stale marker", searchOpts{out: out, limit: 5, ftsOnly: true}, nil); mode != "fts5" || len(hits) == 0 {
		t.Fatalf("expected stale fixture to be indexed before live refresh, mode=%q hits=%+v", mode, hits)
	}

	// Run the live pipeline. It should NOT touch _schema/.
	o := pullOpts{out: out}
	if err := stageAndExtract([]byte(`create table public.live_t (id int);`),
		source, "live_snap.sql", o, []string{"extract", "pg-introspect"}); err != nil {
		t.Fatalf("stageAndExtract: %v", err)
	}

	// _schema/migrations/20260101_init.md must still exist verbatim.
	if data, err := os.ReadFile(priorPath); err != nil {
		t.Fatalf("pre-existing _schema/ doc was clobbered: %v", err)
	} else if string(data) != "# pre-existing migration doc\n" {
		t.Fatalf("pre-existing _schema/ doc body changed: %q", data)
	}

	// _live/migrations/live_snap.md must exist (synthetic migration from DDL).
	live := filepath.Join(srcOut, "_live", "migrations", "live_snap.md")
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("_live snapshot doc not written: %v", err)
	}
	// _live/tables/public.live_t.md must exist (per-table pivot).
	liveTable := filepath.Join(srcOut, "_live", "tables", "public.live_t.md")
	if _, err := os.Stat(liveTable); err != nil {
		t.Fatalf("_live per-table doc not written: %v", err)
	}
	if _, err := os.Stat(staleLiveMigration); !os.IsNotExist(err) {
		t.Fatalf("stale _live migration should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(staleLiveTable); !os.IsNotExist(err) {
		t.Fatalf("stale _live table should be removed, stat err=%v", err)
	}
	afterManifest, err := loadOrMigrateManifest(srcOut)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range afterManifest.Entries {
		if strings.Contains(r.Path, "_live/migrations/stale_snap.md") || strings.Contains(r.Path, "_live/tables/public.old_t.md") {
			t.Fatalf("stale _live manifest entry survived: %+v", r)
		}
	}
	if _, ok := afterManifest.Entries["file://schema"]; !ok {
		t.Fatalf("_schema manifest entry was pruned: %+v", afterManifest.Entries)
	}
	if hits, _, mode := dispatchSearch("stale marker", searchOpts{out: out, limit: 5, ftsOnly: true}, nil); mode != "fts5" || len(hits) != 0 {
		t.Fatalf("stale _live docs should be removed from FTS, mode=%q hits=%+v", mode, hits)
	}
}
