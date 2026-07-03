package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nstranquist/docs-puller/searchruntime"
)

type evalSuiteRow struct {
	Fixture string           `json:"fixture"`
	Summary evalSummary      `json:"summary"`
	Error   string           `json:"error,omitempty"`
	Results []evalCaseResult `json:"-"`
}

type evalSuiteReport struct {
	BaselinePath string               `json:"baseline_path,omitempty"`
	Rows         []evalSuiteRow       `json:"rows"`
	Diff         *evalSuiteDiffReport `json:"diff,omitempty"`
}

type evalSuiteDiffReport struct {
	Fixtures        []evalSuiteFixtureDiff `json:"fixtures"`
	MissingCurrent  []string               `json:"missing_current,omitempty"`
	MissingBaseline []string               `json:"missing_baseline,omitempty"`
}

type evalSuiteFixtureDiff struct {
	Fixture     string            `json:"fixture"`
	BaselineN   int               `json:"baseline_n"`
	CurrentN    int               `json:"current_n"`
	HitAt1Delta float64           `json:"hit_at_1_delta_pp"`
	HitAt5Delta float64           `json:"hit_at_5_delta_pp"`
	MRRDelta    float64           `json:"mrr_delta"`
	P50MSDelta  float64           `json:"p50_ms_delta"`
	P99MSDelta  float64           `json:"p99_ms_delta"`
	SourceDrift []evalSourceDrift `json:"source_drift,omitempty"`
}

func cmdEvalSuite(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("eval-suite", flag.ExitOnError)
	fixturesDir := fs.String("fixtures", defaultFixturesDir(), "directory containing *.yaml eval fixtures")
	includeFixture := fs.String("include-fixture", "", "comma-separated fixture basenames or paths to run; useful for frozen-baseline gates")
	excludeFixture := fs.String("exclude-fixture", "", "comma-separated fixture basenames or paths to skip")
	asJSON := fs.Bool("json", false, "emit JSON instead of human-readable")
	diffPath := fs.String("diff", "", "compare current suite summary against a prior eval-suite --json output")
	useScan := fs.Bool("scan", false, "force substring scan backend")
	limit := fs.Int("limit", 10, "search limit per query")
	tokenBudget := fs.Int("token-budget", defaultEvalTokenBudget, "approximate result-context token budget for token_budget_hit metrics (0 disables budget hits)")
	answerContext := fs.Bool("answer-context", false, "include full returned-document token counts in answer_context_* metrics")
	overviewMD := fs.String("overview-md", "", "write a Markdown retrieval metrics overview to this path")
	overviewHTML := fs.String("overview-html", "", "write a single-file HTML retrieval metrics overview to this path")
	rerankLLM := fs.Bool("rerank-llm", false, "run each fixture through the full rerank pipeline (LLM-as-judge cross-encoder). Without this flag, eval-suite is BM25-only.")
	rerankGate := fs.Float64("rerank-gate", 0.10, "with --rerank-llm: BM25 confidence margin to skip rerank (0 = always rerank)")
	rerankK := fs.Int("rerank-k", 10, "with --rerank-llm: BM25 candidate pool size before reranking")
	rerankHybrid := fs.Bool("rerank-hybrid", true, "with --rerank-llm: expand BM25 pool with embedding cosine top-K (mirrors search default)")
	rerankHyde := fs.Bool("rerank-hyde", true, "with --rerank-llm: HyDE query rewriting on the hybrid first-stage (mirrors search default)")
	bindOpts(fs, &o)
	fs.Parse(args)

	selection, err := parseEvalSuiteFixtureSelection(*includeFixture, *excludeFixture)
	if err != nil {
		die(err)
	}
	paths, err := evalSuiteFixturePathsForSelection(*fixturesDir, selection)
	if err != nil {
		die(err)
	}
	if len(paths) == 0 {
		die(fmt.Errorf("no eval fixtures found in %s", *fixturesDir))
	}
	// Build the per-query searchOpts from the rerank flags so each fixture
	// runs the same pipeline configuration. Without --rerank-llm this stays
	// a zero searchOpts (BM25-only, matches historical eval-suite behavior).
	var perQueryOpts searchOpts
	if *rerankLLM {
		perQueryOpts = searchOpts{
			rerankLLM:    true,
			rerankK:      *rerankK,
			rerankGate:   *rerankGate,
			rerankHybrid: *rerankHybrid,
			rerankHyde:   *rerankHyde,
		}
	}
	answerContextForRun := *answerContext || *overviewMD != "" || *overviewHTML != ""
	rows := runEvalSuite(paths, o.out, *useScan, *limit, *tokenBudget, answerContextForRun, perQueryOpts)
	hasErrors := evalSuiteHasErrors(rows)
	var diff *evalSuiteDiffReport
	if *diffPath != "" {
		diff, err = loadAndDiffEvalSuite(*diffPath, rows)
		if err != nil {
			die(searchruntime.EvalSuiteDiffError(*diffPath, err))
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if *diffPath == "" {
			_ = enc.Encode(rows)
		} else {
			_ = enc.Encode(evalSuiteReport{BaselinePath: *diffPath, Rows: rows, Diff: diff})
		}
		if hasErrors {
			os.Exit(1)
		}
		return
	}
	fmt.Printf("=== Eval suite (%d fixtures) ===\n", len(rows))
	fmt.Printf("%-28s %5s %7s %7s %7s %8s %8s\n", "fixture", "n", "hit@1", "hit@5", "mrr", "p50ms", "p99ms")
	for _, row := range rows {
		name := filepath.Base(row.Fixture)
		if row.Error != "" {
			fmt.Printf("%-28s ERROR %s\n", name, row.Error)
			continue
		}
		s := row.Summary
		fmt.Printf("%-28s %5d %7.1f%% %7.1f%% %7.3f %8.0f %8.0f\n",
			name, s.N, 100*s.HitAt1, 100*s.HitAt5, s.MRR, s.P50MS, s.P99MS)
	}
	if diff != nil {
		printEvalSuiteDiff(*diffPath, diff)
	}
	if *overviewMD != "" || *overviewHTML != "" {
		report := buildEvalSuiteOverview(rows, o.out, *limit, *tokenBudget, answerContextForRun, time.Now())
		if *overviewMD != "" {
			if err := writeEvalSuiteOverviewMarkdown(*overviewMD, report); err != nil {
				die(err)
			}
			fmt.Fprintf(os.Stderr, "eval-suite: wrote Markdown overview %s\n", *overviewMD)
		}
		if *overviewHTML != "" {
			if err := writeEvalSuiteOverviewHTML(*overviewHTML, report); err != nil {
				die(err)
			}
			fmt.Fprintf(os.Stderr, "eval-suite: wrote HTML overview %s\n", *overviewHTML)
		}
	}
	if hasErrors {
		os.Exit(1)
	}
}

func defaultFixturesDir() string {
	return defaultFixturesDirFromRoot(repoRoot(), func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.IsDir()
	})
}

