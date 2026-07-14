package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nstranquist/docs-puller/searchruntime"
	"gopkg.in/yaml.v3"
)

type queryLogEntry = searchruntime.SearchTelemetryEntry

func cmdTelemetry(args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, searchruntime.SearchTelemetryUsage())
		os.Exit(2)
	}
	switch args[0] {
	case "log":
		cmdTelemetryLog(args[1:])
	case "summary":
		cmdTelemetrySummary(args[1:])
	case "fixture":
		cmdTelemetryFixture(args[1:])
	case "-h", "--help", "help":
		fmt.Print(searchruntime.SearchTelemetryUsage())
	default:
		fmt.Fprint(os.Stderr, searchruntime.SearchTelemetryUnknownSubcommandMessage(args[0]))
		os.Exit(2)
	}
}

// shouldLogSearchQuery decides whether the current search call should be
// appended to query-log.jsonl. As of 2026-05-04 the default is ON; the
// env var DOCS_PULLER_QUERY_LOG can hard-disable. The order is:
//  1. DOCS_PULLER_QUERY_LOG=0|false|no  → never log (env wins)
//  2. --log-query=false                 → don't log
//  3. anything else                     → log (default ON)
func shouldLogSearchQuery(o searchOpts) bool {
	return searchruntime.ShouldLogSearchQuery(o.logQuery, os.Getenv("DOCS_PULLER_QUERY_LOG"))
}

func newSearchQueryLogEntry(query string, scanned int, hits []searchHit, mode string, o searchOpts) queryLogEntry {
	return searchruntime.NewSearchTelemetryEntry(searchruntime.SearchTelemetryInput{
		Timestamp:    time.Now(),
		Query:        query,
		Intent:       o.queryIntent,
		Client:       o.queryClient,
		RunContext:   o.queryRunContext,
		SourceFilter: o.source,
		Mode:         mode,
		Scanned:      scanned,
		Limit:        o.limit,
		Output:       searchOutputOptions(o),
		Hits:         hits,
		Rerank:       o.rerank,
		RerankLLM:    o.rerankLLM,
		RerankHybrid: o.rerankHybrid,
	})
}

type queryTelemetryBucket struct {
	TrafficClass  string  `json:"traffic_class"`
	Entries       int     `json:"entries"`
	UniqueQueries int     `json:"unique_queries"`
	ZeroResults   int     `json:"zero_results"`
	ZeroRatePct   float64 `json:"zero_rate_pct"`
}

type queryTelemetrySummary struct {
	SchemaVersion int                    `json:"schema_version"`
	TotalEntries  int                    `json:"total_entries"`
	UniqueQueries int                    `json:"unique_queries"`
	ByClass       []queryTelemetryBucket `json:"by_traffic_class"`
}

func summarizeQueryTelemetry(entries []queryLogEntry) queryTelemetrySummary {
	type accumulator struct {
		entries int
		zero    int
		queries map[string]bool
	}
	allQueries := map[string]bool{}
	byClass := map[string]*accumulator{}
	for _, entry := range entries {
		class := searchruntime.SearchTelemetryEntryTrafficClass(entry)
		bucket := byClass[class]
		if bucket == nil {
			bucket = &accumulator{queries: map[string]bool{}}
			byClass[class] = bucket
		}
		bucket.entries++
		if entry.ResultCount == 0 {
			bucket.zero++
		}
		key := searchruntime.SearchTelemetryQueryKey(entry.Query)
		if key != "" {
			bucket.queries[key] = true
			allQueries[key] = true
		}
	}
	summary := queryTelemetrySummary{SchemaVersion: 1, TotalEntries: len(entries), UniqueQueries: len(allQueries)}
	for _, class := range []string{"real", "synthetic", "unknown"} {
		bucket := byClass[class]
		if bucket == nil {
			bucket = &accumulator{queries: map[string]bool{}}
		}
		zeroRate := 0.0
		if bucket.entries > 0 {
			zeroRate = float64(bucket.zero) * 100 / float64(bucket.entries)
		}
		summary.ByClass = append(summary.ByClass, queryTelemetryBucket{
			TrafficClass:  class,
			Entries:       bucket.entries,
			UniqueQueries: len(bucket.queries),
			ZeroResults:   bucket.zero,
			ZeroRatePct:   zeroRate,
		})
	}
	return summary
}

func cmdTelemetrySummary(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("telemetry summary", flag.ExitOnError)
	limit := fs.Int("limit", 0, "max newest entries to summarize (0 = all)")
	asJSON := fs.Bool("json", false, "emit JSON instead of human-readable")
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.Parse(args)
	entries, err := readSearchQueryLog(o.out, *limit)
	if err != nil {
		die(err)
	}
	summary := summarizeQueryTelemetry(entries)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(summary); err != nil {
			die(searchruntime.SearchTelemetryJSONEncodeError(err))
		}
		return
	}
	fmt.Printf("telemetry summary: entries=%d unique_queries=%d\n", summary.TotalEntries, summary.UniqueQueries)
	for _, bucket := range summary.ByClass {
		fmt.Printf("  %-9s entries=%d unique=%d zero_results=%d (%.1f%%)\n", bucket.TrafficClass, bucket.Entries, bucket.UniqueQueries, bucket.ZeroResults, bucket.ZeroRatePct)
	}
}

