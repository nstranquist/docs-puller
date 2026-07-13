package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Per-operation ingest log at <out>/_INGEST_LOG.jsonl. Append-only; each line
// records one `pull` / `pull-url` / `--local` / `--github-repo` invocation
// with timestamps, the original CLI args, the sources touched, and rollup
// counts.
//
// Distinct from manifest.json, which is per-URL current state. The log
// answers "when did I last ingest X, and what did that operation look like"
// without forcing a diff against prior manifest snapshots.

const ingestLogFile = "_INGEST_LOG.jsonl"

type logEntry struct {
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at"`
	ElapsedMs  int64    `json:"elapsed_ms"`
	Mode       string   `json:"mode"` // sitemap | llms-txt | gatsby-pagedata | from-file | pull-url | local | github-repo
	Args       []string `json:"args,omitempty"`
	Sources    []string `json:"sources,omitempty"`
	URLs       int      `json:"urls"`
	Pulled     int      `json:"pulled"`
	Unchanged  int      `json:"unchanged,omitempty"`
	Skipped    int      `json:"skipped"`
	Warned     int      `json:"warned,omitempty"`
}

// appendIngestLog appends one entry to <out>/_INGEST_LOG.jsonl. Caller is
// expected to hold the write lock so concurrent pull invocations don't
// interleave. Failure is non-fatal at the call site — log loss shouldn't
// block a successful pull.
func appendIngestLog(out string, e logEntry) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	path := filepath.Join(out, ingestLogFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(e)
}

// readIngestLog returns the most recent `limit` entries (newest first). A
// limit of 0 returns everything. Missing log returns (nil, nil).
func readIngestLog(out string, limit int) ([]logEntry, error) {
	path := filepath.Join(out, ingestLogFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []logEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e logEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// distinctSources returns the unique source names from a slice of results,
// sorted. Used for log entries on multi-source HTTP pulls.
func distinctSources(results []result) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range results {
		if r.Source == "" {
			continue
		}
		if _, ok := seen[r.Source]; ok {
			continue
		}
		seen[r.Source] = struct{}{}
		out = append(out, r.Source)
	}
	// Cheap sort without importing sort just for this — len is tiny (<=20).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func cmdLog(args []string) {
	o := defaultOpts()
	var (
		limit  int
		asJSON bool
	)
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.IntVar(&limit, "limit", 20, "max entries to show (0 = all)")
	fs.BoolVar(&asJSON, "json", false, "emit JSON instead of human-readable")
	fs.Parse(args)

	entries, err := readIngestLog(o.out, limit)
	if err != nil {
		die(err)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(entries); err != nil {
			die(err)
		}
		return
	}

	if len(entries) == 0 {
		fmt.Println("no ingest history at", filepath.Join(o.out, ingestLogFile))
		return
	}
	fmt.Printf("last %d ingest operations (%s):\n\n", len(entries), filepath.Join(o.out, ingestLogFile))
	for _, e := range entries {
		srcs := strings.Join(e.Sources, ",")
		if srcs == "" {
			srcs = "—"
		}
		dur := time.Duration(e.ElapsedMs) * time.Millisecond
		fmt.Printf("  %s  %-14s  %5d urls  %5d pulled  %4d skipped",
			e.StartedAt, e.Mode, e.URLs, e.Pulled, e.Skipped)
		if e.Warned > 0 {
			fmt.Printf("  %3d warned", e.Warned)
		}
		fmt.Printf("  in %6s  [%s]\n", dur.Round(time.Millisecond), srcs)
	}
}
