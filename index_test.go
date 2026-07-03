package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTitle(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"frontmatter", "---\ntitle: 'Drizzle'\nid: 'drizzle'\n---\n\nbody\n", "Drizzle"},
		{"frontmatter-double-quote", "---\ntitle: \"Foo Bar\"\n---\nbody", "Foo Bar"},
		{"frontmatter-bare", "---\ntitle: Plain\n---\nbody", "Plain"},
		{"h1-no-frontmatter", "# Real Title\n\nbody\n", "Real Title"},
		{"h1-after-frontmatter", "---\nid: x\n---\n\n# H1 Title\n\nbody", "H1 Title"},
		// DevSite preamble scrubbed from H1 — Google's content-
		// categorization shell ("Stay organized with collections...")
		// bleeds into html-to-markdown output for chrome/android docs.
		{"chrome-devsite", "# Manifest file format Stay organized with collections Save and categorize content based on your preferences.\n", "Manifest file format"},
		// Filename fallback: when no frontmatter title and no H1, derive
		// a title from the basename so the FTS5 title-boost has something
		// to match. Index pages collapse to "" so they don't poison
		// search with "index" as a generic title.
		{"empty", "", "empty"},
		{"no-title", "just paragraphs\n\nno headings\n", "no title"},
		{"index", "no headings here", ""},
		{"h2-not-h1", "## H2\n\n# H1 Later\n", "H1 Later"},
		{"snake_case_name", "no headings", "snake case name"},
		// Generic-shell-title fallthrough: Supabase CLI ref pages all share
		// "# CLI Reference" as the H1; the per-page signal is the H2.
		{"supabase-db-dump", "# CLI Reference\n\nsidebar\n\n## supabase db dump\n\nbody\n", "supabase db dump"},
		// Generic H1, no H2 at all — fall through to filename.
		{"docs-only-h1", "# Documentation\n\nbody\n", "docs only h1"},
		// Generic frontmatter title still falls through to a meaningful H1.
		{"frontmatter-generic", "---\ntitle: 'Reference'\n---\n\n# Connect to your database\n\nbody\n", "Connect to your database"},
		// Both H1 and H2 generic — filename wins.
		{"both-generic-fallback", "# Reference\n\n## Overview\n\nbody\n", "both generic fallback"},
		// Real H2 doesn't displace a real H1.
		{"real-h1-keeps-priority", "# Connect to your database\n\n## Overview\n\nbody\n", "Connect to your database"},
		// Supabase CLI-ref shape: H1 generic AND first H2 is "Global flags"
		// (a sidebar artifact). Filename should win over both.
		{"supabase-db-diff", "# CLI Reference\n\n## Global flags\n\nstuff\n\n## supabase db diff\n\nbody\n", "supabase db diff"},
		// No H1 at all + a local-section H2 ("Properties") — filename must
		// win, NOT the H2. Mirrors obsidian-app-docs/Reference/Manifest.md
		// which has no H1 and "## Properties" as first H2.
		{"obsidian-Manifest", "---\ncssclasses: reference\n---\n\nbody\n\n## Properties\n\ntable\n", "obsidian Manifest"},
	}
	for _, c := range cases {
		path := filepath.Join(dir, c.name+".md")
		if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
			t.Fatal(err)
		}
		got := extractTitle(path)
		if got != c.want {
			t.Errorf("%s: extractTitle = %q, want %q", c.name, got, c.want)
		}
		gotFromBytes := extractTitleFromBytes(path, []byte(c.content))
		if gotFromBytes != c.want {
			t.Errorf("%s: extractTitleFromBytes = %q, want %q", c.name, gotFromBytes, c.want)
		}
	}
}

func TestExtractTitleHandlesLongLines(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("x", 70*1024) + "\n# Real Title\nbody\n"
	path := filepath.Join(dir, "long-line.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := extractTitle(path); got != "Real Title" {
		t.Fatalf("extractTitle = %q, want Real Title", got)
	}
	if got := extractTitleFromBytes(path, []byte(content)); got != "Real Title" {
		t.Fatalf("extractTitleFromBytes = %q, want Real Title", got)
	}
}

