package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
)

// Per-source title cache at <out>/<source>/.titles.json. Avoids re-opening
// every doc in a touched source for `extractTitle` on every pull —
// `regenerateIndex` for a 2200-doc source was opening 2200 files for one
// changed URL. With this cache, cache-hit lookup is a map read + mtime
// compare; cache-miss falls back to extractTitle and updates the cache.
//
// Hidden-dot prefix keeps it out of every existing file walk (collectEntries,
// FTS5 rebuild, serve directory listing) — they all filter on `.md` suffix
// or skip non-md names. No new exclusion rules needed.
//
// Per ADR-001 (2026-04-29 revision): bounded keyed-state, single writer,
// always full-rewrite — same shape as manifest.json. JSON object + atomic
// temp-file + rename writes.

const (
	titleCacheFile = ".titles.json"
	// titleCacheVersion: BUMP THIS WHENEVER extractTitle SEMANTICS CHANGE.
	// The cache stores titles keyed on (path, mtime). If extractTitle's
	// logic changes (new genericTitles entry, new H2 fallback, frontmatter
	// parsing change, scrubTitle pattern, etc.) but the version doesn't,
	// every existing cache entry continues serving the OLD title for
	// unchanged files — silent-incorrect. Reindex won't help; the mtime
	// matches so the cache hit short-circuits before extractTitle runs.
	//
	// History:
	//   1 — initial schema
	//   2 — 2026-05-01: extractTitle gained generic-H1 fallback + H2
	//        lookup + genericTitles map (CLI Reference, Global flags, etc.)
	titleCacheVersion = 2
)

type titleCache struct {
	Version int                   `json:"version"`
	Titles  map[string]titleEntry `json:"titles"`
	dirty   bool                  // set when a title is added/changed; saveTitleCache no-ops when false
}

type titleEntry struct {
	Title   string `json:"title"`
	MtimeNs int64  `json:"mtime_ns"`
}

func loadTitleCache(srcDir string) titleCache {
	data, err := os.ReadFile(filepath.Join(srcDir, titleCacheFile))
	if err != nil {
		return titleCache{Version: titleCacheVersion, Titles: map[string]titleEntry{}}
	}
	var c titleCache
	if jerr := json.Unmarshal(data, &c); jerr != nil || c.Version != titleCacheVersion {
		return titleCache{Version: titleCacheVersion, Titles: map[string]titleEntry{}}
	}
	if c.Titles == nil {
		c.Titles = map[string]titleEntry{}
	}
	return c
}

// titleFor returns the title for `rel` (path relative to srcDir). Cache hit
// when the file's mtime matches the cached mtime; otherwise extractTitle is
// called and the cache is updated. Returns "" when the file can't be read.
func (c *titleCache) titleFor(srcDir, rel string, info fs.FileInfo) string {
	mtimeNs := info.ModTime().UnixNano()
	if entry, ok := c.Titles[rel]; ok && entry.MtimeNs == mtimeNs {
		return entry.Title
	}
	title := extractTitle(filepath.Join(srcDir, rel))
	c.Titles[rel] = titleEntry{Title: title, MtimeNs: mtimeNs}
	c.dirty = true
	return title
}

// pruneUnvisited removes cache entries whose paths weren't visited this
// walk — keeps the cache from accumulating ghost entries when docs are
// deleted from the source. Caller passes the set of paths it actually saw.
func (c *titleCache) pruneUnvisited(visited map[string]bool) {
	for k := range c.Titles {
		if !visited[k] {
			delete(c.Titles, k)
			c.dirty = true
		}
	}
}

// saveTitleCache writes the cache via temp-file + rename. No-op when the
// cache wasn't modified — avoids spurious mtime bumps on read-only walks.
func saveTitleCache(srcDir string, c titleCache) error {
	if !c.dirty {
		return nil
	}
	final := filepath.Join(srcDir, titleCacheFile)
	tmp, err := os.CreateTemp(srcDir, ".titles.json.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