func defaultFixturesDirFromRoot(root string, exists func(string) bool) string {
	if root == "" {
		root = "."
	}
	if exists == nil {
		exists = func(string) bool { return false }
	}
	candidates := []string{
		filepath.Join(root, "cmd", "docs-puller", "eval"),
		filepath.Join(root, "eval"),
		filepath.Join("cmd", "docs-puller", "eval"),
		filepath.Join("eval"),
	}
	for _, candidate := range candidates {
		if exists(candidate) {
			return candidate
		}
	}
	return candidates[0]
}

func evalSuiteFixturePaths(dir string) ([]string, error) {
	return evalSuiteFixturePathsForSelection(dir, evalSuiteFixtureSelection{})
}

type evalSuiteFixtureSelection struct {
	Include map[string]bool
	Exclude map[string]bool
}

func parseEvalSuiteFixtureSelection(includeCSV, excludeCSV string) (evalSuiteFixtureSelection, error) {
	include := parseEvalSuiteFixtureNameList(includeCSV)
	exclude := parseEvalSuiteFixtureNameList(excludeCSV)
	for name := range include {
		if exclude[name] {
			return evalSuiteFixtureSelection{}, searchruntime.EvalSuiteFixtureIncludeExcludeConflictError(name)
		}
	}
	return evalSuiteFixtureSelection{Include: include, Exclude: exclude}, nil
}

func parseEvalSuiteFixtureNameList(csv string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.Split(csv, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		out[filepath.Base(filepath.Clean(name))] = true
	}
	return out
}

func evalSuiteFixturePathsForSelection(dir string, selection evalSuiteFixtureSelection) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	out := matches[:0]
	matchedInclude := map[string]bool{}
	for _, p := range matches {
		base := filepath.Base(p)
		if strings.HasPrefix(base, ".") {
			continue
		}
		if len(selection.Include) > 0 {
			if !selection.Include[base] {
				continue
			}
			matchedInclude[base] = true
		}
		if selection.Exclude[base] {
			continue
		}
		// YAML safety check: drop a non-fixture YAML in eval/ and the
		// suite must skip it cleanly rather than erroring out the whole
		// run. loadFixture returns a structured error on malformed YAML
		// or empty queries, but it ALSO succeeds silently on a YAML with
		// no `queries:` key at all (just returns fix.Queries==nil). So
		// we check both: parse error OR zero queries → skip.
		fix, err := loadFixture(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "eval-suite: skipping %s (%v)\n", p, err)
			continue
		}
		if len(fix.Queries) == 0 {
			fmt.Fprintf(os.Stderr, "eval-suite: skipping %s (no queries:)\n", p)
			continue
		}
		out = append(out, p)
	}
	for name := range selection.Include {
		if !matchedInclude[name] {
			return nil, searchruntime.EvalSuiteIncludeFixtureNotFoundError(name, dir)
		}
	}
	return out, nil
}