func TestGroupKey(t *testing.T) {
	cases := map[string]string{
		"index.md":                    "",
		"foo.md":                      "",
		"guides/database/drizzle.md":  "guides/database",
		"guides/local-development.md": "guides",
		"reference/cli/start.md":      "reference/cli",
		"a/b/c/d.md":                  "a/b",
	}
	for in, want := range cases {
		if got := groupKey(in); got != want {
			t.Errorf("groupKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegenerateIndexEndToEnd(t *testing.T) {
	out := t.TempDir()

	supabase := filepath.Join(out, "supabase")
	if err := os.MkdirAll(filepath.Join(supabase, "guides", "database"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(supabase, "guides", "database", "drizzle.md"),
		[]byte("---\ntitle: 'Drizzle'\n---\n\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newManifest()
	m.Entries["https://supabase.com/docs/guides/database/drizzle"] = result{
		URL: "https://supabase.com/docs/guides/database/drizzle", Source: "supabase",
		Path: "supabase/guides/database/drizzle.md", FetchedAt: "2026-04-28T00:00:00Z",
	}
	if err := writeManifestAtomic(supabase, m); err != nil {
		t.Fatal(err)
	}

	if err := regenerateIndex(out, nil); err != nil {
		t.Fatal(err)
	}

	top, err := os.ReadFile(filepath.Join(out, "_INDEX.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Vendor docs", "[supabase]", "| 1 |", "2026-04-28T00:00:00Z"} {
		if !strings.Contains(string(top), want) {
			t.Errorf("top INDEX missing %q:\n%s", want, top)
		}
	}

	src, err := os.ReadFile(filepath.Join(supabase, "_INDEX.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# supabase",
		"1 docs",
		"## guides/database",
		"[Drizzle](guides/database/drizzle.md)",
		"<https://supabase.com/docs/guides/database/drizzle>",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("supabase INDEX missing %q:\n%s", want, src)
		}
	}
}

func TestRegenerateIndexFromMemoryDoesNotNeedCopiedBodyRead(t *testing.T) {
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "kb"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := regenerateIndexFromMemory(out, []memoryIndexDoc{
		{
			source:  "kb",
			rel:     "contracts/retrieval.md",
			absPath: filepath.Join(out, "kb", "contracts", "retrieval.md"),
			body:    []byte("body without a markdown title"),
			title:   "Retrieval Contract",
			url:     "file:///source/retrieval.md",
			fetched: "2026-05-03T00:00:00Z",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	top, err := os.ReadFile(filepath.Join(out, "_INDEX.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(top), "| [kb](kb/_INDEX.md) | 1 | 2026-05-03T00:00:00Z |") {
		t.Fatalf("top INDEX missing memory summary:\n%s", top)
	}
	src, err := os.ReadFile(filepath.Join(out, "kb", "_INDEX.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# kb",
		"1 docs",
		"## contracts",
		"[Retrieval Contract](contracts/retrieval.md)",
		"<file:///source/retrieval.md>",
	} {
		if !strings.Contains(string(src), want) {
			t.Fatalf("source INDEX missing %q:\n%s", want, src)
		}
	}
}

func TestRegenerateIndexFromMemoryWrapsSourceIndexWriteError(t *testing.T) {
	out := t.TempDir()

	err := regenerateIndexFromMemory(out, []memoryIndexDoc{
		{
			source: "missing-source",
			rel:    "contracts/retrieval.md",
			body:   []byte("# Retrieval\n"),
			title:  "Retrieval",
		},
	})
	if err == nil || !strings.HasPrefix(err.Error(), "write missing-source/_INDEX.md: ") {
		t.Fatalf("regenerateIndexFromMemory error = %v, want source index write wrapper", err)
	}
}
