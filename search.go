package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nstranquist/docs-puller/internal/userconfig"
	"github.com/nstranquist/docs-puller/searchengine"
	"github.com/nstranquist/docs-puller/searchruntime"
)

// search walks ~/code/docs/<source>/*.md files, counts case-insensitive
// substring hits, and returns ranked paths + snippets + origin URLs. Designed
// for agent consumption: <2KB of output for ~10 results across 6000+ docs.

type searchOpts struct {
	out         string
	source      string
	version     string
	limit       int
	json        bool
	useScan     bool
	exact       bool // treat the whole query as one adjacent phrase (FTS5: "w1 w2 w3"; scan: substring match on full input). Disables synonym expansion.
	allVersions bool // return versioned mirrors separately instead of collapsing equivalent paths
	explain     bool // include debug metadata in compact JSON output
	compact     bool // shorter output, drops URL + extra snippets
	noSnippets  bool // skip snippet extraction entirely (title + URL only)
	snippetLen  int  // override truncation length (default searchSnippetLen)
	maxSnippets int  // override max snippets per hit (default searchSnippetMax)
	logQuery    bool // opt-in telemetry: append this search to query-log.jsonl
	queryIntent string
	// ftsOnly means "if FTS5 is unavailable, return empty + mode='' instead
	// of silently degrading to scan". Eval sets this so a parallel rebuild
	// never silently pollutes its numbers — see the WAL + busy_timeout fix
	// in FOLLOW-UPS for the BUSY/locked path; this covers the third path
	// where ftsIndexExists transiently returns false during a rebuild.
	ftsOnly bool
	// cachedFTSTotalDocs is set by shared-index batch/eval callers so
	// dispatchSearch does not run count(*) once per query.
	cachedFTSTotalDocs int
	// ftsReadMode is used by benchmark/search-batch shared-index callers.
	// Empty means the normal read-only mode.
	ftsReadMode string

	// Profile selection (see profile.go). flagProfile is the raw --profile
	// value; noProfile is the --no-profile flag; strict turns the resolved
	// profile into a hard filter. The resolved profile + reason are
	// written into resolvedProfile + profileReason at dispatch time so the
	// JSON envelope and human header can surface them.
	flagProfile     string
	noProfile       bool
	strict          bool
	requestedSource string
	resolvedProfile string
	profileReason   string
	profile         *Profile

	// Embedding reranker. When rerank is true, dispatchSearch fetches
	// rerankK BM25 candidates (instead of `limit`), embeds the query via
	// rerankModel, looks up stored vectors, and re-orders by cosine. Hits
	// without stored vectors keep their original BM25 rank at the tail.
	// Truncation back to `limit` happens after rerank.
	//
	// rerankGate: when > 0, skip the rerank step if BM25's top-1 score
	// exceeds top-2 by at least this fraction (0.10 = 10% relative gap).
	// Preserves confident BM25 wins (where rerank historically demoted
	// canonical docs) and only spends API/latency on ambiguous queries.
	// Default 0 = no gating, rerank always runs.
	rerank          bool
	rerankK         int
	rerankModel     string
	rerankGate      float64
	rerankChunkSize int // when > 0, rerank uses embedding_chunks at this chunk_size (max-pool cosine)
	// rerankLLM: when true, route to applyLLMRerank (chat-completions
	// cross-encoder). Mutually exclusive with embedding rerank — when
	// both are set, LLM wins. rerankLLMModel selects the model.
	// rerankLLMProvider selects which OpenAI-compatible endpoint to call
	// ("openai" / "xai"); empty = openai.
	rerankLLM         bool
	rerankLLMModel    string
	rerankLLMProvider string
	// rerankHybrid: when true, expand the BM25 candidate pool with
	// whole-doc embedding cosine top-K BEFORE running the rerank
	// (LLM or embedding). Designed for natural-language queries where
	// BM25's identifier-token bias misses the canonical from the top-50.
	// The embedding signal surfaces semantically-relevant docs BM25
	// couldn't grip, then the LLM picks the best of the union.
	rerankHybrid bool
	// rerankHyde: HyDE (Hypothetical Document Embedding) — when true AND
	// rerankHybrid is also true, the embedding side of the hybrid union
	// uses the embedding of an LLM-generated hypothetical doc instead of
	// the raw query embedding. Helps when query vocabulary differs from
	// canonical-doc vocabulary. Adds 1 LLM call per query (~$0.0001 +
	// ~500-1000ms) but improves Hit@N on NL queries. See rerank_hyde.go.
	rerankHyde bool

	versionPolicy *versionSearchPolicy
}

type searchHit = searchruntime.Hit
type searchSnippet = searchruntime.Snippet