func runEvalSuite(paths []string, outDir string, useScan bool, limit int, tokenBudget int, answerContext bool, perQueryOpts searchOpts) []evalSuiteRow {
	rows := make([]evalSuiteRow, 0, len(paths))
	for _, path := range paths {
		fix, err := loadFixture(path)
		if err != nil {
			rows = append(rows, evalSuiteRow{Fixture: path, Error: err.Error()})
			continue
		}
		applyDefaultQueryType(fix, path)
		results, summary := runEvalWithIdx(fix, outDir, useScan, limit, nil, perQueryOpts, tokenBudget, answerContext)
		rows = append(rows, evalSuiteRow{Fixture: path, Summary: summary, Results: results})
	}
	return rows
}

func evalSuiteHasErrors(rows []evalSuiteRow) bool {
	for _, row := range rows {
		if row.Error != "" {
			return true
		}
	}
	return false
}

func loadAndDiffEvalSuite(baselinePath string, current []evalSuiteRow) (*evalSuiteDiffReport, error) {
	body, err := os.ReadFile(baselinePath)
	if err != nil {
		return nil, err
	}
	var baseline []evalSuiteRow
	if err := json.Unmarshal(body, &baseline); err != nil {
		var report evalSuiteReport
		if reportErr := json.Unmarshal(body, &report); reportErr != nil {
			return nil, searchruntime.EvalSuiteParseError(err)
		}
		baseline = report.Rows
	}
	return diffEvalSuiteRows(baseline, current), nil
}

func diffEvalSuiteRows(baseline, current []evalSuiteRow) *evalSuiteDiffReport {
	baseByName := make(map[string]evalSuiteRow, len(baseline))
	for _, row := range baseline {
		baseByName[filepath.Base(row.Fixture)] = row
	}
	curByName := make(map[string]evalSuiteRow, len(current))
	for _, row := range current {
		curByName[filepath.Base(row.Fixture)] = row
	}
	names := make([]string, 0, len(baseByName)+len(curByName))
	seen := map[string]bool{}
	for name := range baseByName {
		seen[name] = true
		names = append(names, name)
	}
	for name := range curByName {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := &evalSuiteDiffReport{}
	for _, name := range names {
		base, hasBase := baseByName[name]
		cur, hasCur := curByName[name]
		switch {
		case !hasBase:
			out.MissingBaseline = append(out.MissingBaseline, name)
			continue
		case !hasCur:
			out.MissingCurrent = append(out.MissingCurrent, name)
			continue
		case base.Error != "" || cur.Error != "":
			continue
		}
		sourceDrift := buildEvalDriftReport(base.Summary, cur.Summary).SourceDrift
		out.Fixtures = append(out.Fixtures, evalSuiteFixtureDiff{
			Fixture:     name,
			BaselineN:   base.Summary.N,
			CurrentN:    cur.Summary.N,
			HitAt1Delta: (cur.Summary.HitAt1 - base.Summary.HitAt1) * 100,
			HitAt5Delta: (cur.Summary.HitAt5 - base.Summary.HitAt5) * 100,
			MRRDelta:    cur.Summary.MRR - base.Summary.MRR,
			P50MSDelta:  cur.Summary.P50MS - base.Summary.P50MS,
			P99MSDelta:  cur.Summary.P99MS - base.Summary.P99MS,
			SourceDrift: sourceDrift,
		})
	}
	return out
}

func printEvalSuiteDiff(baselinePath string, diff *evalSuiteDiffReport) {
	fmt.Printf("\n=== Suite diff %s → current ===\n", baselinePath)
	fmt.Printf("%-28s %9s %9s %9s %9s %9s\n", "fixture", "hit@1", "hit@5", "mrr", "p50ms", "p99ms")
	for _, row := range diff.Fixtures {
		fmt.Printf("%-28s %+8.1f %+8.1f %+9.3f %+9.0f %+9.0f\n",
			row.Fixture, row.HitAt1Delta, row.HitAt5Delta, row.MRRDelta, row.P50MSDelta, row.P99MSDelta)
		for _, drift := range row.SourceDrift {
			if drift.DeltaPP >= 0 {
				continue
			}
			fmt.Printf("  source %-22s hit@5 %5.1f%% → %5.1f%% (%+.1f pp)\n",
				drift.Source, 100*drift.BaselineHitAt5, 100*drift.CurrentHitAt5, drift.DeltaPP)
		}
	}
	for _, name := range diff.MissingCurrent {
		fmt.Printf("missing current fixture: %s\n", name)
	}
	for _, name := range diff.MissingBaseline {
		fmt.Printf("missing baseline fixture: %s\n", name)
	}
}
