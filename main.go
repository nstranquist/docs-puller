// docs-puller pulls vendor docs into ~/code/docs/<source>/ as plain markdown
// plus a per-source manifest.json (URL → entry map). After each pull it
// regenerates ~/code/docs/_INDEX.md (source summaries) and
// <source>/_INDEX.md (every doc grouped by directory) so agents have a
// single at-a-glance entry point. Underscore prefix avoids case-insensitive
// collision with fetched index.md.
//
// Resolution order per URL:
//  1. Source mode — supabase.com/docs/guides/* -> sparse checkout MDX at
//     ~/code/docs/.cache/supabase-src/apps/docs/content/<path>.mdx
//  2. Supabase YAML spec — /docs/reference/cli/<name> renders from
//     cli_v1_commands.yaml; /docs/guides/local-development/cli/config from
//     cli_v1_config.yaml.
//  3. github.com/<owner>/<repo> -> raw README.md from raw.githubusercontent.com.
//  4. HTTP fallback — GET + per-host content selector (extract.go) + html-to-markdown.
//
// Subcommands:
//
//	docs-puller init                              # sparse-clone supabase upstream cache
//	docs-puller pull --from <file-or-url>         # extract URLs from text file (or fetch URL first)
//	docs-puller pull --sitemap <url>              # pull every URL in a sitemap.xml
//	docs-puller pull --llms-txt <url>             # pull every document named by llms.txt
//	docs-puller pull --gatsby-pagedata <url>      # discover URLs via Gatsby page-data.json (allMdx.nodes[].slug)
//	docs-puller pull --local <path>               # walk a local dir for .md/.mdx
//	docs-puller pull --github-repo <owner/repo>   # walk a GitHub repo for .md/.mdx
//	docs-puller pull-url <url>                    # one-off
//	docs-puller crawl-refs                        # ingest ref-dissection cases into refs-dissections
//	docs-puller refresh                           # git pull the source cache
//	docs-puller status                            # health summary for corpus + FTS5
//
// HTTP fetches run in parallel (--concurrency, default 8) with 3-attempt
// retry on 5xx/429 (exponential backoff, honors Retry-After). Converted
// pages under 200 bytes trigger a low-content warning.
//
// Search is backed by SQLite FTS5 (porter stemmer + BM25 ranking) at
// <out>/.cache/search.db. The index is rebuilt automatically after every
// pull; manual rebuild via `docs-puller reindex`. `--scan` on `search`
// forces the substring scan path (no stemming, slower, but no index
// dependency and exact byte matching for queries with punctuation).
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nstranquist/docs-puller/searchruntime"

	htmltomd "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/PuerkitoBio/goquery"
)

const (
	supabaseHost = "supabase.com"
	userAgent    = "docs-puller/0.3 (+https://github.com/nstranquist/docs-puller)"
)

var urlRE = regexp.MustCompile(`https?://[^\s)>"']+`)

