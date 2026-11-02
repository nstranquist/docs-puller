package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Orphan files: .md files under a source that no manifest entry covers.
//
// A pull writes page files as it fetches and records them in manifest.json
// only at the end. An interrupted pull (killed, timed out) therefore leaves
// files on disk that no manifest knows about. Pages that upstream renamed or
// removed can leave the same residue. reindex walks the disk, so orphans stay
// searchable as stale content. On 2026-11-02 a killed ClickHouse pull left
// 4,661 translated pages, and 7 other sources held 5,114 more (mostly a
// crawled copy of a deleted nicos-tools worktree).

// sourceOrphanFiles returns the orphan .md files of one source, relative to
// the source dir and sorted. ok is false when the source has no manifest.json
// (then nothing is known to be managed, so nothing counts as an orphan).
func sourceOrphanFiles(out, src string) (orphans []string, ok bool) {
	srcDir := filepath.Join(out, src)
	if _, err := os.Stat(filepath.Join(srcDir, manifestFile)); err != nil {
		return nil, false
	}
	m, err := loadOrMigrateManifest(srcDir)
	if err != nil {
		return nil, false
	}
	managed := make(map[string]bool, len(m.Entries))
	prefix := src + "/"
	for _, r := range m.Entries {
		if rel := strings.TrimPrefix(r.Path, prefix); rel != "" && rel != r.Path {
			managed[rel] = true
		}
	}
	_ = filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") || d.Name() == "_INDEX.md" {
			return nil
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return nil
		}
		if rel = filepath.ToSlash(rel); !managed[rel] {
			orphans = append(orphans, rel)
		}
		return nil
	})
	sort.Strings(orphans)
	return orphans, true
}

// pruneOrphanFiles deletes every orphan file in every source and removes the
// directories that become empty. It returns the deleted count per source.
// The caller must hold the corpus write lock.
func pruneOrphanFiles(out string) (map[string]int, error) {
	sources, err := listSources(out)
	if err != nil {
		return nil, err
	}
	pruned := map[string]int{}
	for _, src := range sources {
		orphans, ok := sourceOrphanFiles(out, src)
		if !ok || len(orphans) == 0 {
			continue
		}
		srcDir := filepath.Join(out, src)
		dirs := map[string]bool{}
		for _, rel := range orphans {
			p := filepath.Join(srcDir, filepath.FromSlash(rel))
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return pruned, fmt.Errorf("prune %s/%s: %w", src, rel, err)
			}
			pruned[src]++
			for d := filepath.Dir(p); d != srcDir && strings.HasPrefix(d, srcDir); d = filepath.Dir(d) {
				dirs[d] = true
			}
		}
		// Deepest first, so a parent empties after its children go.
		ordered := make([]string, 0, len(dirs))
		for d := range dirs {
			ordered = append(ordered, d)
		}
		sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
		for _, d := range ordered {
			_ = os.Remove(d) // fails harmlessly when the dir is not empty
		}
	}
	return pruned, nil
}

func sortedIntKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
