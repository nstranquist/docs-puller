package main

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nstranquist/docs-puller/searchruntime"
)

type evalSuiteOverviewReport struct {
	GeneratedAt          string
	DocsRoot             string
	Limit                int
	TokenBudgetTokens    int
	AnswerContextEnabled bool
	Fixtures             []evalSuiteRow
	Overall              evalOverviewMetric
	ByLibrary            []evalOverviewMetric
	ByQueryType          []evalOverviewMetric
	SlowQueries          []evalCaseResult
	TokenHeavyQueries    []evalCaseResult
}

type evalOverviewMetric struct {
	Name                       string
	N                          int
	HitAt1                     float64
	HitAt3                     float64
	HitAt5                     float64
	HitAt10                    float64
	RecallAt5                  float64
	MRR                        float64
	AvgMS                      float64
	P50MS                      float64
	P95MS                      float64
	P99MS                      float64
	AvgTokensReturned          float64
	P50TokensReturned          float64
	P95TokensReturned          float64
	P99TokensReturned          float64
	AvgAnswerContextTokens     float64
	P50AnswerContextTokens     float64
	P95AnswerContextTokens     float64
	P99AnswerContextTokens     float64
	TokenBudgetHitRate         float64
	AnswerContextBudgetHitRate float64
}

func buildEvalSuiteOverview(rows []evalSuiteRow, docsRoot string, limit, tokenBudget int, answerContext bool, now time.Time) evalSuiteOverviewReport {
	results := evalOverviewResults(rows)
	return evalSuiteOverviewReport{
		GeneratedAt:          now.UTC().Format(time.RFC3339),
		DocsRoot:             docsRoot,
		Limit:                limit,
		TokenBudgetTokens:    tokenBudget,
		AnswerContextEnabled: answerContext,
		Fixtures:             rows,
		Overall:              evalOverviewMetricForResults("overall", results),
		ByLibrary:            evalOverviewMetricsBy(results, evalOverviewLibraryName, evalOverviewSortLibraries),
		ByQueryType:          evalOverviewMetricsBy(results, evalOverviewQueryTypeName, evalOverviewSortQueryTypes),
		SlowQueries:          evalOverviewTopQueries(results, evalOverviewQueryLatency, 10),
		TokenHeavyQueries:    evalOverviewTopQueries(results, evalOverviewQueryTokenWeight(answerContext), 10),
	}
}

func evalOverviewResults(rows []evalSuiteRow) []evalCaseResult {
	var out []evalCaseResult
	for _, row := range rows {
		if row.Error != "" {
			continue
		}
		out = append(out, row.Results...)
	}
	return out
}

func evalOverviewMetricsBy(results []evalCaseResult, keyFn func(evalCaseResult) string, sortFn func([]evalOverviewMetric)) []evalOverviewMetric {
	grouped := map[string][]evalCaseResult{}
	for _, r := range results {
		key := keyFn(r)
		grouped[key] = append(grouped[key], r)
	}
	metrics := make([]evalOverviewMetric, 0, len(grouped))
	for key, group := range grouped {
		metrics = append(metrics, evalOverviewMetricForResults(key, group))
	}
	sortFn(metrics)
	return metrics
}

