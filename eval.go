package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nstranquist/docs-puller/searchruntime"
	"gopkg.in/yaml.v3"
)

// eval runs a fixture of (query, expected-paths) pairs through the same
// dispatchSearch the CLI uses, then computes Hit@K, MRR, recall, and
// latency. Output is a per-query table + per-source aggregate + overall
// summary.
//
// Output flag matrix:
//
//	--json              emit one JSON object {summary, results} to stdout
//	                    (suitable as a baseline for --diff)
//	--write-baseline P  write the same {summary, results} object to file P
//	                    (the natural --write-baseline + --diff workflow)
//	--write P           append a {timestamp, summary} JSONL row to file P
//	                    (summary-only, designed for time-series tracking)
//	--diff P            compare current run against a {summary, results}
//	                    baseline previously emitted by --json or
//	                    --write-baseline (NOT compatible with --write)
//
// The fixture is the load-bearing artifact — see eval/fixture.yaml.
const defaultEvalTokenBudget = 4000

type evalQuery struct {
	Q         string   `yaml:"q"      json:"q"`
	Expect    []string `yaml:"expect" json:"expect"`
	Source    string   `yaml:"source,omitempty" json:"source,omitempty"`
	QueryType string   `yaml:"query_type,omitempty" json:"query_type,omitempty"`
	Note      string   `yaml:"note,omitempty"   json:"note,omitempty"`
}

type evalFixture struct {
	QueryType string      `yaml:"query_type,omitempty" json:"query_type,omitempty"`
	Queries   []evalQuery `yaml:"queries" json:"queries"`
}

type evalCaseResult = searchruntime.EvalCaseResult
type evalSummary = searchruntime.EvalSummary

type evalRunReport = searchruntime.EvalRunReport
type evalSourceDrift = searchruntime.EvalSourceDrift

type evalDriftReport struct {
	SourceDrift []evalSourceDrift
}