func appendSearchQueryLog(out string, e queryLogEntry) error {
	return withWriteLock(out, func() error {
		path := searchruntime.SearchTelemetryQueryLogPath(out)
		cacheDir := filepath.Dir(path)
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return searchruntime.SearchTelemetryQueryLogCreateDirError(cacheDir, err)
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return searchruntime.SearchTelemetryQueryLogAppendOpenError(path, err)
		}
		if err := json.NewEncoder(f).Encode(e); err != nil {
			closeErr := f.Close()
			return searchruntime.SearchTelemetryQueryLogEncodeError(err, closeErr)
		}
		if err := f.Close(); err != nil {
			return searchruntime.SearchTelemetryQueryLogCloseError(err)
		}
		return nil
	})
}

func readSearchQueryLog(out string, limit int) ([]queryLogEntry, error) {
	path := searchruntime.SearchTelemetryQueryLogPath(out)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []queryLogEntry{}, nil
		}
		return nil, searchruntime.SearchTelemetryQueryLogOpenError(path, err)
	}
	var entries []queryLogEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e queryLogEntry
		if err := json.Unmarshal(line, &e); err == nil && e.Query != "" {
			entries = append(entries, e)
		}
	}
	scanErr := scanner.Err()
	closeErr := f.Close()
	if err := searchruntime.SearchTelemetryQueryLogReadError(scanErr, closeErr); err != nil {
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

func cmdTelemetryLog(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("telemetry log", flag.ExitOnError)
	limit := fs.Int("limit", 50, "max entries to show (0 = all)")
	asJSON := fs.Bool("json", false, "emit JSON instead of human-readable")
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.Parse(args)

	entries, err := readSearchQueryLog(o.out, *limit)
	if err != nil {
		die(err)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(entries); err != nil {
			die(searchruntime.SearchTelemetryJSONEncodeError(err))
		}
		return
	}
	if len(entries) == 0 {
		fmt.Print(searchruntime.SearchTelemetryEmptyLogMessage(searchruntime.SearchTelemetryQueryLogPath(o.out)))
		return
	}
	for _, e := range entries {
		fmt.Print(searchruntime.SearchTelemetryLogRow(e))
	}
}

func cmdTelemetryFixture(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("telemetry fixture", flag.ExitOnError)
	limit := fs.Int("limit", 200, "max telemetry entries to consider (0 = all)")
	intent := fs.String("intent", "", "only include entries with this intent label")
	trafficClass := fs.String("traffic-class", "real", "only include real|synthetic|unknown traffic; use all for legacy behavior")
	since := fs.String("since", "", "only include entries at or after this RFC3339 timestamp")
	outFile := fs.String("out-file", "", "fixture YAML path to write")
	excludeFixture := fs.String("exclude-fixture", "", "skip queries already present (exact, case-insensitive) in this fixture YAML; pass multiple times via comma-separated list. Use to avoid re-sampling eval-fixture queries that polluted the telemetry log.")
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.Parse(args)
	normalizedTrafficClass, err := searchruntime.ParseSearchTelemetryTrafficClassFilter(*trafficClass)
	if err != nil {
		die(err)
	}
	if *outFile == "" {
		fmt.Fprint(os.Stderr, searchruntime.SearchTelemetryFixtureOutFileRequiredMessage())
		os.Exit(2)
	}
	entries, err := readSearchQueryLog(o.out, *limit)
	if err != nil {
		die(err)
	}
	var sinceTime time.Time
	if *since != "" {
		sinceTime, err = time.Parse(time.RFC3339, *since)
		if err != nil {
			die(searchruntime.SearchTelemetrySinceParseError(err))
		}
	}
	excludeQueries := map[string]bool{}
	if *excludeFixture != "" {
		for _, path := range strings.Split(*excludeFixture, ",") {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			fix, err := loadFixture(path)
			if err != nil {
				die(searchruntime.SearchTelemetryExcludeFixtureLoadError(path, err))
			}
			for _, q := range fix.Queries {
				excludeQueries[searchruntime.SearchTelemetryQueryKey(q.Q)] = true
			}
		}
	}
	fixture := fixtureFromTelemetry(entries, *intent, normalizedTrafficClass, sinceTime, excludeQueries)
	if len(fixture.Queries) == 0 {
		die(searchruntime.SearchTelemetryFixtureNoMatchesError())
	}
	sort.SliceStable(fixture.Queries, func(i, j int) bool { return fixture.Queries[i].Q < fixture.Queries[j].Q })
	outDir := filepath.Dir(*outFile)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		die(searchruntime.SearchTelemetryFixtureCreateDirError(outDir, err))
	}
	f, err := os.Create(*outFile)
	if err != nil {
		die(searchruntime.SearchTelemetryFixtureCreateFileError(*outFile, err))
	}
	enc := yaml.NewEncoder(f)
	enc.SetIndent(2)
	if err := enc.Encode(fixture); err != nil {
		closeErr := f.Close()
		die(searchruntime.SearchTelemetryFixtureEncodeError(err, closeErr))
	}
	if err := f.Close(); err != nil {
		die(searchruntime.SearchTelemetryFixtureCloseError(err))
	}
	fmt.Print(searchruntime.SearchTelemetryFixtureWrittenMessage(len(fixture.Queries), *outFile))
}

func fixtureFromTelemetry(entries []queryLogEntry, intent, trafficClass string, since time.Time, exclude map[string]bool) evalFixture {
	var fixture evalFixture
	for _, candidate := range searchruntime.SearchTelemetryFixtureQueries(searchruntime.SearchTelemetryFixtureInput{
		Entries:      entries,
		Intent:       intent,
		TrafficClass: trafficClass,
		Since:        since,
		Exclude:      exclude,
	}) {
		q := evalQuery{
			Q:      candidate.Query,
			Expect: candidate.Expect,
			Note:   candidate.Note,
		}
		if candidate.Source != "" {
			q.Source = candidate.Source
		}
		fixture.Queries = append(fixture.Queries, q)
	}
	return fixture
}