func evalOverviewMetricForResults(name string, results []evalCaseResult) evalOverviewMetric {
	m := evalOverviewMetric{Name: name, N: len(results)}
	if len(results) == 0 {
		return m
	}
	var (
		h1, h3, h5, h10     int
		tokenBudgetHits     int
		answerBudgetHits    int
		recallSum           float64
		mrrSum              float64
		latencies           []float64
		tokens              []float64
		answerContextTokens []float64
		latencyTotal        float64
		tokenTotal          float64
		answerContextTotal  float64
	)
	for _, r := range results {
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
			mrrSum += 1 / float64(r.FirstHitRank)
		}
		if r.TokenBudgetHit {
			tokenBudgetHits++
		}
		if r.AnswerContextBudgetHit {
			answerBudgetHits++
		}
		recallSum += r.RecallAt5
		latencyTotal += r.LatencyMS
		latencies = append(latencies, r.LatencyMS)
		v := float64(r.TokensReturned)
		tokenTotal += v
		tokens = append(tokens, v)
		answerV := float64(r.AnswerContextTokensReturned)
		answerContextTotal += answerV
		answerContextTokens = append(answerContextTokens, answerV)
	}
	n := float64(len(results))
	m.HitAt1 = float64(h1) / n
	m.HitAt3 = float64(h3) / n
	m.HitAt5 = float64(h5) / n
	m.HitAt10 = float64(h10) / n
	m.RecallAt5 = recallSum / n
	m.MRR = mrrSum / n
	m.AvgMS = latencyTotal / n
	m.P50MS = searchruntime.EvalPercentile(latencies, 0.50)
	m.P95MS = searchruntime.EvalPercentile(latencies, 0.95)
	m.P99MS = searchruntime.EvalPercentile(latencies, 0.99)
	if len(tokens) > 0 {
		m.AvgTokensReturned = tokenTotal / float64(len(tokens))
		m.P50TokensReturned = searchruntime.EvalPercentile(tokens, 0.50)
		m.P95TokensReturned = searchruntime.EvalPercentile(tokens, 0.95)
		m.P99TokensReturned = searchruntime.EvalPercentile(tokens, 0.99)
	}
	if len(answerContextTokens) > 0 {
		m.AvgAnswerContextTokens = answerContextTotal / float64(len(answerContextTokens))
		m.P50AnswerContextTokens = searchruntime.EvalPercentile(answerContextTokens, 0.50)
		m.P95AnswerContextTokens = searchruntime.EvalPercentile(answerContextTokens, 0.95)
		m.P99AnswerContextTokens = searchruntime.EvalPercentile(answerContextTokens, 0.99)
	}
	m.TokenBudgetHitRate = float64(tokenBudgetHits) / n
	m.AnswerContextBudgetHitRate = float64(answerBudgetHits) / n
	return m
}

func evalOverviewLibraryName(r evalCaseResult) string {
	if strings.TrimSpace(r.ExpectedSource) != "" {
		return strings.TrimSpace(r.ExpectedSource)
	}
	if len(r.Expect) > 0 {
		if source := searchruntime.EvalSourceFromPath(r.Expect[0]); source != "" {
			return source
		}
	}
	if strings.TrimSpace(r.Source) != "" {
		return strings.TrimSpace(r.Source)
	}
	return "unknown"
}

func evalOverviewQueryTypeName(r evalCaseResult) string {
	queryType := strings.TrimSpace(r.QueryType)
	if queryType == "" {
		return "unknown"
	}
	return queryType
}

func evalOverviewSortLibraries(metrics []evalOverviewMetric) {
	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].HitAt5 != metrics[j].HitAt5 {
			return metrics[i].HitAt5 < metrics[j].HitAt5
		}
		if metrics[i].P95MS != metrics[j].P95MS {
			return metrics[i].P95MS > metrics[j].P95MS
		}
		return metrics[i].Name < metrics[j].Name
	})
}

func evalOverviewSortQueryTypes(metrics []evalOverviewMetric) {
	order := map[string]int{
		"library_search":       0,
		"tech_stack_search":    1,
		"production_telemetry": 2,
		"natural_language":     3,
		"lexical":              4,
		"unknown":              99,
	}
	sort.Slice(metrics, func(i, j int) bool {
		oi, ok := order[metrics[i].Name]
		if !ok {
			oi = 50
		}
		oj, ok := order[metrics[j].Name]
		if !ok {
			oj = 50
		}
		if oi != oj {
			return oi < oj
		}
		return metrics[i].Name < metrics[j].Name
	})
}

func evalOverviewTopQueries(results []evalCaseResult, score func(evalCaseResult) float64, limit int) []evalCaseResult {
	sorted := append([]evalCaseResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool {
		si := score(sorted[i])
		sj := score(sorted[j])
		if si != sj {
			return si > sj
		}
		return sorted[i].Query < sorted[j].Query
	})
	if len(sorted) > limit {
		sorted = sorted[:limit]
	}
	return sorted
}

