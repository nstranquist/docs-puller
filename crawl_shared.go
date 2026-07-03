package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type crawlDoc struct {
	Rel      string
	URL      string
	Content  []byte
	ReadFrom string
}

func replaceCrawlSource(out, source, mode string, docs []crawlDoc, cmdArgs []string) (int, error) {
	source = sanitizeSourceName(source)
	if source == "" || source == "unnamed" {
		return 0, fmt.Errorf("invalid source name %q", source)
	}
	start := time.Now()
	now := start.UTC().Format(time.RFC3339)
	srcDir := filepath.Join(out, source)

	oldPaths := sourceMarkdownPaths(srcDir, source)
	sort.Slice(docs, func(i, j int) bool { return docs[i].Rel < docs[j].Rel })

	var copied int
	err := withWriteLock(out, func() error {
		if err := ensureSourcePathInsideOut(out, srcDir); err != nil {
			return err
		}
		if err := os.RemoveAll(srcDir); err != nil {
			return err
		}
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			return err
		}

		results := make([]result, 0, len(docs))
		changedPaths := append([]string{}, oldPaths...)
		for _, d := range docs {
			rel, err := cleanCrawlRel(d.Rel)
			if err != nil {
				return err
			}
			data := d.Content
			if d.ReadFrom != "" {
				data, err = os.ReadFile(d.ReadFrom)
				if err != nil {
					continue
				}
			}
			if len(data) == 0 {
				continue
			}
			outPath := filepath.Join(srcDir, rel)
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(outPath, data, 0o644); err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			path := source + "/" + filepath.ToSlash(rel)
			results = append(results, result{
				URL:       d.URL,
				Source:    source,
				Path:      path,
				Mode:      mode,
				SHA256:    hex.EncodeToString(sum[:]),
				FetchedAt: now,
			})
			changedPaths = append(changedPaths, path)
			copied++
		}
		if err := writeManifests(out, results, false, nil); err != nil {
			return err
		}
		if err := regenerateIndex(out, []string{source}); err != nil {
			return err
		}
		if idx, err := openFTSIndex(out); err == nil {
			if rerr := idx.updateFTS(out, uniqueStrings(changedPaths)); rerr != nil {
				fmt.Fprintf(os.Stderr, "fts5: update failed: %v\n", rerr)
			}
			idx.close()
		}
		entry := logEntry{
			StartedAt:  now,
			FinishedAt: time.Now().UTC().Format(time.RFC3339),
			ElapsedMs:  time.Since(start).Milliseconds(),
			Mode:       mode,
			Args:       cmdArgs,
			Sources:    []string{source},
			URLs:       len(docs),
			Pulled:     copied,
		}
		if err := appendIngestLog(out, entry); err != nil {
			fmt.Fprintf(os.Stderr, "ingest-log: append failed: %v\n", err)
		}
		return nil
	})
	return copied, err
}

func sourceMarkdownPaths(srcDir, source string) []string {
	var paths []string
	_ = filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") || d.Name() == "_INDEX.md" {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return nil
		}
		paths = append(paths, source+"/"+filepath.ToSlash(rel))
		return nil
	})
	return paths
}

func ensureSourcePathInsideOut(out, srcDir string) error {
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	srcAbs, err := filepath.Abs(srcDir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(outAbs, srcAbs)
	if err != nil {
		return err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return fmt.Errorf("refusing to replace source outside out dir: %s", srcDir)
	}
	return nil
}

func cleanCrawlRel(rel string) (string, error) {
	rel = filepath.ToSlash(filepath.Clean(rel))
	rel = strings.TrimPrefix(rel, "/")
	if rel == "." || rel == "" || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		return "", fmt.Errorf("invalid generated doc path %q", rel)
	}
	if strings.HasSuffix(strings.ToLower(rel), ".mdx") {
		rel = strings.TrimSuffix(rel, rel[len(rel)-4:]) + ".md"
	}
	if !strings.HasSuffix(strings.ToLower(rel), ".md") {
		return "", fmt.Errorf("generated doc path must end in .md: %s", rel)
	}
	return rel, nil
}

func shouldSkipCrawlDir(name string) bool {
	if _, ok := skipDirs[name]; ok {
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case ".expo", ".local", ".ndev", ".vercel", ".sst", "coverage", "tmp", "temp", "_ignore_snapshots", "_ignore_tasks-workspace":
		return true
	}
	return false
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