func cmdEval(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	fixturePath := fs.String("fixture", "", "fixture YAML path (default: eval/fixture.yaml in this repository)")
	asJSON := fs.Bool("json", false, "emit per-case + summary JSON instead of human-readable")
	useScan := fs.Bool("scan", false, "force substring scan backend (compares scan against FTS5)")
	limit := fs.Int("limit", 10, "search limit per query")
	tokenBudget := fs.Int("token-budget", defaultEvalTokenBudget, "approximate result-context token budget for token_budget_hit metrics (0 disables budget hits)")
	answerContext := fs.Bool("answer-context", false, "include full returned-document token counts in answer_context_* metrics")
	verbose := fs.Bool("verbose", false, "print every case (default: only misses)")
	writeSummary := fs.String("write", "", "append {timestamp, summary} as JSONL to this path (for time-series tracking; summary-only — use --write-baseline for a --diff-compatible artifact)")
	writeBaseline := fs.String("write-baseline", "", "write the full {summary, results} JSON to this path (suitable as input to --diff)")
	diffPath := fs.String("diff", "", "compare current run against a previous --json or --write-baseline output (per-query rank deltas + metric drift)")
	recordRun := fs.Bool("record-run", false, "write a stable report to ~/.docs-puller/evals/runs/<timestamp>/report.json and update latest.json")
	runRoot := fs.String("run-root", "", "override eval run artifact root (default: ~/.docs-puller/evals; legacy: DOCS_PULLER_LEGACY_NDEV_PATHS=1 → ~/.nicos-dev/evals/docs-puller)")
	checkFixture := fs.Bool("check-fixture", false, "verify every fixture expect: path exists in the corpus; skip running search")
	bySource := fs.Bool("by-source", false, "run each source's queries independently and print a per-source summary (measures source-internal ranking quality without cross-source interference)")
	profileName := fs.String("profile", "", "active profile name — applies the rerank boost (and --strict if set) so eval can measure profile-on impact against a no-profile baseline")
	strictProfile := fs.Bool("strict", false, "with --profile: hard-filter to profile-matched docs only (mirrors search --strict)")
	rerank := fs.Bool("rerank", false, "rerank BM25 top-K by OpenAI embedding cosine (mirrors search --rerank). Requires `embed` to have run.")
	rerankK := fs.Int("rerank-k", searchruntime.DefaultRerankK, "with --rerank: BM25 candidate pool size before reranking")
	rerankModel := fs.String("rerank-model", searchruntime.DefaultEmbeddingModel, "with --rerank: embedding model")
	rerankGate := fs.Float64("rerank-gate", 0, "with --rerank: skip rerank when BM25 top-1 exceeds top-2 by this fraction (mirrors search --rerank-gate)")
	rerankChunkSize := fs.Int("rerank-chunk-size", 0, "with --rerank: chunked-embedding rerank at this chunk_size (mirrors search --rerank-chunk-size)")
	rerankLLM := fs.Bool("rerank-llm", false, "use LLM-as-judge rerank (cross-encoder) — see search --rerank-llm")
	rerankLLMModel := fs.String("rerank-llm-model", searchruntime.DefaultLLMRerankModel, "with --rerank-llm: chat model name")
	rerankLLMProvider := fs.String("rerank-llm-provider", searchruntime.DefaultRerankProvider, "with --rerank-llm: provider (openai|xai)")
	rerankHybrid := fs.Bool("rerank-hybrid", true, "expand BM25 candidate pool with embedding cosine top-K before rerank (mirrors search --rerank-hybrid; default ON since 2026-05-04)")
	rerankHyde := fs.Bool("rerank-hyde", true, "use HyDE query rewriting on the hybrid first-stage embedding (mirrors search --rerank-hyde; default ON since 2026-05-04)")
	bindOpts(fs, &o)
	fs.Parse(args)

	var profileOpts searchOpts
	if *profileName != "" {
		p, err := LoadProfile(*profileName, o.out)
		if err != nil {
			die(searchruntime.EvalProfileLoadError(*profileName, err))
		}
		profileOpts.profile = p
		profileOpts.resolvedProfile = p.Name
		profileOpts.profileReason = "flag-explicit"
		profileOpts.strict = *strictProfile
	}
	profileOpts.rerank = *rerank
	profileOpts.rerankK = *rerankK
	profileOpts.rerankModel = *rerankModel
	profileOpts.rerankGate = *rerankGate
	profileOpts.rerankChunkSize = *rerankChunkSize
	profileOpts.rerankLLM = *rerankLLM
	profileOpts.rerankLLMModel = *rerankLLMModel
	profileOpts.rerankLLMProvider = *rerankLLMProvider
	profileOpts.rerankHybrid = *rerankHybrid
	profileOpts.rerankHyde = *rerankHyde

	path := *fixturePath
	if path == "" {
		path = defaultFixturePath()
	}
	fix, err := loadFixture(path)
	if err != nil {
		die(searchruntime.EvalFixtureLoadError(path, err))
	}
	if len(fix.Queries) == 0 {
		die(searchruntime.EvalFixtureNoQueriesError(path))
	}
	applyDefaultQueryType(fix, path)

	if *checkFixture {
		runCheckFixture(fix, o.out)
		return
	}

	if *bySource {
		if profileOpts.profile != nil {
			fmt.Fprint(os.Stderr, searchruntime.EvalBySourceProfileIgnoredWarning())
		}
		runEvalBySource(fix, o.out, *useScan, *limit, *tokenBudget, *answerContext)
		return
	}

	results, summary := runEvalWithIdx(fix, o.out, *useScan, *limit, nil, profileOpts, *tokenBudget, *answerContext)

	var drift evalDriftReport
	if *diffPath != "" {
		var err error
		drift, err = emitEvalDiff(*diffPath, results, summary)
		if err != nil {
			die(searchruntime.EvalDiffError(*diffPath, err))
		}
	} else if *asJSON {
		if err := emitEvalJSON(os.Stdout, results, summary); err != nil {
			die(searchruntime.EvalJSONEncodeError(err))
		}
	} else {
		emitEvalText(results, summary, *verbose)
	}
	if *writeBaseline != "" {
		if err := writeBaselineJSON(*writeBaseline, results, summary); err != nil {
			fmt.Fprint(os.Stderr, searchruntime.EvalWriteBaselineWarning(err))
		}
	}
	if *writeSummary != "" {
		if err := appendSummaryJSONL(*writeSummary, summary); err != nil {
			fmt.Fprint(os.Stderr, searchruntime.EvalWriteSummaryWarning(err))
		}
	}
	if *recordRun {
		report := buildEvalRunReport(path, o.out, *limit, *tokenBudget, *diffPath, results, summary, drift)
		paths, err := writeEvalRunArtifactsForRecord(*runRoot, report)
		if err != nil {
			die(err)
		} else {
			fmt.Fprint(os.Stderr, searchruntime.EvalRecordRunReportWrittenMessage(paths.ReportPath))
			fmt.Fprint(os.Stderr, searchruntime.EvalRecordRunLatestUpdatedMessage(paths.LatestPath))
		}
	}
}

type evalSweepConfig struct {
	FixturePath     string
	Out             string
	UseScan         bool
	Limit           int
	BaselinePath    string
	Command         []string
	ProfileName     string
	StrictProfile   bool
	Rerank          bool
	RerankK         int
	RerankModel     string
	RerankGate      float64
	RerankChunkSize int
	ProfileOpts     searchOpts
	TokenBudget     int
	TokenBudgetSet  bool
	AnswerContext   bool
}