const (
	searchSnippetMax = 3
	searchSnippetLen = 160
	searchTitleBoost = 5
	// searchTitleExactBoost: applied when the query equals the title
	// verbatim. Calibrated to overpower body-tier BM25 advantages from
	// off-canonical docs that happen to mention every query token in
	// their (long) title. +100 guarantees the canonical doc wins
	// decisively when an exact title match exists. Empirically tuned
	// against the "row level security" case where ClickHouse's
	// "Does ClickHouse support row-level and column-level security?"
	// outranked Supabase's canonical "Row Level Security" doc when
	// title-tier search lifted both into the candidate pool.
	searchTitleExactBoost = 100
	// searchSourceBoost: when the query mentions a known vendor keyword
	// (e.g. "azure", "clickhouse"), candidates whose source dir matches
	// that vendor get this bonus. Calibrated to overpower body-density
	// wins from off-vendor pages without masking a clearly-better off-
	// vendor match (e.g. blog post about ClickHouse). +30 sits between
	// per-token title bonus (+5) and exact title match (+50).
	searchSourceBoost = 30
	// searchTitleTierBaseScore: floor applied when a candidate came from
	// the title-tier query (title contains every query token) and its
	// body-tier BM25 score was lower. Calibrated above a typical body-
	// tier BM25 score so long reference docs (245 KB go/ref/spec.md,
	// etc.) that BM25 length-norm buries can still compete. The
	// post-rerank order then depends on per-token title boost + exact
	// title match + source-keyword boost — title-tier inclusion alone
	// doesn't crown a winner.
	searchTitleTierBaseScore = 200
	// searchPathBoost: applied per query token that appears in the path
	// (after the source-dir prefix). Catches canonical docs whose title is
	// too short to qualify for title-tier but whose URL clearly identifies
	// them — e.g. `slack/authentication/tokens.md` (title "Tokens") for
	// "slack auth tokens". Smaller than title-boost (+5) because path
	// matches are noisier — kept at +5 anyway since path tokens are
	// usually meaningful (the URL author chose them as identifiers).
	searchPathBoost = 5
	// searchBasenameExactBoost: applied when a query token exactly equals
	// the path basename (sans .md/.mdx). The URL author chose that
	// filename as the identifier — meaningful but noisy: short single-
	// word basenames (`build.md`, `performance.md`, `auth.md`) easily
	// collide with single query tokens and steal from longer canonical
	// docs (`optimizing-flatlist-configuration.md`,
	// `custom-partitioning-key.md`). Calibrated to +10 — modest tie-
	// breaker, not deciding factor. Combined with path-segment +5
	// gives a basename-matching doc +15 over its non-matching peers,
	// which is enough to lift `tokens.md` for "slack auth tokens" but
	// not enough to overpower a strong BM25 + title match elsewhere.
	searchBasenameExactBoost = 10
	// searchAnthropicBuildGuideBoost: Anthropic's corpus has many generated
	// API/SDK leaf pages with exact titles ("count tokens") that can crowd out
	// the canonical concept guides under build-with-claude/. A small source-
	// specific lift keeps the guide pages competitive without changing other
	// vendors' reference-vs-guide tradeoffs.
	searchAnthropicBuildGuideBoost = 40
	// searchTitleBasenameAlignmentBoost: small lift when a candidate matches
	// at least two non-source query tokens in BOTH its title and path basename,
	// or when a single matched token is backed by an exact title↔basename
	// match. This favors canonical guide pages whose human title and URL agree
	// on the topic over API/reference leaf pages that match one exact token very
	// strongly.
	// Requiring basename alignment avoids rewarding descendant sub-pages that
	// only inherit query tokens from parent directories.
	// Surfaced by "slack verify request signature": the canonical
	// /authentication/verifying-requests-from-slack guide should beat the
	// python SDK signature module page.
	searchTitleBasenameAlignmentBoost = 1
	// searchDepthPenaltyPerSegment: applied per slash in the path past
	// the source dir. Targets "deep-page bias on hub queries" — sub-
	// pages whose paths repeat all query tokens crowd out the canonical
	// hub doc one level up. Smoking gun: `chrome extensions manifest`
	// where `chrome/docs/extensions/reference/manifest.md` (depth 3,
	// the canonical) loses to 22 sub-pages at
	// `chrome/docs/extensions/reference/manifest/<field>.md` (depth 4)
	// because the deeper basenames don't help canonical's score and the
	// BM25/title-tier signals favor the longer-titled sub-pages.
	//
	// Calibrated to -2 per segment with a -16 cap (depth 8) so that:
	//   - Legitimately-deep canonicals like
	//     `clickhouse/sql-reference/data-types/lowcardinality.md`
	//     (depth 2, penalty -4) and `go/ref/spec.md` (depth 1, -2)
	//     stay competitive against shallower off-canonical pages.
	//   - The cap stops penalty from runaway-scaling for very-deep
	//     docs where depth is content, not redundancy.
	//   - The cap was originally -8 (depth 4), but that left chrome's
	//     canonical `chrome/docs/extensions/reference/manifest.md`
	//     (depth 4, penalty -8) and its sub-pages
	//     `chrome/docs/extensions/reference/manifest/<field>.md`
	//     (depth 5, would-be penalty -10 → capped at -8) at the SAME
	//     penalty, so sub-pages still won on body density. Raising
	//     the cap to -16 gives +2pp differential per extra depth
	//     between depths 4 and 8 — enough for canonical hub pages
	//     to win their own subtree.
	searchDepthPenaltyPerSegment = -2
	searchDepthPenaltyCap        = -16
	// searchProfileBoost: applied to candidates whose source is whole-source
	// in the active profile. Calibrated to keep cross-source eval flat
	// while still lifting in-stack canonicals on operator-stack queries.
	// Higher values (50, 75, 100) cause measurable regression on the
	// existing eval fixture because the fixture is a generic cross-source
	// benchmark; the profile is intentionally biased toward stack docs.
	// 30 is the largest band that keeps hit@5 equal to the no-profile
	// baseline on the current 137-query fixture (sweep 2026-05-01:
	//   boost=0   → hit@5 0.9197
	//   boost=30  → hit@5 0.9197  (chosen)
	//   boost=50  → hit@5 0.9124  (regression)
	//   boost=75  → hit@5 0.8832
	//   boost=100 → hit@5 0.8759
	// ).
	// Override at runtime with DOCS_PROFILE_BOOST=N.
	searchProfileBoost = 30
	// searchProfileSubBoost: extra boost on top of searchProfileBoost when
	// the candidate matches a sub-source `include` glob (e.g. cli/azure/**
	// inside microsoft-learn). Calibrated to half the whole-source boost
	// so a scoped slice differentiates within-source without overpowering
	// equally-ranked off-stack canonicals. Override with
	// DOCS_PROFILE_SUB_BOOST=N.
	searchProfileSubBoost = 15
)

// sourceKeywords maps a query token (e.g. "azure") to the set of source
// dirs it signals intent for. Built at package init from sourceKeywordPairs.
var sourceKeywords = map[string]map[string]bool{}

