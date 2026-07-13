package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nstranquist/docs-puller/searchruntime"
)

// === Corpus-agnostic ===
// This file compares two eval JSON files keyed by `(query, expected_paths,
// got_paths)` tuples. Nothing in the regression detection, suspected-kind
// classifier, or markdown report formatter depends on the docs-vs-refs
// distinction. The four kind heuristics (cross-source-bleed,
// same-source-thematic-adjacency, vendor-generic-page-promoted,
// canonical-fully-missed) generalize to any corpus where paths are
// hierarchical and a "canonical" doc is the goal. Candidate for extraction
// to internal/corpora/eval/diagnose/ once a second consumer needs the
// same shape. Until then: copy on Phase 2, extract on Phase 3 per the
// rule-of-three.
// See docs/active/05-02-2244-corpus-core-extraction-strategy.md.

// eval-diagnose: deterministic per-query regression analysis between two
// eval JSON outputs. Codifies the workflow that historically required a
// subagent (read the JSON, find regressions, identify what displaced the
// canonical, look at displacing doc bodies).
//
// Inputs: two `eval --json` outputs (typically baseline and experimental).
// Output: a markdown report listing every regression, ordered by rank
// delta. For each regression: query, canonical, baseline rank, experimental
// rank, the docs that newly appeared in the experimental top-5, and a
// hint at the suspected pattern (cross-source bleed, thematic-adjacency
// in same source, generic-page-over-canonical).
//
// Use cases:
//   - After every rerank/retrieval change, run `eval-diagnose` to see
//     exactly which queries moved and why.
//   - When a per-source aggregate regresses, find the binding queries.
//   - As input to a deeper investigation by a subagent — pass the
//     report to the agent as its starting point so it doesn't redo the
//     deterministic work.

type evalDiagnoseOpts struct {
	baselinePath string
	currentPath  string
	source       string // optional: filter to one source
	minDelta     int    // minimum rank-delta to count as a regression (default 1)
	asJSON       bool   // emit JSON instead of markdown
	docsRoot     string // for resolving displacing-doc titles
	maxItems     int    // cap regressions in the report
}

func cmdEvalDiagnose(args []string) {
	o := evalDiagnoseOpts{minDelta: 1, maxItems: 50}
	home, _ := os.UserHomeDir()
	if home != "" {
		o.docsRoot = filepath.Join(home, "code", "docs")
	}
	fs := flag.NewFlagSet("eval-diagnose", flag.ExitOnError)
	fs.StringVar(&o.baselinePath, "baseline", "", "path to baseline eval JSON (required)")
	fs.StringVar(&o.currentPath, "current", "", "path to current/experimental eval JSON (required)")
	fs.StringVar(&o.source, "source", "", "filter to a single source (e.g. clickhouse)")
	fs.IntVar(&o.minDelta, "min-delta", 1, "minimum rank delta (positive = regression) to include")
	fs.BoolVar(&o.asJSON, "json", false, "emit JSON instead of markdown")
	fs.StringVar(&o.docsRoot, "docs-root", o.docsRoot, "docs corpus root (for resolving displacing-doc titles)")
	fs.IntVar(&o.maxItems, "max-items", 50, "cap the number of regressions printed")
	fs.Parse(args)
	if o.baselinePath == "" || o.currentPath == "" {
		fmt.Fprintln(os.Stderr, "eval-diagnose: --baseline and --current are required")
		os.Exit(2)
	}
	if err := runEvalDiagnose(o, os.Stdout); err != nil {
		die(err)
	}
}

type evalDiagFile struct {
	Results []evalCaseResult `json:"results"`
	Summary evalSummary      `json:"summary"`
}

type regression struct {
	Query           string   `json:"query"`
	Source          string   `json:"source,omitempty"`
	Expect          []string `json:"expect"`
	BaselineRank    int      `json:"baseline_rank"` // 1-indexed; 0 = miss
	CurrentRank     int      `json:"current_rank"`
	BaselineTop5    []string `json:"baseline_top5"`
	CurrentTop5     []string `json:"current_top5"`
	NewlyDisplacing []string `json:"newly_displacing"` // paths in current top-5 NOT in baseline top-5 AND ranked above the canonical
	SuspectedKind   string   `json:"suspected_kind"`   // "cross-source-bleed" | "same-source-thematic-adjacency" | "vendor-generic-page-promoted" | "canonical-fully-missed" | ""
}

