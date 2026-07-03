package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Local + GitHub-repo ingestion modes.
//
// Both flow through walkAndIngest: walk a source dir for .md/.mdx/.mdoc, copy each
// file to ~/code/docs/<source>/<rel-with-md-ext>/, and emit a manifest entry
// with a stable origin URL (github blob URL or file:// path).
//
// `--github-repo` clones into ~/code/docs/.cache/<source>-src/ (sparse when a
// --subdir is given, full shallow otherwise) and refreshes via `git pull`. A
// repo can be re-pulled idempotently — the cache is updated in place.
//
// `--local` skips the clone and walks a user-provided directory directly.

// localFileSizeLimit is the upper bound for individual files to ingest. Docs
// repos occasionally check in giant generated blobs (test fixtures, schema
// dumps); those have no business in a search corpus.
const localFileSizeLimit = 1 << 20 // 1 MB
const localBatchMaxParallelSources = 8

// skipDirs are walked-around without descending. Keeps the corpus clean of
// dependencies, vendored sources, and build artifacts.
var skipDirs = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"vendor":       {},
	"target":       {},
	"dist":         {},
	"build":        {},
	".next":        {},
	".turbo":       {},
	".cache":       {},
	"__pycache__":  {},
}

// githubRepoRE validates the owner/repo shape we accept. GitHub's own rules:
// owner can contain alphanumerics + hyphens; repo can contain alphanumerics,
// dots, hyphens, underscores. Defensive check beyond shell quoting.
var githubRepoRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9._-]+$`)

type localPullOpts struct {
	pullOpts
	name     string   // override for the source dir name under <out>
	subdir   string   // walk only this subpath
	ref      string   // git ref (only meaningful for --github-repo)
	excludes []string // glob patterns matched against rel path; repeatable
}

type localBatchSource struct {
	name     string
	walkRoot string
}

type localIngestStats struct {
	walked    int
	copied    int
	skipped   int
	excluded  int
	written   int
	unchanged int
}

type localIngestDoc struct {
	result  result
	body    []byte
	absOut  string
	title   string
	changed bool
}

type localBatchCollectResult struct {
	source string
	docs   []localIngestDoc
	stats  localIngestStats
	err    error
}

type localBatchSummary struct {
	Mode             string   `json:"mode"`
	SourceCnt        int      `json:"source_count"`
	Sources          []string `json:"sources"`
	Walked           int      `json:"walked"`
	Pulled           int      `json:"pulled"`
	Skipped          int      `json:"skipped"`
	Excluded         int      `json:"excluded"`
	WriteCount       int      `json:"write_count"`
	UnchangedCount   int      `json:"unchanged_write_count"`
	ChangedPathCount int      `json:"changed_path_count"`
	ElapsedMs        float64  `json:"elapsed_ms"`
	CopyMs           float64  `json:"copy_ms"`
	LockMs           float64  `json:"lock_ms"`
	ManifestMs       float64  `json:"manifest_ms"`
	IndexRegenMs     float64  `json:"index_regen_ms"`
	FTSUpdateMs      float64  `json:"fts_update_ms"`
	FTSDocCount      int      `json:"fts_doc_count"`
	FTSSizeBytes     int64    `json:"fts_size_bytes,omitempty"`
	LogMs            float64  `json:"log_ms"`
	FTSUpdated       bool     `json:"fts_updated"`
}

// excludeMatch reports whether rel (a path relative to walkRoot, separator-
// normalized to "/") should be skipped given the user-supplied patterns.
// Pattern semantics:
//   - "name"        — exact match against any path segment (skip dirs/files named "name")
//   - "foo/bar"     — filepath.Match against full rel path
//   - "foo/**"      — recursive: rel == "foo" or rel starts with "foo/"
//   - "*.ext"       — filepath.Match against the basename
//
// Designed to be obvious to reason about; avoids pulling in a full glob lib.
func excludeMatch(rel string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	rel = filepath.ToSlash(rel)
	base := filepath.Base(rel)
	segs := strings.Split(rel, "/")
	for _, pat := range patterns {
		pat = filepath.ToSlash(pat)
		// Recursive prefix match.
		if strings.HasSuffix(pat, "/**") {
			prefix := strings.TrimSuffix(pat, "/**")
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				return true
			}
			continue
		}
		// No slash, no glob — treat as exact segment match.
		if !strings.ContainsAny(pat, "/*?[") {
			for _, s := range segs {
				if s == pat {
					return true
				}
			}
			continue
		}
		// Glob without slashes — match against basename.
		if !strings.Contains(pat, "/") {
			if ok, _ := filepath.Match(pat, base); ok {
				return true
			}
			continue
		}
		// Glob with slashes — match against full rel.
		if ok, _ := filepath.Match(pat, rel); ok {
			return true
		}
	}
	return false
}

func cmdPullLocal(localPath string, lo localPullOpts, cmdArgs []string) {
	abs, err := filepath.Abs(localPath)
	if err != nil {
		die(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		die(fmt.Errorf("--local %s: %w", localPath, err))
	}
	if !info.IsDir() {
		die(fmt.Errorf("--local %s: not a directory", localPath))
	}

	source := lo.name
	if source == "" {
		source = filepath.Base(abs)
	}
	source = sanitizeSourceName(source)

	walkRoot := abs
	if lo.subdir != "" {
		walkRoot = filepath.Join(abs, lo.subdir)
		if _, err := os.Stat(walkRoot); err != nil {
			die(fmt.Errorf("--subdir %s: %w", lo.subdir, err))
		}
	}

	urlFor := func(rel string) string {
		full := filepath.Join(walkRoot, rel)
		return "file://" + full
	}
	ingest(walkRoot, source, lo.pullOpts, urlFor, "local", cmdArgs, lo.excludes)
}

func cmdPullLocalBatch(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("pull-local-batch", flag.ExitOnError)
	var sources stringSliceFlag
	var excludes stringSliceFlag
	jsonOut := fs.Bool("json", false, "emit JSON summary")
	fs.Var(&sources, "source", "local source in NAME=PATH form (repeatable)")
	fs.Var(&excludes, "exclude", "skip paths matching glob for all sources (repeatable)")
	bindOpts(fs, &o)
	fs.Parse(args)

	if len(sources) == 0 {
		die(fmt.Errorf("pull-local-batch: pass at least one --source NAME=PATH"))
	}

	batchSources := make([]localBatchSource, 0, len(sources))
	seenSources := map[string]struct{}{}
	for _, spec := range sources {
		source, path, ok := strings.Cut(spec, "=")
		if !ok || strings.TrimSpace(source) == "" || strings.TrimSpace(path) == "" {
			die(fmt.Errorf("pull-local-batch: --source must be NAME=PATH, got %q", spec))
		}
		sourceName := sanitizeSourceName(source)
		if _, ok := seenSources[sourceName]; ok {
			die(fmt.Errorf("pull-local-batch: duplicate source name %q", sourceName))
		}
		seenSources[sourceName] = struct{}{}
		abs, err := filepath.Abs(path)
		if err != nil {
			die(err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			die(fmt.Errorf("--source %s: %w", spec, err))
		}
		if !info.IsDir() {
			die(fmt.Errorf("--source %s: not a directory", spec))
		}
		batchSources = append(batchSources, localBatchSource{
			name:     sourceName,
			walkRoot: abs,
		})
	}

	cmdArgs := append([]string{"pull-local-batch"}, args...)
	summary, err := ingestLocalBatch(batchSources, o, cmdArgs, excludes)
	if err != nil {
		die(err)
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(summary); err != nil {
			die(err)
		}
		return
	}
	if summary.Excluded > 0 {
		fmt.Printf("ingested %d docs from %d sources (%d walked, %d skipped, %d excluded, mode=%s)\n",
			summary.Pulled, summary.SourceCnt, summary.Walked, summary.Skipped, summary.Excluded, summary.Mode)
	} else {
		fmt.Printf("ingested %d docs from %d sources (%d walked, %d skipped, mode=%s)\n",
			summary.Pulled, summary.SourceCnt, summary.Walked, summary.Skipped, summary.Mode)
	}
}

func cmdPullGithubRepo(spec string, lo localPullOpts, cmdArgs []string) {
	if !githubRepoRE.MatchString(spec) {
		die(fmt.Errorf("--github-repo %q: expected <owner>/<repo>", spec))
	}
	parts := strings.SplitN(spec, "/", 2)
	owner, repo := parts[0], parts[1]

	source := lo.name
	if source == "" {
		source = repo
	}
	source = sanitizeSourceName(source)

	cacheDir := filepath.Join(lo.sourceCache, source+"-src")
	resolvedRef, err := ensureGithubCheckout(owner, repo, lo.ref, lo.subdir, cacheDir)
	if err != nil {
		die(err)
	}

	walkRoot := cacheDir
	if lo.subdir != "" {
		walkRoot = filepath.Join(cacheDir, lo.subdir)
		if _, err := os.Stat(walkRoot); err != nil {
			die(fmt.Errorf("--subdir %s not found in repo: %w", lo.subdir, err))
		}
	}

	// Stable origin URL: github.com/<owner>/<repo>/blob/<ref>/<subdir>/<rel>.
	// Using the resolved branch keeps URLs tracking-friendly — a SHA would
	// be more stable but breaks when the upstream branch moves.
	urlBase := fmt.Sprintf("https://github.com/%s/%s/blob/%s", owner, repo, resolvedRef)
	urlFor := func(rel string) string {
		// Path components on disk are filepath-separated; URLs need forward slashes.
		relURL := filepath.ToSlash(rel)
		if lo.subdir != "" {
			return urlBase + "/" + filepath.ToSlash(lo.subdir) + "/" + relURL
		}
		return urlBase + "/" + relURL
	}
	ingest(walkRoot, source, lo.pullOpts, urlFor, "github-repo", cmdArgs, lo.excludes)
}

// ensureGithubCheckout creates or refreshes a sparse checkout under cacheDir.
// First call clones; subsequent calls `git fetch + checkout` to update. When
// subdir is given, sparse-checkout limits the working tree to that subtree.
//
// If userRef is empty, the upstream's default branch (HEAD) is auto-detected
// via `git ls-remote --symref` so repos with non-conventional default branches
// (e.g. cli/cli uses `trunk`) work without the caller having to know.
//
// Returns the resolved ref name (used by callers to build stable origin URLs).
func ensureGithubCheckout(owner, repo, userRef, subdir, cacheDir string) (string, error) {
	httpsURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	sshURL := fmt.Sprintf("git@github.com:%s/%s.git", owner, repo)

	if _, err := os.Stat(cacheDir); err == nil {
		// Existing checkout — refresh against whatever ref it was created with
		// (or the userRef if explicit). We assume the existing checkout's
		// branch name is correct; this is the case as long as the user hasn't
		// rm'd the upstream branch.
		ref := userRef
		if ref == "" {
			// Read the local branch name to refresh against the same one.
			out, _ := exec.Command("git", "-C", cacheDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
			ref = strings.TrimSpace(string(out))
			if ref == "" || ref == "HEAD" {
				ref = "main"
			}
		}
		fmt.Fprintf(os.Stderr, "→ refreshing %s/%s @ %s\n", owner, repo, ref)
		c := exec.Command("git", "-C", cacheDir, "fetch", "--depth=1", "origin", ref)
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
		if err := c.Run(); err != nil {
			return "", fmt.Errorf("git fetch: %w", err)
		}
		c = exec.Command("git", "-C", cacheDir, "checkout", "-B", ref, "FETCH_HEAD")
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
		if err := c.Run(); err != nil {
			return "", fmt.Errorf("git checkout: %w", err)
		}
		return ref, nil
	}

	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		return "", err
	}

	// Resolve the ref to clone. Priority:
	//  1. explicit --ref → use as-is.
	//  2. ls-remote symref discovery → tells us the actual default branch.
	//  3. fall back to "main" (and let the cascade try "master" if that fails).
	ref := userRef
	if ref == "" {
		if discovered := discoverDefaultBranch(httpsURL, sshURL); discovered != "" {
			ref = discovered
			fmt.Fprintf(os.Stderr, "→ default branch for %s/%s is %s\n", owner, repo, ref)
		} else {
			ref = "main"
		}
	}

	// Try (URL × ref) combinations until one succeeds. HTTPS first because it
	// works for public repos without any local config; SSH falls in for private
	// repos where the dev has an SSH key on file with GitHub. The `master`
	// fallback only kicks in when we defaulted to `main` and discovery failed.
	type attempt struct{ url, ref string }
	attempts := []attempt{{httpsURL, ref}, {sshURL, ref}}
	if userRef == "" && ref == "main" {
		attempts = append(attempts, attempt{httpsURL, "master"}, attempt{sshURL, "master"})
	}

	var lastErr error
	resolvedRef := ref
	for _, a := range attempts {
		fmt.Fprintf(os.Stderr, "→ cloning %s @ %s into %s\n", a.url, a.ref, cacheDir)
		cloneArgs := []string{"clone", "--depth=1", "--branch", a.ref}
		if subdir != "" {
			cloneArgs = append(cloneArgs, "--filter=blob:none", "--sparse")
		}
		cloneArgs = append(cloneArgs, a.url, cacheDir)
		c := exec.Command("git", cloneArgs...)
		// Suppress interactive credential prompts so HTTPS-on-private-repo fails
		// fast instead of hanging waiting for stdin.
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
		if err := c.Run(); err == nil {
			resolvedRef = a.ref
			lastErr = nil
			break
		} else {
			lastErr = err
			// Each failed attempt may have left a partial cacheDir behind.
			os.RemoveAll(cacheDir)
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("git clone: tried HTTPS and SSH for %s/%s: %w", owner, repo, lastErr)
	}

	if subdir != "" {
		c := exec.Command("git", "-C", cacheDir, "sparse-checkout", "set", subdir)
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
		if err := c.Run(); err != nil {
			return "", fmt.Errorf("git sparse-checkout: %w", err)
		}
	}
	return resolvedRef, nil
}

// discoverDefaultBranch asks git for the upstream HEAD symref. Tries HTTPS
// then SSH (mirroring the clone fallback). Returns "" on failure so the
// caller can fall back to a hardcoded default.
func discoverDefaultBranch(httpsURL, sshURL string) string {
	for _, u := range []string{httpsURL, sshURL} {
		c := exec.Command("git", "ls-remote", "--symref", u, "HEAD")
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := c.Output()
		if err != nil {
			continue
		}
		// First line looks like: "ref: refs/heads/trunk\tHEAD"
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "ref: refs/heads/") {
				continue
			}
			rest := strings.TrimPrefix(line, "ref: refs/heads/")
			if i := strings.IndexAny(rest, "\t "); i > 0 {
				return rest[:i]
			}
		}
	}
	return ""
}

// ingest walks walkRoot for .md/.mdx/.mdoc files, writes them to <out>/<source>/,
// builds manifest entries, regenerates _INDEX.md, and rebuilds the FTS5
// index. Mirrors the post-processing pipeline of run() so search picks up
// the new docs without a separate command.
func ingest(walkRoot, source string, o pullOpts, urlFor func(rel string) string, mode string, cmdArgs []string, excludes []string) {
	start := time.Now()
	now := start.UTC().Format(time.RFC3339)
	preExistingSources, err := listSources(o.out)
	if err != nil {
		die(err)
	}
	docs, stats, err := collectLocalIngestResults(walkRoot, source, o.out, urlFor, mode, now, excludes)
	if err != nil {
		die(err)
	}
	results := localIngestResults(docs)

	// Same critical-section serialization as run() — if the user kicks off
	// multiple --local/--github-repo pulls in parallel, we wait our turn
	// rather than racing on the FTS5 index.
	if err := withWriteLock(o.out, func() error {
		if err := writeManifests(o.out, results, false, nil); err != nil {
			return err
		}
		if len(preExistingSources) == 0 {
			if err := regenerateIndexFromMemory(o.out, localIndexMemoryDocs(docs)); err != nil {
				return err
			}
		} else {
			if err := regenerateIndex(o.out, []string{source}); err != nil {
				return err
			}
		}
		var changedPaths []string
		for _, doc := range docs {
			if doc.changed && doc.result.Path != "" {
				changedPaths = append(changedPaths, doc.result.Path)
			}
		}
		if idx, err := openFTSIndex(o.out); err == nil {
			coversCorpus := len(preExistingSources) == 0
			if rerr := idx.updateFTSFromMemory(o.out, changedPaths, localFTSMemoryDocs(docs, coversCorpus), coversCorpus); rerr != nil {
				fmt.Fprintf(os.Stderr, "fts5: update failed: %v\n", rerr)
			}
			idx.close()
		}
		entry := logEntry{
			StartedAt:  now,
			FinishedAt: time.Now().UTC().Format(time.RFC3339),
			ElapsedMs:  time.Since(start).Milliseconds(),
			Mode:       mode,
			Args:       cmdArgs,
			Sources:    []string{source},
			URLs:       stats.walked,
			Pulled:     stats.copied,
			Skipped:    stats.skipped,
		}
		if err := appendIngestLog(o.out, entry); err != nil {
			fmt.Fprintf(os.Stderr, "ingest-log: append failed: %v\n", err)
		}
		return nil
	}); err != nil {
		die(err)
	}

	if stats.excluded > 0 {
		fmt.Printf("ingested %d docs into %s/ (%d walked, %d skipped, %d excluded, mode=%s)\n",
			stats.copied, source, stats.walked, stats.skipped, stats.excluded, mode)
	} else {
		fmt.Printf("ingested %d docs into %s/ (%d walked, %d skipped, mode=%s)\n",
			stats.copied, source, stats.walked, stats.skipped, mode)
	}
}

func collectLocalIngestResults(walkRoot, source, outRoot string, urlFor func(rel string) string, mode, now string, excludes []string) ([]localIngestDoc, localIngestStats, error) {
	srcOut := filepath.Join(outRoot, source)
	if err := os.MkdirAll(srcOut, 0o755); err != nil {
		return nil, localIngestStats{}, err
	}
	priorByURL := readManifestEntriesNoMigrate(srcOut)

	var (
		docs        []localIngestDoc
		stats       localIngestStats
		createdDirs = map[string]struct{}{srcOut: {}}
	)
	err := filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == walkRoot {
				return nil
			}
			if _, skip := skipDirs[d.Name()]; skip {
				return fs.SkipDir
			}
			// Don't descend into hidden dirs except the root.
			if strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			rel, _ := filepath.Rel(walkRoot, p)
			if excludeMatch(rel, excludes) {
				return fs.SkipDir
			}
			return nil
		}
		stats.walked++
		name := d.Name()
		if !isMarkdownDocName(name) {
			return nil
		}
		rel, err := filepath.Rel(walkRoot, p)
		if err != nil {
			return err
		}
		if excludeMatch(rel, excludes) {
			stats.excluded++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			stats.skipped++
			return nil
		}
		if info.Size() > localFileSizeLimit {
			fmt.Fprintf(os.Stderr, "  SKIP %s — file > %d bytes\n", p, localFileSizeLimit)
			stats.skipped++
			return nil
		}

		// Normalize markdown-family files on the way out so search treats them
		// uniformly and origin URLs resolved by the agent flow always end .md.
		outRel := markdownOutputRel(rel)
		outPath := filepath.Join(srcOut, outRel)
		outDir := filepath.Dir(outPath)
		if _, ok := createdDirs[outDir]; !ok {
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return err
			}
			createdDirs[outDir] = struct{}{}
		}

		data, err := os.ReadFile(p)
		if err != nil {
			stats.skipped++
			return nil
		}
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		url := urlFor(rel)
		changed := true
		if prior, ok := priorByURL[url]; ok && prior.Path == filepath.Join(source, outRel) && prior.SHA256 == sha {
			if info, err := os.Stat(outPath); err == nil && info.Size() == int64(len(data)) {
				changed = false
			}
		}
		if changed {
			if err := os.WriteFile(outPath, data, 0o644); err != nil {
				return err
			}
			stats.written++
		} else {
			stats.unchanged++
		}
		title := extractTitleFromBytes(outPath, data)
		r := result{
			URL:       url,
			Source:    source,
			Path:      filepath.Join(source, outRel),
			Mode:      mode,
			SHA256:    sha,
			FetchedAt: now,
		}
		docs = append(docs, localIngestDoc{
			result:  r,
			body:    data,
			absOut:  outPath,
			title:   title,
			changed: changed,
		})
		stats.copied++
		return nil
	})
	if err != nil {
		return nil, stats, err
	}

	// Sort results so manifest ordering is deterministic across runs.
	sort.Slice(docs, func(i, j int) bool { return docs[i].result.Path < docs[j].result.Path })
	return docs, stats, nil
}

func isMarkdownDocName(name string) bool {
	return strings.HasSuffix(name, ".md") ||
		strings.HasSuffix(name, ".mdx") ||
		strings.HasSuffix(name, ".mdoc")
}

func markdownOutputRel(rel string) string {
	switch {
	case strings.HasSuffix(rel, ".mdx"):
		return strings.TrimSuffix(rel, ".mdx") + ".md"
	case strings.HasSuffix(rel, ".mdoc"):
		return strings.TrimSuffix(rel, ".mdoc") + ".md"
	default:
		return rel
	}
}

func readManifestEntriesNoMigrate(srcDir string) map[string]result {
	data, err := os.ReadFile(filepath.Join(srcDir, manifestFile))
	if err != nil {
		return map[string]result{}
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil || m.Entries == nil {
		return map[string]result{}
	}
	return m.Entries
}

func localIngestResults(docs []localIngestDoc) []result {
	results := make([]result, 0, len(docs))
	for _, doc := range docs {
		results = append(results, doc.result)
	}
	return results
}

func localFTSMemoryDocs(docs []localIngestDoc, coversCorpus bool) []ftsMemoryDoc {
	ftsDocs := make([]ftsMemoryDoc, 0, len(docs))
	for _, doc := range docs {
		ftsDocs = append(ftsDocs, ftsMemoryDoc{
			Path:    doc.result.Path,
			AbsPath: doc.absOut,
			Body:    doc.body,
			URL:     doc.result.URL,
			Warning: doc.result.Warning,
			Title:   doc.title,
		})
	}
	if coversCorpus {
		applyFTSMemoryHubFlags(ftsDocs)
	}
	return ftsDocs
}

func localIndexMemoryDocs(docs []localIngestDoc) []memoryIndexDoc {
	indexDocs := make([]memoryIndexDoc, 0, len(docs))
	for _, doc := range docs {
		source, rel, ok := splitFTSPath(doc.result.Path)
		if !ok {
			continue
		}
		indexDocs = append(indexDocs, memoryIndexDoc{
			source:  source,
			rel:     rel,
			absPath: doc.absOut,
			body:    doc.body,
			title:   doc.title,
			url:     doc.result.URL,
			fetched: doc.result.FetchedAt,
		})
	}
	return indexDocs
}

func collectLocalBatchSources(sources []localBatchSource, outRoot, now string, excludes []string) ([]localBatchCollectResult, error) {
	results := make([]localBatchCollectResult, len(sources))
	if len(sources) == 0 {
		return results, nil
	}
	workers := localBatchSourceWorkers(len(sources))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				src := sources[i]
				walkRoot := src.walkRoot
				urlFor := func(rel string) string {
					full := filepath.Join(walkRoot, rel)
					return "file://" + full
				}
				docs, stats, err := collectLocalIngestResults(walkRoot, src.name, outRoot, urlFor, "local", now, excludes)
				results[i] = localBatchCollectResult{
					source: src.name,
					docs:   docs,
					stats:  stats,
					err:    err,
				}
			}
		}()
	}
	for i := range sources {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for _, result := range results {
		if result.err != nil {
			return nil, result.err
		}
	}
	return results, nil
}

func localBatchSourceWorkers(sourceCount int) int {
	if sourceCount <= 1 {
		return sourceCount
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > localBatchMaxParallelSources {
		workers = localBatchMaxParallelSources
	}
	if workers > sourceCount {
		workers = sourceCount
	}
	if workers < 1 {
		return 1
	}
	return workers
}

func ingestLocalBatch(sources []localBatchSource, o pullOpts, cmdArgs []string, excludes []string) (localBatchSummary, error) {
	start := time.Now()
	now := start.UTC().Format(time.RFC3339)
	var summary localBatchSummary
	preExistingSources, err := listSources(o.out)
	if err != nil {
		return localBatchSummary{}, err
	}

	var (
		allDocs    []localIngestDoc
		allResults []result
		allSources []string
		stats      localIngestStats
	)
	copyStart := time.Now()
	collectResults, err := collectLocalBatchSources(sources, o.out, now, excludes)
	if err != nil {
		return localBatchSummary{}, err
	}
	totalDocs := 0
	for _, collected := range collectResults {
		totalDocs += len(collected.docs)
	}
	allDocs = make([]localIngestDoc, 0, totalDocs)
	allResults = make([]result, 0, totalDocs)
	allSources = make([]string, 0, len(collectResults))
	for _, collected := range collectResults {
		results := localIngestResults(collected.docs)
		allDocs = append(allDocs, collected.docs...)
		allResults = append(allResults, results...)
		allSources = append(allSources, collected.source)
		stats.walked += collected.stats.walked
		stats.copied += collected.stats.copied
		stats.skipped += collected.stats.skipped
		stats.excluded += collected.stats.excluded
		stats.written += collected.stats.written
		stats.unchanged += collected.stats.unchanged
	}
	summary.CopyMs = durationMillis(time.Since(copyStart))

	ftsUpdated := false
	ftsDocCount := 0
	ftsSizeBytes := int64(0)
	lockStart := time.Now()
	if err := withWriteLock(o.out, func() error {
		manifestStart := time.Now()
		if err := writeManifests(o.out, allResults, false, nil); err != nil {
			return err
		}
		summary.ManifestMs = durationMillis(time.Since(manifestStart))
		indexStart := time.Now()
		if len(preExistingSources) == 0 {
			if err := regenerateIndexFromMemory(o.out, localIndexMemoryDocs(allDocs)); err != nil {
				return err
			}
		} else {
			if err := regenerateIndex(o.out, allSources); err != nil {
				return err
			}
		}
		summary.IndexRegenMs = durationMillis(time.Since(indexStart))
		var changedPaths []string
		for _, doc := range allDocs {
			if doc.changed && doc.result.Path != "" {
				changedPaths = append(changedPaths, doc.result.Path)
			}
		}
		summary.ChangedPathCount = len(changedPaths)
		ftsStart := time.Now()
		if idx, err := openFTSIndex(o.out); err == nil {
			coversCorpus := len(preExistingSources) == 0
			if rerr := idx.updateFTSFromMemory(o.out, changedPaths, localFTSMemoryDocs(allDocs, coversCorpus), coversCorpus); rerr != nil {
				fmt.Fprintf(os.Stderr, "fts5: update failed: %v\n", rerr)
			} else {
				ftsUpdated = true
				if n, err := idx.totalDocs(); err == nil {
					ftsDocCount = n
				}
			}
			idx.close()
		}
		if st, err := os.Stat(ftsDBPath(o.out)); err == nil {
			ftsSizeBytes = st.Size()
		}
		summary.FTSUpdateMs = durationMillis(time.Since(ftsStart))
		entry := logEntry{
			StartedAt:  now,
			FinishedAt: time.Now().UTC().Format(time.RFC3339),
			ElapsedMs:  time.Since(start).Milliseconds(),
			Mode:       "local-batch",
			Args:       cmdArgs,
			Sources:    allSources,
			URLs:       stats.walked,
			Pulled:     stats.copied,
			Skipped:    stats.skipped,
		}
		logStart := time.Now()
		if err := appendIngestLog(o.out, entry); err != nil {
			fmt.Fprintf(os.Stderr, "ingest-log: append failed: %v\n", err)
		}
		summary.LogMs = durationMillis(time.Since(logStart))
		return nil
	}); err != nil {
		return localBatchSummary{}, err
	}
	summary.LockMs = durationMillis(time.Since(lockStart))
	return localBatchSummary{
		Mode:             "local-batch",
		SourceCnt:        len(allSources),
		Sources:          allSources,
		Walked:           stats.walked,
		Pulled:           stats.copied,
		Skipped:          stats.skipped,
		Excluded:         stats.excluded,
		WriteCount:       stats.written,
		UnchangedCount:   stats.unchanged,
		ElapsedMs:        durationMillis(time.Since(start)),
		CopyMs:           summary.CopyMs,
		LockMs:           summary.LockMs,
		ManifestMs:       summary.ManifestMs,
		IndexRegenMs:     summary.IndexRegenMs,
		FTSUpdateMs:      summary.FTSUpdateMs,
		FTSDocCount:      ftsDocCount,
		FTSSizeBytes:     ftsSizeBytes,
		LogMs:            summary.LogMs,
		ChangedPathCount: summary.ChangedPathCount,
		FTSUpdated:       ftsUpdated,
	}, nil
}

func durationMillis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// sanitizeSourceName trims a name down to a safe directory name. Lowercases,
// replaces unsafe chars with `-`, collapses consecutive `-`. Defensive: any
// upstream caller has already given us a sane string, but cheap to enforce.
func sanitizeSourceName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := false
	for _, r := range name {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if out == "" {
		out = "unnamed"
	}
	return out
}
