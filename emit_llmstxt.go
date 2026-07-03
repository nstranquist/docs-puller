package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// cmdEmitLLMsTxt generates an llms.txt index for one or all corpus sources
// straight from the locally-mirrored docs. It closes the loop for sources that
// lacked an upstream llms.txt (the ones we had to curate or sitemap-crawl
// during ingest): docs-puller already holds the pages, titles, and origin URLs,
// so it can emit its own llms.txt with no crawl and no API calls.
//
// Contrast with create-llmstxt-py (the ref this was ported from), which maps a
// live site via Firecrawl and writes descriptions via OpenAI. We don't need
// either — the corpus is already on disk and titled.
func cmdEmitLLMsTxt(args []string) {
	o := defaultOpts()
	var (
		source  string
		all     bool
		outFile string
	)
	fset := flag.NewFlagSet("emit-llmstxt", flag.ExitOnError)
	fset.StringVar(&o.out, "out", o.out, "output root dir")
	fset.StringVar(&source, "source", "", "emit for this source only")
	fset.BoolVar(&all, "all", false, "emit llms.txt for every source")
	fset.StringVar(&outFile, "file", "", "write to this path instead of <source>/llms.txt (single-source only)")

	// Accept the source as a leading positional (`emit-llmstxt <source>`),
	// matching `show`'s ergonomics.
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		source = rest[0]
		rest = rest[1:]
	}
	fset.Parse(rest)

	if !all && source == "" {
		die(fmt.Errorf("emit-llmstxt: pass a <source> (or --source NAME) or --all"))
	}
	if all && outFile != "" {
		die(fmt.Errorf("emit-llmstxt: --file cannot be combined with --all"))
	}

	var sources []string
	if all {
		var err error
		if sources, err = listSources(o.out); err != nil {
			die(err)
		}
	} else {
		sources = []string{source}
	}

	for _, src := range sources {
		srcDir := filepath.Join(o.out, src)
		entries, latest, err := collectEntries(srcDir, src)
		if err != nil {
			if all {
				fmt.Fprintf(os.Stderr, "  skip %s: %v\n", src, err)
				continue
			}
			die(err)
		}
		if len(entries) == 0 {
			if all {
				continue
			}
			die(fmt.Errorf("emit-llmstxt: source %q has no docs at %s", src, srcDir))
		}
		content := renderLLMsTxt(src, entries, latest)
		dest := filepath.Join(srcDir, "llms.txt")
		if outFile != "" {
			dest = outFile
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			die(err)
		}
		fmt.Printf("emit-llmstxt: %s — %d docs → %s\n", src, len(entries), dest)
	}
}

// renderLLMsTxt builds an llms.txt (llmstxt.org format): an H1 title, a
// blockquote summary, then doc links grouped into H2 sections by their
// top-level path segment. Deterministic — no wall-clock, so output is stable
// for the same corpus (the `latest` fetch timestamp is the only date stamped).
func renderLLMsTxt(src string, entries []indexEntry, latest string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", src)
	summary := fmt.Sprintf("Vendor docs for %s, mirrored locally by docs-puller (%d pages).", src, len(entries))
	if latest != "" {
		summary += fmt.Sprintf(" Last upstream fetch: %s.", latest)
	}
	fmt.Fprintf(&b, "> %s\n", summary)

	// Group by first path segment so large sources read as sections.
	groups := map[string][]indexEntry{}
	var order []string
	for _, e := range entries {
		key := "Docs"
		if i := strings.IndexByte(e.path, '/'); i > 0 {
			key = e.path[:i]
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], e)
	}
	sort.Strings(order)

	for _, key := range order {
		fmt.Fprintf(&b, "\n## %s\n\n", key)
		for _, e := range groups[key] {
			link := e.url
			if link == "" {
				// No recorded origin URL — reference the local mirror path so
				// the entry is still resolvable.
				link = e.path
			}
			fmt.Fprintf(&b, "- [%s](%s)\n", e.title, link)
		}
	}
	return b.String()
}
