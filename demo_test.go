package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDemoProvesIsolatedFTS5Journey(t *testing.T) {
	out := filepath.Join(t.TempDir(), "corpus")
	result, err := runDemo(out, demoQuery, false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Mode != "fts5" || result.Documents != 3 || len(result.Results) == 0 {
		t.Fatalf("incomplete demo result: %+v", result)
	}
	if result.Results[0].Path != "docs-puller-demo/sqlite-fts5.md" {
		t.Fatalf("top result = %q, want sqlite demo doc", result.Results[0].Path)
	}
	if !strings.HasPrefix(result.Results[0].URL, demoOriginBase) {
		t.Fatalf("demo origin leaks its temporary input path: %q", result.Results[0].URL)
	}
	if _, err := os.Stat(filepath.Join(out, ".cache", "search.db")); err != nil {
		t.Fatalf("demo did not create FTS5 index: %v", err)
	}
}

func TestMaterializeDemoDataHasThreeMarkdownDocs(t *testing.T) {
	dir := t.TempDir()
	if err := materializeDemoData(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("demo file count = %d, want 3", len(entries))
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			t.Fatalf("unexpected demo entry: %s", entry.Name())
		}
	}
}