func runEvalDiagnose(o evalDiagnoseOpts, w *os.File) error {
	base, err := loadEvalDiagFile(o.baselinePath)
	if err != nil {
		return searchruntime.EvalDiagnoseBaselineLoadError(o.baselinePath, err)
	}
	cur, err := loadEvalDiagFile(o.currentPath)
	if err != nil {
		return searchruntime.EvalDiagnoseCurrentLoadError(o.currentPath, err)
	}

	regs := computeRegressions(base.Results, cur.Results, o)
	improved, regressed := countDirections(base.Results, cur.Results, o)

	if o.asJSON {
		out := struct {
			BaselineFile string       `json:"baseline_file"`
			CurrentFile  string       `json:"current_file"`
			SourceFilter string       `json:"source_filter,omitempty"`
			TotalQueries int          `json:"total_queries"`
			Improved     int          `json:"improved"`
			Regressed    int          `json:"regressed"`
			Regressions  []regression `json:"regressions"`
		}{
			BaselineFile: o.baselinePath,
			CurrentFile:  o.currentPath,
			SourceFilter: o.source,
			TotalQueries: len(base.Results),
			Improved:     improved,
			Regressed:    regressed,
			Regressions:  regs,
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	fmt.Fprintf(w, "# Eval regression diagnosis\n\n")
	fmt.Fprintf(w, "- baseline: `%s`\n- current:  `%s`\n", o.baselinePath, o.currentPath)
	if o.source != "" {
		fmt.Fprintf(w, "- source filter: `%s`\n", o.source)
	}
	fmt.Fprintf(w, "- queries: %d (baseline) → %d (current)\n", len(base.Results), len(cur.Results))
	fmt.Fprintf(w, "- improved: %d, regressed: %d, regressions printed: %d\n\n",
		improved, regressed, len(regs))

	if len(regs) == 0 {
		fmt.Fprintln(w, "_No regressions at this threshold._")
		return nil
	}

	fmt.Fprintf(w, "## Regressions (worst first)\n\n")
	for i, r := range regs {
		if i >= o.maxItems {
			fmt.Fprintf(w, "\n_(%d more regressions; pass --max-items %d to see all)_\n",
				len(regs)-o.maxItems, len(regs))
			break
		}
		fmt.Fprintf(w, "### %d. %q\n", i+1, r.Query)
		if r.Source != "" {
			fmt.Fprintf(w, "- source filter: `%s`\n", r.Source)
		}
		fmt.Fprintf(w, "- expected: ")
		for j, e := range r.Expect {
			if j > 0 {
				fmt.Fprintf(w, ", ")
			}
			fmt.Fprintf(w, "`%s`", e)
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "- rank: **%s → %s** (Δ %+d)\n",
			rankLabel(r.BaselineRank), rankLabel(r.CurrentRank),
			rankDelta(r.BaselineRank, r.CurrentRank))
		if r.SuspectedKind != "" {
			fmt.Fprintf(w, "- suspected: %s\n", r.SuspectedKind)
		}
		fmt.Fprintf(w, "- baseline top-5:\n")
		for k, p := range r.BaselineTop5 {
			fmt.Fprintf(w, "    %d. `%s`\n", k+1, p)
		}
		fmt.Fprintf(w, "- current top-5:\n")
		for k, p := range r.CurrentTop5 {
			marker := " "
			if !contains(r.BaselineTop5, p) {
				marker = "+"
			}
			fmt.Fprintf(w, "    %d.%s `%s`\n", k+1, marker, p)
		}
		if len(r.NewlyDisplacing) > 0 {
			fmt.Fprintf(w, "- newly-displacing (above canonical):\n")
			for _, p := range r.NewlyDisplacing {
				title := titleForReport(filepath.Join(o.docsRoot, p))
				if title != "" {
					fmt.Fprintf(w, "    - `%s` — %s\n", p, title)
				} else {
					fmt.Fprintf(w, "    - `%s`\n", p)
				}
			}
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "## Reading guide")
	fmt.Fprint(w, `
- `+"`+`"+` marks paths in the current top-5 that weren't in the baseline top-5.
- "Newly-displacing" is the subset that ranked above the canonical and wasn't there before — those are the prime suspects for the rerank/retrieval change that caused the regression.
- Suspected-kind heuristics:
  - **cross-source-bleed**: top-1 displacer is in a different source from the canonical → embedding/rerank promoted off-vendor content.
  - **same-source-thematic-adjacency**: displacer is in the same source as the canonical but at a different path depth → embedding pulled in adjacent pages and the reranker preferred the broader/shorter one.
  - **vendor-generic-page-promoted**: displacer is short and well-titled but topical (e.g. ".../partitions.md" for "MergeTree partition") → conceptual overview chosen over specific reference.
  - **canonical-fully-missed**: canonical isn't in the current top-10 at all → first-stage retrieval lost it, not a rerank ordering issue.
`)

	return nil
}

func loadEvalDiagFile(path string) (*evalDiagFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f evalDiagFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// computeRegressions pairs baseline and current results by query string,
// scopes to the optional source filter, and returns regressions sorted
// worst-first. A regression is a query whose canonical rank moved later
// (rank increased) or fell out of top-10 (rank == 0 in current).
func computeRegressions(base, cur []evalCaseResult, o evalDiagnoseOpts) []regression {
	curByQuery := map[string]evalCaseResult{}
	for _, r := range cur {
		curByQuery[r.Query] = r
	}
	var regs []regression
	for _, b := range base {
		if o.source != "" && !belongsToSource(b, o.source) {
			continue
		}
		c, ok := curByQuery[b.Query]
		if !ok {
			continue
		}
		delta := rankDelta(b.FirstHitRank, c.FirstHitRank)
		if delta < o.minDelta {
			continue
		}
		regs = append(regs, regression{
			Query:           b.Query,
			Source:          b.Source,
			Expect:          b.Expect,
			BaselineRank:    b.FirstHitRank,
			CurrentRank:     c.FirstHitRank,
			BaselineTop5:    topN(b.GotPaths, 5),
			CurrentTop5:     topN(c.GotPaths, 5),
			NewlyDisplacing: newlyDisplacing(b.GotPaths, c.GotPaths, b.Expect),
			SuspectedKind:   suspectKind(b.GotPaths, c.GotPaths, b.Expect, b.Source),
		})
	}
	sort.SliceStable(regs, func(i, j int) bool {
		di := rankDelta(regs[i].BaselineRank, regs[i].CurrentRank)
		dj := rankDelta(regs[j].BaselineRank, regs[j].CurrentRank)
		if di != dj {
			return di > dj
		}
		return regs[i].Query < regs[j].Query
	})
	return regs
}

// countDirections returns counts of queries that improved vs regressed.
// Used in the report header. Both rank-delta directions are counted with
// the same source filter as the regression list.
func countDirections(base, cur []evalCaseResult, o evalDiagnoseOpts) (improved, regressed int) {
	curByQuery := map[string]evalCaseResult{}
	for _, r := range cur {
		curByQuery[r.Query] = r
	}
	for _, b := range base {
		if o.source != "" && !belongsToSource(b, o.source) {
			continue
		}
		c, ok := curByQuery[b.Query]
		if !ok {
			continue
		}
		d := rankDelta(b.FirstHitRank, c.FirstHitRank)
		if d > 0 {
			regressed++
		} else if d < 0 {
			improved++
		}
	}
	return
}

// rankDelta returns positive for regression, negative for improvement.
// "0 -> 0" (still missing) is no change. "0 -> N" is improvement (-(N+1)?
// no — promote to a number that means "miss → top-N is good"). We model
// rank 0 (miss) as worse than any concrete rank, so:
//
//	baseline 0, current 5 → improvement (delta -∞-equivalent; we use -100)
//	baseline 5, current 0 → regression (delta +100)
//	baseline 1, current 3 → regression (delta +2)
//	baseline 3, current 1 → improvement (delta -2)
func rankDelta(baseline, current int) int {
	const missPenalty = 100
	bb, cc := baseline, current
	if bb == 0 {
		bb = missPenalty
	}
	if cc == 0 {
		cc = missPenalty
	}
	return cc - bb
}

func rankLabel(r int) string {
	if r == 0 {
		return "miss"
	}
	return fmt.Sprintf("%d", r)
}

func belongsToSource(r evalCaseResult, source string) bool {
	if r.Source == source {
		return true
	}
	for _, e := range r.Expect {
		if strings.HasPrefix(e, source+"/") {
			return true
		}
	}
	return false
}

// newlyDisplacing returns paths in current top-5 that:
//   - were NOT in baseline top-5
//   - are ranked above any of the expected paths in current
//
// These are the prime suspects: docs that the change promoted past the canonical.
func newlyDisplacing(baseGot, curGot, expect []string) []string {
	curTop5 := topN(curGot, 5)
	baseTop5Set := map[string]bool{}
	for _, p := range topN(baseGot, 5) {
		baseTop5Set[p] = true
	}
	expectSet := map[string]bool{}
	for _, e := range expect {
		expectSet[e] = true
	}
	// Find rank of first canonical in current; everything above it that's
	// new in this run is a displacing candidate.
	firstCanonRank := 0
	for i, p := range curTop5 {
		if expectSet[p] {
			firstCanonRank = i + 1
			break
		}
	}
	cap := len(curTop5)
	if firstCanonRank > 0 {
		cap = firstCanonRank - 1
	}
	var out []string
	for i := 0; i < cap; i++ {
		if !baseTop5Set[curTop5[i]] && !expectSet[curTop5[i]] {
			out = append(out, curTop5[i])
		}
	}
	return out
}

// suspectKind classifies a regression by the dominant pattern its
// newly-displacing docs exhibit. Heuristic; meant to seed an investigator,
// not be authoritative.
func suspectKind(baseGot, curGot, expect []string, fixtureSource string) string {
	curTop5 := topN(curGot, 5)
	if len(curTop5) == 0 {
		return ""
	}
	expectSet := map[string]bool{}
	for _, e := range expect {
		expectSet[e] = true
	}
	canonInCur := false
	for _, p := range curGot {
		if expectSet[p] {
			canonInCur = true
			break
		}
	}
	if !canonInCur {
		return "canonical-fully-missed"
	}
	displacing := newlyDisplacing(baseGot, curGot, expect)
	if len(displacing) == 0 {
		return ""
	}
	expectSource := ""
	for _, e := range expect {
		if i := strings.IndexByte(e, '/'); i > 0 {
			expectSource = e[:i]
			break
		}
	}
	if expectSource == "" {
		expectSource = fixtureSource
	}
	for _, d := range displacing {
		ds := ""
		if i := strings.IndexByte(d, '/'); i > 0 {
			ds = d[:i]
		}
		if expectSource != "" && ds != expectSource {
			return "cross-source-bleed"
		}
	}
	// All displacers in same source — check depth pattern.
	canonDepth := 0
	for _, e := range expect {
		canonDepth = strings.Count(e, "/")
		break
	}
	for _, d := range displacing {
		if strings.Count(d, "/") < canonDepth {
			return "vendor-generic-page-promoted"
		}
	}
	return "same-source-thematic-adjacency"
}

func topN(paths []string, n int) []string {
	if len(paths) > n {
		return paths[:n]
	}
	return append([]string{}, paths...)
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// titleForReport returns a human-readable title for a doc path. Best-
// effort; falls back to "" when extractTitle errors. Used in the report
// to make displacing-doc lists scannable.
func titleForReport(absPath string) string {
	if absPath == "" {
		return ""
	}
	if _, err := os.Stat(absPath); err != nil {
		return ""
	}
	return extractTitle(absPath)
}
