package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nstranquist/docs-puller/searchruntime"
)

type failingEvalJSONWriter struct {
	err error
}

func (w failingEvalJSONWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestSummarizeMetrics(t *testing.T) {
	rs := []evalCaseResult{
		// Hit at 1 → contributes to all hit@K + MRR=1.
		{Query: "a", FirstHitRank: 1, HitAt1: true, HitAt3: true, HitAt5: true, HitAt10: true, RecallAt5: 1.0, LatencyMS: 100},
		// Hit at 4 → only hit@5 + hit@10, MRR=0.25.
		{Query: "b", FirstHitRank: 4, HitAt5: true, HitAt10: true, RecallAt5: 0.5, LatencyMS: 200},
		// Miss → no hits, MRR contribution = 0.
		{Query: "c", FirstHitRank: 0, RecallAt5: 0, LatencyMS: 300},
	}
	by := map[string][]evalCaseResult{
		"supabase": {rs[0]},
		"go":       {rs[1], rs[2]},
	}
	s := summarize(rs, by, "fts5")
	if s.N != 3 {
		t.Errorf("N: got %d, want 3", s.N)
	}
	if s.HitAt1 != 1.0/3 {
		t.Errorf("HitAt1: got %v, want %v", s.HitAt1, 1.0/3)
	}
	if s.HitAt5 != 2.0/3 {
		t.Errorf("HitAt5: got %v, want %v", s.HitAt5, 2.0/3)
	}
	wantMRR := (1.0 + 0.25 + 0.0) / 3
	if s.MRR != wantMRR {
		t.Errorf("MRR: got %v, want %v", s.MRR, wantMRR)
	}
	if s.RecallAt5 != (1.0+0.5+0.0)/3 {
		t.Errorf("RecallAt5: got %v, want %v", s.RecallAt5, (1.0+0.5+0.0)/3)
	}
	if s.BySource["supabase"] != 1.0 {
		t.Errorf("BySource[supabase] (1 hit / 1): got %v", s.BySource["supabase"])
	}
	if s.BySource["go"] != 0.5 {
		t.Errorf("BySource[go] (1 hit / 2): got %v", s.BySource["go"])
	}
}

func TestLoadFixtureValidates(t *testing.T) {
	// Empty q rejected.
	dir := t.TempDir()
	path := dir + "/bad.yaml"
	if err := writeYAML(path, "queries:\n  - q: \"   \"\n    expect: [a]\n"); err != nil {
		t.Fatal(err)
	}
	fix, err := loadFixture(path)
	if fix != nil {
		t.Fatalf("loadFixture empty q returned fixture: %+v", fix)
	}
	if err == nil || err.Error() != searchruntime.EvalFixtureEmptyQueryError(0).Error() {
		t.Fatalf("loadFixture empty q err = %v", err)
	}
	// Missing expect rejected.
	if err := writeYAML(path, "queries:\n  - q: hello\n    expect: []\n"); err != nil {
		t.Fatal(err)
	}
	fix, err = loadFixture(path)
	if fix != nil {
		t.Fatalf("loadFixture empty expect returned fixture: %+v", fix)
	}
	if err == nil || err.Error() != searchruntime.EvalFixtureNoExpectEntriesError(0, "hello").Error() {
		t.Fatalf("loadFixture empty expect err = %v", err)
	}
	// Valid fixture parses with all fields.
	if err := writeYAML(path, "query_type: library_search\nqueries:\n  - q: hello\n    expect: [a/b.md, c/d.md]\n    source: a\n    note: x\n"); err != nil {
		t.Fatal(err)
	}
	fix, err = loadFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a/b.md", "c/d.md"}
	if !reflect.DeepEqual(fix.Queries[0].Expect, want) {
		t.Errorf("expect: got %v, want %v", fix.Queries[0].Expect, want)
	}
	if fix.QueryType != "library_search" || fix.Queries[0].Source != "a" || fix.Queries[0].Note != "x" {
		t.Errorf("fixture fields not parsed: query_type=%q query=%+v", fix.QueryType, fix.Queries[0])
	}
}

func TestEvalProfileOptsWrapsProfileLoadError(t *testing.T) {
	opts, err := evalProfileOpts("does-not-exist", false, false, 0, "", 0, 0, t.TempDir())
	if opts.profile != nil || opts.resolvedProfile != "" {
		t.Fatalf("evalProfileOpts missing profile returned opts: %+v", opts)
	}
	want := searchruntime.EvalProfileLoadError("does-not-exist", searchruntime.ProfileNotFoundError("does-not-exist")).Error()
	if err == nil || err.Error() != want {
		t.Fatalf("evalProfileOpts missing profile err = %v, want %q", err, want)
	}
}

func TestApplyDefaultQueryType(t *testing.T) {
	fix := &evalFixture{Queries: []evalQuery{{Q: "a", QueryType: "  ", Expect: []string{"demo/a.md"}}, {Q: "b", QueryType: " custom ", Expect: []string{"demo/b.md"}}}}
	applyDefaultQueryType(fix, "eval/natural-language.yaml")
	if got := fix.Queries[0].QueryType; got != "tech_stack_search" {
		t.Fatalf("default natural query type = %q, want tech_stack_search", got)
	}
	if got := fix.Queries[1].QueryType; got != " custom " {
		t.Fatalf("explicit query type overwritten: %q", got)
	}

	fix = &evalFixture{Queries: []evalQuery{{Q: "a", Expect: []string{"demo/a.md"}}}}
	applyDefaultQueryType(fix, "eval/fixture.yaml")
	if got := fix.Queries[0].QueryType; got != "library_search" {
		t.Fatalf("default library query type = %q, want library_search", got)
	}

	fix = &evalFixture{QueryType: "production_telemetry", Queries: []evalQuery{{Q: "a", Expect: []string{"demo/a.md"}}}}
	applyDefaultQueryType(fix, "eval/fixture.yaml")
	if got := fix.Queries[0].QueryType; got != "production_telemetry" {
		t.Fatalf("fixture-level query type = %q, want production_telemetry", got)
	}
}

func TestTokenBudgetStats(t *testing.T) {
	hits := []searchHit{
		{Path: "demo/alpha.md", Title: "Alpha"},
		{Path: "demo/beta.md", Title: "Beta canonical"},
		{Path: "demo/gamma.md", Title: "Gamma"},
	}
	expect := map[string]bool{"demo/beta.md": true}
	firstTwoBudget := searchruntime.ApproxEvalHitTokens(hits[0]) + searchruntime.ApproxEvalHitTokens(hits[1])
	tokensReturned, tokensToHit, ok := tokenBudgetStats(hits, expect, firstTwoBudget)
	if tokensReturned <= tokensToHit || tokensToHit != firstTwoBudget {
		t.Fatalf("tokensReturned=%d tokensToHit=%d firstTwoBudget=%d", tokensReturned, tokensToHit, firstTwoBudget)
	}
	if !ok {
		t.Fatal("expected canonical hit to fit within budget")
	}
	_, _, ok = tokenBudgetStats(hits, expect, firstTwoBudget-1)
	if ok {
		t.Fatal("expected canonical hit to miss a too-small budget")
	}
}

func TestAnswerContextStatsUsesReturnedDocuments(t *testing.T) {
	out := t.TempDir()
	alpha := strings.Repeat("a", 40)
	beta := strings.Repeat("b", 80)
	gamma := strings.Repeat("g", 20)
	writeDoc(t, out, "demo/alpha.md", alpha)
	writeDoc(t, out, "demo/beta.md", beta)
	writeDoc(t, out, "demo/gamma.md", gamma)

	hits := []searchHit{
		{Path: "demo/alpha.md", Title: "Alpha"},
		{Path: "demo/beta.md", Title: "Beta canonical"},
		{Path: "demo/gamma.md", Title: "Gamma"},
	}
	expect := map[string]bool{"demo/beta.md": true}
	firstTwoBudget := searchruntime.ApproxEvalTokens(alpha) + searchruntime.ApproxEvalTokens(beta)
	tokensReturned, tokensToHit, ok := answerContextStats(out, hits, expect, firstTwoBudget)
	if want := searchruntime.ApproxEvalTokens(alpha) + searchruntime.ApproxEvalTokens(beta) + searchruntime.ApproxEvalTokens(gamma); tokensReturned != want {
		t.Fatalf("tokensReturned=%d, want %d", tokensReturned, want)
	}
	if tokensToHit != firstTwoBudget {
		t.Fatalf("tokensToHit=%d, want %d", tokensToHit, firstTwoBudget)
	}
	if !ok {
		t.Fatal("expected canonical answer context to fit within budget")
	}
	_, _, ok = answerContextStats(out, hits, expect, firstTwoBudget-1)
	if ok {
		t.Fatal("expected canonical answer context to miss a too-small budget")
	}
	if _, ok := searchruntime.EvalAnswerContextFilePath(out, "../secret.md"); ok {
		t.Fatal("expected path traversal to be rejected")
	}
}

func TestSummarizeAnswerContextMetrics(t *testing.T) {
	rs := []evalCaseResult{
		{Query: "a", FirstHitRank: 1, HitAt1: true, HitAt3: true, HitAt5: true, HitAt10: true, RecallAt5: 1, LatencyMS: 1, AnswerContextTokensReturned: 100, AnswerContextBudgetHit: true},
		{Query: "b", FirstHitRank: 2, HitAt3: true, HitAt5: true, HitAt10: true, RecallAt5: 1, LatencyMS: 2, AnswerContextTokensReturned: 300},
	}
	s := summarize(rs, map[string][]evalCaseResult{"demo": rs}, "fts5")
	if s.AnswerContextBudgetHitRate != 0.5 {
		t.Fatalf("answer context budget hit rate = %v, want 0.5", s.AnswerContextBudgetHitRate)
	}
	if s.P50AnswerContextTokens != 100 || s.P99AnswerContextTokens != 100 {
		t.Fatalf("answer context percentiles p50=%v p99=%v, want 100/100", s.P50AnswerContextTokens, s.P99AnswerContextTokens)
	}
}

func TestEmitEvalTextUsesRuntimeSummaryLines(t *testing.T) {
	rs := []evalCaseResult{{Query: "alpha", FirstHitRank: 1, HitAt1: true, HitAt3: true, HitAt5: true, HitAt10: true, RecallAt5: 1, LatencyMS: 10}}
	summary := evalSummary{
		Mode:                       "fts5",
		N:                          1,
		HitAt1:                     1,
		HitAt3:                     1,
		HitAt5:                     1,
		HitAt10:                    1,
		RecallAt5:                  1,
		MRR:                        1,
		P50MS:                      10,
		P99MS:                      10,
		AnswerContextEnabled:       true,
		P50AnswerContextTokens:     100,
		P99AnswerContextTokens:     300,
		AnswerContextBudgetHitRate: 0.5,
		BySource:                   map[string]float64{"supabase": 1},
	}
	out := captureStdout(t, func() {
		emitEvalText(rs, summary, false)
	})
	for _, want := range []string{
		searchruntime.EvalSummaryHeader(summary.Mode, summary.N),
		searchruntime.EvalSummaryMetricsLine(summary.HitAt1, summary.HitAt3, summary.HitAt5, summary.HitAt10, summary.MRR, summary.RecallAt5),
		searchruntime.EvalSummaryLatencyLine(summary.P50MS, summary.P99MS),
		searchruntime.EvalSummaryAnswerContextLine(summary.P50AnswerContextTokens, summary.P99AnswerContextTokens, summary.AnswerContextBudgetHitRate),
		searchruntime.EvalSummaryBySourceHeader(),
		searchruntime.EvalSummaryBySourceLine("supabase", 1),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("emitEvalText output missing %q:\n%s", want, out)
		}
	}
}

