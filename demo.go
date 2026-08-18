package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	demoSourceName = "docs-puller-demo"
	demoQuery      = "how do I create an FTS5 virtual table"
	demoOriginBase = "https://github.com/nstranquist/docs-puller/blob/main/demo_data"
)

//go:embed demo_data/*.md
var demoData embed.FS

type demoHit struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

type demoResult struct {
	SchemaVersion int       `json:"schema_version"`
	OK            bool      `json:"ok"`
	Query         string    `json:"query"`
	Mode          string    `json:"mode"`
	Documents     int       `json:"documents"`
	Scanned       int       `json:"scanned"`
	Results       []demoHit `json:"results"`
	Ephemeral     bool      `json:"ephemeral"`
	CorpusRoot    string    `json:"corpus_root,omitempty"`
	ElapsedMs     float64   `json:"elapsed_ms"`
}

func cmdDemo(args []string) {
	flags := flag.NewFlagSet("demo", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	query := flags.String("query", demoQuery, "query to run against the built-in sample")
	out := flags.String("out", "", "keep the demo corpus at this path instead of using an ephemeral directory")
	keep := flags.Bool("keep", false, "keep the auto-created demo corpus and print its path")
	jsonOut := flags.Bool("json", false, "emit a stable JSON result")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: docs-puller demo [--query TEXT] [--out DIR] [--keep] [--json]")
		os.Exit(2)
	}

	result, err := runDemo(*out, *query, *keep)
	if err != nil {
		die(err)
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			die(err)
		}
		return
	}

	fmt.Printf("docs-puller demo: indexed %d docs and searched with %s\n", result.Documents, result.Mode)
	if len(result.Results) > 0 {
		fmt.Printf("top result: %s — %s\n", result.Results[0].Path, result.Results[0].Title)
	} else {
		fmt.Println("top result: none")
	}
	if result.CorpusRoot != "" {
		fmt.Printf("corpus kept at %s\n", result.CorpusRoot)
	}
}

func runDemo(out, query string, keep bool) (demoResult, error) {
	started := time.Now()
	result := demoResult{SchemaVersion: 1, Query: query}

	corpusRoot := out
	if corpusRoot == "" {
		var err error
		corpusRoot, err = os.MkdirTemp("", "docs-puller-demo-corpus-")
		if err != nil {
			return demoResult{}, fmt.Errorf("create demo corpus: %w", err)
		}
		result.Ephemeral = !keep
		if keep {
			result.CorpusRoot = corpusRoot
		} else {
			defer os.RemoveAll(corpusRoot)
		}
	} else {
		abs, err := filepath.Abs(corpusRoot)
		if err != nil {
			return demoResult{}, fmt.Errorf("resolve demo output: %w", err)
		}
		corpusRoot = abs
		result.CorpusRoot = corpusRoot
	}

	inputRoot, err := os.MkdirTemp("", "docs-puller-demo-input-")
	if err != nil {
		return demoResult{}, fmt.Errorf("create demo input: %w", err)
	}
	defer os.RemoveAll(inputRoot)
	if err := materializeDemoData(inputRoot); err != nil {
		return demoResult{}, err
	}

	summary, err := ingestLocalBatch(
		[]localBatchSource{{name: demoSourceName, walkRoot: inputRoot, originBase: demoOriginBase}},
		pullOpts{out: corpusRoot, sourceCache: filepath.Join(corpusRoot, ".cache"), concurrency: 1},
		[]string{"demo"},
		nil,
	)
	if err != nil {
		return demoResult{}, fmt.Errorf("ingest demo corpus: %w", err)
	}

	hits, scanned, mode := dispatchSearch(query, searchOpts{
		out:             corpusRoot,
		source:          demoSourceName,
		requestedSource: demoSourceName,
		limit:           3,
		noProfile:       true,
		ftsOnly:         true,
	}, nil)
	result.Mode = mode
	result.Documents = summary.FTSDocCount
	result.Scanned = scanned
	result.Results = make([]demoHit, 0, len(hits))
	for _, hit := range hits {
		result.Results = append(result.Results, demoHit{Path: hit.Path, Title: hit.Title, URL: hit.URL})
	}
	result.OK = mode == "fts5" && summary.FTSDocCount == 3 && len(hits) > 0
	result.ElapsedMs = durationMillis(time.Since(started))
	if !result.OK {
		return result, fmt.Errorf("demo did not produce an indexed FTS5 result")
	}
	return result, nil
}

func materializeDemoData(dst string) error {
	entries, err := fs.ReadDir(demoData, "demo_data")
	if err != nil {
		return fmt.Errorf("read embedded demo data: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := demoData.ReadFile("demo_data/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read embedded demo file %s: %w", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), body, 0o644); err != nil {
			return fmt.Errorf("write demo file %s: %w", entry.Name(), err)
		}
	}
	return nil
}