// httpClient timeout has to cover slow vendor sitemaps (Chrome's 3 sub-sitemap
// fetches were timing out at 30s under load). 60s is generous enough for
// real-world tail latencies without making genuinely-broken hosts hang forever.
var httpClient = &http.Client{Timeout: 60 * time.Second}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "pull":
		cmdPull(os.Args[2:])
	case "pull-local-batch":
		cmdPullLocalBatch(os.Args[2:])
	case "pull-url":
		cmdPullURL(os.Args[2:])
	case "pull-article":
		cmdPullArticle(os.Args[2:])
	case "crawl-refs":
		cmdCrawlRefs(os.Args[2:])
	case "curation":
		cmdCuration(os.Args[2:])
	case "refresh":
		cmdRefresh(os.Args[2:])
	case "init":
		cmdInit(os.Args[2:])
	case "config":
		cmdConfig(os.Args[2:])
	case "search":
		cmdSearch(os.Args[2:])
	case "search-batch":
		cmdSearchBatch(os.Args[2:])
	case "reindex":
		cmdReindex(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "list":
		cmdList(os.Args[2:])
	case "show":
		cmdShow(os.Args[2:])
	case "log":
		cmdLog(os.Args[2:])
	case "pins":
		cmdPins(os.Args[2:])
	case "pull-pins":
		cmdPullPins(os.Args[2:])
	case "eval":
		cmdEval(os.Args[2:])
	case "eval-diagnose":
		cmdEvalDiagnose(os.Args[2:])
	case "eval-sweep":
		cmdEvalSweep(os.Args[2:])
	case "eval-suite":
		cmdEvalSuite(os.Args[2:])
	case "eval-leaderboard":
		cmdEvalLeaderboard(os.Args[2:])
	case "extract":
		cmdExtract(os.Args[2:])
	case "embed":
		cmdEmbed(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "profile":
		cmdProfile(os.Args[2:])
	case "telemetry":
		cmdTelemetry(os.Args[2:])
	case "emit-llmstxt":
		cmdEmitLLMsTxt(os.Args[2:])
	case "version":
		cmdVersion(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

const topLevelUsage = `docs-puller — pull vendor docs into ~/code/docs/<source>/

Usage:
  docs-puller version [--json] [--expect VERSION] # build identity + CLI contract
  docs-puller init [--source-cache DIR]
  docs-puller config init [--profile NAME] [--force]
  docs-puller config path [--json]
  docs-puller pull --from <file-or-url>      [common-flags]
  docs-puller pull --sitemap <url>           [--filter PREFIX] [--max N] [common-flags]
  docs-puller pull --llms-txt <url>          [--filter PREFIX] [--max N] [common-flags]
  docs-puller pull --gatsby-pagedata <url>   [--filter PREFIX] [--max N] [common-flags]
  docs-puller pull --docc <url>              [--filter PREFIX] [--max N] [--name NAME]
                                             [--follow-see-also] [--follow-relationships] [common-flags]
  docs-puller pull --local <path>            [--name NAME] [--subdir SUBDIR]
  docs-puller pull-local-batch --source NAME=PATH [--source NAME=PATH ...]
                                             [--out DIR] [--json]
  docs-puller pull --github-repo <o/r>       [--ref REF] [--name NAME] [--subdir SUBDIR]
  docs-puller pull --git-repo <url>           [--ref REF] [--name NAME] [--subdir SUBDIR]
                                             [--origin-base URL]
  docs-puller pull-url <url>                 [common-flags]
  docs-puller pull-article <url>             [--name SLUG] [common-flags]
  docs-puller crawl-refs                     [--cases-root DIR] [--source NAME] [--out DIR]
                                             # ingest ~/code/refs/_cases/<slug>/*.md → refs-dissections
  docs-puller curation lint                  [--json]
  docs-puller refresh [--source-cache DIR]
  docs-puller reindex [--out DIR]            # rebuild FTS5 search index
  docs-puller status [--out DIR] [--check] [--check-embeddings]
                                             # health summary for corpus + FTS5
  docs-puller list                           [--out DIR] [--json]
  docs-puller show <source>                  [--out DIR] [--json]
  docs-puller emit-llmstxt <source>          [--out DIR] [--file PATH]
  docs-puller emit-llmstxt --all             [--out DIR]   # generate llms.txt from the local corpus
  docs-puller log                            [--out DIR] [--limit N] [--json]
  docs-puller pins {show,refresh,sync,gc,lint}
                                             [--out DIR] [--json]
  docs-puller pull-pins                      [--out DIR] [--source FAMILY] [--write] [--json]
  docs-puller search <query>                 [--source NAME] [--profile NAME] [--strict] [--no-profile]
                                             [--version latest|TAG]
                                             [--limit N] [--json] [--scan] [--compact] [--exact]
                                             [--log-query] [--intent LABEL]
                                             [--client ID] [--run-context CONTEXT]
                                             [--rerank] [--rerank-gate N] [--rerank-chunk-size N]
                                             [--rerank-llm] [--rerank-llm-model NAME] [--rerank-k N]
                                             [--rerank-hybrid]
  docs-puller search-batch --queries JSON    [--out DIR] [--limit N] [--source NAME]
                                             [--no-profile] [--no-snippets]
  docs-puller profile {list,show,lint}       [--profile NAME] [--out DIR] [--json]
  docs-puller eval                           [--fixture PATH] [--json] [--scan] [--verbose]
                                             [--write JSONL] [--diff JSON] [--check-fixture]
                                             [--token-budget N] [--record-run] [--run-root DIR]
                                             [--answer-context]
  docs-puller eval-suite                     [--fixtures DIR] [--include-fixture A.yaml,B.yaml]
                                             [--exclude-fixture C.yaml] [--json] [--diff JSON] [--scan] [--limit N]
                                             [--token-budget N] [--answer-context]
                                             [--overview-md PATH] [--overview-html PATH]
  docs-puller eval-leaderboard               [--leaderboard-out PATH] [--format html|json]
                                             [--fixtures DIR] [--limit N] [--include-internal-fixtures]
                                             # public, reproducible by default
  docs-puller eval-sweep                     [--fixture PATH] [--baseline PATH]
                                             [--token-budget N] [--answer-context] -- <command...>
  docs-puller eval-diagnose                  --baseline PATH --current PATH [--source NAME]
                                             [--min-delta N] [--json] [--max-items N]
  docs-puller telemetry {log,summary,fixture} [--out DIR]
  docs-puller extract sql-schema --local <path> --name SOURCE
                                             [--subdir migrations] [--exclude PAT]
  docs-puller embed                          [--source NAME] [--model MODEL] [--chunk-size N]
                                             [--reembed] [--dry-run] [--max-cost USD]
                                             [--migrate-legacy] [--write-flat-only]
  docs-puller serve [--port 7799]            # web UI at http://127.0.0.1:7799/

Common flags: --out DIR  --source-cache DIR  --concurrency N (default 8)
              --source-only (skip HTTP fallback)
              --replace-source [--allow-large-prune]

Recommended rerank invocations (gpt-4.1-mini cross-encoder; eval discipline
in eval/CONTRIBUTING.md):

  Default (hybrid first stage + LLM rerank, 1.2s p50, Hit@1 79%):
    docs-puller search <q> --rerank-llm

  Latency-sensitive (BM25-only first stage, 660ms p50, Hit@1 76%):
    docs-puller search <q> --rerank-llm --rerank-hybrid=false --rerank-k 5

The default since 2026-05-04 is hybrid mode: BM25 candidates UNION embedding
cosine top-K before reranking. Lifts Hit@1 +3pp, Hit@5 +5.6pp, MRR +0.041
on the identifier fixture. Requires the embed subcommand to have run first.
Embedding rerank without --rerank-llm regresses Hit@1; not recommended.

Resolution: source repo → YAML spec → github raw README → HTTP + html-to-markdown.
--local / --github-repo skip HTTP entirely and walk an in-tree dir or a sparse
GitHub clone for Markdown-family and reStructuredText docs. --git-repo adds
generic Git hosts (for example projects.blender.org); --origin-base maps local
source paths back to their canonical published documentation URLs.

HTTP fetches retry on 5xx/429 (3 attempts, 500ms exponential backoff, honors
Retry-After). Pages with <200 bytes of converted content emit a low-content
warning (likely client-rendered). Run 'init' once before the first pull.
`

func usage() {
	fmt.Fprint(os.Stderr, topLevelUsage)
}

type pullOpts struct {
	out             string
	sourceCache     string
	sourceOnly      bool
	concurrency     int
	replaceSource   bool // prune manifest entries (and files) not in this run per source
	allowLargePrune bool // explicit acknowledgement for destructive replacement plans
}

func defaultOpts() pullOpts {
	out := os.Getenv("DOCS_PULLER_OUT")
	if out == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// On a system without a resolvable HOME we'd produce paths like
			// "/code/docs" which is a footgun. Fail loudly so the user supplies
			// DOCS_PULLER_OUT or --out / --source-cache explicitly.
			fmt.Fprintf(os.Stderr, "docs-puller: cannot resolve home dir: %v — set DOCS_PULLER_OUT or pass --out and --source-cache explicitly\n", err)
			os.Exit(2)
		}
		out = filepath.Join(home, "code", "docs")
	}
	return pullOpts{
		out:         out,
		sourceCache: filepath.Join(out, ".cache"),
		concurrency: 8,
	}
}

func bindOpts(fs *flag.FlagSet, o *pullOpts) {
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.StringVar(&o.sourceCache, "source-cache", o.sourceCache, "source-repo cache dir")
	fs.BoolVar(&o.sourceOnly, "source-only", false, "do not fall back to HTTP fetch")
	fs.BoolVar(&o.replaceSource, "replace-source", false, "replace each touched source's manifest (prune URLs/files not in this pull)")
	fs.IntVar(&o.concurrency, "concurrency", o.concurrency, "parallel HTTP fetches")
}

func cmdPull(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	from := fs.String("from", "", "input file or URL (urls or markdown notes; URLs fetch first — useful for vendor llms-sitemap.md)")
	sitemap := fs.String("sitemap", "", "sitemap.xml URL — pull every URL it lists")
	llmsTxt := fs.String("llms-txt", "", "llms.txt URL — pull linked docs or native markdown pages named by ===/path=== sections")
	gatsbyPageData := fs.String("gatsby-pagedata", "", "Gatsby page-data.json URL — discover URLs via allMdx.nodes[].slug")
	doccURL := fs.String("docc", "", "DocC archive root URL — BFS-walk an Apple-style JSON-driven doc archive (developer.apple.com / Swift package docs)")
	filter := fs.String("filter", "", "with --sitemap/--llms-txt/--gatsby-pagedata/--from/--docc: keep only URLs starting with this prefix")
	maxN := fs.Int("max", 0, "with --sitemap/--llms-txt/--gatsby-pagedata/--from/--docc: cap the URL count (0 = no cap)")
	local := fs.String("local", "", "ingest .md/.mdx/.mdoc/.rst from a local directory")
	githubRepo := fs.String("github-repo", "", "ingest docs from a GitHub repo (owner/repo)")
	gitRepo := fs.String("git-repo", "", "ingest docs from a generic Git repository URL")
	name := fs.String("name", "", "with --local/--github-repo/--git-repo/--docc: override source dir name")
	subdir := fs.String("subdir", "", "with --local/--github-repo/--git-repo: walk only this subpath")
	ref := fs.String("ref", "", "with --github-repo/--git-repo: git ref/branch (default: upstream HEAD)")
	originBase := fs.String("origin-base", "", "with --local/--github-repo/--git-repo: canonical published URL prefix for ingested paths")
	followSeeAlso := fs.Bool("follow-see-also", false, "with --docc: include See Also identifiers in BFS frontier (default: render-only)")
	followRel := fs.Bool("follow-relationships", false, "with --docc: include Relationships identifiers in BFS frontier (default: render-only)")
	allowLargePrune := fs.Bool("allow-large-prune", false, "with --replace-source: allow a guarded large source deletion after reviewing discovery input")
	var excludes stringSliceFlag
	fs.Var(&excludes, "exclude", "with --local/--github-repo/--git-repo: skip paths matching glob (repeatable; e.g. 'internal-notes/**', 'attachments/**', '*.tmp.md')")
	bindOpts(fs, &o)
	fs.Parse(args)
	o.allowLargePrune = *allowLargePrune
	if err := validateReplacementFlags(o.replaceSource, o.allowLargePrune, *filter, *maxN); err != nil {
		die(err)
	}

	modes := 0
	for _, s := range []string{*from, *sitemap, *llmsTxt, *gatsbyPageData, *doccURL, *local, *githubRepo, *gitRepo} {
		if s != "" {
			modes++
		}
	}
	if modes != 1 {
		fmt.Fprintln(os.Stderr,
			"pull: pass exactly one of --from <file>, --sitemap <url>, --llms-txt <url>, --gatsby-pagedata <url>, --docc <url>, --local <path>, --github-repo <owner/repo>, or --git-repo <url>")
		os.Exit(2)
	}

	if *doccURL != "" {
		runDocC(*doccURL, *filter, *name, *maxN, *followSeeAlso, *followRel, o, os.Args[1:])
		return
	}

	if *local != "" || *githubRepo != "" || *gitRepo != "" {
		lo := localPullOpts{
			pullOpts:   o,
			name:       *name,
			subdir:     *subdir,
			ref:        *ref,
			originBase: *originBase,
			excludes:   excludes,
		}
		if *local != "" {
			cmdPullLocal(*local, lo, os.Args[1:])
		} else if *githubRepo != "" {
			cmdPullGithubRepo(*githubRepo, lo, os.Args[1:])
		} else {
			cmdPullGitRepo(*gitRepo, lo, os.Args[1:])
		}
		return
	}

	var (
		urls []string
		mode string
		err  error
	)
	switch {
	case *from != "":
		mode = "from-file"
		urls, err = extractURLsFromFile(*from)
		if err != nil {
			die(err)
		}
		// Filter applies here too — useful when --from points at an HTML
		// index page that links to off-site URLs we don't want (twitter,
		// stackoverflow, vendor's pkg.<host>, etc.). Always pass through
		// filterURLs so hash-heavy llms.txt files dedupe to one fetch per
		// canonical path even when no explicit filter/max is needed.
		urls = filterURLs(urls, *filter, *maxN)
	case *sitemap != "":
		mode = "sitemap"
		fmt.Fprintf(os.Stderr, "loading sitemap %s ...\n", *sitemap)
		urls, err = loadSitemap(*sitemap)
		if err != nil {
			die(err)
		}
		urls = filterURLs(urls, *filter, *maxN)
		fmt.Fprintf(os.Stderr, "sitemap → %d URLs (filter=%q max=%d)\n", len(urls), *filter, *maxN)
	case *llmsTxt != "":
		mode = "llms-txt"
		fmt.Fprintf(os.Stderr, "loading llms.txt %s ...\n", *llmsTxt)
		urls, err = loadLLMsTxt(*llmsTxt)
		if err != nil {
			die(err)
		}
		urls = filterURLs(urls, *filter, *maxN)
		fmt.Fprintf(os.Stderr, "llms.txt → %d URLs (filter=%q max=%d)\n", len(urls), *filter, *maxN)
	case *gatsbyPageData != "":
		mode = "gatsby-pagedata"
		fmt.Fprintf(os.Stderr, "loading gatsby page-data %s ...\n", *gatsbyPageData)
		var slugs []string
		slugs, err = loadGatsbyPageData(*gatsbyPageData)
		if err != nil {
			die(err)
		}
		urls, err = gatsbySlugsToURLs(*gatsbyPageData, slugs)
		if err != nil {
			die(err)
		}
		urls = filterURLs(urls, *filter, *maxN)
		fmt.Fprintf(os.Stderr, "gatsby page-data → %d URLs (filter=%q max=%d)\n", len(urls), *filter, *maxN)
	}
	if len(urls) == 0 {
		fmt.Fprintln(os.Stderr, "no URLs to pull")
		os.Exit(1)
	}
	run(urls, o, mode, os.Args[1:])
}

func cmdPullURL(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("pull-url", flag.ExitOnError)
	bindOpts(fs, &o)
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "pull-url: URL required")
		os.Exit(2)
	}
	run([]string{fs.Arg(0)}, o, "pull-url", os.Args[1:])
}

