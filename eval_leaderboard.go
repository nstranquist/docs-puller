package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// eval-leaderboard renders a public, dated, reproducible retrieval-quality
// leaderboard from a real eval run. It is distinct from `eval-suite
// --overview-html` (an internal, debug-framed, worst-first overview): the
// leaderboard leads with a number, documents the methodology, gives a
// copy-pasteable "reproduce this", and carries an HONEST competitor matrix
// (measured vs not-yet-measured) — the marketing engine the autoresearch verdict
// recommended over a billing page.
//
// It runs BM25-only by default (no --rerank-llm) so the published numbers are
// reproducible with no API key. The LLM-rerank uplift is cited as a separately
// measured figure, never silently blended in.

const defaultLeaderboardRel = "docs/human/docs-puller-leaderboard.html"

// competitor is one row of the honest positioning matrix. Measured == false
// means we publish positioning + an open invitation, never an invented number.
type competitor struct {
	Name     string
	What     string
	Measured bool
	Note     string
}

// leaderboardCompetitors is positioning data, grounded in the 2026-06-02
// autoresearch verdict (28 sources). Only docs-puller carries measured numbers;
// every other row is explicitly "not yet measured (open methodology)".
var leaderboardCompetitors = []competitor{
	{Name: "docs-puller", What: "local-first FTS5 + embeddings, pull ANY docs", Measured: true, Note: "measured below — Hit@1/Hit@5/MRR on the public fixture"},
	{Name: "Context7", What: "hosted MCP docs server", Measured: false, Note: "free incumbent; no public retrieval-quality number. Methodology open — run our fixture against it and we'll publish the cell."},
	{Name: "Ref.tools", What: "hosted MCP docs server", Measured: false, Note: "reported to beat Context7 on UX/token-efficiency; no published Hit@k. Open invitation to co-measure."},
	{Name: "Mintlify", What: "AI-native docs publishing (~50% of hosted traffic is agents)", Measured: false, Note: "different segment (publishing, not retrieval-as-a-tool); not directly comparable."},
}

type leaderboardOpts struct {
	pullOpts
	out      string // HTML/JSON output path
	fixtures string
	format   string
	limit    int
	budget   int
	internal bool
}

func cmdEvalLeaderboard(args []string) {
	o := defaultOpts()
	lb := leaderboardOpts{pullOpts: o, format: "html", limit: 10, budget: defaultEvalTokenBudget}
	fs := flag.NewFlagSet("eval-leaderboard", flag.ExitOnError)
	fs.StringVar(&lb.out, "leaderboard-out", "", "output path (default docs/human/docs-puller-leaderboard.html for html; stdout for json)")
	fs.StringVar(&lb.fixtures, "fixtures", defaultFixturesDir(), "directory containing *.yaml eval fixtures")
	fs.StringVar(&lb.format, "format", "html", "html | json")
	fs.IntVar(&lb.limit, "limit", 10, "search limit per query")
	fs.IntVar(&lb.budget, "token-budget", defaultEvalTokenBudget, "result-context token budget for budget-hit metrics")
	fs.BoolVar(&lb.internal, "include-internal-fixtures", false, "include operator-internal fixtures in the public leaderboard run")
	bindOpts(fs, &lb.pullOpts)
	fs.Parse(args)

	paths, err := leaderboardFixturePaths(lb.fixtures, lb.internal)
	if err != nil {
		die(err)
	}
	if len(paths) == 0 {
		die(fmt.Errorf("eval-leaderboard: no public eval fixtures found in %s", lb.fixtures))
	}

	// BM25-only (zero searchOpts) → reproducible without an API key. answerContext
	// on so the token-budget columns are populated.
	rows := runEvalSuite(paths, lb.out0(), false, lb.limit, lb.budget, true, searchOpts{})
	report := buildEvalSuiteOverview(rows, lb.pullOpts.out, lb.limit, lb.budget, true, time.Now())

	// Fail closed: a zero-query run (broken corpus / empty fixtures) must NOT
	// overwrite an existing published page with an empty one.
	if err := leaderboardPublishable(report); err != nil {
		die(err)
	}

	switch lb.format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(leaderboardJSON(report)); err != nil {
			die(err)
		}
		return
	case "html":
		outPath := lb.out
		if outPath == "" {
			outPath = filepath.Join(repoRoot(), defaultLeaderboardRel)
		}
		htmlDoc := renderLeaderboardHTML(report, len(paths))
		if err := writeLeaderboardHTMLFile(outPath, htmlDoc); err != nil {
			die(err)
		}
		fmt.Printf("eval-leaderboard: wrote %s (Hit@1=%s Hit@5=%s over %d queries / %d fixtures)\n",
			outPath, evalOverviewPct(report.Overall.HitAt1), evalOverviewPct(report.Overall.HitAt5), report.Overall.N, len(paths))
	default:
		die(fmt.Errorf("eval-leaderboard: unknown --format %q (want html|json)", lb.format))
	}
}