func cmdEvalSweep(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("eval-sweep", flag.ExitOnError)
	fixturePath := fs.String("fixture", "", "fixture YAML path (default: eval/fixture.yaml in this repository)")
	baselinePath := fs.String("baseline", "", "fresh baseline output path (default: <out>/.cache/eval-baseline-<timestamp>.json)")
	useScan := fs.Bool("scan", false, "force substring scan backend")
	limit := fs.Int("limit", 10, "search limit per query")
	tokenBudget := fs.Int("token-budget", defaultEvalTokenBudget, "approximate result-context token budget for token_budget_hit metrics (0 disables budget hits)")
	answerContext := fs.Bool("answer-context", false, "include full returned-document token counts in answer_context_* metrics")
	profileName := fs.String("profile", "", "active profile name for both baseline and diff")
	strictProfile := fs.Bool("strict", false, "with --profile: hard-filter to profile-matched docs only")
	rerank := fs.Bool("rerank", false, "rerank BM25 top-K by OpenAI embedding cosine for both baseline and diff")
	rerankK := fs.Int("rerank-k", searchruntime.DefaultRerankK, "with --rerank: BM25 candidate pool size before reranking")
	rerankModel := fs.String("rerank-model", searchruntime.DefaultEmbeddingModel, "with --rerank: embedding model")
	rerankGate := fs.Float64("rerank-gate", 0, "with --rerank: skip rerank when BM25 top-1 exceeds top-2 by this fraction")
	rerankChunkSize := fs.Int("rerank-chunk-size", 0, "with --rerank: chunked-embedding rerank at this chunk_size")
	bindOpts(fs, &o)
	fs.Parse(args)
	tokenBudgetSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "token-budget" {
			tokenBudgetSet = true
		}
	})

	command := fs.Args()
	if len(command) == 0 {
		fmt.Fprint(os.Stderr, searchruntime.EvalSweepCommandRequiredMessage())
		os.Exit(2)
	}

	path := *fixturePath
	if path == "" {
		path = defaultFixturePath()
	}
	baseline := *baselinePath
	if baseline == "" {
		baseline = searchruntime.DefaultEvalSweepBaselinePath(o.out, nil)
	}

	profileOpts, err := evalProfileOpts(*profileName, *strictProfile, *rerank, *rerankK, *rerankModel, *rerankGate, *rerankChunkSize, o.out)
	if err != nil {
		die(err)
	}
	cfg := evalSweepConfig{
		FixturePath:     path,
		Out:             o.out,
		UseScan:         *useScan,
		Limit:           *limit,
		BaselinePath:    baseline,
		Command:         command,
		ProfileName:     *profileName,
		StrictProfile:   *strictProfile,
		Rerank:          *rerank,
		RerankK:         *rerankK,
		RerankModel:     *rerankModel,
		RerankGate:      *rerankGate,
		RerankChunkSize: *rerankChunkSize,
		ProfileOpts:     profileOpts,
		TokenBudget:     *tokenBudget,
		TokenBudgetSet:  tokenBudgetSet,
		AnswerContext:   *answerContext,
	}
	if err := runEvalSweep(cfg, runEvalSweepCommand, runEvalSweepChildDiff); err != nil {
		die(err)
	}
}

func evalProfileOpts(profileName string, strictProfile, rerank bool, rerankK int, rerankModel string, rerankGate float64, rerankChunkSize int, out string) (searchOpts, error) {
	var profileOpts searchOpts
	if profileName != "" {
		p, err := LoadProfile(profileName, out)
		if err != nil {
			return profileOpts, searchruntime.EvalProfileLoadError(profileName, err)
		}
		profileOpts.profile = p
		profileOpts.resolvedProfile = p.Name
		profileOpts.profileReason = "flag-explicit"
		profileOpts.strict = strictProfile
	}
	profileOpts.rerank = rerank
	profileOpts.rerankK = rerankK
	profileOpts.rerankModel = rerankModel
	profileOpts.rerankGate = rerankGate
	profileOpts.rerankChunkSize = rerankChunkSize
	return profileOpts, nil
}

func runEvalSweep(cfg evalSweepConfig, runCommand func([]string) error, runDiff func(evalSweepConfig) error) error {
	fix, err := loadFixture(cfg.FixturePath)
	if err != nil {
		return searchruntime.EvalFixtureLoadError(cfg.FixturePath, err)
	}
	if len(fix.Queries) == 0 {
		return searchruntime.EvalFixtureNoQueriesError(cfg.FixturePath)
	}
	applyDefaultQueryType(fix, cfg.FixturePath)
	results, summary := runEvalWithIdx(fix, cfg.Out, cfg.UseScan, cfg.Limit, nil, cfg.ProfileOpts, cfg.TokenBudget, cfg.AnswerContext)
	if err := searchruntime.WriteEvalSweepBaseline(cfg.BaselinePath, func(path string) error {
		return writeBaselineJSON(path, results, summary)
	}); err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, searchruntime.EvalSweepBaselineCapturedMessage(cfg.BaselinePath))

	if err := runCommand(cfg.Command); err != nil {
		return searchruntime.EvalSweepCommandError(err)
	}
	return runDiff(cfg)
}