// cmdInit clones the upstream supabase repo as a sparse checkout under
// ~/code/docs/.cache/supabase-src/. Idempotent — no-op when the dir exists
// (use `refresh` to update an existing checkout).
func cmdInit(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	bindOpts(fs, &o)
	fs.Parse(args)
	srcDir := filepath.Join(o.sourceCache, "supabase-src")

	if _, err := os.Stat(srcDir); err == nil {
		fmt.Printf("supabase source cache already at %s — use `refresh` to update.\n", srcDir)
		return
	}
	if err := os.MkdirAll(o.sourceCache, 0o755); err != nil {
		die(err)
	}

	steps := [][]string{
		{"git", "clone", "--filter=blob:none", "--sparse", "--depth=1",
			"https://github.com/supabase/supabase.git", srcDir},
		{"git", "-C", srcDir, "sparse-checkout", "set",
			"apps/docs/content/guides", "apps/docs/spec"},
	}
	for _, args := range steps {
		fmt.Fprintf(os.Stderr, "→ %s\n", strings.Join(args, " "))
		c := exec.Command(args[0], args[1:]...)
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
		if err := c.Run(); err != nil {
			die(err)
		}
	}
	fmt.Printf("✓ supabase source cache initialized at %s\n", srcDir)
}

func cmdRefresh(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("refresh", flag.ExitOnError)
	bindOpts(fs, &o)
	fs.Parse(args)
	srcDir := filepath.Join(o.sourceCache, "supabase-src")
	if _, err := os.Stat(srcDir); err != nil {
		die(fmt.Errorf("no supabase source cache at %s — clone it first", srcDir))
	}
	cmd := exec.Command("git", "-C", srcDir, "pull", "--ff-only")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		die(err)
	}
	fmt.Println("refreshed", srcDir)
}

