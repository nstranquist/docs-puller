// Package searchengine contains importable docs-puller dispatch engine pieces
// that no longer need cmd/docs-puller's package main.
package searchengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nstranquist/docs-puller/searchruntime"
)

const defaultTitleBoost = 5

// ScanOptions captures the concrete filesystem-scan dispatch knobs.
type ScanOptions struct {
	Root        string
	Source      string
	Limit       int
	Exact       bool
	TitleBoost  int
	MaxSnippets int
	SnippetLen  int
}

// ScanSharedIndex is the shared-index type for scan dispatch. It is empty
// because filesystem scan does not reuse an FTS handle.
type ScanSharedIndex struct{}

// ScanCallbacks provide source, title, URL, and snippet helpers for scan
// dispatch. Package main supplies its richer historical helpers; importers can
// use DefaultScanCallbacks when they only need the standalone scan engine.
type ScanCallbacks struct {
	ListSources     searchruntime.SourceLister
	LoadURLByPath   searchruntime.SourceURLLoader
	ExtractTitle    searchruntime.TitleExtractor
	ExtractSnippets searchruntime.SnippetExtractor
}

// NewScanSearcher returns an importable searchcore.Searcher backed by the
// concrete filesystem-scan dispatch path.
func NewScanSearcher(opts ScanOptions, callbacks ScanCallbacks) searchruntime.Searcher {
	return searchruntime.NewDispatchEngineSearcher(searchruntime.DispatchEngineConfig[ScanOptions, ScanSharedIndex]{
		BaseOptions:   RuntimeOptions(opts),
		EngineOptions: opts,
		ApplyOptions:  ApplyRuntimeOptions,
		Dispatch:      DispatchScan(callbacks),
	})
}

// RuntimeOptions returns the public query-overridable options for scan search.
func RuntimeOptions(opts ScanOptions) searchruntime.Options {
	return searchruntime.Options{Limit: opts.Limit, Source: opts.Source}
}

// ApplyRuntimeOptions maps public searchcore.Query overrides onto scan
// dispatch options.
func ApplyRuntimeOptions(opts ScanOptions, runtimeOpts searchruntime.Options) ScanOptions {
	opts.Limit = runtimeOpts.Limit
	opts.Source = runtimeOpts.Source
	return opts
}

// DispatchScan returns a dispatch callback suitable for
// searchruntime.NewDispatchEngineSearcher.
func DispatchScan(callbacks ScanCallbacks) searchruntime.DispatchEngineFunc[ScanOptions, ScanSharedIndex] {
	return func(ctx context.Context, req searchruntime.DispatchRequest[ScanOptions, ScanSharedIndex]) searchruntime.DispatchResult[searchruntime.Hit] {
		if err := ctx.Err(); err != nil {
			return searchruntime.DispatchResult[searchruntime.Hit]{}
		}
		return RunScan(req.Query, req.Opts, callbacks)
	}
}

// RunScan executes the concrete filesystem-scan dispatch path.
func RunScan(query string, opts ScanOptions, callbacks ScanCallbacks) searchruntime.DispatchResult[searchruntime.Hit] {
	titleBoost := opts.TitleBoost
	if titleBoost == 0 {
		titleBoost = defaultTitleBoost
	}
	extractSnippets := callbacks.ExtractSnippets
	if extractSnippets != nil && (opts.MaxSnippets > 0 || opts.SnippetLen > 0) {
		extractSnippets = func(body string, queryLower string, maxN int, snippetLen int) []searchruntime.Snippet {
			if opts.MaxSnippets > 0 {
				maxN = opts.MaxSnippets
			}
			if opts.SnippetLen > 0 {
				snippetLen = opts.SnippetLen
			}
			return callbacks.ExtractSnippets(body, queryLower, maxN, snippetLen)
		}
	}
	hits, scanned := searchruntime.RunScan(searchruntime.ScanInput{
		Query: query,
		Options: searchruntime.ScanOptions{
			Root:       opts.Root,
			Source:     opts.Source,
			Limit:      opts.Limit,
			Exact:      opts.Exact,
			TitleBoost: titleBoost,
		},
		ListSources:     callbacks.ListSources,
		LoadURLByPath:   callbacks.LoadURLByPath,
		ExtractTitle:    callbacks.ExtractTitle,
		ExtractSnippets: extractSnippets,
	})
	return searchruntime.DispatchResult[searchruntime.Hit]{
		Hits:    hits,
		Scanned: scanned,
		Mode:    "scan",
	}
}

// DefaultScanCallbacks returns standalone helpers for importers that cannot
// reach cmd/docs-puller's package-main helpers.
func DefaultScanCallbacks() ScanCallbacks {
	return ScanCallbacks{
		ListSources:     ListSources,
		LoadURLByPath:   LoadURLByPath,
		ExtractTitle:    ExtractTitle,
		ExtractSnippets: ExtractSnippets,
	}
}

// ListSources returns visible source directories under root.
func ListSources(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sources []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		sources = append(sources, name)
	}
	sort.Strings(sources)
	return sources, nil
}

// LoadURLByPath loads source-relative markdown paths to canonical URLs from
// docs-puller's manifest.json shape.
func LoadURLByPath(sourceDir string, sourceName string) map[string]string {
	urlByPath := map[string]string{}
	data, err := os.ReadFile(filepath.Join(sourceDir, "manifest.json"))
	if err != nil {
		return urlByPath
	}
	var manifest struct {
		Entries map[string]struct {
			URL  string `json:"url"`
			Path string `json:"path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return urlByPath
	}
	prefix := sourceName + "/"
	for _, entry := range manifest.Entries {
		rel := strings.TrimPrefix(entry.Path, prefix)
		if rel == "" || rel == entry.Path {
			continue
		}
		urlByPath[rel] = entry.URL
	}
	return urlByPath
}

// ExtractTitle returns the first markdown H1, falling back to the filename.
func ExtractTitle(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
		}
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return strings.TrimSpace(strings.ReplaceAll(base, "-", " "))
}

// ExtractSnippets returns line snippets ranked by query-token coverage.
func ExtractSnippets(body string, queryLower string, maxN int, snippetLen int) []searchruntime.Snippet {
	if maxN <= 0 {
		maxN = 3
	}
	if snippetLen <= 0 {
		snippetLen = 160
	}
	if snippetLen < 4 {
		snippetLen = 4
	}
	tokens := strings.Fields(queryLower)
	if len(tokens) == 0 {
		return nil
	}
	type candidate struct {
		snippet searchruntime.Snippet
		score   int
	}
	var candidates []candidate
	for i, line := range strings.Split(body, "\n") {
		lower := strings.ToLower(line)
		score := 0
		for _, token := range tokens {
			if strings.Contains(lower, token) {
				score++
			}
		}
		if score == 0 {
			continue
		}
		text := strings.TrimSpace(line)
		if len(text) > snippetLen {
			text = text[:snippetLen-3] + "..."
		}
		candidates = append(candidates, candidate{
			snippet: searchruntime.Snippet{Line: i + 1, Text: text},
			score:   score,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].snippet.Line < candidates[j].snippet.Line
	})
	if len(candidates) > maxN {
		candidates = candidates[:maxN]
	}
	out := make([]searchruntime.Snippet, len(candidates))
	for i, candidate := range candidates {
		out[i] = candidate.snippet
	}
	return out
}