func evalOverviewQueryLatency(r evalCaseResult) float64 {
	return r.LatencyMS
}

func evalOverviewQueryTokenWeight(answerContext bool) func(evalCaseResult) float64 {
	return func(r evalCaseResult) float64 {
		if answerContext && r.AnswerContextTokensReturned > 0 {
			return float64(r.AnswerContextTokensReturned)
		}
		return float64(r.TokensReturned)
	}
}

func writeEvalSuiteOverviewMarkdown(path string, report evalSuiteOverviewReport) error {
	return writeEvalSuiteOverviewFile(path, renderEvalSuiteOverviewMarkdown(report))
}

func writeEvalSuiteOverviewHTML(path string, report evalSuiteOverviewReport) error {
	return writeEvalSuiteOverviewFile(path, renderEvalSuiteOverviewHTML(report))
}

func writeEvalSuiteOverviewFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func renderEvalSuiteOverviewMarkdown(report evalSuiteOverviewReport) string {
	var b strings.Builder
	b.WriteString("# Docs-puller Retrieval Metrics Overview\n\n")
	fmt.Fprintf(&b, "- Generated: `%s`\n", report.GeneratedAt)
	fmt.Fprintf(&b, "- Docs root: `%s`\n", report.DocsRoot)
	fmt.Fprintf(&b, "- Query limit: `%d`\n", report.Limit)
	fmt.Fprintf(&b, "- Token budget: `%d`\n", report.TokenBudgetTokens)
	fmt.Fprintf(&b, "- Answer-context token count: `%t`\n", report.AnswerContextEnabled)
	b.WriteString("- Token estimator: Go `searchruntime.ApproxEvalTokens`, `ceil(trimmed byte length / 4)`.\n\n")

	b.WriteString("## Overall\n\n")
	b.WriteString("| queries | hit@1 | hit@5 | mrr | p50 ms | p95 ms | p99 ms | avg returned tokens | p95 returned tokens | avg answer tokens | p95 answer tokens | budget hit |\n")
	b.WriteString("| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	b.WriteString(evalOverviewMarkdownMetricRow(report.Overall, report.AnswerContextEnabled))
	b.WriteString("\n")

	b.WriteString("## By Docs Library\n\n")
	evalOverviewWriteMarkdownMetricTable(&b, report.ByLibrary, report.AnswerContextEnabled)
	b.WriteString("\n## By Query Type\n\n")
	evalOverviewWriteMarkdownMetricTable(&b, report.ByQueryType, report.AnswerContextEnabled)

	b.WriteString("\n## Fixture Summary\n\n")
	b.WriteString("| fixture | queries | hit@1 | hit@5 | mrr | p50 ms | p99 ms | answer p50 tokens | answer p99 tokens |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, row := range report.Fixtures {
		if row.Error != "" {
			fmt.Fprintf(&b, "| `%s` | error | | | | | | | |\n", filepath.Base(row.Fixture))
			continue
		}
		s := row.Summary
		fmt.Fprintf(&b, "| `%s` | %d | %s | %s | %.3f | %s | %s | %s | %s |\n",
			filepath.Base(row.Fixture), s.N, evalOverviewPct(s.HitAt1), evalOverviewPct(s.HitAt5), s.MRR,
			evalOverviewFloat(s.P50MS), evalOverviewFloat(s.P99MS),
			evalOverviewFloat(s.P50AnswerContextTokens), evalOverviewFloat(s.P99AnswerContextTokens))
	}

	b.WriteString("\n## Slowest Queries\n\n")
	evalOverviewWriteMarkdownQueryTable(&b, report.SlowQueries, report.AnswerContextEnabled, "latency")
	b.WriteString("\n## Heaviest Returned Context\n\n")
	evalOverviewWriteMarkdownQueryTable(&b, report.TokenHeavyQueries, report.AnswerContextEnabled, "tokens")
	return b.String()
}

func evalOverviewWriteMarkdownMetricTable(b *strings.Builder, metrics []evalOverviewMetric, answerContext bool) {
	b.WriteString("| group | queries | hit@1 | hit@5 | mrr | p50 ms | p95 ms | p99 ms | avg returned tokens | p95 returned tokens | avg answer tokens | p95 answer tokens | budget hit |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, metric := range metrics {
		b.WriteString(evalOverviewMarkdownNamedMetricRow(metric, answerContext))
	}
}

func evalOverviewMarkdownNamedMetricRow(metric evalOverviewMetric, answerContext bool) string {
	return fmt.Sprintf("| %s | %s", evalOverviewMarkdownText(evalOverviewMetricLabel(metric.Name)), strings.TrimPrefix(evalOverviewMarkdownMetricRow(metric, answerContext), "| "))
}

func evalOverviewMarkdownMetricRow(metric evalOverviewMetric, answerContext bool) string {
	avgAnswer := "-"
	p95Answer := "-"
	budgetHit := evalOverviewPct(metric.TokenBudgetHitRate)
	if answerContext {
		avgAnswer = evalOverviewFloat(metric.AvgAnswerContextTokens)
		p95Answer = evalOverviewFloat(metric.P95AnswerContextTokens)
		budgetHit = evalOverviewPct(metric.AnswerContextBudgetHitRate)
	}
	return fmt.Sprintf("| %d | %s | %s | %.3f | %s | %s | %s | %s | %s | %s | %s | %s |\n",
		metric.N, evalOverviewPct(metric.HitAt1), evalOverviewPct(metric.HitAt5), metric.MRR,
		evalOverviewFloat(metric.P50MS), evalOverviewFloat(metric.P95MS), evalOverviewFloat(metric.P99MS),
		evalOverviewFloat(metric.AvgTokensReturned), evalOverviewFloat(metric.P95TokensReturned),
		avgAnswer, p95Answer, budgetHit)
}

func evalOverviewWriteMarkdownQueryTable(b *strings.Builder, rows []evalCaseResult, answerContext bool, sortKey string) {
	b.WriteString("| query | library | type | rank | hit@5 | latency ms | returned tokens | answer tokens |\n")
	b.WriteString("| --- | --- | --- | ---: | --- | ---: | ---: | ---: |\n")
	for _, r := range rows {
		answerTokens := "-"
		if answerContext {
			answerTokens = fmt.Sprintf("%d", r.AnswerContextTokensReturned)
		}
		fmt.Fprintf(b, "| %s | `%s` | %s | %d | %t | %s | %d | %s |\n",
			evalOverviewMarkdownText(r.Query), evalOverviewLibraryName(r),
			evalOverviewMarkdownText(evalOverviewMetricLabel(evalOverviewQueryTypeName(r))), r.FirstHitRank, r.HitAt5,
			evalOverviewFloat(r.LatencyMS), r.TokensReturned, answerTokens)
	}
	if len(rows) == 0 {
		fmt.Fprintf(b, "| _no %s data_ | | | | | | | |\n", sortKey)
	}
}

func renderEvalSuiteOverviewHTML(report evalSuiteOverviewReport) string {
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>docs-puller retrieval metrics</title>
<style>
  :root {
    --bg:#0f1115; --panel:#171b22; --panel-2:#202631; --line:#2d3542;
    --text:#d7dde7; --strong:#ffffff; --muted:#8b95a5;
    --blue:#7ab7ff; --green:#7ee787; --amber:#f4b860; --red:#ff7b72; --teal:#56d4dc;
  }
  body.light {
    --bg:#fbfbf9; --panel:#ffffff; --panel-2:#f3f5f8; --line:#dce1e8;
    --text:#24292f; --strong:#0d1117; --muted:#68707c;
    --blue:#0969da; --green:#1a7f37; --amber:#9a6700; --red:#cf222e; --teal:#1b7b85;
  }
  * { box-sizing: border-box; }
  body { margin:0; padding:42px 24px 64px; background:var(--bg); color:var(--text); font:14px/1.55 -apple-system,BlinkMacSystemFont,"SF Pro Text",system-ui,sans-serif; }
  main { max-width:1240px; margin:0 auto; }
  .toolbar { position:fixed; top:16px; right:20px; display:flex; gap:6px; background:var(--panel); border:1px solid var(--line); border-radius:6px; padding:4px; }
  .toolbar button { border:0; background:transparent; color:var(--muted); padding:4px 10px; border-radius:4px; cursor:pointer; font:inherit; font-size:12px; }
  .toolbar button.on { background:var(--panel-2); color:var(--strong); }
  h1 { margin:0 0 8px; color:var(--strong); font-size:34px; font-weight:650; letter-spacing:0; }
  .lede { color:var(--muted); max-width:850px; margin:0 0 10px; font-size:16px; }
  .meta { color:var(--muted); font:12px/1.5 "SF Mono",Menlo,monospace; margin:0 0 26px; }
  h2 { margin:36px 0 12px; color:var(--strong); font-size:16px; text-transform:uppercase; letter-spacing:.04em; }
  .stats { display:grid; grid-template-columns:repeat(5,1fr); gap:12px; margin:18px 0 28px; }
  .stat { background:var(--panel); border:1px solid var(--line); border-radius:8px; padding:14px 16px; }
  .stat .label { color:var(--muted); font-size:11px; text-transform:uppercase; letter-spacing:.06em; }
  .stat .num { color:var(--strong); font-size:25px; font-weight:650; margin-top:3px; }
  .stat .num.good { color:var(--green); } .stat .num.warn { color:var(--amber); } .stat .num.red { color:var(--red); } .stat .num.blue { color:var(--blue); }
  table { width:100%; border-collapse:collapse; background:var(--panel); border:1px solid var(--line); border-radius:8px; overflow:hidden; }
  th,td { padding:8px 10px; border-bottom:1px solid var(--line); vertical-align:top; }
  th { color:var(--muted); font-size:11px; text-transform:uppercase; letter-spacing:.05em; text-align:left; background:var(--panel-2); }
  td.num, th.num { text-align:right; font-family:"SF Mono",Menlo,monospace; white-space:nowrap; }
  tr:last-child td { border-bottom:0; }
  code, .mono { font-family:"SF Mono",Menlo,monospace; font-size:12px; }
  .pill { display:inline-block; border:1px solid var(--line); border-radius:999px; padding:1px 7px; color:var(--strong); background:var(--panel-2); font-size:12px; }
  .hit { color:var(--green); } .miss { color:var(--red); }
  @media (max-width: 900px) { body { padding:34px 14px 52px; } .stats { grid-template-columns:1fr 1fr; } table { font-size:12px; } th,td { padding:7px 8px; } }
</style>
</head>
<body>
<div class="toolbar"><button id="tdark" class="on">dark</button><button id="tlight">light</button></div>
<main>
`)
	fmt.Fprintf(&b, "<h1>docs-puller retrieval metrics</h1>\n")
	fmt.Fprintf(&b, "<p class=\"lede\">Per-library and per-query-type retrieval quality, latency, and returned-context token counts from <code>docs-puller eval-suite</code>.</p>\n")
	fmt.Fprintf(&b, "<p class=\"meta\">generated=%s docs_root=%s limit=%d token_budget=%d answer_context=%t token_estimator=ceil(trimmed_bytes/4)</p>\n",
		html.EscapeString(report.GeneratedAt), html.EscapeString(report.DocsRoot), report.Limit, report.TokenBudgetTokens, report.AnswerContextEnabled)
	b.WriteString("<section class=\"stats\">\n")
	evalOverviewWriteHTMLStat(&b, "Queries", fmt.Sprintf("%d", report.Overall.N), "")
	evalOverviewWriteHTMLStat(&b, "Hit@1", evalOverviewPct(report.Overall.HitAt1), evalOverviewClassForPct(report.Overall.HitAt1))
	evalOverviewWriteHTMLStat(&b, "Hit@5", evalOverviewPct(report.Overall.HitAt5), evalOverviewClassForPct(report.Overall.HitAt5))
	evalOverviewWriteHTMLStat(&b, "P95 latency", evalOverviewFloat(report.Overall.P95MS)+" ms", "blue")
	if report.AnswerContextEnabled {
		evalOverviewWriteHTMLStat(&b, "P95 answer tokens", evalOverviewFloat(report.Overall.P95AnswerContextTokens), "warn")
	} else {
		evalOverviewWriteHTMLStat(&b, "P95 returned tokens", evalOverviewFloat(report.Overall.P95TokensReturned), "warn")
	}
	b.WriteString("</section>\n")

	b.WriteString("<h2>By Docs Library</h2>\n")
	evalOverviewWriteHTMLMetricTable(&b, report.ByLibrary, report.AnswerContextEnabled)
	b.WriteString("<h2>By Query Type</h2>\n")
	evalOverviewWriteHTMLMetricTable(&b, report.ByQueryType, report.AnswerContextEnabled)
	b.WriteString("<h2>Fixture Summary</h2>\n")
	evalOverviewWriteHTMLFixtureTable(&b, report.Fixtures)
	b.WriteString("<h2>Slowest Queries</h2>\n")
	evalOverviewWriteHTMLQueryTable(&b, report.SlowQueries, report.AnswerContextEnabled)
	b.WriteString("<h2>Heaviest Returned Context</h2>\n")
	evalOverviewWriteHTMLQueryTable(&b, report.TokenHeavyQueries, report.AnswerContextEnabled)
	b.WriteString(`</main>
<script>
const dark = document.getElementById('tdark');
const light = document.getElementById('tlight');
function setTheme(t){ document.body.classList.toggle('light', t === 'light'); dark.classList.toggle('on', t !== 'light'); light.classList.toggle('on', t === 'light'); localStorage.setItem('docsPullerMetricsTheme', t); }
dark.onclick = () => setTheme('dark');
light.onclick = () => setTheme('light');
setTheme(localStorage.getItem('docsPullerMetricsTheme') || 'dark');
</script>
</body>
</html>
`)
	return b.String()
}

func evalOverviewWriteHTMLStat(b *strings.Builder, label, value, className string) {
	fmt.Fprintf(b, "<div class=\"stat\"><div class=\"label\">%s</div><div class=\"num %s\">%s</div></div>\n",
		html.EscapeString(label), html.EscapeString(className), html.EscapeString(value))
}

func evalOverviewWriteHTMLMetricTable(b *strings.Builder, metrics []evalOverviewMetric, answerContext bool) {
	b.WriteString("<table><thead><tr><th>Group</th><th class=\"num\">Queries</th><th class=\"num\">Hit@1</th><th class=\"num\">Hit@5</th><th class=\"num\">MRR</th><th class=\"num\">P50 ms</th><th class=\"num\">P95 ms</th><th class=\"num\">P99 ms</th><th class=\"num\">Avg tokens</th><th class=\"num\">P95 tokens</th><th class=\"num\">Avg answer</th><th class=\"num\">P95 answer</th><th class=\"num\">Budget hit</th></tr></thead><tbody>\n")
	for _, metric := range metrics {
		avgAnswer := "-"
		p95Answer := "-"
		budgetHit := evalOverviewPct(metric.TokenBudgetHitRate)
		if answerContext {
			avgAnswer = evalOverviewFloat(metric.AvgAnswerContextTokens)
			p95Answer = evalOverviewFloat(metric.P95AnswerContextTokens)
			budgetHit = evalOverviewPct(metric.AnswerContextBudgetHitRate)
		}
		fmt.Fprintf(b, "<tr><td><span class=\"pill\">%s</span></td><td class=\"num\">%d</td><td class=\"num\">%s</td><td class=\"num\">%s</td><td class=\"num\">%.3f</td><td class=\"num\">%s</td><td class=\"num\">%s</td><td class=\"num\">%s</td><td class=\"num\">%s</td><td class=\"num\">%s</td><td class=\"num\">%s</td><td class=\"num\">%s</td><td class=\"num\">%s</td></tr>\n",
			html.EscapeString(evalOverviewMetricLabel(metric.Name)), metric.N,
			evalOverviewPct(metric.HitAt1), evalOverviewPct(metric.HitAt5), metric.MRR,
			evalOverviewFloat(metric.P50MS), evalOverviewFloat(metric.P95MS), evalOverviewFloat(metric.P99MS),
			evalOverviewFloat(metric.AvgTokensReturned), evalOverviewFloat(metric.P95TokensReturned),
			avgAnswer, p95Answer, budgetHit)
	}
	b.WriteString("</tbody></table>\n")
}

func evalOverviewWriteHTMLFixtureTable(b *strings.Builder, rows []evalSuiteRow) {
	b.WriteString("<table><thead><tr><th>Fixture</th><th class=\"num\">Queries</th><th class=\"num\">Hit@1</th><th class=\"num\">Hit@5</th><th class=\"num\">MRR</th><th class=\"num\">P50 ms</th><th class=\"num\">P99 ms</th><th class=\"num\">Answer p50</th><th class=\"num\">Answer p99</th></tr></thead><tbody>\n")
	for _, row := range rows {
		if row.Error != "" {
			fmt.Fprintf(b, "<tr><td><code>%s</code></td><td colspan=\"8\">%s</td></tr>\n", html.EscapeString(filepath.Base(row.Fixture)), html.EscapeString(row.Error))
			continue
		}
		s := row.Summary
		fmt.Fprintf(b, "<tr><td><code>%s</code></td><td class=\"num\">%d</td><td class=\"num\">%s</td><td class=\"num\">%s</td><td class=\"num\">%.3f</td><td class=\"num\">%s</td><td class=\"num\">%s</td><td class=\"num\">%s</td><td class=\"num\">%s</td></tr>\n",
			html.EscapeString(filepath.Base(row.Fixture)), s.N, evalOverviewPct(s.HitAt1), evalOverviewPct(s.HitAt5), s.MRR,
			evalOverviewFloat(s.P50MS), evalOverviewFloat(s.P99MS), evalOverviewFloat(s.P50AnswerContextTokens), evalOverviewFloat(s.P99AnswerContextTokens))
	}
	b.WriteString("</tbody></table>\n")
}

func evalOverviewWriteHTMLQueryTable(b *strings.Builder, rows []evalCaseResult, answerContext bool) {
	b.WriteString("<table><thead><tr><th>Query</th><th>Library</th><th>Type</th><th class=\"num\">Rank</th><th class=\"num\">Hit@5</th><th class=\"num\">Latency ms</th><th class=\"num\">Returned tokens</th><th class=\"num\">Answer tokens</th></tr></thead><tbody>\n")
	for _, r := range rows {
		answerTokens := "-"
		if answerContext {
			answerTokens = fmt.Sprintf("%d", r.AnswerContextTokensReturned)
		}
		hitClass := "miss"
		if r.HitAt5 {
			hitClass = "hit"
		}
		fmt.Fprintf(b, "<tr><td>%s</td><td><code>%s</code></td><td>%s</td><td class=\"num\">%d</td><td class=\"num %s\">%t</td><td class=\"num\">%s</td><td class=\"num\">%d</td><td class=\"num\">%s</td></tr>\n",
			html.EscapeString(r.Query), html.EscapeString(evalOverviewLibraryName(r)),
			html.EscapeString(evalOverviewMetricLabel(evalOverviewQueryTypeName(r))), r.FirstHitRank, hitClass, r.HitAt5,
			evalOverviewFloat(r.LatencyMS), r.TokensReturned, answerTokens)
	}
	b.WriteString("</tbody></table>\n")
}

func evalOverviewMetricLabel(name string) string {
	switch name {
	case "library_search":
		return "Library search"
	case "tech_stack_search":
		return "Tech stack search"
	case "production_telemetry":
		return "Production telemetry"
	case "natural_language":
		return "Natural language"
	case "lexical":
		return "Lexical search"
	case "unknown":
		return "Unknown"
	default:
		return name
	}
}

func evalOverviewClassForPct(v float64) string {
	switch {
	case v >= 0.9:
		return "good"
	case v >= 0.75:
		return "warn"
	default:
		return "red"
	}
}

func evalOverviewPct(v float64) string {
	return fmt.Sprintf("%.1f%%", v*100)
}

func evalOverviewFloat(v float64) string {
	return fmt.Sprintf("%.0f", v)
}

func evalOverviewMarkdownText(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
