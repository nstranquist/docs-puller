package main

import (
	"path/filepath"
	"testing"
)

func TestAppendAndReadIngestLog(t *testing.T) {
	dir := t.TempDir()

	first := logEntry{
		StartedAt: "2026-04-29T10:00:00Z", FinishedAt: "2026-04-29T10:00:01Z",
		ElapsedMs: 1000, Mode: "pull-url", Args: []string{"pull-url", "https://x"},
		Sources: []string{"supabase"}, URLs: 1, Pulled: 1,
	}
	second := logEntry{
		StartedAt: "2026-04-29T11:00:00Z", FinishedAt: "2026-04-29T11:00:30Z",
		ElapsedMs: 30000, Mode: "sitemap", Args: []string{"pull", "--sitemap", "https://example.com/sitemap.xml"},
		Sources: []string{"chrome", "android"}, URLs: 50, Pulled: 48, Skipped: 2, Warned: 1,
	}

	for _, e := range []logEntry{first, second} {
		if err := appendIngestLog(dir, e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	if _, err := readIngestLog(filepath.Join(dir, "does-not-exist"), 0); err != nil {
		t.Fatalf("missing log should return nil error, got %v", err)
	}

	got, err := readIngestLog(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(got), got)
	}
	// Newest first.
	if got[0].StartedAt != second.StartedAt {
		t.Errorf("expected newest entry first, got %q first", got[0].StartedAt)
	}
	if got[1].Mode != "pull-url" || got[1].Pulled != 1 {
		t.Errorf("oldest entry mismatch: %+v", got[1])
	}
	if got[0].Warned != 1 || len(got[0].Sources) != 2 {
		t.Errorf("newest entry mismatch: %+v", got[0])
	}

	limited, err := readIngestLog(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].StartedAt != second.StartedAt {
		t.Errorf("limit=1 should return only newest, got %+v", limited)
	}
}

func TestDistinctSources(t *testing.T) {
	results := []result{
		{Source: "supabase"},
		{Source: ""}, // unmapped
		{Source: "chrome"},
		{Source: "supabase"}, // dup
		{Source: "android"},
	}
	got := distinctSources(results)
	want := []string{"android", "chrome", "supabase"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] got %q, want %q", i, got[i], w)
		}
	}
}
