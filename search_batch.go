package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nstranquist/docs-puller/searchruntime"
)

type searchBatchQuery struct {
	Query  string `json:"query"`
	Q      string `json:"q,omitempty"`
	Source string `json:"source,omitempty"`
}

type searchBatchResult struct {
	Query   string      `json:"query"`
	Source  string      `json:"source,omitempty"`
	Mode    string      `json:"mode"`
	Scanned int         `json:"scanned"`
	Results []searchHit `json:"results"`
}

type searchBatchResponse struct {
	QueryCount   int                 `json:"query_count"`
	ReadMode     string              `json:"read_mode"`
	QueryInputMS float64             `json:"query_input_ms"`
	IndexLoadMS  float64             `json:"index_load_ms"`
	SearchMS     float64             `json:"search_ms"`
	Results      []searchBatchResult `json:"results"`
}

func cmdSearchBatch(args []string) {
	o := searchOpts{limit: 10, noSnippets: true, noProfile: true, ftsOnly: true}
	home, err := os.UserHomeDir()
	if err == nil {
		o.out = filepath.Join(home, "code", "docs")
	}

	fs := flag.NewFlagSet("search-batch", flag.ExitOnError)
	queriesPath := fs.String("queries", "", "JSON array of query strings or objects with query/q fields")
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.StringVar(&o.source, "source", "", "limit to one source")
	fs.IntVar(&o.limit, "limit", o.limit, "max results to return per query")
	fs.BoolVar(&o.exact, "exact", false, "treat each query as one adjacent phrase")
	fs.BoolVar(&o.noSnippets, "no-snippets", true, "skip snippet extraction")
	fs.StringVar(&o.flagProfile, "profile", "", "active profile name")
	fs.BoolVar(&o.noProfile, "no-profile", true, "ignore active profile")
	fs.BoolVar(&o.strict, "strict", false, "with active profile: hard-filter to profile-matched docs only")
	fs.StringVar(&o.ftsReadMode, "read-mode", ftsReadModeReadOnly, "shared FTS read mode: ro or immutable (benchmark-only)")
	fs.BoolVar(&o.logQuery, "log-query", true, "append each query to <out>/.cache/query-log.jsonl; disable per-call with --log-query=false or globally with DOCS_PULLER_QUERY_LOG=0")
	fs.StringVar(&o.queryIntent, "intent", "", "with --log-query: short intent label for later fixture curation")
	fs.StringVar(&o.queryClient, "client", strings.TrimSpace(os.Getenv("DOCS_PULLER_QUERY_CLIENT")), "with --log-query: caller id (or DOCS_PULLER_QUERY_CLIENT)")
	fs.StringVar(&o.queryRunContext, "run-context", strings.TrimSpace(os.Getenv("DOCS_PULLER_RUN_CONTEXT")), "with --log-query: operator|agent|mcp|production|eval|test|benchmark|batch (or DOCS_PULLER_RUN_CONTEXT)")
	fs.Parse(args)

	if *queriesPath == "" {
		fmt.Fprintln(os.Stderr, "search-batch: --queries required")
		os.Exit(2)
	}

	inputStart := time.Now()
	queries, err := loadSearchBatchQueries(*queriesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "search-batch: %v\n", err)
		os.Exit(2)
	}
	queryInputMS := elapsedMillis(inputStart)

	resolveSearchProfile(&o)

	response, err := runSearchBatch(o, queries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "search-batch: %v\n", err)
		os.Exit(2)
	}
	response.QueryInputMS = queryInputMS
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(response); err != nil {
		fmt.Fprintf(os.Stderr, "search-batch: encode response: %v\n", err)
		os.Exit(2)
	}
}

func runSearchBatch(o searchOpts, queries []searchBatchQuery) (searchBatchResponse, error) {
	indexReadyTimeout := 30 * time.Second
	indexStart := time.Now()
	if !waitForFTSReady(o.out, indexReadyTimeout) {
		return searchBatchResponse{}, searchruntime.SearchBatchFTSNotReadyError(indexReadyTimeout)
	}
	readMode := normalizeFTSReadMode(o.ftsReadMode)
	idx, err := openFTSIndexReadMode(o.out, readMode)
	if err != nil {
		return searchBatchResponse{}, searchruntime.SearchBatchFTSIndexOpenError(err)
	}
	defer idx.close()
	indexLoadMS := elapsedMillis(indexStart)
	totalDocs, err := idx.totalDocs()
	if err != nil {
		return searchBatchResponse{}, searchruntime.SearchBatchFTSDocCountError(err)
	}

	results := make([]searchBatchResult, 0, len(queries))
	searchStart := time.Now()
	for _, q := range queries {
		query := q.Query
		if query == "" {
			query = q.Q
		}
		if query == "" {
			return searchBatchResponse{}, searchruntime.SearchBatchEmptyQueryError()
		}
		perQueryOpts := o
		if q.Source != "" {
			perQueryOpts.source = q.Source
		}
		perQueryOpts.cachedFTSTotalDocs = totalDocs
		hits, scanned, mode := dispatchSearch(query, perQueryOpts, idx)
		if mode != "fts5" {
			return searchBatchResponse{}, searchruntime.SearchBatchNonFTSModeError(query)
		}
		if shouldLogSearchQuery(perQueryOpts) {
			if err := appendSearchQueryLog(perQueryOpts.out, newSearchQueryLogEntry(query, scanned, hits, mode, perQueryOpts)); err != nil {
				fmt.Fprint(os.Stderr, searchruntime.QueryLogAppendFailedWarning(err))
			}
		}
		results = append(results, searchBatchResult{
			Query:   query,
			Source:  perQueryOpts.source,
			Mode:    mode,
			Scanned: scanned,
			Results: hits,
		})
	}
	searchMS := elapsedMillis(searchStart)

	return searchBatchResponse{
		QueryCount:  len(queries),
		ReadMode:    readMode,
		IndexLoadMS: indexLoadMS,
		SearchMS:    searchMS,
		Results:     results,
	}, nil
}

func loadSearchBatchQueries(path string) ([]searchBatchQuery, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var objects []searchBatchQuery
	if err := json.Unmarshal(data, &objects); err == nil {
		return objects, nil
	}

	var strings []string
	if err := json.Unmarshal(data, &strings); err != nil {
		return nil, err
	}
	queries := make([]searchBatchQuery, 0, len(strings))
	for _, query := range strings {
		queries = append(queries, searchBatchQuery{Query: query})
	}
	return queries, nil
}

func elapsedMillis(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}
