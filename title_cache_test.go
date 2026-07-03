package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTitleCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "s")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join(srcDir, "x.md")
	if err := os.WriteFile(docPath, []byte("---\ntitle: 'First'\n---\n\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := loadTitleCache(srcDir)
	if len(c.Titles) != 0 {
		t.Errorf("fresh cache should be empty, got %+v", c.Titles)
	}
	info, err := os.Stat(docPath)
	if err != nil {
		t.Fatal(err)
	}

	// First call: cache miss, extract.
	if got := c.titleFor(srcDir, "x.md", info); got != "First" {
		t.Errorf("first titleFor = %q, want First", got)
	}
	if !c.dirty {
		t.Error("expected dirty after cache miss")
	}
	if err := saveTitleCache(srcDir, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(srcDir, titleCacheFile)); err != nil {
		t.Errorf("expected %s after save, got: %v", titleCacheFile, err)
	}

	// Second load: cache hit; titleFor returns without re-reading.
	c2 := loadTitleCache(srcDir)
	if len(c2.Titles) != 1 {
		t.Fatalf("expected 1 cached title, got %+v", c2.Titles)
	}
	// Mutate the file's content but keep the same mtime — should still be a cache hit.
	if err := os.WriteFile(docPath, []byte("---\ntitle: 'Different'\n---\n\nbody2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(docPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	info2, _ := os.Stat(docPath)
	if got := c2.titleFor(srcDir, "x.md", info2); got != "First" {
		t.Errorf("with same mtime, expected cached 'First', got %q", got)
	}

	// Bump mtime — cache miss, fresh title.
	future := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(docPath, future, future); err != nil {
		t.Fatal(err)
	}
	info3, _ := os.Stat(docPath)
	if got := c2.titleFor(srcDir, "x.md", info3); got != "Different" {
		t.Errorf("with bumped mtime, expected fresh 'Different', got %q", got)
	}
}

func TestTitleCachePrunesUnvisited(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "s")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := titleCache{
		Version: titleCacheVersion,
		Titles: map[string]titleEntry{
			"a.md": {Title: "A", MtimeNs: 1},
			"b.md": {Title: "B", MtimeNs: 2},
			"c.md": {Title: "C", MtimeNs: 3},
		},
	}
	c.pruneUnvisited(map[string]bool{"a.md": true, "c.md": true})
	if len(c.Titles) != 2 {
		t.Errorf("expected 2 entries after prune, got %+v", c.Titles)
	}
	if _, ok := c.Titles["b.md"]; ok {
		t.Error("expected b.md removed")
	}
	if !c.dirty {
		t.Error("expected dirty after prune")
	}
}

func TestSaveTitleCacheNoOpWhenClean(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "s")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := titleCache{Version: titleCacheVersion, Titles: map[string]titleEntry{}}
	if err := saveTitleCache(srcDir, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(srcDir, titleCacheFile)); !os.IsNotExist(err) {
		t.Errorf("clean save should not create file, stat err: %v", err)
	}
}