// cmdReindex rebuilds the FTS5 search index AND regenerates the per-source
// _INDEX.md files from the current state of <out>/<source>/*.md. Runs
// automatically after every pull; this subcommand is for manual rebuilds
// (e.g. after files are edited externally, or after `rm` of a stale doc).
func cmdReindex(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("reindex", flag.ExitOnError)
	bindOpts(fs, &o)
	fs.Parse(args)

	start := time.Now()
	var n int
	if err := withWriteLock(o.out, func() error {
		fmt.Fprintf(os.Stderr, "regenerating per-source _INDEX.md ...\n")
		if err := regenerateIndex(o.out, nil); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "rebuilding FTS5 index at %s ...\n", ftsDBPath(o.out))
		idx, err := openFTSIndex(o.out)
		if err != nil {
			return err
		}
		defer idx.close()
		if err := idx.rebuild(o.out); err != nil {
			return err
		}
		n, _ = idx.totalDocs()
		return nil
	}); err != nil {
		die(err)
	}
	fmt.Printf("indexed %d docs in %s\n", n, time.Since(start).Round(time.Millisecond))
}

// cmdList enumerates pulled sources with doc counts and last-pull timestamps.
// Cheaper than `_INDEX.md` regeneration: counts .md files via WalkDir without
// reading bodies, and pulls last_pull from manifest.json. Designed for agent
// self-discovery — `--json` is the canonical surface for "which docs do we
// have, narrow my search".
func cmdList(args []string) {
	o := defaultOpts()
	var asJSON bool
	fset := flag.NewFlagSet("list", flag.ExitOnError)
	fset.StringVar(&o.out, "out", o.out, "output root dir")
	fset.BoolVar(&asJSON, "json", false, "emit JSON instead of human-readable")
	fset.Parse(args)

	type listEntry struct {
		Name     string `json:"name"`
		Docs     int    `json:"docs"`
		LastPull string `json:"last_pull,omitempty"`
	}
	type listOutput struct {
		Out       string      `json:"out"`
		Sources   []listEntry `json:"sources"`
		TotalDocs int         `json:"total_docs"`
	}

	sources, err := listSources(o.out)
	if err != nil {
		die(err)
	}
	out := listOutput{Out: o.out, Sources: []listEntry{}}
	for _, src := range sources {
		srcDir := filepath.Join(o.out, src)
		count := 0
		_ = filepath.WalkDir(srcDir, func(_ string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			name := d.Name()
			if strings.HasSuffix(name, ".md") && name != "_INDEX.md" {
				count++
			}
			return nil
		})
		if count == 0 {
			continue
		}
		_, fetchedByPath := loadManifestMaps(srcDir, src)
		latest := ""
		for _, t := range fetchedByPath {
			if t > latest {
				latest = t
			}
		}
		out.Sources = append(out.Sources, listEntry{Name: src, Docs: count, LastPull: latest})
		out.TotalDocs += count
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			die(err)
		}
		return
	}

	fmt.Printf("%d sources, %d docs in %s\n\n", len(out.Sources), out.TotalDocs, out.Out)
	maxName := 0
	for _, s := range out.Sources {
		if len(s.Name) > maxName {
			maxName = len(s.Name)
		}
	}
	for _, s := range out.Sources {
		latest := s.LastPull
		if latest == "" {
			latest = "—"
		}
		fmt.Printf("  %-*s  %5d  %s\n", maxName, s.Name, s.Docs, latest)
	}
}

// cmdShow prints details for a single source: doc count, last-pull, and
// per-group counts using the same groupKey logic as `_INDEX.md`. Designed
// to let an agent confirm "is tech X mirrored, and what's inside it?"
// before issuing a search query.
func cmdShow(args []string) {
	o := defaultOpts()
	var asJSON bool
	fset := flag.NewFlagSet("show", flag.ExitOnError)
	fset.StringVar(&o.out, "out", o.out, "output root dir")
	fset.BoolVar(&asJSON, "json", false, "emit JSON instead of human-readable")

	// Pull the first positional out before flag.Parse so flags can appear
	// either before or after the source name (Go's flag stops at the first
	// non-flag arg by default).
	var src string
	var fsArgs []string
	for _, a := range args {
		if src == "" && !strings.HasPrefix(a, "-") {
			src = a
			continue
		}
		fsArgs = append(fsArgs, a)
	}
	fset.Parse(fsArgs)
	if src == "" || len(fset.Args()) > 0 {
		fmt.Fprintln(os.Stderr, "show: expected exactly one source name (try `docs-puller list`)")
		os.Exit(2)
	}
	srcDir := filepath.Join(o.out, src)
	info, err := os.Stat(srcDir)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "show: source %q not found at %s — try `docs-puller list`\n", src, srcDir)
		os.Exit(1)
	}

	type groupEntry struct {
		Name string `json:"name"`
		Docs int    `json:"docs"`
	}
	type showOutput struct {
		Name     string       `json:"name"`
		Out      string       `json:"out"`
		Path     string       `json:"path"`
		Docs     int          `json:"docs"`
		LastPull string       `json:"last_pull,omitempty"`
		Groups   []groupEntry `json:"groups"`
	}

	groupCounts := map[string]int{}
	totalDocs := 0
	_ = filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".md") || name == "_INDEX.md" {
			return nil
		}
		rel, relErr := filepath.Rel(srcDir, p)
		if relErr != nil {
			return nil
		}
		totalDocs++
		groupCounts[groupKey(rel)]++
		return nil
	})

	_, fetchedByPath := loadManifestMaps(srcDir, src)
	latest := ""
	for _, t := range fetchedByPath {
		if t > latest {
			latest = t
		}
	}

	groupNames := make([]string, 0, len(groupCounts))
	for g := range groupCounts {
		groupNames = append(groupNames, g)
	}
	sort.Strings(groupNames)
	groups := make([]groupEntry, 0, len(groupNames))
	for _, g := range groupNames {
		display := g
		if display == "" {
			display = "(root)"
		}
		groups = append(groups, groupEntry{Name: display, Docs: groupCounts[g]})
	}

	out := showOutput{
		Name:     src,
		Out:      o.out,
		Path:     srcDir,
		Docs:     totalDocs,
		LastPull: latest,
		Groups:   groups,
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			die(err)
		}
		return
	}

	header := fmt.Sprintf("%s — %d docs", out.Name, out.Docs)
	if out.LastPull != "" {
		header += ", last pulled " + out.LastPull
	}
	fmt.Println(header)
	fmt.Println(out.Path)
	fmt.Println()
	if len(out.Groups) == 0 {
		fmt.Println("(no docs)")
		return
	}
	fmt.Println("Top-level groups:")
	maxName := 0
	for _, g := range out.Groups {
		if len(g.Name) > maxName {
			maxName = len(g.Name)
		}
	}
	for _, g := range out.Groups {
		fmt.Printf("  %-*s  %5d\n", maxName, g.Name, g.Docs)
	}
}