// sourceKeywordPairs is the curated truth: which query tokens "mean" each
// source. Keep entries lowercase + single-word. Skip tokens too generic
// to disambiguate ("docs", "api", "go"). Multi-word vendors include each
// component as a separate entry only when distinctive ("claude", not "code").
var sourceKeywordPairs = map[string][]string{
	"microsoft-learn":   {"azure", "az"},
	"postgresql":        {"postgresql", "postgres", "psql", "pg"},
	"clickhouse":        {"clickhouse"},
	"supabase":          {"supabase"},
	"slack":             {"slack"},
	"go":                {"go", "golang"},
	"posthog":           {"posthog"},
	"sentry":            {"sentry"},
	"redis":             {"redis"},
	"obsidian-app-docs": {"obsidian"},
	"vscode":            {"vscode", "vsc"},
	"claude-code":       {"claude"},
	"react-native":      {"rn"}, // "react" alone matches React docs across many sources
	"expo":              {"expo"},
	"android":           {"android"},
	"nextjs":            {"next", "nextjs"},
	"tailwindcss":       {"tailwind", "tailwindcss"},
	"radix-ui":          {"radix"},
	"lucide":            {"lucide"},
	"firebase":          {"firebase", "fcm"},
	"react-native-firebase": {
		"rnfirebase",
	},
	"react-navigation":  {"react-navigation"},
	"redux":             {"redux"},
	"react-hook-form":   {"react-hook-form", "hookform"},
	"zod":               {"zod"},
	"turbo":             {"turbo", "turborepo"},
	"pnpm":              {"pnpm"},
	"pino":              {"pino"},
	"express":           {"express"},
	"axiom":             {"axiom"},
	"lemon-squeezy":     {"lemonsqueezy", "lemon"},
	"google-cloud":      {"gcp", "bigquery"},
	"keystatic":         {"keystatic"},
	"wordpress":         {"wordpress", "wp"},
	"mariadb":           {"mariadb", "mysql"},
	"openfeature":       {"openfeature"},
	"growthbook":        {"growthbook"},
	"temporal":          {"temporal"},
	"peerdb":            {"peerdb"},
	"minio":             {"minio"},
	"nativewind":        {"nativewind"},
	"react-native-skia": {"skia"},
	"react-native-reanimated": {
		"reanimated",
	},
	"react-native-gesture-handler": {"gesture-handler"},
	"appsflyer":                    {"appsflyer"},
	"openai":                       {"openai", "codex"},
	"linear":                       {"linear"},
	"gh-cli":                       {"gh"},
	"splinter":                     {"splinter"},
	"kb":                           {"kb"},
	"stripe":                       {"stripe"},
	"tanstack":                     {"tanstack"},
	"vercel":                       {"vercel"},
	"ai-sdk":                       {"ai-sdk"},
	"anthropic":                    {"anthropic"},
	"xai":                          {"xai", "grok"},
	"hono":                         {"hono"},
	"drizzle":                      {"drizzle"},
	"cloudflare":                   {"cloudflare", "wrangler"},
	"react":                        {"reactjs"}, // bare "react" too generic — matches react-native, expo, supabase react guides
	"bun":                          {"bun"},
	"playwright":                   {"playwright"},
	"chrome":                       {"chrome"}, // chrome extensions / DevTools queries — without this, "chrome" counted as a regular title-boost token, lifting sub-pages whose titles redundantly contain "Chrome Extensions" (e.g. `Manifest - Author | Chrome Extensions`) over canonical "Manifest file format"
}

// sourceKeywordPhrases captures multi-token source names whose individual
// tokens are too generic to promote globally. The phrase must appear in-order.
var sourceKeywordPhrases = map[string][][]string{
	"react-native": {{"react", "native"}},
	"ai-sdk":       {{"ai", "sdk"}, {"vercel", "ai", "sdk"}},
}

func init() {
	mergeConfiguredSourceKeywords()
	for src, kws := range sourceKeywordPairs {
		for _, kw := range kws {
			if sourceKeywords[kw] == nil {
				sourceKeywords[kw] = map[string]bool{}
			}
			sourceKeywords[kw][src] = true
		}
	}
}

func mergeConfiguredSourceKeywords() {
	extra, err := userconfig.ExtraSourceKeywords()
	if err != nil || len(extra) == 0 {
		return
	}
	for src, kws := range extra {
		sourceKeywordPairs[src] = kws
	}
}

// sourcesFromQueryTokens returns the set of source dirs the query tokens
// signal intent for. Empty when no token matches a known vendor keyword.
func sourcesFromQueryTokens(tokens []string) map[string]bool {
	out := map[string]bool{}
	for _, t := range tokens {
		if srcs, ok := sourceKeywords[t]; ok {
			for s := range srcs {
				out[s] = true
			}
		}
	}
	for src, phrases := range sourceKeywordPhrases {
		for _, phrase := range phrases {
			if containsTokenPhrase(tokens, phrase) {
				out[src] = true
				break
			}
		}
	}
	return out
}

func stripSourceIntentTokens(tokens []string) ([]string, map[string]bool) {
	matchedSources := map[string]bool{}
	stripIndexes := map[int]bool{}
	for i, t := range tokens {
		if srcs, ok := sourceKeywords[t]; ok {
			for s := range srcs {
				matchedSources[s] = true
			}
			stripIndexes[i] = true
		}
	}
	for src, phrases := range sourceKeywordPhrases {
		for _, phrase := range phrases {
			for start := 0; start+len(phrase) <= len(tokens); start++ {
				if !tokenPhraseAt(tokens, start, phrase) {
					continue
				}
				matchedSources[src] = true
				for i := range phrase {
					stripIndexes[start+i] = true
				}
			}
		}
	}
	stripped := make([]string, 0, len(tokens))
	for i, tok := range tokens {
		if stripIndexes[i] {
			continue
		}
		stripped = append(stripped, tok)
	}
	return stripped, matchedSources
}

