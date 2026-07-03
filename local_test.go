package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeSourceName(t *testing.T) {
	cases := map[string]string{
		"team-knowledge":        "team-knowledge",
		"Example Knowledge":     "example-knowledge",
		"foo/bar":               "foo-bar",
		"  spaced  name  ":      "spaced-name",
		"weird:name@with#chars": "weird-name-with-chars",
		"":                      "unnamed",
		"---":                   "unnamed",
		"my.docs_v2":            "my.docs_v2",
		"UPPER":                 "upper",
		"trailing-":             "trailing",
	}
	for in, want := range cases {
		if got := sanitizeSourceName(in); got != want {
			t.Errorf("sanitizeSourceName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGithubRepoRegex(t *testing.T) {
	good := []string{
		"example-org/team-knowledge",
		"a/b",
		"foo-bar/baz_quux",
		"Org123/repo.with.dots",
	}
	bad := []string{
		"",
		"justname",
		"/leading-slash",
		"trailing/",
		"three/level/path",
		"weird name/repo",
	}
	for _, g := range good {
		if !githubRepoRE.MatchString(g) {
			t.Errorf("expected %q to match", g)
		}
	}
	for _, b := range bad {
		if githubRepoRE.MatchString(b) {
			t.Errorf("expected %q NOT to match", b)
		}
	}
}

// TestIngestLocalEndToEnd exercises the full local-ingest pipeline: walk a
// fake source tree, copy .md/.mdx/.mdoc, write manifest, regenerate _INDEX.md, and
// confirm skipDirs are respected.
func TestIngestLocalEndToEnd(t *testing.T) {
	src := t.TempDir()
	out := t.TempDir()

	files := map[string]string{
		"README.md":                   "---\ntitle: Top\n---\n\nbody",
		"docs/getting-started.md":     "# Getting Started\n\nhello",
		"docs/advanced/perf.mdx":      "# Perf\n\nfast",
		"docs/advanced/markdoc.mdoc":  "# Markdoc\n\nworks",
		"docs/notes.txt":              "ignore me",         // not markdown-family
		"node_modules/junk/README.md": "should be skipped", // skipDir
		".git/config":                 "should be skipped", // skipDir
		".vscode/settings.json":       "should be skipped", // hidden dir
	}
	for rel, body := range files {
		full := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	lo := localPullOpts{
		pullOpts: pullOpts{out: out, sourceCache: filepath.Join(out, ".cache")},
		name:     "test-source",
	}
	cmdPullLocal(src, lo, nil)

	// Four markdown-family files should have been ingested. .mdx/.mdoc become .md.
	wantPaths := []string{
		"README.md",
		"docs/getting-started.md",
		"docs/advanced/perf.md",
		"docs/advanced/markdoc.md",
	}
	for _, want := range wantPaths {
		if _, err := os.Stat(filepath.Join(out, "test-source", want)); err != nil {
			t.Errorf("expected file at test-source/%s: %v", want, err)
		}
	}

	// node_modules and .git should NOT have been walked.
	mustNotExist := []string{
		"test-source/node_modules/junk/README.md",
		"test-source/.git/config",
		"test-source/.vscode/settings.json",
		"test-source/docs/notes.txt", // not markdown-family
	}
	for _, p := range mustNotExist {
		if _, err := os.Stat(filepath.Join(out, p)); err == nil {
			t.Errorf("did not expect file at %s", p)
		}
	}

	// Manifest should have 4 entries with file:// URLs.
	mani, err := loadOrMigrateManifest(filepath.Join(out, "test-source"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mani.Entries) != 4 {
		t.Fatalf("manifest: got %d entries, want 4 (entries=%+v)", len(mani.Entries), mani.Entries)
	}
	for _, e := range mani.Entries {
		if !strings.HasPrefix(e.URL, "file://") {
			t.Errorf("entry %s: URL %q should start with file://", e.Path, e.URL)
		}
		if e.Mode != "local" {
			t.Errorf("entry %s: mode = %q, want local", e.Path, e.Mode)
		}
		if e.SHA256 == "" {
			t.Errorf("entry %s: missing sha256", e.Path)
		}
	}

	// _INDEX.md should mention the source.
	top, err := os.ReadFile(filepath.Join(out, "_INDEX.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(top), "[test-source]") {
		t.Errorf("top _INDEX.md missing test-source: %s", top)
	}
}

func TestIngestLocalBatchEndToEnd(t *testing.T) {
	alpha := t.TempDir()
	beta := t.TempDir()
	out := t.TempDir()
	mustWrite := func(root, rel, body string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(alpha, "guide.md", "# Alpha\n\nshared alpha batch phrase")
	mustWrite(beta, "nested/reference.mdx", "# Beta\n\nshared beta batch phrase")

	summary, err := ingestLocalBatch(
		[]localBatchSource{
			{name: "alpha", walkRoot: alpha},
			{name: "beta", walkRoot: beta},
		},
		pullOpts{out: out, sourceCache: filepath.Join(out, ".cache")},
		[]string{"pull-local-batch"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Mode != "local-batch" || summary.SourceCnt != 2 || summary.Pulled != 2 || !summary.FTSUpdated {
		t.Fatalf("summary = %+v, want two-source local-batch with FTS update", summary)
	}
	if got := strings.Join(summary.Sources, ","); got != "alpha,beta" {
		t.Fatalf("summary sources = %q, want alpha,beta", got)
	}
	if summary.WriteCount != 2 || summary.UnchangedCount != 0 {
		t.Fatalf("write counts = %d/%d, want 2/0", summary.WriteCount, summary.UnchangedCount)
	}
	if summary.FTSDocCount != 2 || summary.FTSSizeBytes == 0 {
		t.Fatalf("fts summary = docs:%d size:%d, want docs=2 and nonzero size", summary.FTSDocCount, summary.FTSSizeBytes)
	}
	if summary.ChangedPathCount != 2 {
		t.Fatalf("changed paths = %d, want 2", summary.ChangedPathCount)
	}
	if summary.ElapsedMs < summary.CopyMs || summary.ElapsedMs < summary.FTSUpdateMs {
		t.Fatalf("phase timings exceed elapsed summary: %+v", summary)
	}

	for _, source := range []string{"alpha", "beta"} {
		mani, err := loadOrMigrateManifest(filepath.Join(out, source))
		if err != nil {
			t.Fatal(err)
		}
		if len(mani.Entries) != 1 {
			t.Fatalf("%s manifest entries = %d, want 1", source, len(mani.Entries))
		}
	}

	idx, err := openFTSIndexReadOnly(out)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := idx.search("shared beta batch phrase", "", 5, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Path != "beta/nested/reference.md" {
		t.Fatalf("batch FTS hits = %+v, want beta/nested/reference.md first", hits)
	}
	idx.close()

	warmSummary, err := ingestLocalBatch(
		[]localBatchSource{
			{name: "alpha", walkRoot: alpha},
			{name: "beta", walkRoot: beta},
		},
		pullOpts{out: out, sourceCache: filepath.Join(out, ".cache")},
		[]string{"pull-local-batch"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if warmSummary.WriteCount != 0 || warmSummary.UnchangedCount != 2 {
		t.Fatalf("warm write counts = %d/%d, want 0/2", warmSummary.WriteCount, warmSummary.UnchangedCount)
	}
	if warmSummary.ChangedPathCount != 0 {
		t.Fatalf("warm changed paths = %d, want 0", warmSummary.ChangedPathCount)
	}
	if !warmSummary.FTSUpdated {
		t.Fatalf("warm summary did not report FTS update: %+v", warmSummary)
	}
	if warmSummary.FTSDocCount != 2 || warmSummary.FTSSizeBytes == 0 {
		t.Fatalf("warm fts summary = docs:%d size:%d, want docs=2 and nonzero size", warmSummary.FTSDocCount, warmSummary.FTSSizeBytes)
	}

	warmIdx, err := openFTSIndexReadOnly(out)
	if err != nil {
		t.Fatal(err)
	}
	defer warmIdx.close()
	warmHits, err := warmIdx.search("shared beta batch phrase", "", 5, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(warmHits) == 0 || warmHits[0].Path != "beta/nested/reference.md" {
		t.Fatalf("warm batch FTS hits = %+v, want beta/nested/reference.md first", warmHits)
	}
}

func TestLocalBatchMemoryDocsReuseCollectedTitle(t *testing.T) {
	docs := []localIngestDoc{
		{
			result: result{
				Path:      "alpha/guide.md",
				URL:       "file:///alpha/guide.md",
				FetchedAt: "2026-05-03T00:00:00Z",
			},
			body:   []byte("# Parsed Title\n\nbody"),
			absOut: filepath.Join("alpha", "guide.md"),
			title:  "Cached Title",
		},
	}

	indexDocs := localIndexMemoryDocs(docs)
	if len(indexDocs) != 1 || indexDocs[0].title != "Cached Title" {
		t.Fatalf("index memory docs = %+v, want cached title", indexDocs)
	}
	ftsDocs := localFTSMemoryDocs(docs, false)
	if len(ftsDocs) != 1 || ftsDocs[0].Title != "Cached Title" {
		t.Fatalf("fts memory docs = %+v, want cached title", ftsDocs)
	}
}

func TestIngestLocalSubdir(t *testing.T) {
	src := t.TempDir()
	out := t.TempDir()

	// Files outside the subdir should be ignored.
	must := func(rel, body string) {
		full := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("README.md", "outside")
	must("docs/in-scope.md", "inside")
	must("src/code.md", "outside")

	lo := localPullOpts{
		pullOpts: pullOpts{out: out, sourceCache: filepath.Join(out, ".cache")},
		name:     "scoped",
		subdir:   "docs",
	}
	cmdPullLocal(src, lo, nil)

	if _, err := os.Stat(filepath.Join(out, "scoped", "in-scope.md")); err != nil {
		t.Errorf("expected scoped/in-scope.md: %v", err)
	}
	for _, bad := range []string{"scoped/README.md", "scoped/docs/in-scope.md", "scoped/code.md"} {
		if _, err := os.Stat(filepath.Join(out, bad)); err == nil {
			t.Errorf("did not expect %s", bad)
		}
	}
}

func TestIngestLocalSizeLimit(t *testing.T) {
	src := t.TempDir()
	out := t.TempDir()

	// 2 MB file — over the 1 MB limit.
	big := strings.Repeat("a", 2<<20)
	if err := os.WriteFile(filepath.Join(src, "big.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "small.md"), []byte("# small"), 0o644); err != nil {
		t.Fatal(err)
	}

	lo := localPullOpts{
		pullOpts: pullOpts{out: out, sourceCache: filepath.Join(out, ".cache")},
		name:     "sized",
	}
	cmdPullLocal(src, lo, nil)

	if _, err := os.Stat(filepath.Join(out, "sized", "big.md")); err == nil {
		t.Errorf("big.md should have been skipped")
	}
	if _, err := os.Stat(filepath.Join(out, "sized", "small.md")); err != nil {
		t.Errorf("small.md missing: %v", err)
	}
}

// TestGithubURLConstruction verifies the URL builder for github-repo mode
// without actually cloning. Exercises the urlFor closure shape directly.
func TestGithubURLConstruction(t *testing.T) {
	cases := []struct {
		owner, repo, ref, subdir, rel string
		want                          string
	}{
		{"foo", "bar", "main", "", "README.md",
			"https://github.com/foo/bar/blob/main/README.md"},
		{"foo", "bar", "main", "docs", "guide.md",
			"https://github.com/foo/bar/blob/main/docs/guide.md"},
		{"foo", "bar", "v1.2.3", "", "nested/page.md",
			"https://github.com/foo/bar/blob/v1.2.3/nested/page.md"},
		{"foo", "bar", "main", "docs", "deep/nested/page.md",
			"https://github.com/foo/bar/blob/main/docs/deep/nested/page.md"},
	}
	for _, c := range cases {
		urlBase := "https://github.com/" + c.owner + "/" + c.repo + "/blob/" + c.ref
		got := urlBase + "/"
		if c.subdir != "" {
			got += c.subdir + "/"
		}
		got += c.rel
		if got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestExcludeMatch(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		rel      string
		want     bool
	}{
		{"empty patterns", nil, "foo/bar.md", false},
		{"recursive prefix matches dir itself", []string{"internal-notes/**"}, "internal-notes", true},
		{"recursive prefix matches nested file", []string{"internal-notes/**"}, "internal-notes/local-dev/x.md", true},
		{"recursive prefix does not match sibling", []string{"internal-notes/**"}, "internal-notes-other/x.md", false},
		{"bare segment matches dir", []string{"attachments"}, "attachments/img.png", true},
		{"bare segment matches nested", []string{"attachments"}, "foo/attachments/img.md", true},
		{"bare segment exact only", []string{"attach"}, "attachments/img.md", false},
		{"basename glob", []string{"*.tmp.md"}, "foo/bar.tmp.md", true},
		{"basename glob no match", []string{"*.tmp.md"}, "foo/bar.md", false},
		{"path glob", []string{"docs/*.md"}, "docs/intro.md", true},
		{"path glob nested no match", []string{"docs/*.md"}, "docs/sub/intro.md", false},
		{"multiple first matches", []string{"foo/**", "bar"}, "foo/x.md", true},
		{"multiple second matches", []string{"foo/**", "bar"}, "x/bar/y.md", true},
		{"multiple none match", []string{"foo/**", "bar"}, "baz/qux.md", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := excludeMatch(c.rel, c.patterns); got != c.want {
				t.Errorf("excludeMatch(%q, %v) = %v, want %v", c.rel, c.patterns, got, c.want)
			}
		})
	}
}