type result struct {
	URL       string `json:"url"`
	Source    string `json:"source"`
	Path      string `json:"path,omitempty"`
	Mode      string `json:"mode,omitempty"` // "source" | "yaml" | "http" | "github-readme"
	SHA256    string `json:"sha256,omitempty"`
	FetchedAt string `json:"fetched_at"`
	Skipped   string `json:"skipped,omitempty"`
	Warning   string `json:"warning,omitempty"` // e.g. "low-content"
	Unchanged bool   `json:"unchanged,omitempty"`
}

// thinContentThreshold is the byte length below which a converted page is
// flagged as suspiciously small. Catches client-rendered pages where the
// SSR shell has near-zero real content.
const thinContentThreshold = 200

func run(urls []string, o pullOpts, mode string, cmdArgs []string) {
	start := time.Now()
	startedAt := start.UTC().Format(time.RFC3339)
	now := startedAt
	if o.concurrency < 1 {
		o.concurrency = 1
	}

	results := make([]result, len(urls))
	sem := make(chan struct{}, o.concurrency)
	var wg sync.WaitGroup
	for i, raw := range urls {
		idx, rawURL := i, raw
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[idx] = processURL(rawURL, o, now)
		}()
	}
	wg.Wait()
	fetchElapsed := time.Since(start)

	var pulled, unchanged, skipped, warned int
	for _, r := range results {
		switch {
		case r.Skipped != "":
			skipped++
		case r.Unchanged:
			unchanged++
		default:
			pulled++
		}
		if r.Warning != "" {
			warned++
		}
	}

	// Serialize the manifest+index+FTS5 critical section so concurrent
	// `pull` invocations don't race on the shared corpus state.
	if err := withWriteLock(o.out, func() error {
		var prunedPaths []string
		if err := writeManifestsWithPolicy(o.out, results, o.replaceSource, o.allowLargePrune, &prunedPaths); err != nil {
			return err
		}
		if len(prunedPaths) > 0 {
			if err := deletePrunedDocPaths(o.out, prunedPaths); err != nil {
				return err
			}
		}
		// Refresh the FTS5 search index so `docs-puller search` reflects this
		// pull. Incremental upsert when the index is warm; full rebuild only
		// on a cold start. Failure here is logged but not fatal — search
		// falls back to substring scan. We also pass the touched sources to
		// regenerateIndex so untouched sources skip their O(N) title walk.
		var changedPaths, successfulPaths []string
		for _, r := range results {
			if r.Skipped != "" || r.Path == "" {
				continue
			}
			successfulPaths = append(successfulPaths, r.Path)
			if !r.Unchanged {
				changedPaths = append(changedPaths, r.Path)
			}
		}
		changedPaths = append(changedPaths, prunedPaths...)
		if err := regenerateIndex(o.out, distinctSources(results)); err != nil {
			return err
		}
		if idx, err := openFTSIndex(o.out); err == nil {
			repaired, rerr := idx.updateFTSAndRepair(o.out, changedPaths, successfulPaths)
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "fts5: update failed: %v (search will fall back to scan)\n", rerr)
			} else if repaired > 0 {
				fmt.Fprintf(os.Stderr, "fts5: repaired %d missing path entr%s\n", repaired, map[bool]string{true: "y", false: "ies"}[repaired == 1])
			}
			idx.close()
		}
		finished := time.Now().UTC()
		entry := logEntry{
			StartedAt:  startedAt,
			FinishedAt: finished.Format(time.RFC3339),
			ElapsedMs:  finished.Sub(start.UTC()).Milliseconds(),
			Mode:       mode,
			Args:       cmdArgs,
			Sources:    distinctSources(results),
			URLs:       len(results),
			Pulled:     pulled,
			Unchanged:  unchanged,
			Skipped:    skipped,
			Warned:     warned,
		}
		if err := appendIngestLog(o.out, entry); err != nil {
			fmt.Fprintf(os.Stderr, "ingest-log: append failed: %v\n", err)
		}
		return nil
	}); err != nil {
		die(err)
	}

	throughput := ""
	if fetchElapsed.Seconds() > 0 {
		throughput = fmt.Sprintf(" (fetch %.0f URLs/s)", float64(len(results))/fetchElapsed.Seconds())
	}
	totalElapsed := time.Since(start)
	if warned > 0 {
		fmt.Printf("pulled: %d  unchanged: %d  skipped: %d  warned: %d  total: %d  in %s%s\n",
			pulled, unchanged, skipped, warned, len(results), totalElapsed.Round(time.Millisecond), throughput)
	} else {
		fmt.Printf("pulled: %d  unchanged: %d  skipped: %d  total: %d  in %s%s\n",
			pulled, unchanged, skipped, len(results), totalElapsed.Round(time.Millisecond), throughput)
	}
	for _, r := range results {
		switch {
		case r.Skipped != "":
			fmt.Printf("  SKIP %s — %s\n", r.URL, r.Skipped)
		case r.Warning != "":
			fmt.Printf("  WARN %s — %s\n", r.URL, r.Warning)
		case r.Unchanged:
			continue
		case r.Mode != "" && r.Mode != "source":
			fmt.Printf("  %-14s %s\n", r.Mode, r.URL)
		}
	}
}

func validateReplacementFlags(replaceSource, allowLargePrune bool, filter string, maxN int) error {
	if allowLargePrune && !replaceSource {
		return fmt.Errorf("--allow-large-prune requires --replace-source")
	}
	if replaceSource && !allowLargePrune && (strings.TrimSpace(filter) != "" || maxN > 0) {
		return fmt.Errorf("--replace-source with --filter or --max can prune documents outside the selected subset; review the discovery input and pass --allow-large-prune to acknowledge the deletion")
	}
	return nil
}