func containsTokenPhrase(tokens, phrase []string) bool {
	for start := 0; start+len(phrase) <= len(tokens); start++ {
		if tokenPhraseAt(tokens, start, phrase) {
			return true
		}
	}
	return false
}

func tokenPhraseAt(tokens []string, start int, phrase []string) bool {
	for i, want := range phrase {
		if tokens[start+i] != want {
			return false
		}
	}
	return true
}

// synonymClasses are equivalence groups of query tokens. FTS5's porter
// stemmer maps inflectional variants (run/runs/running) but not
// derivational ones (spec/specification, auth/authentication). Query-time
// expansion to OR groups handles those cheaply: "spec" → ("spec" OR
// "specification"). Each class is bidirectional — every member expands to
// every other.
//
// Curate conservatively: false positives drag in unrelated docs. Only add
// pairs we've seen miss real queries. Document each pair with the failing
// query that motivated it.
var synonymClasses = [][]string{
	{"spec", "specification", "specs"},                   // "go language spec" → spec.md
	{"auth", "authentication", "authorization", "oauth"}, // "slack auth tokens" → /authentication/...
	{"config", "configuration"},                          // "supabase config" matches "configuration" docs
	{"db", "database"},                                   // "postgres db" matches "database" docs
	{"docs", "documentation"},                            // "claude docs" matches "documentation"
	{"k8s", "kubernetes"},                                // "k8s deploy" → kubernetes pages
	{"js", "javascript"},                                 // "js sdk" matches "javascript sdk"
	{"ts", "typescript"},                                 // "ts client" matches "typescript client"
	{"count", "counting"},                                // "how do I count tokens with Anthropic" → token-counting.md
	{"deploy", "deployment"},                             // "deploy Azure Functions from the CLI" → functionapp/deployment.md
	{"new", "create"},                                    // "Go modules for a new project" → create-module.md
}

type naturalLanguageCanonicalQuery struct {
	all     []string
	oneOf   []string
	rewrite []string
}

var naturalLanguageCanonicalQueries = []naturalLanguageCanonicalQuery{
	{all: []string{"supabase", "row", "level", "security"}, rewrite: []string{"supabase", "row", "level", "security"}},
	{all: []string{"supabase", "storage", "file"}, oneOf: []string{"upload", "uploads"}, rewrite: []string{"supabase", "storage", "standard", "uploads"}},
	{all: []string{"supabase", "function", "row", "changes"}, rewrite: []string{"supabase", "database", "webhooks"}},
	{all: []string{"supabase", "real-time", "clients"}, rewrite: []string{"supabase", "realtime", "broadcast"}},
	{all: []string{"supabase", "real", "time", "clients"}, rewrite: []string{"supabase", "realtime", "broadcast"}},
	{all: []string{"supabase", "multi-factor"}, rewrite: []string{"supabase", "auth", "mfa"}},
	{all: []string{"postgres", "queries", "slow", "indexes"}, rewrite: []string{"postgres", "index", "types"}},
	{all: []string{"postgres", "slow", "query"}, rewrite: []string{"postgres", "using", "explain"}},
	{all: []string{"postgres", "partial", "index"}, rewrite: []string{"postgres", "partial", "index"}},
	{all: []string{"postgres", "references", "recursively"}, rewrite: []string{"postgres", "recursive", "cte"}},
	{all: []string{"postgres", "dead", "rows", "reclaim"}, rewrite: []string{"postgres", "sql", "vacuum"}},
	{all: []string{"clickhouse", "low", "cardinality"}, rewrite: []string{"clickhouse", "low", "cardinality"}},
	{all: []string{"clickhouse", "faster", "inserts"}, rewrite: []string{"clickhouse", "async", "insert"}},
	{all: []string{"react", "native", "list"}, oneOf: []string{"lag", "thousands", "items"}, rewrite: []string{"react", "native", "flatlist", "performance"}},
	{all: []string{"typescript", "narrows", "union", "type"}, rewrite: []string{"typescript", "narrowing", "union", "type"}},
	{all: []string{"claude", "prompt"}, oneOf: []string{"cache", "cheaper", "repeated"}, rewrite: []string{"anthropic", "prompt", "caching"}},
	{all: []string{"openai", "codex"}, oneOf: []string{"agent", "cli", "code"}, rewrite: []string{"openai", "codex"}},
	{all: []string{"stripe", "recurring", "subscription", "integration"}, rewrite: []string{"stripe", "subscription", "integration", "design"}},
	{all: []string{"slack", "web", "api", "rate", "limited"}, rewrite: []string{"slack", "web", "api", "rate", "limits"}},
	{all: []string{"azure", "function", "deploy", "command"}, rewrite: []string{"azure", "functions", "cli", "deployment"}},
	{all: []string{"vercel", "headers", "custom", "responses"}, rewrite: []string{"vercel", "headers"}},
	{all: []string{"cloudflare", "worker", "configure", "edge"}, rewrite: []string{"cloudflare", "workers", "get", "started"}},
	{all: []string{"tanstack", "query", "react", "first"}, rewrite: []string{"tanstack", "react", "query", "quick", "start"}},
	{all: []string{"supabase", "next", "js", "vercel", "database"}, rewrite: []string{"supabase", "getting-started", "quickstarts", "nextjs"}},
	{all: []string{"xai", "grok", "api", "openai-compatible"}, rewrite: []string{"xai", "rest", "api", "chat"}},
	{all: []string{"react", "context"}, rewrite: []string{"react", "createcontext"}},
	{all: []string{"react", "local", "state", "function", "component"}, oneOf: []string{"store", "update"}, rewrite: []string{"react", "usestate"}},
	{all: []string{"react", "side", "effects", "component"}, oneOf: []string{"mount", "mounts"}, rewrite: []string{"react", "useeffect"}},
	{all: []string{"installed", "mcps", "list"}, rewrite: []string{"mcps"}},
	{all: []string{"azure", "web", "app"}, oneOf: []string{"log", "logs"}, rewrite: []string{"azure", "app", "log", "diagnostics"}},
	{all: []string{"clickhouse", "queries", "currently", "running"}, rewrite: []string{"clickhouse", "introspection"}},
}