func runEvalSweepCommand(command []string) error {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func runEvalSweepChildDiff(cfg evalSweepConfig) error {
	exe, err := os.Executable()
	if err != nil {
		return searchruntime.EvalSweepResolveExecutableError(err)
	}
	diffArgs := searchruntime.EvalSweepDiffArgs(searchruntime.EvalSweepDiffArgsConfig{
		FixturePath:        cfg.FixturePath,
		Out:                cfg.Out,
		UseScan:            cfg.UseScan,
		Limit:              cfg.Limit,
		BaselinePath:       cfg.BaselinePath,
		ProfileName:        cfg.ProfileName,
		StrictProfile:      cfg.StrictProfile,
		Rerank:             cfg.Rerank,
		RerankK:            cfg.RerankK,
		RerankModel:        cfg.RerankModel,
		RerankGate:         cfg.RerankGate,
		RerankChunkSize:    cfg.RerankChunkSize,
		TokenBudget:        cfg.TokenBudget,
		TokenBudgetSet:     cfg.TokenBudgetSet,
		DefaultTokenBudget: defaultEvalTokenBudget,
		AnswerContext:      cfg.AnswerContext,
	})
	cmd := exec.Command(exe, diffArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func defaultFixturePath() string {
	return searchruntime.DefaultEvalFixturePath(repoRoot(), func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

// repoRoot walks up from the running binary's source location to find the
// module checkout root. Best-effort — falls back to current dir.
func repoRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return searchruntime.EvalRepoRootFromExecutable(exe, func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

func loadFixture(path string) (*evalFixture, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fix evalFixture
	if err := yaml.Unmarshal(body, &fix); err != nil {
		return nil, searchruntime.EvalFixtureParseError(err)
	}
	for i, q := range fix.Queries {
		if !searchruntime.EvalQueryTextPresent(q.Q) {
			return nil, searchruntime.EvalFixtureEmptyQueryError(i)
		}
		if len(q.Expect) == 0 {
			return nil, searchruntime.EvalFixtureNoExpectEntriesError(i, q.Q)
		}
	}
	return &fix, nil
}

func applyDefaultQueryType(fix *evalFixture, fixturePath string) {
	defaultType := searchruntime.ResolveEvalQueryType(fixturePath, fix.QueryType)
	for i := range fix.Queries {
		if strings.TrimSpace(fix.Queries[i].QueryType) == "" {
			fix.Queries[i].QueryType = defaultType
			continue
		}
		fix.Queries[i].QueryType = searchruntime.ResolveEvalQueryType(fixturePath, fix.Queries[i].QueryType)
	}
}

// runEvalBySource buckets queries by the first segment of their first
// expected path (= the source dir) and runs each bucket independently
// against the corresponding source filter. Prints a comparison table
// sorted by Hit@5 ascending so the worst-performing sources surface
// first — that's where ranking work matters.
//
// Why this matters: a global eval mixes 137 queries with different
// difficulty profiles and source filters. The headline Hit@5 averages
// across them, hiding source-specific weakness. Running per-source
// shows exactly which corners of the corpus need attention.
func runEvalBySource(fix *evalFixture, outDir string, useScan bool, limit int, tokenBudget int, answerContext bool) {
	bucketed := map[string][]evalQuery{}
	for _, q := range fix.Queries {
		if len(q.Expect) == 0 {
			continue
		}
		src := searchruntime.EvalSourceFromPath(q.Expect[0])
		bucketed[src] = append(bucketed[src], q)
	}

	// Pre-flight wait + shared FTS open ONCE for the whole sweep, then
	// thread that into each per-bucket runEvalWithIdx. Without this, each
	// bucket paid ~30s pre-flight retry + ~800ms FTS open, which made a
	// 25-bucket sweep take 10+ minutes wall-clock. Now: ~30s once, plus
	// just the per-query search cost.
	var sharedIdx *ftsIndex
	if !useScan {
		if !waitForFTSReady(outDir, 30*time.Second) {
			fmt.Fprint(os.Stderr, searchruntime.EvalBySourceFTSNotReadyWarning())
		}
		if ftsIndexExists(outDir) {
			if idx, err := openFTSIndexReadOnly(outDir); err == nil {
				sharedIdx = idx
				defer idx.close()
			}
		}
	}
	rows := make([]searchruntime.EvalBySourceMetricRow, 0, len(bucketed))
	for src, qs := range bucketed {
		// Run with the source filter applied so the eval measures
		// source-internal ranking, not "did this source bubble up against
		// the whole corpus". A query with `source:` already set keeps
		// that override (so an internal-fixture query targeting `vault` only
		// runs against `vault`); all others get the bucket's source.
		filtered := make([]evalQuery, len(qs))
		for i, q := range qs {
			fq := q
			if fq.Source == "" {
				fq.Source = src
			}
			filtered[i] = fq
		}
		_, summ := runEvalWithIdx(&evalFixture{Queries: filtered}, outDir, useScan, limit, sharedIdx, searchOpts{}, tokenBudget, answerContext)
		rows = append(rows, searchruntime.EvalBySourceMetricRow{
			Source:  src,
			N:       len(qs),
			HitAt1:  summ.HitAt1,
			HitAt3:  summ.HitAt3,
			HitAt5:  summ.HitAt5,
			HitAt10: summ.HitAt10,
			P50MS:   summ.P50MS,
			P99MS:   summ.P99MS,
		})
	}

	fmt.Print(searchruntime.EvalBySourceTextOutput(rows))
}

// === Corpus-agnostic boundary starts here ===
// The fixture loader, `runEvalWithIdx` core, eval JSON shape
// (evalCaseResult, evalSummary), `emitEvalDiff`, and the metric helpers
// (Hit@K, MRR, Recall@5, P50/P99) below this line do not depend on the
// docs-vs-refs distinction. They take a fixture of (query, expected_paths)
// pairs and a search-callable, returning a structured per-query result.
// Candidate for extraction to internal/corpora/eval/ once a second consumer
// needs the same shape. The cmdEval flag plumbing above this line is
// doc-specific and stays here.
// See docs/active/05-02-2244-corpus-core-extraction-strategy.md.

// runEvalWithIdx is the inner runEval that accepts a pre-opened shared FTS
// index. When `preopened` is nil, falls through to the pre-flight wait + open
// path (the standalone-eval default). When non-nil, skips both — meaning the
// caller (e.g. runEvalBySource) is responsible for the wait and the close.
// This avoids paying ~30s pre-flight + ~800ms FTS open per source bucket
// when running a 25-bucket by-source sweep, which dominated wall-clock time.
func runEvalWithIdx(fix *evalFixture, outDir string, useScan bool, limit int, preopened *ftsIndex, profileOpts searchOpts, tokenBudget int, answerContext bool) ([]evalCaseResult, evalSummary) {
	results := make([]evalCaseResult, 0, len(fix.Queries))
	mode := "fts5"
	if useScan {
		mode = "scan"
	}
	bySource := map[string][]evalCaseResult{}
	var degraded []string

	// Open the FTS5 index once and reuse across all queries — opening per
	// query adds ~5-10ms each, which dominates the actual search latency
	// and inflates p99 numbers.
	//
	// Read-only open: eval never writes. Using openFTSIndex (R/W) runs the
	// schema-init Exec on connect, which takes a write lock — that conflicts
	// with any concurrent reader (e.g. another `docs-puller search` from a
	// parallel agent or hook) and causes every query to fail SQLITE_BUSY and
	// silently degrade to scan, polluting the eval numbers.
	//
	// Pre-flight wait: in FTS5 mode, wait up to 30s for the index to be
	// ready. Covers the case where a parallel `docs pull` is mid-rebuild
	// when eval starts (count(*)=0 transient window). Without this, eval
	// would run with sharedIdx=nil and silently scan every query.
	sharedIdx := preopened
	if sharedIdx == nil && !useScan {
		if !waitForFTSReady(outDir, 30*time.Second) {
			fmt.Fprint(os.Stderr, searchruntime.EvalFTSNotReadyWarning())
		}
		if ftsIndexExists(outDir) {
			if idx, err := openFTSIndexReadOnly(outDir); err == nil {
				sharedIdx = idx
				defer idx.close()
			}
		}
	}
	cachedFTSTotalDocs := 0
	if sharedIdx != nil {
		if n, err := sharedIdx.totalDocs(); err == nil {
			cachedFTSTotalDocs = n
		}
	}

	for _, q := range fix.Queries {
		so := searchOpts{out: outDir, source: q.Source, limit: limit, useScan: useScan, noSnippets: true, ftsOnly: !useScan}
		so.cachedFTSTotalDocs = cachedFTSTotalDocs
		so.profile = profileOpts.profile
		so.resolvedProfile = profileOpts.resolvedProfile
		so.profileReason = profileOpts.profileReason
		so.strict = profileOpts.strict
		so.rerank = profileOpts.rerank
		so.rerankK = profileOpts.rerankK
		so.rerankModel = profileOpts.rerankModel
		so.rerankGate = profileOpts.rerankGate
		so.rerankChunkSize = profileOpts.rerankChunkSize
		so.rerankLLM = profileOpts.rerankLLM
		so.rerankLLMModel = profileOpts.rerankLLMModel
		so.rerankLLMProvider = profileOpts.rerankLLMProvider
		so.rerankHybrid = profileOpts.rerankHybrid
		so.rerankHyde = profileOpts.rerankHyde
		start := time.Now()
		hits, _, gotMode := dispatchSearch(q.Q, so, sharedIdx)
		elapsed := time.Since(start)
		// Sticky degrade: once any query falls back to scan, the run is no
		// longer pure FTS5 and we report "scan" in the summary. A naïve
		// `mode = gotMode` overwrites with whatever the last query used,
		// which silently masks a single mid-run degrade.
		if gotMode == "scan" {
			mode = "scan"
		}
		// ftsOnly mode: empty gotMode means FTS unavailable even after
		// retry. Record the query as degraded (skipped from FTS5 measurement)
		// so the summary surfaces the contamination.
		if !useScan && gotMode == "" {
			degraded = append(degraded, q.Q)
		}

		gotPaths := make([]string, 0, len(hits))
		for _, h := range hits {
			gotPaths = append(gotPaths, h.Path)
		}

		expectSet := make(map[string]bool, len(q.Expect))
		for _, e := range q.Expect {
			expectSet[searchruntime.NormalizeEvalPath(e)] = true
		}
		expectedDoc := ""
		expectedSource := ""
		if len(q.Expect) > 0 {
			expectedDoc = searchruntime.NormalizeEvalPath(q.Expect[0])
			expectedSource = searchruntime.EvalSourceFromPath(expectedDoc)
		}

		firstHitRank := 0
		recallHits := 0
		for i, p := range gotPaths {
			if expectSet[searchruntime.NormalizeEvalPath(p)] {
				if firstHitRank == 0 {
					firstHitRank = i + 1
				}
				if i < 5 {
					recallHits++
				}
			}
		}
		tokensReturned, tokensToFirstHit, tokenBudgetHit := tokenBudgetStats(hits, expectSet, tokenBudget)
		answerContextTokensReturned := 0
		answerContextTokensToFirstHit := 0
		answerContextBudgetHit := false
		if answerContext {
			answerContextTokensReturned, answerContextTokensToFirstHit, answerContextBudgetHit = answerContextStats(outDir, hits, expectSet, tokenBudget)
		}

		recallAt5 := 0.0
		if len(q.Expect) > 0 {
			recallAt5 = float64(recallHits) / float64(len(q.Expect))
		}

		r := evalCaseResult{
			Query:                         q.Q,
			QueryType:                     q.QueryType,
			Source:                        q.Source,
			ExpectedSource:                expectedSource,
			ExpectedDoc:                   expectedDoc,
			Expect:                        q.Expect,
			GotPaths:                      searchruntime.TruncateEvalStrings(gotPaths, 10),
			HitAt1:                        firstHitRank == 1,
			HitAt3:                        firstHitRank > 0 && firstHitRank <= 3,
			HitAt5:                        firstHitRank > 0 && firstHitRank <= 5,
			HitAt10:                       firstHitRank > 0 && firstHitRank <= 10,
			RecallAt5:                     recallAt5,
			FirstHitRank:                  firstHitRank,
			Rank:                          firstHitRank,
			TokensReturned:                tokensReturned,
			TokensToFirstHit:              tokensToFirstHit,
			TokenBudgetHit:                tokenBudgetHit,
			AnswerContextTokensReturned:   answerContextTokensReturned,
			AnswerContextTokensToFirstHit: answerContextTokensToFirstHit,
			AnswerContextBudgetHit:        answerContextBudgetHit,
			LatencyMS:                     float64(elapsed.Microseconds()) / 1000.0,
			Note:                          q.Note,
			BackendMode:                   gotMode,
		}
		results = append(results, r)
		// Bucket by the first segment of an expected path (its source dir).
		for _, e := range q.Expect {
			src := searchruntime.EvalSourceFromPath(e)
			bySource[src] = append(bySource[src], r)
			break // only count once per case
		}
	}

	sum := summarize(results, bySource, mode)
	sum.TokenBudgetTokens = tokenBudget
	sum.AnswerContextEnabled = answerContext
	sum.DegradedQueries = degraded
	return results, sum
}

// waitForFTSReady polls ftsIndexExists at 200ms intervals until it returns
// true or the deadline passes. Covers the rebuild-window race where a
// parallel `docs pull` has dropped/recreated the FTS table but hasn't
// finished the bulk INSERT yet (count(*) = 0). Returns true if ready
// within the budget, false otherwise.
func waitForFTSReady(outDir string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if ftsIndexExists(outDir) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func summarize(rs []evalCaseResult, bySource map[string][]evalCaseResult, mode string) evalSummary {
	if len(rs) == 0 {
		return evalSummary{Mode: mode}
	}
	var (
		h1, h3, h5, h10         int
		tokenBudgetHits         int
		answerContextBudgetHits int
		mrrSum                  float64
		recallSum               float64
		lats                    []float64
		tokenCounts             []float64
		answerContextCounts     []float64
		tokenBudget             int
	)
	for _, r := range rs {
		if r.HitAt1 {
			h1++
		}
		if r.HitAt3 {
			h3++
		}
		if r.HitAt5 {
			h5++
		}
		if r.HitAt10 {
			h10++
		}
		if r.FirstHitRank > 0 {
			mrrSum += 1.0 / float64(r.FirstHitRank)
		}
		if r.TokenBudgetHit {
			tokenBudgetHits++
		}
		if r.TokensReturned > 0 {
			tokenCounts = append(tokenCounts, float64(r.TokensReturned))
		}
		if r.AnswerContextBudgetHit {
			answerContextBudgetHits++
		}
		if r.AnswerContextTokensReturned > 0 {
			answerContextCounts = append(answerContextCounts, float64(r.AnswerContextTokensReturned))
		}
		if tokenBudget == 0 && r.TokensReturned > 0 {
			// The exact budget is copied onto the summary by runEvalWithIdx;
			// this branch keeps hand-built test fixtures deterministic.
			tokenBudget = defaultEvalTokenBudget
		}
		recallSum += r.RecallAt5
		lats = append(lats, r.LatencyMS)
	}
	n := float64(len(rs))
	srcMap := make(map[string]float64, len(bySource))
	for src, list := range bySource {
		hits := 0
		for _, r := range list {
			if r.HitAt5 {
				hits++
			}
		}
		srcMap[src] = float64(hits) / float64(len(list))
	}
	return evalSummary{
		Mode:                       mode,
		N:                          len(rs),
		HitAt1:                     float64(h1) / n,
		HitAt3:                     float64(h3) / n,
		HitAt5:                     float64(h5) / n,
		HitAt10:                    float64(h10) / n,
		RecallAt5:                  recallSum / n,
		MRR:                        mrrSum / n,
		P50MS:                      searchruntime.EvalPercentile(lats, 0.50),
		P99MS:                      searchruntime.EvalPercentile(lats, 0.99),
		BySource:                   srcMap,
		TokenBudgetTokens:          tokenBudget,
		TokenBudgetHitRate:         float64(tokenBudgetHits) / n,
		P50TokensReturned:          searchruntime.EvalPercentile(tokenCounts, 0.50),
		P99TokensReturned:          searchruntime.EvalPercentile(tokenCounts, 0.99),
		AnswerContextBudgetHitRate: float64(answerContextBudgetHits) / n,
		P50AnswerContextTokens:     searchruntime.EvalPercentile(answerContextCounts, 0.50),
		P99AnswerContextTokens:     searchruntime.EvalPercentile(answerContextCounts, 0.99),
	}
}

func tokenBudgetStats(hits []searchHit, expectSet map[string]bool, budget int) (tokensReturned int, tokensToFirstHit int, budgetHit bool) {
	for _, h := range hits {
		tokensReturned += searchruntime.ApproxEvalHitTokens(h)
		if tokensToFirstHit == 0 && expectSet[searchruntime.NormalizeEvalPath(h.Path)] {
			tokensToFirstHit = tokensReturned
		}
	}
	if budget > 0 && tokensToFirstHit > 0 && tokensToFirstHit <= budget {
		budgetHit = true
	}
	return tokensReturned, tokensToFirstHit, budgetHit
}

func answerContextStats(outDir string, hits []searchHit, expectSet map[string]bool, budget int) (tokensReturned int, tokensToFirstHit int, budgetHit bool) {
	for _, h := range hits {
		tokensReturned += approxAnswerContextTokens(outDir, h)
		if tokensToFirstHit == 0 && expectSet[searchruntime.NormalizeEvalPath(h.Path)] {
			tokensToFirstHit = tokensReturned
		}
	}
	if budget > 0 && tokensToFirstHit > 0 && tokensToFirstHit <= budget {
		budgetHit = true
	}
	return tokensReturned, tokensToFirstHit, budgetHit
}

func approxAnswerContextTokens(outDir string, h searchHit) int {
	path, ok := searchruntime.EvalAnswerContextFilePath(outDir, h.Path)
	if ok {
		if body, err := os.ReadFile(path); err == nil {
			return searchruntime.ApproxEvalTokens(string(body))
		}
	}
	return searchruntime.ApproxEvalHitTokens(h)
}

func emitEvalText(rs []evalCaseResult, s evalSummary, verbose bool) {
	fmt.Print(searchruntime.EvalTextOutput(rs, s, verbose))
}

func emitEvalJSON(w io.Writer, rs []evalCaseResult, s evalSummary) error {
	return searchruntime.EncodeEvalJSONArtifact(w, rs, s)
}

// writeBaselineJSON writes the full {summary, results} artifact to a file.
// Same shape as `--json` stdout, suitable as input to `--diff`. Overwrites
// rather than appending — a baseline is a snapshot at one point in time,
// not a time-series.
func writeBaselineJSON(path string, rs []evalCaseResult, s evalSummary) error {
	return searchruntime.WriteEvalJSONArtifact(path, func(w io.Writer) error {
		return emitEvalJSON(w, rs, s)
	})
}

// emitEvalDiff loads a previous --json eval output and prints per-query
// rank deltas + aggregate metric drift. Designed for tuning sweeps where
// the question is "did this change help or hurt?".
//
// Match strategy: queries are matched by the `query` string verbatim, since
// fixture rewrites that change query text are deliberate (and a different
// experiment). Adds/removes are reported separately so a fixture growth
// commit doesn't get hidden inside the metrics drift.
func emitEvalDiff(baselinePath string, current []evalCaseResult, currentSummary evalSummary) (evalDriftReport, error) {
	body, err := os.ReadFile(baselinePath)
	if err != nil {
		return evalDriftReport{}, err
	}
	var prev searchruntime.EvalJSONArtifact
	if err := json.Unmarshal(body, &prev); err != nil {
		return evalDriftReport{}, searchruntime.EvalDiffParseError(err)
	}
	drift := buildEvalDriftReport(prev.Summary, currentSummary)

	prevByQuery := make(map[string]evalCaseResult, len(prev.Results))
	for _, r := range prev.Results {
		prevByQuery[r.Query] = r
	}
	curByQuery := make(map[string]evalCaseResult, len(current))
	for _, r := range current {
		curByQuery[r.Query] = r
	}

	// Per-query rank changes.
	gained, lost, moved := []string{}, []string{}, []string{}
	for q, c := range curByQuery {
		p, ok := prevByQuery[q]
		if !ok {
			continue
		}
		if p.FirstHitRank == c.FirstHitRank {
			continue
		}
		switch {
		case p.FirstHitRank == 0 && c.FirstHitRank > 0:
			gained = append(gained, searchruntime.EvalDiffGainedLine(q, c.FirstHitRank))
		case p.FirstHitRank > 0 && c.FirstHitRank == 0:
			lost = append(lost, searchruntime.EvalDiffLostLine(q, p.FirstHitRank))
		default:
			moved = append(moved, searchruntime.EvalDiffMovedLine(q, p.FirstHitRank, c.FirstHitRank))
		}
	}
	added, removed := []string{}, []string{}
	for q := range curByQuery {
		if _, ok := prevByQuery[q]; !ok {
			added = append(added, searchruntime.EvalDiffAddedLine(q))
		}
	}
	for q := range prevByQuery {
		if _, ok := curByQuery[q]; !ok {
			removed = append(removed, searchruntime.EvalDiffRemovedLine(q))
		}
	}
	diffText := searchruntime.EvalDiffTextOutput(searchruntime.EvalDiffTextInput{
		BaselinePath:    baselinePath,
		BaselineSummary: prev.Summary,
		CurrentSummary:  currentSummary,
		SourceDrift:     drift.SourceDrift,
		Gained:          gained,
		Lost:            lost,
		Moved:           moved,
		Added:           added,
		Removed:         removed,
	})
	written, err := fmt.Print(diffText)
	if err != nil {
		return evalDriftReport{}, err
	}
	if written != len(diffText) {
		return evalDriftReport{}, io.ErrShortWrite
	}
	return drift, nil
}

func buildEvalDriftReport(prev, current evalSummary) evalDriftReport {
	return evalDriftReport{
		SourceDrift: searchruntime.BuildEvalSourceDrift(prev.BySource, current.BySource),
	}
}

// runCheckFixture verifies every fixture `expect:` path exists in the corpus
// at <out>/<source>/.... Catches silent fixture rot when vendors reorganize
// their docs and the path we expect is gone — without this, a stale fixture
// entry shows up as a generic miss indistinguishable from a ranking issue.
// Exits non-zero when anything's missing.
func runCheckFixture(fix *evalFixture, outDir string) {
	missing := []searchruntime.EvalCheckFixtureMissingPath{}
	for _, q := range fix.Queries {
		for _, e := range q.Expect {
			p := searchruntime.EvalCheckFixturePath(outDir, e)
			if _, err := os.Stat(p); err != nil {
				missing = append(missing, searchruntime.EvalCheckFixtureMissingPath{
					Expect: e,
					Query:  q.Q,
					Path:   p,
				})
			}
		}
	}
	fmt.Print(searchruntime.EvalCheckFixtureTextOutput(len(fix.Queries), missing))
	if len(missing) == 0 {
		return
	}
	fmt.Fprint(os.Stderr, searchruntime.EvalCheckFixtureMissingSummary(len(missing)))
	os.Exit(1)
}

// appendSummaryJSONL appends a {timestamp, summary} JSONL row. Designed for
// time-series tracking (eval against a corpus once a week, plot the trend).
// NOT compatible with --diff because it omits per-query results — use
// writeBaselineJSON for that.
func appendSummaryJSONL(path string, s evalSummary) error {
	return searchruntime.AppendEvalJSONLArtifact(path, func(w io.Writer) error {
		return searchruntime.EncodeEvalSummaryJSONLRow(w, s, time.Now())
	})
}

func buildEvalRunReport(fixturePath, docsRoot string, limit, tokenBudget int, baselinePath string, results []evalCaseResult, summary evalSummary, drift evalDriftReport) evalRunReport {
	return searchruntime.BuildEvalRunReport(searchruntime.EvalRunReportInput{
		FixturePath:       fixturePath,
		DocsRoot:          docsRoot,
		Limit:             limit,
		TokenBudgetTokens: tokenBudget,
		BaselinePath:      baselinePath,
		Results:           results,
		Summary:           summary,
		SourceDrift:       drift.SourceDrift,
		Now:               time.Now(),
	})
}

type evalRunArtifactPaths = searchruntime.EvalRunArtifactPaths

func writeEvalRunArtifactsForRecord(root string, report evalRunReport) (evalRunArtifactPaths, error) {
	paths, err := writeEvalRunArtifacts(root, report)
	if err != nil {
		return evalRunArtifactPaths{}, searchruntime.EvalRecordRunError(err)
	}
	return paths, nil
}

func writeEvalRunArtifacts(root string, report evalRunReport) (evalRunArtifactPaths, error) {
	paths, err := searchruntime.PlanEvalRunArtifacts(searchruntime.EvalRunArtifactPlan{
		Root:      root,
		Timestamp: report.Timestamp,
	})
	if err != nil {
		return evalRunArtifactPaths{}, err
	}
	if err := searchruntime.WriteEvalRunArtifacts(paths, func(path string) error {
		return writeEvalReportJSON(path, report)
	}); err != nil {
		return evalRunArtifactPaths{}, err
	}
	return paths, nil
}

func writeEvalReportJSON(path string, report evalRunReport) error {
	return searchruntime.WriteEvalJSONArtifact(path, func(w io.Writer) error {
		return searchruntime.EncodeEvalRunReportJSON(w, report)
	})
}
