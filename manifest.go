package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nstranquist/docs-puller/searchruntime"
)

// Per-source manifest at <out>/<source>/manifest.json. URL → entry map,
// always rewritten in full (bounded — hundreds to a few thousand URLs per
// source) with atomic temp-file + rename. Replaces the legacy
// manifest.jsonl shape per ADR-001 (2026-04-29 revision): full-rewrite +
// keyed-lookup state is bounded JSON, not append-only JSONL. Legacy
// manifest.jsonl files are auto-migrated on first read and removed.
//
// `version` is intentional: the file outlives any single run and we want
// the option to evolve the schema without parsing heuristics. Bump on any
// breaking change to the value shape.

const (
	manifestFile       = "manifest.json"
	legacyManifestFile = "manifest.jsonl"
	manifestVersion    = 1
)

type manifest struct {
	Version int               `json:"version"`
	Entries map[string]result `json:"entries"`
}

func newManifest() manifest {
	return manifest{Version: manifestVersion, Entries: map[string]result{}}
}

// loadOrMigrateManifest reads manifest.json from srcDir. If it doesn't
// exist but manifest.jsonl does, the JSONL is parsed, written out as JSON,
// and the legacy file removed. Returns an empty manifest when neither file
// exists. Caller must hold the write lock when expecting to write back.
func loadOrMigrateManifest(srcDir string) (manifest, error) {
	path := filepath.Join(srcDir, manifestFile)
	data, err := os.ReadFile(path)
	if err == nil {
		var m manifest
		if uerr := json.Unmarshal(data, &m); uerr != nil {
			return manifest{}, searchruntime.ManifestParseError(path, uerr)
		}
		if m.Entries == nil {
			m.Entries = map[string]result{}
		}
		if m.Version == 0 {
			m.Version = manifestVersion
		}
		return m, nil
	}
	if !os.IsNotExist(err) {
		return manifest{}, err
	}

	legacy := filepath.Join(srcDir, legacyManifestFile)
	legacyData, lerr := os.ReadFile(legacy)
	if lerr != nil {
		if os.IsNotExist(lerr) {
			return newManifest(), nil
		}
		return manifest{}, lerr
	}
	m := newManifest()
	for _, line := range strings.Split(string(legacyData), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r result
		if jerr := json.Unmarshal([]byte(line), &r); jerr != nil {
			continue
		}
		if r.URL != "" {
			m.Entries[r.URL] = r
		}
	}
	if werr := writeManifestAtomic(srcDir, m); werr != nil {
		return manifest{}, searchruntime.ManifestMigrationError(legacy, path, werr)
	}
	if rerr := os.Remove(legacy); rerr != nil {
		fmt.Fprintf(os.Stderr, "manifest: migrated %s but failed to remove legacy: %v\n", legacy, rerr)
	}
	return m, nil
}

// writeManifestAtomic serializes m to <srcDir>/manifest.json via a temp
// file in the same directory + os.Rename. The same-dir temp ensures rename
// is atomic on POSIX (no cross-filesystem fallback to copy+unlink).
func writeManifestAtomic(srcDir string, m manifest) error {
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return err
	}
	final := filepath.Join(srcDir, manifestFile)
	tmp, err := os.CreateTemp(srcDir, ".manifest.json.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
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

// dedupeManifestPaths removes entries whose Path is claimed by another URL
// with a newer fetch (ties break to the lexicographically smaller URL).
// Two URLs can converge on one on-disk file when a page is pulled under
// URL variants (e.g. with and without a trailing .md); the file's content
// belongs to exactly one fetch, so keeping both rows misrepresents the
// corpus and forces every reader to re-resolve the collision. Returns the
// number of entries removed.
func dedupeManifestPaths(m *manifest) int {
	winnerByPath := map[string]string{} // path → winning URL
	for url, r := range m.Entries {
		if r.Path == "" {
			continue
		}
		prevURL, seen := winnerByPath[r.Path]
		if !seen {
			winnerByPath[r.Path] = url
			continue
		}
		prev := m.Entries[prevURL]
		if r.FetchedAt > prev.FetchedAt || (r.FetchedAt == prev.FetchedAt && url < prevURL) {
			winnerByPath[r.Path] = url
		}
	}
	removed := 0
	for url, r := range m.Entries {
		if r.Path == "" {
			continue
		}
		if winnerByPath[r.Path] != url {
			delete(m.Entries, url)
			removed++
		}
	}
	return removed
}

// loadManifestMaps returns the (urlByPath, fetchedByPath) maps that the
// _INDEX.md regen + FTS rebuild + agent listings consume. Keys are the
// in-source relative path ("guides/database/drizzle.md") — same shape as
// the legacy loadManifest helper this replaces.
func loadManifestMaps(srcDir, srcName string) (urlByPath, fetchedByPath map[string]string) {
	urlByPath = map[string]string{}
	fetchedByPath = map[string]string{}
	m, err := loadOrMigrateManifest(srcDir)
	if err != nil {
		return
	}
	prefix := srcName + "/"
	for _, r := range m.Entries {
		rel := strings.TrimPrefix(r.Path, prefix)
		if rel == "" || rel == r.Path {
			continue
		}
		// Two URLs can map to one on-disk path (e.g. a page pulled both
		// with and without a trailing .md). Resolve the collision
		// deterministically — newest fetch wins, ties break on URL — so
		// map iteration order never decides last_pull or the URL column.
		if prev, seen := fetchedByPath[rel]; seen {
			if r.FetchedAt < prev || (r.FetchedAt == prev && r.URL > urlByPath[rel]) {
				continue
			}
		}
		urlByPath[rel] = r.URL
		fetchedByPath[rel] = r.FetchedAt
	}
	return
}