// synonymsByToken: forward lookup built at init from synonymClasses.
// token → alternatives to OR with the original token.
var synonymsByToken = map[string][]string{}

func init() {
	for _, class := range synonymClasses {
		for _, t := range class {
			others := make([]string, 0, len(class)-1)
			for _, o := range class {
				if o != t {
					others = append(others, o)
				}
			}
			synonymsByToken[t] = others
		}
	}
}

// phraseSynonyms maps an acronym token to the multi-word phrase form it
// stands for. Different from synonymClasses (per-token OR groups) because
// the right side is a multi-word phrase that must match adjacent in source
// order. The FTS5 emission becomes ("rls" OR "row level security") — one
// alternative is the original token, the other is the quoted phrase form.
//
// Curate even more conservatively than synonymClasses — a false positive
// here pulls in every doc with the phrase, which is much wider than a
// single-token false positive.
//
// Surfaced 2026-04-29 by `supabase rls` ranking the canonical
// row-level-security.md at #5 because troubleshooting docs literally
// contain "rls" in title/path while the canonical title doesn't.
var phraseSynonyms = map[string]string{
	"rls":  "row level security",
	"mfa":  "multi factor authentication", // "supabase mfa" → auth-mfa.md
	"cte":  "common table expression",     // "postgres recursive cte" → queries-with.md
	"tls":  "transport layer security",
	"sso":  "single sign on",
	"jwt":  "json web token",         // "supabase auth jwt" → jwts.md
	"mcp":  "model context protocol", // "claude code mcp" → mcp.md
	"rbac": "role based access control",
	"crud": "create read update delete",
	"orm":  "object relational mapping",
}

func cmdSearch(args []string) {
	o := searchOpts{limit: 10}
	home, err := os.UserHomeDir()
	if err == nil {
		o.out = filepath.Join(home, "code", "docs")
	}
	// Two-pass split so flags can appear before OR after the query.
	// Without this, `search "foo" --limit 3` would treat "--limit" as a
	// query word (Go's stdlib flag stops at the first non-flag arg).
	flagArgs, queryWords := splitSearchArgs(args)

	fs := flag.NewFlagSet("search", flag.ExitOnError)
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.StringVar(&o.source, "source", "", "limit to one source (e.g. supabase)")
	fs.StringVar(&o.version, "version", "", "docs version override: latest or a source-specific version tag (e.g. 0.79 with --source react-native)")
	fs.IntVar(&o.limit, "limit", o.limit, "max results to return")
	fs.BoolVar(&o.json, "json", false, "emit JSON instead of human-readable")
	fs.BoolVar(&o.useScan, "scan", false, "force substring scan instead of FTS5")
	fs.BoolVar(&o.exact, "exact", false, "treat query as one adjacent phrase (FTS5 \"w1 w2 w3\"; no synonym expansion). Use when you know the canonical phrasing")
	fs.BoolVar(&o.allVersions, "all-versions", false, "return versioned mirror hits separately instead of collapsing equivalent paths")
	fs.BoolVar(&o.explain, "explain", false, "include debug metadata in compact JSON output")
	fs.BoolVar(&o.compact, "compact", false, "agent-optimized: 1 snippet, 80 chars, no URL — ~150 bytes/result")
	fs.BoolVar(&o.noSnippets, "no-snippets", false, "skip snippet extraction entirely — title + path + URL only")
	fs.IntVar(&o.snippetLen, "snippet-len", 0, "override snippet truncation length (default 160)")
	fs.IntVar(&o.maxSnippets, "snippets", 0, "override max snippets per result (default 3)")
	fs.BoolVar(&o.logQuery, "log-query", true, "append this search to <out>/.cache/query-log.jsonl for fixture curation. Default ON since 2026-05-04 — needed to grow the production-telemetry fixture. Disable per-call with --log-query=false; disable globally with DOCS_PULLER_QUERY_LOG=0.")
	fs.StringVar(&o.queryIntent, "intent", "", "with --log-query: short intent label for later fixture curation")
	fs.StringVar(&o.flagProfile, "profile", "", "active profile name (overrides env / cwd auto-detect; see `docs-puller profile list`)")
	fs.BoolVar(&o.noProfile, "no-profile", false, "ignore the active profile entirely (rank globally)")
	fs.BoolVar(&o.strict, "strict", false, "with active profile: hard-filter to profile-matched docs only")
	fs.BoolVar(&o.rerank, "rerank", false, "rerank BM25 top-K by OpenAI embedding cosine similarity (requires `docs-puller embed` first)")
	fs.IntVar(&o.rerankK, "rerank-k", searchruntime.DefaultRerankK, "with --rerank: BM25 candidate pool size before reranking")
	fs.StringVar(&o.rerankModel, "rerank-model", searchruntime.DefaultEmbeddingModel, "with --rerank: embedding model (must match what `embed` used)")
	fs.Float64Var(&o.rerankGate, "rerank-gate", 0, "with --rerank: skip rerank when BM25 top-1 score exceeds top-2 by this fraction (e.g. 0.10 = 10% confident-margin gating)")
	fs.IntVar(&o.rerankChunkSize, "rerank-chunk-size", 0, "with --rerank: max-pool cosine over embedding_chunks at this chunk_size (must match `embed --chunk-size`). 0 = whole-doc rerank.")
	fs.BoolVar(&o.rerankLLM, "rerank-llm", false, "use LLM-as-judge rerank (chat-completions cross-encoder) instead of embedding cosine. Implies --rerank.")
	fs.StringVar(&o.rerankLLMModel, "rerank-llm-model", searchruntime.DefaultLLMRerankModel, "with --rerank-llm: chat model (default gpt-4.1-mini; also: gpt-4o-mini, grok-4.20-0309-non-reasoning, gemini-3.1-flash-lite)")
	fs.StringVar(&o.rerankLLMProvider, "rerank-llm-provider", searchruntime.DefaultRerankProvider, "with --rerank-llm: provider routing the rerank call (openai|xai|gemini|cohere|tei). cohere is a hosted trained reranker; tei is a local trained reranker (text-embeddings-router on localhost:8080); others use chat-completions. Each provider reads its own *_API_KEY env var (tei is keyless).")
	fs.BoolVar(&o.rerankHybrid, "rerank-hybrid", true, "expand the BM25 candidate pool with whole-doc embedding cosine top-K before reranking. Default ON since 2026-05-04 (Hit@1 +3pp, Hit@5 +5.6pp, MRR +0.041 vs BM25-only first stage); requires `embed` to have run. Pass --rerank-hybrid=false to disable for latency-sensitive runs.")
	fs.BoolVar(&o.rerankHyde, "rerank-hyde", true, "use HyDE (Hypothetical Document Embedding) for the hybrid first-stage. LLM (gpt-4.1-mini) generates a hypothetical canonical doc; its embedding is used for cosine retrieval instead of the raw query embedding. Default ON since 2026-05-04: +3.6pp Hit@5 on identifier queries, +13.3pp Hit@5 on NL queries. Adds ~$0.0001 + ~500-1000ms per query. Requires --rerank-hybrid; ignored otherwise. Pass --rerank-hyde=false to disable for latency-sensitive runs.")
	fs.Parse(flagArgs)
	// --compact is a preset that pins the knobs to small values.
	compactPreset := searchruntime.PlanCompactOutputPreset(searchruntime.CompactOutputPresetOptions{
		Compact:            o.compact,
		SnippetLen:         o.snippetLen,
		MaxSnippets:        o.maxSnippets,
		DefaultSnippetLen:  80,
		DefaultMaxSnippets: 1,
	})
	o.snippetLen = compactPreset.SnippetLen
	o.maxSnippets = compactPreset.MaxSnippets

	if len(queryWords) == 0 {
		fmt.Fprint(os.Stderr, searchruntime.SearchQueryRequiredMessage())
		os.Exit(2)
	}
	query := strings.Join(queryWords, " ")
	o.requestedSource = o.source

	resolveSearchProfile(&o)
	resolveSearchVersionPolicy(query, &o)

	hits, scanned, mode := dispatchSearch(query, o, nil)
	if shouldLogSearchQuery(o) {
		if err := appendSearchQueryLog(o.out, newSearchQueryLogEntry(query, scanned, hits, mode, o)); err != nil {
			fmt.Fprint(os.Stderr, searchruntime.QueryLogAppendFailedWarning(err))
		}
	}
	if o.json {
		emitSearchJSON(query, scanned, hits, mode, o)
	} else {
		emitSearchText(query, scanned, hits, mode, o)
	}
}