func processURL(raw string, o pullOpts, now string) result {
	u, err := url.Parse(raw)
	if err != nil {
		return result{URL: raw, FetchedAt: now, Skipped: "parse error: " + err.Error()}
	}

	// 1. Source mode: supabase guides MDX.
	if u.Host == supabaseHost && strings.HasPrefix(u.Path, "/docs/guides/") {
		if r, err := pullSupabaseGuide(u, o, now); err == nil {
			return r
		}
		// fall through if upstream MDX has been reorged/removed
	}

	// 2. Supabase YAML-rendered routes (CLI reference, config).
	if r, ok, err := pullSupabaseYAML(u, o, now); ok {
		if err != nil {
			return result{URL: raw, Source: "supabase", FetchedAt: now, Skipped: err.Error()}
		}
		return r
	}

	if o.sourceOnly {
		return result{URL: raw, FetchedAt: now, Source: hostToSource(u.Host),
			Skipped: "no source-mode handler (--source-only set)"}
	}

	source, rel := resolveTarget(u)
	if source == "" {
		return result{URL: raw, FetchedAt: now, Skipped: "no target mapping for host"}
	}

	var (
		data []byte
		mode string
	)
	switch {
	case u.Host == "github.com" && isGithubRepoRoot(u.Path):
		data, err = fetchGithubReadme(u)
		mode = "github-readme"
	case isNativeMarkdownURL(u):
		// Vendor-published .md mirror (e.g. docs.slack.dev's `<page>.md` per
		// llms.txt convention). Fetch raw — html-to-markdown on plain
		// markdown mangles fenced blocks and link syntax.
		data, err = httpGet(u.String())
		mode = "http-md"
	default:
		data, err = fetchAndConvert(u.String())
		mode = "http"
	}
	if err != nil {
		return result{URL: raw, FetchedAt: now, Source: source, Skipped: err.Error()}
	}

	outPath := filepath.Join(o.out, source, rel)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return result{URL: raw, FetchedAt: now, Source: source, Skipped: err.Error()}
	}
	unchanged, err := writeFileIfChanged(outPath, data)
	if err != nil {
		return result{URL: raw, FetchedAt: now, Source: source, Skipped: err.Error()}
	}
	sum := sha256.Sum256(data)
	r := result{
		URL: raw, Source: source, Path: filepath.Join(source, rel),
		Mode: mode, SHA256: hex.EncodeToString(sum[:]), FetchedAt: now, Unchanged: unchanged,
	}
	if mode == "http" && len(strings.TrimSpace(string(data))) < thinContentThreshold {
		r.Warning = fmt.Sprintf("low-content (%d bytes) — selector may have missed real content or page is client-rendered", len(data))
	}
	return r
}

func writeFileIfChanged(path string, data []byte) (bool, error) {
	existing, err := os.ReadFile(path)
	if err == nil && bytes.Equal(existing, data) {
		return true, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return false, os.WriteFile(path, data, 0o644)
}

func pullSupabaseGuide(u *url.URL, o pullOpts, now string) (result, error) {
	rel := strings.TrimPrefix(u.Path, "/docs/")
	srcRel := filepath.Join("apps", "docs", "content", rel+".mdx")
	srcPath := filepath.Join(o.sourceCache, "supabase-src", srcRel)

	data, err := os.ReadFile(srcPath)
	if err != nil {
		cacheRoot := filepath.Join(o.sourceCache, "supabase-src")
		if _, statErr := os.Stat(cacheRoot); os.IsNotExist(statErr) {
			return result{}, fmt.Errorf("supabase source cache missing at %s - run `docs-puller init`", cacheRoot)
		}
		return result{}, fmt.Errorf("source not found at %s", srcPath)
	}
	sum := sha256.Sum256(data)
	outRel := rel + ".md"
	outPath := filepath.Join(o.out, "supabase", outRel)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return result{}, err
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return result{}, err
	}
	return result{
		URL: u.String(), Source: "supabase",
		Path: filepath.Join("supabase", outRel), Mode: "source",
		SHA256: hex.EncodeToString(sum[:]), FetchedAt: now,
	}, nil
}

func hostToSource(host string) string {
	if host == "" {
		return ""
	}
	host = strings.TrimPrefix(host, "www.")
	if i := strings.Index(host, "."); i >= 0 {
		return host[:i]
	}
	return host
}

func isGithubRepoRoot(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 2
}

func fetchGithubReadme(u *url.URL) ([]byte, error) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	owner, repo := parts[0], parts[1]
	for _, branch := range []string{"HEAD", "main", "master"} {
		raw := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/README.md", owner, repo, branch)
		body, err := httpGet(raw)
		if err == nil {
			return body, nil
		}
	}
	return nil, fmt.Errorf("no README.md found for %s/%s", owner, repo)
}

func fetchAndConvert(rawURL string) ([]byte, error) {
	body, err := httpGet(rawURL)
	if err != nil {
		return nil, err
	}
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Host
	}
	title := extractHTMLTitle(body)
	main := extractMain(host, body)
	md, err := htmltomd.ConvertString(string(main))
	if err != nil {
		return nil, fmt.Errorf("html->md: %w", err)
	}
	md = cleanMarkdown(md)
	// Prepend page title as H1 when the converted body doesn't already
	// have one matching the title. Handles three cases:
	//   - Body has no H1: prepend (Docusaurus, MDX-rendered).
	//   - Body has matching H1: skip prepend (MS Learn — title in body).
	//   - Body has unrelated H1 (e.g. pg_hba.conf header lines that fooled
	//     the converter): prepend; the title is more likely correct.
	if title != "" && !bodyHasMatchingH1(md, title) {
		md = "# " + title + "\n\n" + md
	}
	return []byte(md), nil
}

// emptyAnchorRE matches header-permalink anchors that header-anchor patterns
// (Mintlify, Docusaurus, MkDocs Material, Sphinx) emit next to every heading.
// Visible text can be empty/whitespace, a literal "#", or "¶". The hash-link
// target may carry a title attribute (`"Permanent link"` etc.). Examples:
//
//	[​](#section)                         — zero-width space inside
//	[](#section)                          — truly empty
//	  [ ](#section)                       — whitespace only
//	[#](#activecampaign-node "Permanent link")  — MkDocs Material
//	[¶](#section)                         — Sphinx
//
// The regex tolerates surrounding whitespace and trims one trailing space so
// the heading line collapses cleanly: "## [#](#x) Title" -> "## Title".
var emptyAnchorRE = regexp.MustCompile(`\[[\s\x{200B}#¶]*\]\(#[^)]+(?:\s+"[^"]*")?\)\s?`)

// emptyExternalAnchorRE strips truly-empty bracket links paired with an
// external URL — typically MkDocs Material's "Edit this page" pencil icon
// (`[](https://github.com/.../edit/... "Edit this page")`). Conservative: only
// matches when the visible text is empty or whitespace, so genuine icon-bearing
// links like `[![alt](img.png)](url)` are unaffected (their bracket content is
// non-empty).
var emptyExternalAnchorRE = regexp.MustCompile(`\[[\s\x{200B}]*\]\(https?://[^)]+(?:\s+"[^"]*")?\)\s?`)

// cleanMarkdown applies post-conversion fixups to Markdown produced by
// html-to-markdown. Strips empty/permalink header anchors and "Edit this page"
// style empty-bracket external links.
func cleanMarkdown(md string) string {
	md = emptyAnchorRE.ReplaceAllString(md, "")
	md = emptyExternalAnchorRE.ReplaceAllString(md, "")
	return md
}

