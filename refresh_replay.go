package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Source replay: `docs-puller refresh --source NAME` re-runs the newest
// recorded pull for NAME from _INGEST_LOG.jsonl.
//
// Why: status already flagged stale sources by age, but gave no way to fix
// them. Nobody remembered how a source was first pulled, so sources stayed
// stale for months. The claude-code mirror stayed at a 5-URL test fixture
// from May until November. The ingest log already records the exact args of
// every pull, so the refresh recipe is in the log and needs no new registry.

// replayableModes are the ingest modes whose recorded args are a complete
// `docs-puller pull ...` invocation that can be run again as-is.
var replayableModes = map[string]bool{
	"sitemap":         true,
	"llms-txt":        true,
	"gatsby-pagedata": true,
	"from-file":       true,
	"docc":            true,
	"local":           true,
	"github-repo":     true,
	"git-repo":        true,
}

// latestReplayableIngest picks the recipe to replay for source. entries
// must be newest first. It prefers the newest owning *discovery* recipe
// (llms.txt, sitemap, page-data, DocC, repo, local dir, or a remote --from
// index), because only discovery finds pages added upstream. It falls back to
// the newest owning URL-list recipe. It skips recipes whose local input is
// relative (unknown cwd) or no longer exists (test temp dirs).
//
// Why discovery first: a 2026-08-04 batch refresh replayed small curated or
// fixture URL lists (inputs/*-fixture.txt) over full-site sources. A
// newest-wins rule would then keep claude-code, bun, clickhouse, and others
// pinned to a handful of pages forever.
func latestReplayableIngest(entries []logEntry, source string) (logEntry, bool) {
	var fallback *logEntry
	for i := range entries {
		e := entries[i]
		if !replayableModes[e.Mode] || len(e.Args) == 0 || e.Args[0] != "pull" {
			continue
		}
		if !localInputsReplayable(e.Args) || !ingestOwnsSource(e, source) {
			continue
		}
		if isDiscoveryRecipe(e) {
			return e, true
		}
		if fallback == nil {
			fallback = &entries[i]
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return logEntry{}, false
}

// isDiscoveryRecipe reports a pull that enumerates the upstream site or tree
// itself rather than a fixed URL list.
func isDiscoveryRecipe(e logEntry) bool {
	if e.Mode != "from-file" {
		return true
	}
	return strings.Contains(flagValue(e.Args, "from"), "://")
}

// ingestOwnsSource reports whether e is the pull that defines source, not a
// pull that only spilled a few cross-links into it. Vendor llms.txt files
// link to github.com, so a Temporal pull records sources [github temporal];
// replaying it would refresh Temporal, not the github corpus.
func ingestOwnsSource(e logEntry, source string) bool {
	if len(e.Sources) == 1 {
		return e.Sources[0] == source
	}
	return primarySource(e.Args) == source
}

// primarySource derives the source a recorded pull targets: --name for the
// modes that honor it, else the routed source of the input URL.
func primarySource(args []string) string {
	if v := flagValue(args, "name"); v != "" {
		for _, f := range []string{"local", "github-repo", "git-repo", "docc"} {
			if flagValue(args, f) != "" {
				return v
			}
		}
	}
	for _, f := range []string{"llms-txt", "sitemap", "gatsby-pagedata", "docc", "from"} {
		raw := flagValue(args, f)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return ""
		}
		if src, _ := resolveTarget(u); src != "" {
			return src
		}
		return hostToSource(u.Host)
	}
	return ""
}

// flagValue returns the value of --name / -name / --name=v in args.
func flagValue(args []string, name string) string {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		key, val, hasEq := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if key != name {
			continue
		}
		if hasEq {
			return val
		}
		if i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// localInputsReplayable reports whether every --from/--local filesystem
// input is an absolute path that still exists. A relative recipe only worked
// from its original cwd; a missing one was usually a test temp dir.
func localInputsReplayable(args []string) bool {
	for _, f := range []string{"from", "local"} {
		v := flagValue(args, f)
		if v == "" || strings.Contains(v, "://") {
			continue
		}
		if !filepath.IsAbs(v) {
			return false
		}
		if _, err := os.Stat(v); err != nil {
			return false
		}
	}
	return true
}

// latestIngestForSource returns the newest ingest entry of any mode that
// touched source. It lets refresh name the right command for sources that a
// non-replayable ingest (pull-article, crawl-refs, crawl-nicos, ...) owns.
func latestIngestForSource(entries []logEntry, source string) (logEntry, bool) {
	for _, e := range entries {
		for _, s := range e.Sources {
			if s == source {
				return e, true
			}
		}
	}
	return logEntry{}, false
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return "?"
	}
	return args[0]
}

func otherSources(sources []string, skip string) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		if s != skip {
			out = append(out, s)
		}
	}
	return out
}

