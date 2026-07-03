package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// crawl-refs ingests synthesized ref-dissection narratives under
// ~/code/refs/_cases/<slug>/*.md into a first-class docs-puller source
// (`refs-dissections`), so code-adoption intelligence becomes searchable via
// `docs-puller search`, serveable via `docs-puller serve`, and scorable by the
// eval harness — alongside vendor docs.
//
// It ingests ONLY the analysis markdown, never the raw cloned repos.

type refsCrawlOpts struct {
	pullOpts
	casesRoot string
	source    string
}

// refsCaseManifest is the subset of _cases/<slug>/00-manifest.json we surface in
// the generated overview. All fields are best-effort — a missing or malformed
// manifest degrades to just the slug, never an error.
type refsCaseManifest struct {
	Source struct {
		Slug                string `json:"slug"`
		RepoURL             string `json:"repo_url"`
		PrimaryLanguage     string `json:"primary_language"`
		LatestCommitSubject string `json:"latest_commit_subject"`
	} `json:"source"`
}

// refsCase is one dissected slug: its directory name plus the markdown case
// files found under it and best-effort manifest metadata for the overview.
type refsCase struct {
	Slug     string
	Files    []string // rel paths under the slug dir, slash-form, sorted
	Manifest refsCaseManifest
}

func defaultRefsCrawlOpts() refsCrawlOpts {
	o := defaultOpts()
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "crawl-refs: cannot resolve home dir: %v\n", err)
		os.Exit(2)
	}
	casesRoot := filepath.Join(home, "code", "refs", "_cases")
	if env := os.Getenv("REFS_CASES_ROOT"); env != "" {
		casesRoot = env
	}
	return refsCrawlOpts{
		pullOpts:  o,
		casesRoot: casesRoot,
		source:    "refs-dissections",
	}
}

func cmdCrawlRefs(args []string) {
	o := defaultRefsCrawlOpts()
	fset := flag.NewFlagSet("crawl-refs", flag.ExitOnError)
	fset.StringVar(&o.out, "out", o.out, "output root dir")
	fset.StringVar(&o.casesRoot, "cases-root", o.casesRoot, "ref dissection cases root (default ~/code/refs/_cases)")
	fset.StringVar(&o.source, "source", o.source, "generated source name")
	fset.Parse(args)

	now := time.Now().UTC().Format(time.RFC3339)
	docs, cases, err := collectRefsDissectionDocs(o.casesRoot, now)
	if err != nil {
		die(err)
	}
	source := sanitizeSourceName(o.source)
	count, err := replaceCrawlSource(o.out, source, "refs-crawl", docs, os.Args[1:])
	if err != nil {
		die(err)
	}
	fmt.Printf("refs dissections crawl: %d docs into %s/ from %d cases\n", count, source, len(cases))
}

// collectRefsDissectionDocs walks casesRoot for per-slug dissection markdown and
// returns the crawlDocs (overview README first) plus the discovered cases.
//
// It FAILS CLOSED: a missing/non-directory casesRoot, or a tree that yields zero
// markdown docs, returns an error so replaceCrawlSource is never called with an
// empty doc set — which would otherwise wipe an existing good `refs-dissections`
// source and its FTS rows.
func collectRefsDissectionDocs(casesRoot, generatedAt string) ([]crawlDoc, []refsCase, error) {
	info, err := os.Stat(casesRoot)
	if err != nil || !info.IsDir() {
		return nil, nil, fmt.Errorf("crawl-refs: cases root %q is not a directory: %v", casesRoot, err)
	}

	entries, err := os.ReadDir(casesRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("crawl-refs: reading cases root %q: %w", casesRoot, err)
	}

	var cases []refsCase
	var docs []crawlDoc
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		slug := e.Name()
		slugDir := filepath.Join(casesRoot, slug)
		rc := refsCase{Slug: slug, Manifest: readRefsCaseManifest(slugDir)}

		err := filepath.WalkDir(slugDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != slugDir && shouldSkipCrawlDir(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
				return nil // skips 00-manifest.json and any non-markdown
			}
			fi, err := d.Info()
			if err != nil || fi.Size() == 0 || fi.Size() > localFileSizeLimit {
				return nil
			}
			rel, err := filepath.Rel(slugDir, path)
			if err != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			rc.Files = append(rc.Files, relSlash)
			docs = append(docs, crawlDoc{
				Rel:      filepath.ToSlash(filepath.Join(slug, relSlash)),
				URL:      "file://" + path,
				ReadFrom: path,
			})
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		if len(rc.Files) == 0 {
			continue
		}
		sort.Strings(rc.Files)
		cases = append(cases, rc)
	}

	if len(docs) == 0 {
		return nil, nil, fmt.Errorf("crawl-refs: no dissection markdown found under %q — refusing to replace source with empty set (fail closed)", casesRoot)
	}

	sort.Slice(cases, func(i, j int) bool { return cases[i].Slug < cases[j].Slug })
	overview := renderRefsCrawlReadme(casesRoot, generatedAt, cases)
	docs = append([]crawlDoc{{
		Rel:     "README.md",
		URL:     "refs-crawl://README.md",
		Content: []byte(overview),
	}}, docs...)
	return docs, cases, nil
}

// readRefsCaseManifest reads <slugDir>/00-manifest.json best-effort.
func readRefsCaseManifest(slugDir string) refsCaseManifest {
	var m refsCaseManifest
	data, err := os.ReadFile(filepath.Join(slugDir, "00-manifest.json"))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(data, &m) // malformed → zero value, never fatal
	return m
}

func renderRefsCrawlReadme(casesRoot, generatedAt string, cases []refsCase) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Ref Dissection Cases\n\n")
	fmt.Fprintf(&b, "Generated at %s by `docs-puller crawl-refs` from `%s`.\n\n", generatedAt, casesRoot)
	fmt.Fprintf(&b, "This source mirrors synthesized ref-dissection analysis for each cloned reference repo under `~/code/refs/_cases/<slug>/`. Search it with `docs-puller search \"...\" --source refs-dissections`. It is generated, not hand-edited.\n\n")
	fmt.Fprintf(&b, "## Cases (%d)\n\n", len(cases))
	for _, c := range cases {
		lang := c.Manifest.Source.PrimaryLanguage
		subj := c.Manifest.Source.LatestCommitSubject
		repo := c.Manifest.Source.RepoURL
		line := "- **" + c.Slug + "**"
		var meta []string
		if lang != "" {
			meta = append(meta, "lang `"+lang+"`")
		}
		if repo != "" {
			meta = append(meta, "repo `"+repo+"`")
		}
		if len(meta) > 0 {
			line += " — " + strings.Join(meta, ", ")
		}
		fmt.Fprintf(&b, "%s\n", line)
		if subj != "" {
			fmt.Fprintf(&b, "  - latest: %s\n", subj)
		}
		fmt.Fprintf(&b, "  - files: %s\n", strings.Join(c.Files, ", "))
	}
	return b.String()
}