func bodyHasMatchingH1(md, title string) bool {
	want := strings.ToLower(strings.TrimSpace(title))
	if want == "" {
		return false
	}
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "# ") || strings.HasPrefix(t, "## ") {
			continue
		}
		got := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(t, "# ")))
		if got == want || strings.Contains(got, want) || strings.Contains(want, got) {
			return true
		}
	}
	return false
}

// httpGet fetches rawURL with a small retry loop (3 attempts) on retryable
// failures: network errors, HTTP 429, and HTTP 5xx. Backoff is 500ms doubling.
// Honors the server's Retry-After header on 429 when present.
func httpGet(rawURL string) ([]byte, error) {
	const attempts = 3
	var (
		lastErr error
		backoff = 500 * time.Millisecond
	)
	for i := 0; i < attempts; i++ {
		body, status, retryAfter, err := httpDo(rawURL)
		if err == nil && status < 400 {
			return body, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("HTTP %d for %s", status, rawURL)
		}
		if !isRetryable(status, err) || i == attempts-1 {
			return nil, lastErr
		}
		wait := backoff
		if retryAfter > 0 {
			wait = retryAfter
		}
		time.Sleep(wait)
		backoff *= 2
	}
	return nil, lastErr
}

func httpDo(rawURL string) (body []byte, status int, retryAfter time.Duration, err error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	// Force English content. Some sites (notably developer.android.com) serve
	// the user's geo-locale by default, which silently puts Hindi or Chinese
	// pages into the corpus. Override with a high-quality English preference.
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
			retryAfter = time.Duration(secs) * time.Second
		}
	}
	body, err = io.ReadAll(resp.Body)
	return body, resp.StatusCode, retryAfter, err
}

func isRetryable(status int, err error) bool {
	if err != nil {
		return true
	}
	return status == 429 || status >= 500
}

// writeManifests merges this run's results into each source's manifest.json,
// keyed by URL — the latest result for a given URL replaces the previous one,
// and URLs not in this run keep their prior entries. This means `pull-url`
// updates one entry instead of wiping the whole manifest.
//
// Per ADR-001 (2026-04-29 revision): the manifest is bounded keyed-lookup
// state, not an append-only audit log. JSON object + atomic temp-file +
// rename writes; legacy manifest.jsonl files are auto-migrated on first
// read by loadOrMigrateManifest.
//
// Results without a source (URL had no target mapping) are reported in the
// run summary but NOT persisted to disk — those entries don't represent a
// real artifact and would otherwise leave an `_unmapped/` directory behind.
func writeManifests(outRoot string, results []result, replaceSource bool, prunedPaths *[]string) error {
	return writeManifestsWithPolicy(outRoot, results, replaceSource, false, prunedPaths)
}

const (
	largePruneAbsoluteLimit = 50
	largePruneMinimum       = 5
	largePruneRatio         = 0.10
)

type manifestWritePlan struct {
	source string
	dir    string
	value  manifest
	pruned []string
}

// writeManifestsWithPolicy plans every touched source before committing any
// manifest. Authoritative replacement fails closed when discovery would remove
// a suspiciously large portion of an existing source unless the operator has
// explicitly acknowledged the plan with --allow-large-prune.
func writeManifestsWithPolicy(outRoot string, results []result, replaceSource, allowLargePrune bool, prunedPaths *[]string) error {
	bySource := map[string][]result{}
	for _, r := range results {
		if r.Source == "" {
			continue
		}
		bySource[r.Source] = append(bySource[r.Source], r)
	}
	sources := make([]string, 0, len(bySource))
	for source := range bySource {
		sources = append(sources, source)
	}
	sort.Strings(sources)

	plans := make([]manifestWritePlan, 0, len(sources))
	for _, source := range sources {
		fresh := bySource[source]
		dir := filepath.Join(outRoot, source)
		m, err := loadOrMigrateManifest(dir)
		if err != nil {
			return searchruntime.ManifestLoadSourceError(source, err)
		}
		plan := manifestWritePlan{source: source, dir: dir, value: m}
		if replaceSource {
			existingDocs, err := sourceDocPaths(outRoot, source)
			if err != nil {
				return fmt.Errorf("scan %s before replacement: %w", source, err)
			}
			removed := replaceManifestForSource(&plan.value, fresh)
			unmanifested, err := findUnmanifestedDocPaths(outRoot, source, plan.value)
			if err != nil {
				return fmt.Errorf("scan %s for unmanifested docs: %w", source, err)
			}
			removed = appendUniqueStrings(removed, unmanifested...)
			// Replacement is keyed by canonical URL, while artifacts are deleted
			// by path. A canonical-URL migration can therefore retire an old URL
			// and add its replacement at the exact same path. Never schedule a
			// path that the final manifest still references for deletion.
			removed = unreferencedManifestPaths(removed, plan.value)
			existingCount := max(len(m.Entries), len(existingDocs))
			if err := validateSourceReplacement(source, existingCount, len(removed), allowLargePrune); err != nil {
				return err
			}
			plan.pruned = removed
		} else {
			mergeIntoManifest(&plan.value, fresh)
		}
		if n := dedupeManifestPaths(&plan.value); n > 0 {
			fmt.Fprintf(os.Stderr, "manifest: %s: pruned %d stale duplicate-path entr%s (older URL variants of the same file)\n",
				source, n, map[bool]string{true: "y", false: "ies"}[n == 1])
		}
		plans = append(plans, plan)
	}

	for _, plan := range plans {
		if err := writeManifestAtomic(plan.dir, plan.value); err != nil {
			return searchruntime.ManifestWriteSourceError(plan.source, err)
		}
		if prunedPaths != nil {
			*prunedPaths = append(*prunedPaths, plan.pruned...)
		}
	}
	return nil
}

func validateSourceReplacement(source string, existing, pruning int, allowLargePrune bool) error {
	if allowLargePrune || pruning == 0 || existing == 0 {
		return nil
	}
	ratio := float64(pruning) / float64(existing)
	if pruning < largePruneAbsoluteLimit && (pruning < largePruneMinimum || ratio <= largePruneRatio) {
		return nil
	}
	return fmt.Errorf("replace-source safety: source %q would prune %d of %d existing documents (%.1f%%); verify discovery completeness and rerun with --allow-large-prune to acknowledge the deletion", source, pruning, existing, ratio*100)
}

// replaceManifestForSource treats the fresh URL set as authoritative while
// preserving last-known-good artifacts for URLs that were requested but
// temporarily skipped. Only URLs absent from fresh are pruned; successful
// results replace prior entries. This prevents a transient 429/5xx/timeout from
// deleting a healthy cached document during --replace-source.
func replaceManifestForSource(m *manifest, fresh []result) []string {
	if m.Entries == nil {
		m.Entries = map[string]result{}
	}
	present := map[string]bool{}
	successful := make([]result, 0, len(fresh))
	for _, r := range fresh {
		if r.URL == "" {
			continue
		}
		present[r.URL] = true
		if r.Skipped == "" {
			successful = append(successful, r)
		}
	}
	var pruned []string
	for url, r := range m.Entries {
		if present[url] {
			continue
		}
		if r.Path != "" {
			pruned = append(pruned, r.Path)
		}
		delete(m.Entries, url)
	}
	mergeIntoManifest(m, successful)
	return pruned
}