func TestEmitEvalTextUsesRuntimeVerboseLines(t *testing.T) {
	rs := []evalCaseResult{
		{Query: "alpha", FirstHitRank: 1, HitAt1: true, HitAt3: true, HitAt5: true, HitAt10: true, LatencyMS: 10},
		{Query: "beta", FirstHitRank: 0, HitAt1: false, HitAt3: false, HitAt5: false, HitAt10: false, LatencyMS: 20},
	}
	summary := evalSummary{Mode: "fts5", N: len(rs)}
	out := captureStdout(t, func() {
		emitEvalText(rs, summary, true)
	})
	for _, want := range []string{
		searchruntime.EvalVerboseTableHeader(),
		searchruntime.EvalVerboseResultLine("alpha", true, true, true, true, 1, 10),
		searchruntime.EvalVerboseResultLine("beta", false, false, false, false, 0, 20),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("emitEvalText verbose output missing %q:\n%s", want, out)
		}
	}
}

func TestEmitEvalTextUsesRuntimeMissSectionLines(t *testing.T) {
	rs := []evalCaseResult{
		{Query: "hit", FirstHitRank: 1, HitAt5: true},
		{Query: "miss", FirstHitRank: 0, HitAt5: false, Expect: []string{"docs/expected.md"}, GotPaths: []string{"docs/actual.md"}},
	}
	summary := evalSummary{Mode: "fts5", N: len(rs)}
	out := captureStdout(t, func() {
		emitEvalText(rs, summary, false)
	})
	for _, want := range []string{
		searchruntime.EvalMissesHeader(1, len(rs)),
		searchruntime.EvalMissLine("miss", 0),
		searchruntime.EvalMissExpectLine("docs/expected.md"),
		searchruntime.EvalMissGotPathLine("docs/actual.md"),
		searchruntime.EvalMissesFooter(),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("emitEvalText miss output missing %q:\n%s", want, out)
		}
	}
}