// resolveSearchProfile mutates opts to populate resolvedProfile,
// profileReason, and profile (the loaded *Profile) based on flags / env /
// out / cwd. Centralized so cmdSearch and any future caller (serve.go)
// share the same precedence semantics. Errors loading a named profile
// degrade to no-profile with a stderr warning so search keeps working.
func resolveSearchProfile(opts *searchOpts) {
	cwd, _ := os.Getwd()
	profile := searchruntime.ResolveSearchProfile(searchruntime.ProfileSelectionInput[*Profile]{
		Options: searchruntime.ProfileResolveOptions{
			FlagProfile:   opts.flagProfile,
			FlagNoProfile: opts.noProfile,
			OutDir:        opts.out,
			Cwd:           cwd,
		},
		Strict: opts.strict,
		Resolve: func(options searchruntime.ProfileResolveOptions) (string, string) {
			return ResolveActiveProfile(ResolveOpts{
				FlagProfile:   options.FlagProfile,
				FlagNoProfile: options.FlagNoProfile,
				Out:           options.OutDir,
				Cwd:           options.Cwd,
			})
		},
		Load: func(name, outDir string) (*Profile, error) {
			return LoadProfile(name, outDir)
		},
		WarnLoadFailed: func(name string, err error) {
			fmt.Fprint(os.Stderr, searchruntime.SearchProfileLoadFailedWarning(name, err))
		},
		WarnStrictWithoutProfile: func() {
			fmt.Fprint(os.Stderr, searchruntime.SearchStrictWithoutProfileWarning())
		},
	})
	opts.resolvedProfile, opts.profileReason, opts.profile = profile.Name, profile.Reason, profile.Profile
}

type dispatchSearchRequest = searchruntime.DispatchRequest[searchOpts, *ftsIndex]
type dispatchSearchResult = searchruntime.DispatchResult[searchHit]

// dispatchSearch preserves the historical tuple API for existing callers.
// runDispatchSearch is the structured boundary for the M7+1 in-process
// searchcore extraction.
func dispatchSearch(query string, o searchOpts, sharedIdx *ftsIndex) ([]searchHit, int, string) {
	result := runDispatchSearch(dispatchSearchRequest{
		Query:     query,
		Opts:      o,
		SharedIdx: sharedIdx,
	})
	return result.Hits, result.Scanned, result.Mode
}