func writeLeaderboardHTMLFile(outPath, htmlDoc string) error {
	if dir := filepath.Dir(outPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(outPath, []byte(htmlDoc), 0o644)
}

// lb.out0 returns the corpus dir for runEvalSuite (the docs root), independent of
// the leaderboard output path.
func (lb leaderboardOpts) out0() string { return lb.pullOpts.out }

func leaderboardFixturePaths(dir string, includeInternal bool) ([]string, error) {
	paths, err := evalSuiteFixturePaths(dir)
	if err != nil {
		return nil, err
	}
	if includeInternal {
		return paths, nil
	}
	filtered := make([]string, 0, len(paths))
	for _, p := range paths {
		if isInternalLeaderboardFixture(filepath.Base(p)) {
			continue
		}
		filtered = append(filtered, p)
	}
	return filtered, nil
}

func isInternalLeaderboardFixture(base string) bool {
	base = strings.ToLower(filepath.Base(base))
	return strings.HasSuffix(base, "-internal.yaml")
}

// leaderboardPublishable is the fail-closed guard: a zero-query report must never
// be published (it would overwrite a good page with an empty one).
func leaderboardPublishable(r evalSuiteOverviewReport) error {
	if r.Overall.N == 0 {
		return fmt.Errorf("eval-leaderboard: eval produced 0 queries — refusing to publish empty leaderboard (fail closed)")
	}
	return nil
}

// leaderboardJSON is the machine-readable payload the HTML renders, so a consumer
// can diff numbers without parsing HTML.
func leaderboardJSON(r evalSuiteOverviewReport) map[string]any {
	fixtures := make([]map[string]any, 0, len(r.Fixtures))
	for _, f := range r.Fixtures {
		if f.Error != "" {
			fixtures = append(fixtures, map[string]any{"fixture": filepath.Base(f.Fixture), "error": f.Error})
			continue
		}
		fixtures = append(fixtures, map[string]any{
			"fixture":  filepath.Base(f.Fixture),
			"n":        f.Summary.N,
			"hit_at_1": f.Summary.HitAt1,
			"hit_at_5": f.Summary.HitAt5,
			"mrr":      f.Summary.MRR,
			"p50_ms":   f.Summary.P50MS,
			"p99_ms":   f.Summary.P99MS,
		})
	}
	return map[string]any{
		"generated_at": r.GeneratedAt,
		"docs_root":    r.DocsRoot,
		"method":       "BM25-only (FTS5), no LLM rerank",
		"overall": map[string]any{
			"n":        r.Overall.N,
			"hit_at_1": r.Overall.HitAt1,
			"hit_at_5": r.Overall.HitAt5,
			"mrr":      r.Overall.MRR,
			"p50_ms":   r.Overall.P50MS,
			"p95_ms":   r.Overall.P95MS,
		},
		"fixtures": fixtures,
	}
}

func renderLeaderboardHTML(r evalSuiteOverviewReport, fixtureCount int) string {
	var b strings.Builder
	esc := html.EscapeString
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>docs-puller — Measured Retrieval Leaderboard</title>`)
	b.WriteString(`<style>
:root{--bg:#0b1020;--card:#141a2e;--ink:#e6ecff;--muted:#93a0c8;--good:#3ddc97;--blue:#5ba8ff;--warn:#ffd166;--line:#243056}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:16px/1.55 ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto}
.wrap{max-width:980px;margin:0 auto;padding:40px 24px 80px}
h1{font-size:30px;margin:0 0 6px}h2{font-size:20px;margin:36px 0 12px;color:var(--blue)}
.sub{color:var(--muted);margin:0 0 24px}
.stats{display:flex;flex-wrap:wrap;gap:14px;margin:22px 0}
.stat{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:16px 20px;min-width:150px}
.stat .v{font-size:30px;font-weight:700}.stat.good .v{color:var(--good)}.stat.blue .v{color:var(--blue)}.stat.warn .v{color:var(--warn)}
.stat .l{color:var(--muted);font-size:13px;text-transform:uppercase;letter-spacing:.04em}
table{width:100%;border-collapse:collapse;background:var(--card);border:1px solid var(--line);border-radius:12px;overflow:hidden;margin:8px 0}
th,td{padding:10px 14px;text-align:left;border-bottom:1px solid var(--line);font-size:14px}
th{color:var(--muted);font-weight:600;text-transform:uppercase;letter-spacing:.03em;font-size:12px}
td.num,th.num{text-align:right;font-variant-numeric:tabular-nums}
tr:last-child td{border-bottom:none}
.yes{color:var(--good);font-weight:600}.no{color:var(--muted)}
.card{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:18px 22px;margin:10px 0}
code,pre{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
pre{background:#0a0f1e;border:1px solid var(--line);border-radius:10px;padding:14px 16px;overflow:auto;font-size:13px;color:#cfe0ff}
.foot{color:var(--muted);font-size:13px;margin-top:34px;border-top:1px solid var(--line);padding-top:16px}
a{color:var(--blue)}
</style></head><body><div class="wrap">`)

	b.WriteString(`<h1>docs-puller — Measured Retrieval Leaderboard</h1>`)
	b.WriteString(`<p class="sub">The docs-retrieval tool that publishes its quality. Numbers below are a real eval run over a frozen fixture — reproducible with no API key. Nobody else in the docs-MCP category leads with a number; this is that number.</p>`)

	// Headline stats.
	b.WriteString(`<div class="stats">`)
	writeLBStat(&b, "good", "Hit@1", evalOverviewPct(r.Overall.HitAt1))
	writeLBStat(&b, "good", "Hit@5", evalOverviewPct(r.Overall.HitAt5))
	writeLBStat(&b, "blue", "MRR", fmt.Sprintf("%.3f", r.Overall.MRR))
	writeLBStat(&b, "blue", "P95 latency", evalOverviewFloat(r.Overall.P95MS)+" ms")
	writeLBStat(&b, "", "Queries", fmt.Sprintf("%d", r.Overall.N))
	b.WriteString(`</div>`)

	// Per-fixture table.
	b.WriteString(`<h2>By fixture</h2><table><thead><tr><th>Fixture</th><th class="num">Queries</th><th class="num">Hit@1</th><th class="num">Hit@5</th><th class="num">MRR</th><th class="num">P50 ms</th><th class="num">P99 ms</th></tr></thead><tbody>`)
	fx := append([]evalSuiteRow(nil), r.Fixtures...)
	sort.Slice(fx, func(i, j int) bool { return filepath.Base(fx[i].Fixture) < filepath.Base(fx[j].Fixture) })
	for _, f := range fx {
		name := esc(strings.TrimSuffix(filepath.Base(f.Fixture), ".yaml"))
		if f.Error != "" {
			fmt.Fprintf(&b, `<tr><td>%s</td><td colspan="6">error: %s</td></tr>`, name, esc(f.Error))
			continue
		}
		s := f.Summary
		fmt.Fprintf(&b, `<tr><td>%s</td><td class="num">%d</td><td class="num">%s</td><td class="num">%s</td><td class="num">%.3f</td><td class="num">%s</td><td class="num">%s</td></tr>`,
			name, s.N, evalOverviewPct(s.HitAt1), evalOverviewPct(s.HitAt5), s.MRR, evalOverviewFloat(s.P50MS), evalOverviewFloat(s.P99MS))
	}
	b.WriteString(`</tbody></table>`)

	// Competitor matrix (honest).
	b.WriteString(`<h2>The category (honest matrix)</h2>`)
	b.WriteString(`<table><thead><tr><th>Tool</th><th>What it is</th><th>Quality published?</th><th>Notes</th></tr></thead><tbody>`)
	for _, c := range leaderboardCompetitors {
		pub := `<span class="no">not yet measured</span>`
		if c.Measured {
			pub = `<span class="yes">measured ✓</span>`
		}
		fmt.Fprintf(&b, `<tr><td><strong>%s</strong></td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			esc(c.Name), esc(c.What), pub, esc(c.Note))
	}
	b.WriteString(`</tbody></table>`)
	b.WriteString(`<p class="sub">We do not invent competitor numbers. Every non-docs-puller cell is "not yet measured" with an open invitation to co-measure on this exact fixture.</p>`)

	// Methodology.
	b.WriteString(`<h2>Methodology</h2><div class="card">`)
	fmt.Fprintf(&b, `<p><strong>Method:</strong> BM25-only (SQLite FTS5, porter stemmer), no LLM rerank — so these numbers reproduce offline. The LLM-rerank pipeline (<code>--rerank-llm</code>) measures higher on Hit@1 (documented separately); we publish the BM25 floor as the honest, key-free baseline.</p>`)
	fmt.Fprintf(&b, `<p><strong>Corpus:</strong> <code>%s</code>. <strong>Fixtures:</strong> %d frozen YAML query sets under <code>eval/</code>. <strong>Search limit:</strong> %d. <strong>Generated:</strong> %s.</p>`,
		esc(sanitizeHomePath(r.DocsRoot)), fixtureCount, r.Limit, esc(r.GeneratedAt))
	fmt.Fprintf(&b, `<p><strong>Metrics:</strong> Hit@k = the correct doc appears in the top-k; MRR = mean reciprocal rank of the first correct hit; latency is wall-clock per query.</p>`)
	b.WriteString(`</div>`)

	// Reproduce this.
	b.WriteString(`<h2>Reproduce this</h2>`)
	b.WriteString(`<pre>git clone https://github.com/nstranquist/docs-puller.git &amp;&amp; cd docs-puller
go build -tags sqlite_fts5 -o bin/docs-puller .
# (point --out at your corpus; the frozen fixtures live in eval/)
./bin/docs-puller eval-leaderboard --format json     # machine-readable numbers
./bin/docs-puller eval-leaderboard                   # regenerate this page</pre>`)

	fmt.Fprintf(&b, `<p class="foot">Generated %s by <code>docs-puller eval-leaderboard</code> · BM25-only · corpus <code>%s</code> · %d queries across %d fixtures. docs-puller is a free, local-first OSS dev-tool; this leaderboard is its marketing engine, not a paywall.</p>`,
		esc(r.GeneratedAt), esc(sanitizeHomePath(r.DocsRoot)), r.Overall.N, fixtureCount)
	b.WriteString(`</div></body></html>`)
	return b.String()
}

// sanitizeHomePath strips the user's home directory prefix to a `~` so the
// PUBLIC leaderboard never leaks an absolute home path.
func sanitizeHomePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if rest := strings.TrimPrefix(p, home+string(filepath.Separator)); rest != p {
			return "~/" + filepath.ToSlash(rest)
		}
	}
	return p
}

func writeLBStat(b *strings.Builder, class, label, value string) {
	cls := "stat"
	if class != "" {
		cls += " " + class
	}
	fmt.Fprintf(b, `<div class="%s"><div class="v">%s</div><div class="l">%s</div></div>`, cls, html.EscapeString(value), html.EscapeString(label))
}
