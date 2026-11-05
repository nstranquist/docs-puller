package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// preflightLocalReplacement runs under the corpus writer lock before reading,
// converting or copying bodies. Final manifest validation remains authoritative.
// Discovery errors fail closed rather than turning missing input into deletion.
func preflightLocalReplacement(sources []localBatchSource, out string, excludes []string) error {
	for _, src := range sources {
		proposed := map[string]bool{}
		err := filepath.WalkDir(src.walkRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(src.walkRoot, p)
			if err != nil {
				return err
			}
			if d.IsDir() {
				if p != src.walkRoot && (shouldSkipLocalDir(d.Name()) || strings.HasPrefix(d.Name(), ".") || excludeMatch(rel, excludes)) {
					return fs.SkipDir
				}
				return nil
			}
			if !isMarkdownDocName(d.Name()) || excludeMatch(rel, excludes) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Size() > localFileSizeLimit {
				return nil
			}
			proposed[filepath.ToSlash(filepath.Join(src.name, markdownOutputRel(rel)))] = true
			return nil
		})
		if err != nil {
			return fmt.Errorf("replacement discovery %s: %w", src.name, err)
		}
		prior, err := replacementManifestEntries(filepath.Join(out, src.name))
		if err != nil {
			return fmt.Errorf("replacement manifest %s: %w", src.name, err)
		}
		paths, err := sourceDocPaths(out, src.name)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		existing := map[string]bool{}
		for _, p := range paths {
			existing[canonicalLogicalPath(p)] = true
		}
		for _, r := range prior {
			if r.Path != "" {
				existing[canonicalLogicalPath(r.Path)] = true
			}
		}
		var removed []string
		for p := range existing {
			if !proposed[p] {
				removed = append(removed, p)
			}
		}
		if err := validateSourceReplacement(src.name, max(len(prior), len(paths)), len(removed), false); err != nil {
			sort.Strings(removed)
			sample := removed
			if len(sample) > 10 {
				sample = sample[:10]
			}
			return fmt.Errorf("early membership check: %w; discovered=%d excluded_patterns=%d missing_path_sample=%q", err, len(proposed), len(excludes), sample)
		}
	}
	return nil
}

// Read without migration: a rejected replacement must not change corpus state.
// Unlike the best-effort reuse reader, this safety check cannot ignore errors.
func replacementManifestEntries(dir string) (map[string]result, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err == nil {
		var m manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		normalizeManifestPaths(&m)
		return m.Entries, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	data, err = os.ReadFile(filepath.Join(dir, legacyManifestFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries := map[string]result{}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("legacy line %d: %w", i+1, err)
		}
		if r.URL != "" {
			r.Path = canonicalLogicalPath(r.Path)
			entries[r.URL] = r
		}
	}
	return entries, nil
}