func TestBuildEvalDriftReport(t *testing.T) {
	drift := buildEvalDriftReport(
		evalSummary{BySource: map[string]float64{"supabase": 0.5, "go": 1.0}},
		evalSummary{BySource: map[string]float64{"supabase": 0.75, "go": 1.0, "clickhouse": 0.25}},
	)
	want := []evalSourceDrift{
		{Source: "clickhouse", BaselineHitAt5: 0, CurrentHitAt5: 0.25, DeltaPP: 25},
		{Source: "supabase", BaselineHitAt5: 0.5, CurrentHitAt5: 0.75, DeltaPP: 25},
	}
	if !reflect.DeepEqual(drift.SourceDrift, want) {
		t.Fatalf("source drift = %+v, want %+v", drift.SourceDrift, want)
	}
}

func TestEmitEvalDiffWrapsParseError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := emitEvalDiff(path, nil, evalSummary{})
	if len(report.SourceDrift) != 0 {
		t.Fatalf("emitEvalDiff parse failure returned report: %+v", report)
	}
	if err == nil || err.Error() != "parse: unexpected end of JSON input" {
		t.Fatalf("emitEvalDiff parse err = %v", err)
	}
}

func TestEmitEvalDiffUsesRuntimeRankLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	prev := []evalCaseResult{
		{Query: "gain query", FirstHitRank: 0},
		{Query: "lost query", FirstHitRank: 2},
		{Query: "moved query", FirstHitRank: 1},
		{Query: "removed query", FirstHitRank: 1},
	}
	if err := writeBaselineJSON(path, prev, evalSummary{Mode: "fts5", N: len(prev)}); err != nil {
		t.Fatal(err)
	}
	current := []evalCaseResult{
		{Query: "gain query", FirstHitRank: 3},
		{Query: "lost query", FirstHitRank: 0},
		{Query: "moved query", FirstHitRank: 4},
		{Query: "added query", FirstHitRank: 1},
	}
	var report evalDriftReport
	var err error
	out := captureStdout(t, func() {
		report, err = emitEvalDiff(path, current, evalSummary{Mode: "fts5", N: len(current)})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SourceDrift) != 0 {
		t.Fatalf("source drift = %+v, want none", report.SourceDrift)
	}
	for _, want := range []string{
		searchruntime.EvalDiffPerQueryChangesHeader(),
		searchruntime.EvalDiffGainedLine("gain query", 3),
		searchruntime.EvalDiffLostLine("lost query", 2),
		searchruntime.EvalDiffMovedLine("moved query", 1, 4),
		searchruntime.EvalDiffFixtureAdditionsHeader(),
		searchruntime.EvalDiffAddedLine("added query"),
		searchruntime.EvalDiffFixtureRemovalsHeader(),
		searchruntime.EvalDiffRemovedLine("removed query"),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("emitEvalDiff output missing %q:\n%s", want, out)
		}
	}
}