func deletePrunedDocPaths(outRoot string, relPaths []string) error {
	rootAbs, err := filepath.Abs(outRoot)
	if err != nil {
		return fmt.Errorf("resolve output root: %w", err)
	}
	root, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return fmt.Errorf("resolve output root symlinks: %w", err)
	}
	paths := make([]string, 0, len(relPaths))
	for _, rel := range relPaths {
		clean := filepath.Clean(filepath.FromSlash(rel))
		if rel == "" || filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing to delete unsafe pruned path %q", rel)
		}
		path := filepath.Join(root, clean)
		within, err := filepath.Rel(root, path)
		if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing to delete pruned path outside output root %q", rel)
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect pruned path %q: %w", rel, err)
		}
		resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("resolve pruned path parent %q: %w", rel, err)
		}
		within, err = filepath.Rel(root, resolvedParent)
		if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing to delete pruned path through a symlink outside output root %q", rel)
		}
		paths = append(paths, path)
	}
	// Validate the complete plan before deleting the first file so one unsafe
	// candidate cannot leave a partially applied replacement behind.
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func unreferencedManifestPaths(candidates []string, m manifest) []string {
	referenced := make(map[string]bool, len(m.Entries))
	for _, r := range m.Entries {
		if r.Path != "" {
			referenced[filepath.Clean(filepath.FromSlash(r.Path))] = true
		}
	}
	filtered := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" || referenced[filepath.Clean(filepath.FromSlash(candidate))] {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered
}

// findUnmanifestedDocPaths closes a replace-source hygiene gap: files left by
// much older manifests (or interrupted/manual pulls) are otherwise invisible
// to replaceManifestForSource and remain searchable forever. Explicit source
// replacement means the successful fresh manifest is authoritative, so any
// other Markdown-family document is stale. Generated _INDEX.md is rebuilt by
// the normal finalize path and is never a vendor document.
func findUnmanifestedDocPaths(outRoot, source string, m manifest) ([]string, error) {
	kept := make(map[string]bool, len(m.Entries))
	for _, r := range m.Entries {
		if r.Path != "" {
			kept[filepath.Clean(filepath.FromSlash(r.Path))] = true
		}
	}

	paths, err := sourceDocPaths(outRoot, source)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, rel := range paths {
		clean := filepath.Clean(filepath.FromSlash(rel))
		if !kept[clean] {
			stale = append(stale, filepath.ToSlash(clean))
		}
	}
	sort.Strings(stale)
	return stale, nil
}

func sourceDocPaths(outRoot, source string) ([]string, error) {
	sourceDir := filepath.Join(outRoot, source)
	var paths []string
	err := filepath.WalkDir(sourceDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() || d.Name() == "_INDEX.md" || !isMarkdownDocName(strings.ToLower(d.Name())) {
			return nil
		}
		rel, err := filepath.Rel(outRoot, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(filepath.Clean(rel)))
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func appendUniqueStrings(base []string, values ...string) []string {
	seen := make(map[string]bool, len(base)+len(values))
	for _, value := range base {
		seen[value] = true
	}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		base = append(base, value)
	}
	return base
}

// mergeIntoManifest applies fresh results to m in-place. URLs already in m
// get overwritten; new URLs get added. Empty-URL results are dropped.
func mergeIntoManifest(m *manifest, fresh []result) {
	if m.Entries == nil {
		m.Entries = map[string]result{}
	}
	for _, r := range fresh {
		if r.URL != "" {
			m.Entries[r.URL] = r
		}
	}
}

// extractURLsFromFile reads the file at path (or fetches the URL if path
// starts with http:// or https://) and returns every URL found in the body,
// dedup'd, with fragments/queries stripped. Useful for ingesting curated
// notes files, vendor-published llms-sitemap.md files, or HTML index pages
// (e.g. https://go.dev/doc/) — when the body is HTML, <a href> values are
// parsed and resolved against the source URL.
func extractURLsFromFile(path string) ([]string, error) {
	var body []byte
	var base string
	var err error
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		body, err = httpGet(path)
		base = path
	} else {
		body, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	return extractURLs(body, base), nil
}

// extractURLs returns every URL found in body. When the body looks like HTML
// (cheap substring check for `<a `), <a href> values are parsed with goquery
// and resolved against base. Otherwise falls back to a plain regex over the
// raw text, which suits markdown notes files and llms-sitemap.md.
func extractURLs(body []byte, base string) []string {
	if bytes.Contains(body, []byte("<a ")) {
		if urls := extractHTMLLinks(body, base); len(urls) > 0 {
			return urls
		}
	}
	matches := urlRE.FindAllString(stripCommentLines(body), -1)
	return dedupeURLs(matches)
}

func stripCommentLines(body []byte) string {
	lines := strings.Split(string(body), "\n")
	var b strings.Builder
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func extractHTMLLinks(body []byte, base string) []string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	baseURL, _ := url.Parse(base)
	raw := make([]string, 0, 64)
	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href, ok := s.Attr("href")
		if !ok {
			return
		}
		href = strings.TrimSpace(href)
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "javascript:") {
			return
		}
		ref, err := url.Parse(href)
		if err != nil {
			return
		}
		var abs *url.URL
		if ref.IsAbs() {
			abs = ref
		} else if baseURL != nil {
			abs = baseURL.ResolveReference(ref)
		} else {
			return
		}
		if abs.Scheme != "http" && abs.Scheme != "https" {
			return
		}
		raw = append(raw, abs.String())
	})
	return dedupeURLs(raw)
}

func dedupeURLs(matches []string) []string {
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		m = strings.TrimRight(m, ".,;:")
		if u, err := url.Parse(m); err == nil {
			u.Fragment = ""
			u.RawQuery = ""
			m = u.String()
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// isNativeMarkdownURL reports whether the URL points at a markdown file the
// server will return verbatim (no html-to-markdown conversion needed). Today
// this is path-based (.md / .markdown / .md.txt suffix); a future version could fall
// back to a HEAD probe for sites that serve markdown without a suffix.
func isNativeMarkdownURL(u *url.URL) bool {
	p := strings.ToLower(u.Path)
	return strings.HasSuffix(p, ".md") ||
		strings.HasSuffix(p, ".markdown") ||
		strings.HasSuffix(p, ".md.txt")
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// stringSliceFlag implements flag.Value to collect a repeatable string flag
// (e.g. `--exclude foo --exclude bar` → []string{"foo","bar"}).
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }
