package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nstranquist/docs-puller/searchruntime"
)

const (
	titleScannerInitialBuffer = 4 * 1024
	titleScannerMaxBuffer     = 1024 * 1024
)

// regenerateIndex regenerates per-source `_INDEX.md` files plus the top-level
// summary `_INDEX.md`. When `touched` is nil, every source is fully rebuilt
// (this is the `reindex` semantics). Otherwise, only the named sources walk
// + open + extractTitle their files; untouched sources fall through a cheap
// count + latest-pull lookup that avoids opening any markdown bodies.
//
// The expensive op is `extractTitle` (one file open per doc), so making the
// per-pull path skip untouched sources drops the work from O(corpus) to
// O(touched-sources) for incremental pulls.
//
// Underscore prefix in `_INDEX.md` is required: macOS APFS is case-insensitive
// by default, so a fetched `<source>/index.md` (e.g. clickhouse.com/docs)
// would collide with `INDEX.md`. Files starting with `_` are filtered out of
// `listSources` already, keeping the index files invisible to source
// enumeration.
//
// Filesystem-driven so partial pulls (pull-url) keep prior docs visible.
func regenerateIndex(out string, touched []string) error {
	sources, err := listSources(out)
	if err != nil {
		return err
	}

	touchedSet := map[string]bool{}
	if touched == nil {
		for _, s := range sources {
			touchedSet[s] = true
		}
	} else {
		for _, s := range touched {
			touchedSet[s] = true
		}
	}

	summaries := make([]sourceSummary, 0, len(sources))
	for _, src := range sources {
		srcDir := filepath.Join(out, src)
		if touchedSet[src] {
			entries, latest, err := collectEntries(srcDir, src)
			if err != nil {
				return searchruntime.IndexCollectSourceError(src, err)
			}
			if len(entries) == 0 {
				continue
			}
			if err := writeSourceIndex(srcDir, src, entries, latest); err != nil {
				return searchruntime.IndexWriteSourceError(src, err)
			}
			summaries = append(summaries, sourceSummary{src, len(entries), latest})
			continue
		}
		count, latest := cheapSourceSummary(srcDir)
		if count == 0 {
			continue
		}
		summaries = append(summaries, sourceSummary{src, count, latest})
	}

	return writeTopIndex(out, summaries)
}

type memoryIndexDoc struct {
	source  string
	rel     string
	absPath string
	body    []byte
	title   string
	url     string
	fetched string
}

// regenerateIndexFromMemory is the fresh-local-corpus fast path for
// pull-local-batch. Call it only when docs is known to represent the whole
// output corpus; stale copied files intentionally stay on regenerateIndex.
func regenerateIndexFromMemory(out string, docs []memoryIndexDoc) error {
	bySource := map[string][]memoryIndexDoc{}
	for _, doc := range docs {
		if doc.source == "" || doc.rel == "" {
			continue
		}
		bySource[doc.source] = append(bySource[doc.source], doc)
	}
	sources := make([]string, 0, len(bySource))
	for source := range bySource {
		sources = append(sources, source)
	}
	sort.Strings(sources)

	summaries := make([]sourceSummary, 0, len(sources))
	for _, source := range sources {
		srcDir := filepath.Join(out, source)
		entries, latest := collectMemoryEntries(srcDir, bySource[source])
		if len(entries) == 0 {
			continue
		}
		if err := writeSourceIndex(srcDir, source, entries, latest); err != nil {
			return searchruntime.IndexWriteSourceError(source, err)
		}
		summaries = append(summaries, sourceSummary{source, len(entries), latest})
	}
	return writeTopIndex(out, summaries)
}

func collectMemoryEntries(srcDir string, docs []memoryIndexDoc) ([]indexEntry, string) {
	tcache := loadTitleCache(srcDir)
	entries := make([]indexEntry, 0, len(docs))
	latest := ""
	visited := map[string]bool{}
	for _, doc := range docs {
		rel := filepath.ToSlash(doc.rel)
		if rel == "" || strings.HasSuffix(rel, "/_INDEX.md") || rel == "_INDEX.md" {
			continue
		}
		title := doc.title
		if title == "" {
			title = extractTitleFromBytes(doc.absPath, doc.body)
		}
		if title == "" {
			title = strings.TrimSuffix(rel, ".md")
		}
		if info, err := os.Stat(doc.absPath); err == nil {
			mtimeNs := info.ModTime().UnixNano()
			if entry, ok := tcache.Titles[rel]; !ok || entry.Title != title || entry.MtimeNs != mtimeNs {
				tcache.Titles[rel] = titleEntry{Title: title, MtimeNs: mtimeNs}
				tcache.dirty = true
			}
		}
		visited[rel] = true
		if doc.fetched > latest {
			latest = doc.fetched
		}
		entries = append(entries, indexEntry{
			path: rel, title: title, url: doc.url, fetched: doc.fetched,
		})
	}
	tcache.pruneUnvisited(visited)
	if serr := saveTitleCache(srcDir, tcache); serr != nil {
		fmt.Fprintf(os.Stderr, "title-cache: save failed for %s: %v\n", filepath.Base(srcDir), serr)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, latest
}

// cheapSourceSummary returns (docCount, latestPull) for an untouched source
// without reading file bodies OR parsing the manifest. WalkDir for the count;
// manifest.json's mtime IS the latest pull time (writeManifestAtomic rewrites
// it on every pull, atomic temp-file + rename). Skipping the JSON parse drops
// the per-source cost from ~10-100ms (slack/microsoft-learn manifests are
// 1.5MB) to a single Stat.
func cheapSourceSummary(srcDir string) (int, string) {
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
	latest := ""
	if info, err := os.Stat(filepath.Join(srcDir, manifestFile)); err == nil {
		latest = info.ModTime().UTC().Format(time.RFC3339)
	}
	return count, latest
}

func listSources(out string) ([]string, error) {
	ents, err := os.ReadDir(out)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var srcs []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		srcs = append(srcs, name)
	}
	sort.Strings(srcs)
	return srcs, nil
}

type indexEntry struct {
	path    string // rel to source dir, e.g. "guides/database/drizzle.md"
	title   string
	url     string
	fetched string
}

func collectEntries(srcDir, srcName string) ([]indexEntry, string, error) {
	urlByPath, fetchedByPath := loadManifestMaps(srcDir, srcName)
	tcache := loadTitleCache(srcDir)

	var (
		entries []indexEntry
		latest  string
		visited = map[string]bool{}
	)
	err := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".md") || name == "_INDEX.md" {
			return nil
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		visited[rel] = true
		title := tcache.titleFor(srcDir, rel, info)
		if title == "" {
			title = strings.TrimSuffix(rel, ".md")
		}
		fetched := fetchedByPath[rel]
		if fetched > latest {
			latest = fetched
		}
		entries = append(entries, indexEntry{
			path: rel, title: title,
			url: urlByPath[rel], fetched: fetched,
		})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	tcache.pruneUnvisited(visited)
	if serr := saveTitleCache(srcDir, tcache); serr != nil {
		fmt.Fprintf(os.Stderr, "title-cache: save failed for %s: %v\n", srcName, serr)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, latest, nil
}

// extractTitle pulls a title from MDX/Markdown frontmatter, the first H1,
// or — when those resolve to a generic site-shell label like "CLI Reference"
// — the first H2 or the filename. The fallback chain is what differentiates
// per-page entries on docs sites that share an H1 across many files (Supabase
// CLI ref, MkDocs Material, Mintlify "Reference" sections).
//
// Order of preference:
//  1. Frontmatter `title:` (if non-generic)
//  2. First H1 in body (if non-generic)
//  3. First H2 in body (if non-generic) — disambiguates generic-H1 sites
//  4. Filename (de-kebab'd) — last resort, but better than a generic shell
//  5. Generic frontmatter / H1 / H2 — only when nothing else worked
//
// Returns "" only when the file can't be opened.
func extractTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, titleScannerInitialBuffer), titleScannerMaxBuffer)
	return extractTitleFromScanner(path, scanner)
}

func extractTitleFromBytes(path string, data []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, titleScannerInitialBuffer), titleScannerMaxBuffer)
	return extractTitleFromScanner(path, scanner)
}

func extractTitleFromScanner(path string, scanner *bufio.Scanner) string {
	var (
		fmTitle, h1, h2      string
		inFrontmatter, fmEnd bool
	)
	for scanner.Scan() {
		trimmed := strings.TrimSpace(scanner.Text())

		if !fmEnd && trimmed == "---" {
			if !inFrontmatter {
				inFrontmatter = true
				continue
			}
			fmEnd = true
			inFrontmatter = false
			continue
		}
		if inFrontmatter {
			if fmTitle == "" && strings.HasPrefix(trimmed, "title:") {
				fmTitle = scrubTitle(strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "title:")), `'"`))
			}
			continue
		}
		if h1 == "" && strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## ") {
			h1 = scrubTitle(strings.TrimSpace(strings.TrimPrefix(trimmed, "# ")))
			continue
		}
		if h2 == "" && strings.HasPrefix(trimmed, "## ") && !strings.HasPrefix(trimmed, "### ") {
			h2 = scrubTitle(strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")))
		}
		if h1 != "" && h2 != "" {
			break
		}
	}

	if fmTitle != "" && !isGenericTitle(fmTitle) {
		return fmTitle
	}
	if h1 != "" && !isGenericTitle(h1) {
		return h1
	}
	// H2 fallback only kicks in when there IS an H1 but it's generic
	// (Supabase CLI ref shape). When the H1 is absent entirely, the body's
	// first H2 is usually a local section header ("Properties", "Examples")
	// that's *worse* than the filename — so we go straight to filename for
	// the no-H1 case to preserve the pre-fix behavior.
	if h1 != "" && h2 != "" && !isGenericTitle(h2) {
		return h2
	}
	if fn := titleFromFilename(path); fn != "" {
		return fn
	}
	// Last resort: return whatever we have, even if generic, so downstream
	// renderers don't show an empty cell.
	for _, c := range []string{fmTitle, h1, h2} {
		if c != "" {
			return c
		}
	}
	return ""
}

// IMPORTANT: any change to genericTitles, isGenericTitle, scrubTitle,
// extractTitle, or titleFromFilename must be accompanied by bumping
// titleCacheVersion in title_cache.go. The cache is keyed on (path,
// mtime) — without a version bump, unchanged files keep the OLD title
// silently. See the "BUMP THIS" comment on titleCacheVersion.

// genericTitles are site-shell H1/title values that vendor docs reuse across
// many pages — the per-page distinction lives in the URL slug or an H2, not
// the H1. When extractTitle sees one of these as the strongest candidate, it
// falls through to the next-best signal so the index doesn't show a column
// of identical "CLI Reference" rows.
//
// Conservative list. Anything plausibly a real one-off page title
// ("Introduction", "Getting Started", "Quickstart") stays off — we'd rather
// keep a true title than accidentally swap it for a filename.
var genericTitles = map[string]bool{
	"cli reference": true,
	"api reference": true,
	"reference":     true,
	"documentation": true,
	"docs":          true,
	"overview":      true,
	"welcome":       true,
	"index":         true,
	"home":          true,
	// MkDocs / Supabase CLI ref shell sections — these are sidebar artifacts
	// that appear as the first H2 on every page in a giant single-doc
	// reference site, ahead of the actual per-command H2 deeper in the body.
	// Without these, the H2 fallback picks the sidebar header and titles
	// like "supabase-db-dump.md" come out as "Global flags".
	"global flags":      true,
	"usage":             true,
	"flags":             true,
	"examples":          true,
	"parameters":        true,
	"options":           true,
	"configuration":     true,
	"supabase cli":      true,
	"table of contents": true,
}

func isGenericTitle(t string) bool {
	return genericTitles[strings.ToLower(strings.TrimSpace(t))]
}

// titlePreambleRE matches Google DevSite's content-categorization shell
// that html-to-markdown sometimes inlines into the H1 line — e.g.
// "Manifest file format Stay organized with collections Save and
// categorize content based on your preferences." → "Manifest file format".
// Affects developer.chrome.com, developer.android.com, and other Google-
// hosted docs.
var titlePreambleRE = regexp.MustCompile(`\s*Stay organized with collections.*$`)

// scrubTitle strips known title-pollution patterns. Currently handles
// Google DevSite's preamble; extend as new sites surface similar shell
// text bleeding into extracted titles.
func scrubTitle(t string) string {
	t = titlePreambleRE.ReplaceAllString(t, "")
	return strings.TrimSpace(t)
}

// titleFromFilename derives a title from the basename when frontmatter and
// H1 both miss. Strips the .md/.mdx extension and replaces `-` and `_`
// (kebab + snake separators) with spaces. Dots are kept because they're
// meaningful in API method names like `chat.postMessage.md`.
func titleFromFilename(path string) string {
	base := filepath.Base(path)
	for _, ext := range []string{".md", ".mdx"} {
		if strings.HasSuffix(strings.ToLower(base), ext) {
			base = base[:len(base)-len(ext)]
			break
		}
	}
	if base == "" || base == "index" {
		return ""
	}
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.ReplaceAll(base, "_", " ")
	return strings.TrimSpace(base)
}

func writeSourceIndex(srcDir, srcName string, entries []indexEntry, latest string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", srcName)
	fmt.Fprintf(&b, "%d docs.", len(entries))
	if latest != "" {
		fmt.Fprintf(&b, " Last updated %s.", latest)
	}
	b.WriteString("\n\nGenerated by `docs-puller`. Don't edit by hand - re-run `docs-puller pull ...` instead.\n\n")

	groups := map[string][]indexEntry{}
	groupOrder := []string{}
	for _, e := range entries {
		key := groupKey(e.path)
		if _, seen := groups[key]; !seen {
			groupOrder = append(groupOrder, key)
		}
		groups[key] = append(groups[key], e)
	}
	sort.Strings(groupOrder)

	for _, g := range groupOrder {
		if g == "" {
			b.WriteString("## (root)\n\n")
		} else {
			fmt.Fprintf(&b, "## %s\n\n", g)
		}
		for _, e := range groups[g] {
			fmt.Fprintf(&b, "- [%s](%s)", e.title, e.path)
			if e.url != "" {
				fmt.Fprintf(&b, " — <%s>", e.url)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return os.WriteFile(filepath.Join(srcDir, "_INDEX.md"), []byte(b.String()), 0o644)
}

// groupKey returns the top-level directory of a relative path, or "" for
// files at the root of the source dir.
func groupKey(rel string) string {
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		// Use the first two segments when present (e.g. "guides/database")
		// so groups stay narrow on large sources like supabase.
		rest := rel[i+1:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rel[:i] + "/" + rest[:j]
		}
		return rel[:i]
	}
	return ""
}

type sourceSummary struct {
	name   string
	count  int
	latest string
}

func writeTopIndex(out string, summaries []sourceSummary) error {
	var b strings.Builder
	b.WriteString("# Vendor docs\n\n")
	b.WriteString("Local mirror of vendor documentation, pulled by `docs-puller`. Each `<source>/_INDEX.md` lists every doc with its origin URL.\n\n")

	if len(summaries) == 0 {
		b.WriteString("_No docs pulled yet. Run `docs-puller pull --from <notes.md>` to populate._\n")
	} else {
		b.WriteString("| Source | Docs | Last pull |\n|---|---:|---|\n")
		for _, s := range summaries {
			latest := s.latest
			if latest == "" {
				latest = "—"
			}
			fmt.Fprintf(&b, "| [%s](%s/_INDEX.md) | %d | %s |\n", s.name, s.name, s.count, latest)
		}
		b.WriteString("\n")
	}

	b.WriteString(`## Tools

- Pull URLs from a notes file: ` + "`docs-puller pull --from <file>`" + `
- Pull a single URL: ` + "`docs-puller pull-url <url>`" + `
- Refresh upstream source caches: ` + "`docs-puller refresh`" + `
- View ingest history: ` + "`docs-puller log [--limit N] [--json]`" + ` (raw log at ` + "`_INGEST_LOG.jsonl`" + `)

Source code: ` + "`https://github.com/nstranquist/docs-puller`" + `.
`)
	return os.WriteFile(filepath.Join(out, "_INDEX.md"), []byte(b.String()), 0o644)
}