func TestEmitEvalDiffUsesRuntimeMetricLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	prev := evalSummary{
		Mode:      "fts5",
		N:         3,
		HitAt1:    0.50,
		HitAt3:    0.75,
		HitAt5:    1,
		HitAt10:   1,
		RecallAt5: 1,
		MRR:       0.883,
		P50MS:     4389,
		P99MS:     10274,
	}
	if err := writeBaselineJSON(path, nil, prev); err != nil {
		t.Fatal(err)
	}
	current := evalSummary{
		Mode:      "fts5",
		N:         4,
		HitAt1:    0.75,
		HitAt3:    0.75,
		HitAt5:    0.75,
		HitAt10:   1,
		RecallAt5: 0.75,
		MRR:       0.855,
		P50MS:     4537,
		P99MS:     6346,
	}
	var report evalDriftReport
	var err error
	out := captureStdout(t, func() {
		report, err = emitEvalDiff(path, nil, current)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SourceDrift) != 0 {
		t.Fatalf("source drift = %+v, want none", report.SourceDrift)
	}
	for _, want := range []string{
		searchruntime.EvalDiffHeader(path, prev.N, current.N),
		searchruntime.EvalDiffMetricHeader(),
		searchruntime.EvalDiffMetricSeparator(),
		searchruntime.EvalDiffPercentPointLine("Hit@1", prev.HitAt1, current.HitAt1),
		searchruntime.EvalDiffPercentPointLine("Hit@3", prev.HitAt3, current.HitAt3),
		searchruntime.EvalDiffPercentPointLine("Hit@5", prev.HitAt5, current.HitAt5),
		searchruntime.EvalDiffPercentPointLine("Hit@10", prev.HitAt10, current.HitAt10),
		searchruntime.EvalDiffPercentPointLine("Recall@5", prev.RecallAt5, current.RecallAt5),
		searchruntime.EvalDiffFloatLine("MRR", prev.MRR, current.MRR, 3),
		searchruntime.EvalDiffFloatLine("p50 ms", prev.P50MS, current.P50MS, 0),
		searchruntime.EvalDiffFloatLine("p99 ms", prev.P99MS, current.P99MS, 0),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("emitEvalDiff output missing %q:\n%s", want, out)
		}
	}
}