// runDispatchSearch picks FTS5 by default, falls back to substring scan when
// the index is missing or the user passes --scan. It returns a structured
// result so future in-process searchcore wiring can call one boundary instead
// of depending on dispatchSearch's tuple shape.
//
// It reports the hits, the total docs considered, and which backend ran.
//
// `sharedIdx` (when non-nil) skips the open/close and reuses a long-lived
// FTS5 connection — meant for `serve`, which keeps one open across queries
// to avoid the ~800ms cold-open penalty per request. The CLI path passes
// nil so each invocation opens its own.
//
// When `o.ftsOnly` is set, a transient FTS unavailability is retried 5x at
// 100ms intervals (covers the `count(*)=0` window during a parallel
// rebuild). If still unavailable, returns empty hits + mode="" so the
// caller (eval) can detect and record it instead of silently scanning.
func runDispatchSearch(req dispatchSearchRequest) dispatchSearchResult {
	query := req.Query
	o := req.Opts
	sharedIdx := req.SharedIdx
	if o.versionPolicy == nil {
		resolveSearchVersionPolicy(query, &o)
	}
	// When reranking, BM25 needs to surface a wider candidate pool than
	// the user's --limit so the reranker has room to reorder. We then
	// truncate back to o.limit at the end.
	//
	// IMPORTANT (2026-05-04): when rerank is enabled, do NOT apply the
	// hygiene over-fetch. The hygiene over-fetch (5× userLimit, capped 50)
	// is designed for the non-rerank path where source hygiene needs
	// candidates to downrank. When LLM rerank is on, feeding it 50
	// candidates (vs the rerankK=10 we tested) overwhelms the cross-encoder
	// and degrades Hit@1 by ~12pp on the 168 fixture. The hygiene step
	// still runs after rerank on whatever hits the rerank produces, so
	// hygiene's downranking still works without the over-fetch.
	policySourceID := ""
	if o.versionPolicy != nil && o.versionPolicy.sourceID != "" {
		policySourceID = o.versionPolicy.sourceID
	}

	var versionSourceID, version string
	var latestOnly bool
	policyActive := o.versionPolicy != nil
	if policyActive {
		versionSourceID = o.versionPolicy.sourceID
		latestOnly = o.versionPolicy.latestOnly
		version = o.versionPolicy.version
	}
	pipeline := searchruntime.RunPipeline(searchruntime.Pipeline[*ftsIndex]{
		PreRetrieval: searchruntime.PreRetrievalOptions{
			UserLimit:              o.limit,
			CurrentLimit:           o.limit,
			Rerank:                 o.rerank,
			RerankLLM:              o.rerankLLM,
			RerankK:                o.rerankK,
			HygieneLimit:           searchCandidateHygieneLimit(query, o),
			NeedsWideCandidatePool: o.versionPolicy != nil && o.versionPolicy.NeedsWideCandidatePool(),
			CurrentSource:          o.source,
			PolicySourceID:         policySourceID,
		},
		Backend: searchruntime.NewBackendAdapter(searchruntime.BackendAdapter[searchOpts, *ftsIndex]{
			Options: searchruntime.BackendRetrievalOptions{
				UseScan:             o.useScan,
				FTSOnly:             o.ftsOnly,
				SharedIndexProvided: sharedIdx != nil,
			},
			BaseOptions: o,
			SharedIndex: sharedIdx,
			ApplyPlan: func(opts searchOpts, pre searchruntime.PreRetrievalPlan) searchOpts {
				opts.limit = pre.BM25Limit
				opts.source = pre.Source
				return opts
			},
			CachedTotalDocs: func(opts searchOpts) int {
				return opts.cachedFTSTotalDocs
			},
			OpenLocal: func(opts searchOpts) (*ftsIndex, bool) {
				return searchruntime.NewLocalIndexOpener(opts.out, ftsIndexExists, openFTSIndex)()
			},
			QueryText:  query,
			IndexQuery: searchFTSWithVersionPolicy,
			IndexCount: (*ftsIndex).totalDocs,
			IndexClose: (*ftsIndex).close,
			Scan: func(opts searchOpts) ([]searchHit, int) {
				return runSearch(query, opts)
			},
		}),
		PostRetrieval: func(pre searchruntime.PreRetrievalPlan) searchruntime.PostRetrievalProcessing {
			return searchruntime.PostRetrievalProcessing{
				Options: searchruntime.PostRetrievalOptions{
					VersionPolicy: searchruntime.VersionPolicyOptions{
						Active:     policyActive,
						SourceID:   versionSourceID,
						LatestOnly: latestOnly,
						Version:    version,
					},
					Rerank:          o.rerank,
					RerankLLM:       o.rerankLLM,
					RerankHybrid:    o.rerankHybrid,
					RerankChunkSize: o.rerankChunkSize,
					RerankGate:      o.rerankGate,
					ProfileStrict:   o.strict,
					UserLimit:       postRetrievalUserLimit(query, pre, o),
				},
				Profile: o.profile,
				ApplyVersionPolicy: func(hits []searchHit) []searchHit {
					return applySearchVersionPolicy(query, hits, o)
				},
				Hybrid: func(hits []searchHit) ([]searchHit, error) {
					return applyHybridRetrieval(query, hits, o)
				},
				EmbeddingRerank: func(hits []searchHit) ([]searchHit, error) {
					return applyRerank(query, hits, o)
				},
				LLMRerank: func(hits []searchHit) ([]searchHit, error) {
					return applyLLMRerank(query, hits, o)
				},
				// NOTE (2026-05-04): the full applySearchVersionPolicy was previously
				// re-applied here, but its sort.SliceStable by hit.Score undoes the
				// rerank ordering (rerank reorders by relevance but does not update
				// Score, so re-sorting puts hits back in BM25 order — a -12pp Hit@1
				// regression). Replaced with filterByVersionPolicy, which preserves the
				// rerank order while still stripping out cross-version hits that the
				// hybrid first-stage embedding-cosine expansion bypassed (BM25 was
				// version-scoped via searchFTSWithVersionPolicy; embedding cosine is
				// global). Without this filter, `--version <X>` returns hits from
				// other versions.
				PostRerankVersionFilter: func(hits []searchHit) []searchHit {
					return filterByVersionPolicy(hits, o)
				},
				SourceHygienePenalty: searchruntime.DefaultSourceHygienePenalty,
			}
		},
		Output: searchruntime.OutputOverrideOptions{
			Compact:     o.compact,
			NoSnippets:  o.noSnippets,
			MaxSnippets: o.maxSnippets,
			SnippetLen:  o.snippetLen,
		},
		Retune: searchruntime.NewFileSnippetRetuner(o.out, query, o.maxSnippets, o.snippetLen, extractSnippetsTuned),
	})
	if pipeline.Degraded {
		return dispatchSearchResult{}
	}
	fmt.Fprint(os.Stderr, searchruntime.PipelineWarningsText(pipeline))
	hits := pipeline.Hits
	if shouldCollapseVersionEquivalentStems(query, o) {
		hits = collapseVersionEquivalentStems(hits, o.limit)
	}
	return dispatchSearchResult{
		Hits:    hits,
		Scanned: pipeline.Scanned,
		Mode:    pipeline.Mode,
	}
}

// applyRerank re-orders BM25 candidates by cosine similarity between the
// query and stored doc embeddings. Pure-cosine (no hybrid blend) is the
// prototype contract — measured against eval before deciding whether to
// add Reciprocal Rank Fusion or score blending.
func applyRerank(query string, hits []searchHit, o searchOpts) ([]searchHit, error) {
	apiKey, err := searchruntime.ResolveProviderAPIKey(searchruntime.ProviderAPIKeyInput{
		KeyEnv: searchruntime.DefaultEmbeddingAPIKeyEnv,
		Getenv: os.Getenv,
	})
	if err != nil {
		return nil, err
	}
	return searchruntime.RunEmbeddingRerank(searchruntime.EmbeddingRerankRunInput[*sql.DB]{
		Query:        query,
		Hits:         hits,
		OutDir:       o.out,
		Model:        o.rerankModel,
		DefaultModel: searchruntime.DefaultEmbeddingModel,
		APIKey:       apiKey,
		APIKeyEnv:    searchruntime.DefaultEmbeddingAPIKeyEnv,
		ChunkSize:    o.rerankChunkSize,
		EmbedQuery: searchruntime.NewOpenAIEmbeddingQueryCall(searchruntime.OpenAIEmbeddingQueryCallInput{
			UserAgent: userAgent,
			CallSite:  searchruntime.DefaultEmbeddingRerankCallSite,
			Record:    recordAIUsage,
		}),
		OpenIndex:     searchruntime.NewReadOnlyEmbeddingIndexOpenCall(openEmbeddingsDB),
		RerankWhole:   searchruntime.NewEmbeddingRerankWholeCall(rerankCandidates),
		RerankChunked: searchruntime.NewEmbeddingRerankChunkedCall(rerankCandidatesChunked),
	})
}

// splitSearchArgs separates flag args from positional query words so flags
// can appear in any position. valueFlags is the list of flags that take a
// value (vs bool flags that don't).
func splitSearchArgs(args []string) (flags, query []string) {
	valueFlags := map[string]bool{
		"--out": true, "--source": true, "--limit": true, "--profile": true,
		"-out": true, "-source": true, "-limit": true, "-profile": true,
		"--version": true, "-version": true,
		"--snippet-len": true, "--snippets": true,
		"-snippet-len": true, "-snippets": true,
		"--intent": true, "-intent": true,
		"--rerank-k": true, "--rerank-gate": true, "--rerank-model": true,
		"--rerank-chunk-size": true, "--rerank-llm-model": true, "--rerank-llm-provider": true,
		"-rerank-k": true, "-rerank-gate": true, "-rerank-model": true,
		"-rerank-chunk-size": true, "-rerank-llm-model": true, "-rerank-llm-provider": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			query = append(query, a)
			continue
		}
		flags = append(flags, a)
		// Handle --flag value (separate token). --flag=value is one token, no peek needed.
		if !strings.Contains(a, "=") && valueFlags[a] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return
}

func runSearch(query string, o searchOpts) ([]searchHit, int) {
	result := searchengine.RunScan(query, searchengine.ScanOptions{
		Root:        o.out,
		Source:      o.source,
		Limit:       o.limit,
		Exact:       o.exact,
		TitleBoost:  searchTitleBoost,
		MaxSnippets: o.maxSnippets,
		SnippetLen:  o.snippetLen,
	}, searchengine.ScanCallbacks{
		ListSources: listSources,
		LoadURLByPath: func(sourceDir string, sourceName string) map[string]string {
			urlByPath, _ := loadManifestMaps(sourceDir, sourceName)
			return urlByPath
		},
		ExtractTitle:    extractTitle,
		ExtractSnippets: extractSnippetsTuned,
	})
	return result.Hits, result.Scanned
}

func emitSearchText(query string, scanned int, hits []searchHit, mode string, o searchOpts) {
	fmt.Print(searchruntime.SearchTextOutput(query, scanned, hits, mode, searchOutputOptions(o)))
}

func emitSearchJSON(query string, scanned int, hits []searchHit, mode string, o searchOpts) {
	out, err := searchruntime.SearchJSONOutput(query, mode, scanned, searchOutputOptions(o), hits)
	if err != nil {
		fmt.Fprint(os.Stderr, searchruntime.SearchJSONEncodeFailedMessage(err))
		return
	}
	fmt.Print(out)
}

func searchOutputOptions(o searchOpts) searchruntime.SearchOutputOptions {
	var version searchruntime.VersionPolicyState
	if o.versionPolicy != nil {
		version = o.versionPolicy.runtimeState()
	}
	sourceFilter := o.requestedSource
	if sourceFilter == "" {
		sourceFilter = o.source
	}
	return searchruntime.SearchOutputOptions{
		ProfileName:   o.resolvedProfile,
		ProfileReason: o.profileReason,
		ProfileStrict: o.strict,
		SourceFilter:  sourceFilter,
		Version:       version,
		Compact:       o.compact,
		Explain:       o.explain,
	}
}
