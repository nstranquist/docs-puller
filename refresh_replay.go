package main

import (
	"fmt"
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

// latestReplayableIngest returns the newest ingest entry that touched source
// and can be replayed. entries must be newest first (as readIngestLog returns).
func latestReplayableIngest(entries []logEntry, source string) (logEntry, bool) {
	for _, e := range entries {
		if !replayableModes[e.Mode] || len(e.Args) == 0 || e.Args[0] != "pull" {
			continue
		}
		for _, s := range e.Sources {
			if s == source {
				return e, true
			}
		}
	}
	return logEntry{}, false
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
		die(fmt.Errorf("refresh: no replayable pull recorded for source %q in %s/%s; pull it once with `docs-puller pull ...`", source, out, ingestLogFile))
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