func TestEmitEvalDiffUsesRuntimePerSourceLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := writeBaselineJSON(path, nil, evalSummary{
		Mode:     "fts5",
		BySource: map[string]float64{"stripe": 0.5, "supabase": 1},
	}); err != nil {
		t.Fatal(err)
	}
	var report evalDriftReport
	var err error
	out := captureStdout(t, func() {
		report, err = emitEvalDiff(path, nil, evalSummary{
			Mode:     "fts5",
			BySource: map[string]float64{"stripe": 0.75, "supabase": 0.5},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SourceDrift) != 2 {
		t.Fatalf("source drift = %+v, want 2 rows", report.SourceDrift)
	}
	for _, want := range []string{
		searchruntime.EvalDiffPerSourceHeader(),
		searchruntime.EvalDiffPerSourceLine("stripe", 0.5, 0.75),
		searchruntime.EvalDiffPerSourceLine("supabase", 1, 0.5),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("emitEvalDiff output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteEvalRunArtifacts(t *testing.T) {
	report := evalRunReport{
		SchemaVersion:     "docs-puller-eval-report/v1",
		Timestamp:         "2026-05-03T01:02:03Z",
		Fixture:           "eval/fixture.yaml",
		DocsRoot:          "/docs",
		Limit:             10,
		TokenBudgetTokens: 4000,
		Summary:           evalSummary{Mode: "fts5", N: 1},
		Results:           []evalCaseResult{{Query: "alpha", ExpectedDoc: "demo/alpha.md"}},
	}
	paths, err := writeEvalRunArtifacts(t.TempDir(), report)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(paths.ReportPath)) != "20260503T010203Z" {
		t.Fatalf("report path = %s, want timestamped run dir", paths.ReportPath)
	}
	for _, path := range []string{paths.ReportPath, paths.LatestPath} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var got evalRunReport
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if got.SchemaVersion != report.SchemaVersion || got.Results[0].Query != "alpha" {
			t.Fatalf("unexpected report at %s: %+v", path, got)
		}
	}
}

func TestEmitEvalJSONReturnsEncodeError(t *testing.T) {
	cause := errors.New("short write")
	err := emitEvalJSON(failingEvalJSONWriter{err: cause}, nil, evalSummary{Mode: "fts5"})
	if !errors.Is(err, cause) {
		t.Fatalf("emitEvalJSON error = %v, want writer error", err)
	}
}

func TestWriteBaselineJSONWritesFullArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	summary := evalSummary{Mode: "fts5", N: 1}
	results := []evalCaseResult{{Query: "alpha", ExpectedDoc: "demo/alpha.md"}}
	if err := writeBaselineJSON(path, results, summary); err != nil {
		t.Fatalf("writeBaselineJSON returned error: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	var got struct {
		Summary evalSummary      `json:"summary"`
		Results []evalCaseResult `json:"results"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse baseline: %v", err)
	}
	if got.Summary.Mode != "fts5" || got.Summary.N != 1 {
		t.Fatalf("summary = %+v, want fts5 n=1", got.Summary)
	}
	if len(got.Results) != 1 || got.Results[0].Query != "alpha" {
		t.Fatalf("results = %+v, want alpha result", got.Results)
	}
}

func TestAppendSummaryJSONLWritesRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.jsonl")
	if err := appendSummaryJSONL(path, evalSummary{Mode: "fts5", N: 1}); err != nil {
		t.Fatalf("appendSummaryJSONL first row returned error: %v", err)
	}
	if err := appendSummaryJSONL(path, evalSummary{Mode: "scan", N: 2}); err != nil {
		t.Fatalf("appendSummaryJSONL second row returned error: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("summary rows = %d, want 2:\n%s", len(lines), body)
	}
	var rows []struct {
		Timestamp string      `json:"timestamp"`
		Summary   evalSummary `json:"summary"`
	}
	for _, line := range lines {
		var row struct {
			Timestamp string      `json:"timestamp"`
			Summary   evalSummary `json:"summary"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse summary row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	if rows[0].Timestamp == "" || rows[1].Timestamp == "" {
		t.Fatalf("summary timestamps must be set: %+v", rows)
	}
	if rows[0].Summary.Mode != "fts5" || rows[0].Summary.N != 1 || rows[1].Summary.Mode != "scan" || rows[1].Summary.N != 2 {
		t.Fatalf("summary rows = %+v, want fts5 n=1 then scan n=2", rows)
	}
}

func TestWriteEvalRunArtifactsForRecordWrapsError(t *testing.T) {
	rootFile := filepath.Join(t.TempDir(), "root-file")
	if err := os.WriteFile(rootFile, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := evalRunReport{
		SchemaVersion: "docs-puller-eval-report/v1",
		Timestamp:     "2026-05-03T01:02:03Z",
	}
	paths, err := writeEvalRunArtifactsForRecord(rootFile, report)
	if paths != (evalRunArtifactPaths{}) {
		t.Fatalf("writeEvalRunArtifactsForRecord paths = %+v, want zero value", paths)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "record-run: ") {
		t.Fatalf("writeEvalRunArtifactsForRecord err = %v", err)
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("writeEvalRunArtifactsForRecord err does not wrap cause: %v", err)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("writeEvalRunArtifactsForRecord err lost filesystem cause: %v", err)
	}
}

func TestRunEvalSweepCapturesBaselineBeforeCommand(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "demo")
	writeDoc(t, src, "alpha.md", "# Alpha\n\nalpha beta gamma\n")
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	fixture := filepath.Join(t.TempDir(), "fixture.yaml")
	if err := writeYAML(fixture, "queries:\n  - q: alpha\n    expect: [demo/alpha.md]\n"); err != nil {
		t.Fatal(err)
	}
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	commandRan := false
	diffRan := false
	cfg := evalSweepConfig{
		FixturePath:  fixture,
		Out:          out,
		Limit:        10,
		BaselinePath: baseline,
		Command:      []string{"apply-change"},
		ProfileOpts:  searchOpts{},
	}
	err = runEvalSweep(cfg, func(command []string) error {
		if !reflect.DeepEqual(command, []string{"apply-change"}) {
			t.Fatalf("command = %v", command)
		}
		if _, err := os.Stat(baseline); err != nil {
			t.Fatalf("baseline was not written before command: %v", err)
		}
		commandRan = true
		return nil
	}, func(evalSweepConfig) error {
		if !commandRan {
			t.Fatal("diff ran before command")
		}
		body, err := os.ReadFile(baseline)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"query": "alpha"`) {
			t.Fatalf("baseline missing eval results:\n%s", body)
		}
		diffRan = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !diffRan {
		t.Fatal("diff runner was not called")
	}
}

func TestRunEvalSweepWrapsCommandError(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "demo")
	writeDoc(t, src, "alpha.md", "# Alpha\n\nalpha beta gamma\n")
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	fixture := filepath.Join(t.TempDir(), "fixture.yaml")
	if err := writeYAML(fixture, "queries:\n  - q: alpha\n    expect: [demo/alpha.md]\n"); err != nil {
		t.Fatal(err)
	}
	commandErr := errors.New("apply failed")
	err = runEvalSweep(evalSweepConfig{
		FixturePath:  fixture,
		Out:          out,
		Limit:        10,
		BaselinePath: filepath.Join(t.TempDir(), "baseline.json"),
		Command:      []string{"apply-change"},
		ProfileOpts:  searchOpts{},
	}, func([]string) error {
		return commandErr
	}, func(evalSweepConfig) error {
		t.Fatal("diff runner should not be called after command failure")
		return nil
	})
	if err == nil || err.Error() != searchruntime.EvalSweepCommandError(commandErr).Error() {
		t.Fatalf("runEvalSweep command err = %v", err)
	}
	if !errors.Is(err, commandErr) {
		t.Fatalf("runEvalSweep command err does not wrap cause: %v", err)
	}
}

// TestDispatchSearchFTSOnlyAbortsWhenIndexMissing pins the eval-reliability
// contract: with ftsOnly=true and no FTS index available, dispatchSearch
// must return mode="" + nil hits instead of silently scanning. Without
// this, a parallel `docs pull` rebuild silently polluted eval numbers
// (the third scan-fallback failure mode documented in FOLLOW-UPS).
func TestDispatchSearchFTSOnlyAbortsWhenIndexMissing(t *testing.T) {
	out := t.TempDir() // empty — no FTS index file at all
	o := searchOpts{out: out, limit: 5, ftsOnly: true}
	hits, _, mode := dispatchSearch("any query", o, nil)
	if mode != "" {
		t.Errorf("ftsOnly with no index: mode = %q, want \"\" (degraded)", mode)
	}
	if hits != nil {
		t.Errorf("ftsOnly with no index: hits = %v, want nil", hits)
	}
}

// TestDispatchSearchFallsBackToScanWithoutFTSOnly confirms the CLI
// behavior is preserved: without ftsOnly, missing FTS still degrades to
// scan with a stderr message. Eval is the only caller that should set
// ftsOnly; the CLI keeps its first-run UX intact.
func TestDispatchSearchFallsBackToScanWithoutFTSOnly(t *testing.T) {
	out := t.TempDir() // empty
	src := filepath.Join(out, "demo")
	writeFTSDoc(t, src, "doc.md", "# Demo\n\nhello world\n")
	o := searchOpts{out: out, limit: 5} // ftsOnly=false (CLI mode)
	hits, _, mode := dispatchSearch("hello", o, nil)
	if mode != "scan" {
		t.Errorf("CLI mode with no FTS: mode = %q, want \"scan\"", mode)
	}
	if len(hits) == 0 {
		t.Error("CLI mode should still return results via scan fallback")
	}
}

func TestRunSearchBatchUsesSharedFTSIndex(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "demo")
	writeFTSDoc(t, src, "alpha.md", "# Alpha\n\nshared batch phrase\n")
	writeFTSDoc(t, src, "beta.md", "# Beta\n\nsecond batch phrase\n")
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	idx.close()

	resp, err := runSearchBatch(
		searchOpts{out: out, limit: 5, noSnippets: true, noProfile: true, ftsOnly: true},
		[]searchBatchQuery{
			{Query: "shared batch phrase"},
			{Query: "second batch phrase"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if resp.QueryCount != 2 {
		t.Fatalf("query_count = %d, want 2", resp.QueryCount)
	}
	if resp.ReadMode != ftsReadModeReadOnly {
		t.Fatalf("read_mode = %q, want %q", resp.ReadMode, ftsReadModeReadOnly)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(resp.Results))
	}
	for _, result := range resp.Results {
		if result.Mode != "fts5" {
			t.Fatalf("mode = %q, want fts5", result.Mode)
		}
		if len(result.Results) == 0 {
			t.Fatalf("query %q returned no hits", result.Query)
		}
	}
	if got := resp.Results[0].Results[0].Path; got != "demo/alpha.md" {
		t.Fatalf("first query top path = %q, want demo/alpha.md", got)
	}
	if got := len(resp.Results[0].Results[0].Snippets); got != 0 {
		t.Fatalf("search-batch snippets = %d, want 0 with noSnippets", got)
	}
	if got := resp.Results[1].Results[0].Path; got != "demo/beta.md" {
		t.Fatalf("second query top path = %q, want demo/beta.md", got)
	}

	readIdx, err := openFTSIndexReadOnly(out)
	if err != nil {
		t.Fatal(err)
	}
	defer readIdx.close()
	hits, err := readIdx.search("shared batch phrase", "", 5, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("direct FTS search returned no hits")
	}
	if got := len(hits[0].Snippets); got == 0 {
		t.Fatal("direct FTS search snippets = 0, want snippets by default")
	}
}

func TestRunSearchBatchPreservesPerQueryTelemetry(t *testing.T) {
	t.Setenv("DOCS_PULLER_QUERY_LOG", "")
	out := t.TempDir()
	src := filepath.Join(out, "demo")
	writeFTSDoc(t, src, "alpha.md", "# Alpha\n\nshared batch phrase\n")
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	idx.close()

	_, err = runSearchBatch(
		searchOpts{
			out:             out,
			limit:           5,
			noSnippets:      true,
			noProfile:       true,
			ftsOnly:         true,
			logQuery:        true,
			queryIntent:     "retrieval",
			queryClient:     "ndev-ask",
			queryRunContext: "agent",
		},
		[]searchBatchQuery{
			{Query: "shared batch phrase", Source: "demo"},
			{Query: "second batch phrase", Source: "demo"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := readSearchQueryLog(out, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("query log entries = %d, want 2", len(entries))
	}
	for _, entry := range entries {
		if entry.Client != "ndev-ask" || entry.RunContext != "agent" || entry.Intent != "retrieval" {
			t.Fatalf("query provenance = %+v", entry)
		}
		if entry.SourceFilter != "demo" || entry.Mode != "fts5" {
			t.Fatalf("query search metadata = %+v", entry)
		}
	}
}

func TestRunSearchBatchRejectsEmptyQuery(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "demo")
	writeFTSDoc(t, src, "alpha.md", "# Alpha\n\nshared batch phrase\n")
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	idx.close()

	resp, err := runSearchBatch(
		searchOpts{out: out, limit: 5, noSnippets: true, noProfile: true, ftsOnly: true},
		[]searchBatchQuery{{}},
	)
	if err == nil {
		t.Fatalf("expected empty-query error, got response %+v", resp)
	}
	if err.Error() != "empty query" {
		t.Fatalf("runSearchBatch error = %v", err)
	}
}

func TestRunSearchBatchCanUseImmutableFTSIndex(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "demo")
	writeFTSDoc(t, src, "alpha.md", "# Alpha\n\nimmutable batch phrase\n")
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	idx.close()

	resp, err := runSearchBatch(
		searchOpts{out: out, limit: 5, noSnippets: true, noProfile: true, ftsOnly: true, ftsReadMode: ftsReadModeImmutable},
		[]searchBatchQuery{{Query: "immutable batch phrase"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ReadMode != ftsReadModeImmutable {
		t.Fatalf("read_mode = %q, want %q", resp.ReadMode, ftsReadModeImmutable)
	}
	if got := resp.Results[0].Results[0].Path; got != "demo/alpha.md" {
		t.Fatalf("top path = %q, want demo/alpha.md", got)
	}
}

func BenchmarkFTSSearchBatchExternalIndex(b *testing.B) {
	out := os.Getenv("NDEV_DOCS_PULLER_BENCH_OUT")
	queriesPath := os.Getenv("NDEV_DOCS_PULLER_BENCH_QUERIES")
	if out == "" || queriesPath == "" {
		b.Skip("set NDEV_DOCS_PULLER_BENCH_OUT and NDEV_DOCS_PULLER_BENCH_QUERIES")
	}
	queries, err := loadSearchBatchQueries(queriesPath)
	if err != nil {
		b.Fatal(err)
	}
	readMode := normalizeFTSReadMode(os.Getenv("NDEV_DOCS_PULLER_BENCH_READ_MODE"))
	idx, err := openFTSIndexReadMode(out, readMode)
	if err != nil {
		b.Fatal(err)
	}
	defer idx.close()
	totalDocs, err := idx.totalDocs()
	if err != nil {
		b.Fatal(err)
	}

	opts := searchOpts{out: out, limit: 10, noSnippets: true, noProfile: true, ftsOnly: true, cachedFTSTotalDocs: totalDocs}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, q := range queries {
			query := q.Query
			if query == "" {
				query = q.Q
			}
			perQueryOpts := opts
			if q.Source != "" {
				perQueryOpts.source = q.Source
			}
			hits, _, mode := dispatchSearch(query, perQueryOpts, idx)
			if mode != "fts5" {
				b.Fatalf("query did not use FTS5: mode=%q", mode)
			}
			if len(hits) == 0 {
				b.Fatal("query returned no hits")
			}
		}
	}
}

func BenchmarkFTSRebuildExternalCorpus(b *testing.B) {
	out := os.Getenv("NDEV_DOCS_PULLER_BENCH_OUT")
	if out == "" {
		b.Skip("set NDEV_DOCS_PULLER_BENCH_OUT")
	}
	idx, err := openFTSIndex(out)
	if err != nil {
		b.Fatal(err)
	}
	defer idx.close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.rebuild(out); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFTSRebuildExternalCorpusFromMemory(b *testing.B) {
	out := os.Getenv("NDEV_DOCS_PULLER_BENCH_OUT")
	if out == "" {
		b.Skip("set NDEV_DOCS_PULLER_BENCH_OUT")
	}
	docs := loadExternalCorpusFTSMemoryDocs(b, out)
	idx, err := openFTSIndex(out)
	if err != nil {
		b.Fatal(err)
	}
	defer idx.close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.rebuildFromMemory(docs); err != nil {
			b.Fatal(err)
		}
	}
}

func loadExternalCorpusFTSMemoryDocs(tb testing.TB, out string) []ftsMemoryDoc {
	tb.Helper()
	sources, err := listSources(out)
	if err != nil {
		tb.Fatal(err)
	}
	var docs []ftsMemoryDoc
	for _, src := range sources {
		srcDir := filepath.Join(out, src)
		metaByPath := loadFTSDocMeta(srcDir, src)
		err := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".md") || name == "_INDEX.md" {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(srcDir, p)
			meta := metaByPath[rel]
			docs = append(docs, ftsMemoryDoc{
				Path:    filepath.Join(src, rel),
				AbsPath: p,
				Body:    data,
				URL:     meta.URL,
				Warning: meta.Warning,
				Title:   extractTitleFromBytes(p, data),
			})
			return nil
		})
		if err != nil {
			tb.Fatal(err)
		}
	}
	applyFTSMemoryHubFlags(docs)
	return docs
}

func TestLoadSearchBatchQueriesAcceptsStringsAndObjects(t *testing.T) {
	dir := t.TempDir()
	stringsPath := filepath.Join(dir, "strings.json")
	if err := os.WriteFile(stringsPath, []byte(`["alpha","beta"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	stringQueries, err := loadSearchBatchQueries(stringsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{stringQueries[0].Query, stringQueries[1].Query}; !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("string queries = %v", got)
	}

	objectsPath := filepath.Join(dir, "objects.json")
	if err := os.WriteFile(objectsPath, []byte(`[{"query":"alpha"},{"q":"beta","source":"demo"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	objectQueries, err := loadSearchBatchQueries(objectsPath)
	if err != nil {
		t.Fatal(err)
	}
	if objectQueries[0].Query != "alpha" || objectQueries[1].Q != "beta" || objectQueries[1].Source != "demo" {
		t.Fatalf("object queries parsed incorrectly: %+v", objectQueries)
	}
}

// TestWaitForFTSReadyTimesOutOnMissingDir confirms the pre-flight wait
// returns false within budget when no index is being built. Important so
// eval doesn't hang forever on an empty docs root.
func TestWaitForFTSReadyTimesOutOnMissingDir(t *testing.T) {
	out := t.TempDir() // no index, won't ever appear
	start := time.Now()
	ready := waitForFTSReady(out, 500*time.Millisecond)
	elapsed := time.Since(start)
	if ready {
		t.Error("waitForFTSReady on empty dir returned true; want false")
	}
	if elapsed < 400*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Errorf("elapsed = %v, want ~500ms (poll interval slop allowed)", elapsed)
	}
}

func TestPercentile(t *testing.T) {
	if got := searchruntime.EvalPercentile(nil, 0.5); got != 0 {
		t.Errorf("empty: got %v, want 0", got)
	}
	vals := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := searchruntime.EvalPercentile(vals, 0.50); got != 5 {
		t.Errorf("p50 of 1..10: got %v, want 5", got)
	}
	// percentile uses int truncation: idx = int((n-1) * p) = int(8.91) = 8 → vals[8] = 9.
	// Acceptable approximation for an eval signal; bumping to ceil would round up.
	if got := searchruntime.EvalPercentile(vals, 0.99); got != 9 {
		t.Errorf("p99 of 1..10: got %v, want 9", got)
	}
}

func TestNormalizeEvalPath(t *testing.T) {
	cases := map[string]string{
		"supabase/guides/x.md":  "supabase/guides/x.md",
		"/supabase/guides/x.md": "supabase/guides/x.md",
		"  /go/ref/spec.md  ":   "go/ref/spec.md",
	}
	for in, want := range cases {
		if got := searchruntime.NormalizeEvalPath(in); got != want {
			t.Errorf("NormalizeEvalPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func writeYAML(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