// replayArgs turns recorded pull args into refresh args. It drops --max: a
// refresh exists to restore full coverage, so it never truncates. It drops
// --out so the replay writes to the current corpus root.
func replayArgs(recorded []string, out string) []string {
	args := make([]string, 0, len(recorded)+2)
	for i := 0; i < len(recorded); i++ {
		a := recorded[i]
		name := strings.TrimLeft(a, "-")
		if strings.HasPrefix(a, "-") && (name == "max" || name == "out") {
			i++ // skip the value
			continue
		}
		if strings.HasPrefix(a, "--max=") || strings.HasPrefix(a, "-max=") ||
			strings.HasPrefix(a, "--out=") || strings.HasPrefix(a, "-out=") {
			continue
		}
		args = append(args, a)
	}
	return append(args, "--out", out)
}

// cmdRefreshSource implements `refresh --source NAME [--dry-run]`.
func cmdRefreshSource(out, source string, dryRun bool) {
	entries, err := readIngestLog(out, 0)
	if err != nil {
		die(fmt.Errorf("refresh: read ingest log: %w", err))
	}
	e, ok := latestReplayableIngest(entries, source)
	if !ok {
		if last, found := latestIngestForSource(entries, source); found {
			if replayableModes[last.Mode] && localInputsReplayable(last.Args) {
				die(fmt.Errorf("refresh: source %q has no owning pull; it only collects cross-links from other pulls (latest: %s via %s). Refresh those sources instead",
					source, strings.Join(otherSources(last.Sources, source), ", "), last.Mode))
			}
			die(fmt.Errorf("refresh: source %q was last ingested by `%s` (mode %s), which refresh cannot replay; rerun: docs-puller %s",
				source, firstArg(last.Args), last.Mode, strings.Join(replayArgs(last.Args, out), " ")))
		}
		die(fmt.Errorf("refresh: no pull recorded for source %q in %s/%s; pull it once with `docs-puller pull ...`", source, out, ingestLogFile))
	}
	if abs, err := filepath.Abs(out); err == nil {
		out = abs // the replay must not depend on the caller's cwd
	}
	args := replayArgs(e.Args, out)
	capNote := ""
	if e.CapHit || e.Capped > 0 {
		capNote = fmt.Sprintf(", %d dropped to --max", e.Capped)
	}
	fmt.Fprintf(os.Stderr, "refresh %s: replaying %s pull from %s (%d URLs%s)\n", source, e.Mode, e.StartedAt, e.URLs, capNote)
	if len(e.Sources) > 1 {
		fmt.Fprintf(os.Stderr, "  note: this pull also refreshes %s\n", strings.Join(otherSources(e.Sources, source), ", "))
	}
	fmt.Fprintf(os.Stderr, "  docs-puller %s\n", strings.Join(args, " "))
	if dryRun {
		return
	}
	self, err := os.Executable()
	if err != nil {
		die(fmt.Errorf("refresh: locate own binary: %w", err))
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		die(fmt.Errorf("refresh %s: %w", source, err))
	}
}

// statusCappedSource is a source whose newest pull was truncated by --max.
type statusCappedSource struct {
	Name     string `json:"name"`
	Capped   int    `json:"capped"` // 0 when the drop count is unknown (DocC)
	PulledAt string `json:"pulled_at"`
}

// collectCappedSources reports sources whose newest ingest dropped URLs to
// --max. Entries must be newest first.
func collectCappedSources(entries []logEntry) []statusCappedSource {
	seen := map[string]bool{}
	var capped []statusCappedSource
	for _, e := range entries {
		for _, s := range e.Sources {
			if seen[s] {
				continue
			}
			seen[s] = true
			if e.CapHit || e.Capped > 0 {
				capped = append(capped, statusCappedSource{Name: s, Capped: e.Capped, PulledAt: e.StartedAt})
			}
		}
	}
	return capped
}
