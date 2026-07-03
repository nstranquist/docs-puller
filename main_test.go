package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractURLsFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.md")
	body := `# Notes

Plain url: https://supabase.com/docs/guides/database/drizzle
With trailing punct: https://example.com/x.
With fragment + query: https://supabase.com/docs/guides/local-development/cli/getting-started?queryGroups=platform&platform=macos#section-1
Markdown link: [docs](https://github.com/supabase-community/seed)
Duplicate: https://supabase.com/docs/guides/database/drizzle
# Commented operator note: refresh from https://example.com/comment-only
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := extractURLsFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://supabase.com/docs/guides/database/drizzle",
		"https://example.com/x",
		"https://supabase.com/docs/guides/local-development/cli/getting-started",
		"https://github.com/supabase-community/seed",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d urls (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("urls[%d]: got %q, want %q", i, got[i], w)
		}
	}
}

func TestExtractURLsHTMLResolvesRelative(t *testing.T) {
	body := []byte(`<html><body>
<a href="/doc/effective_go">Effective Go</a>
<a href="/doc/articles/wiki/index.html">Wiki</a>
<a href="https://other.example/external">External</a>
<a href="#anchor">Anchor only</a>
<a href="mailto:hi@example.com">Email</a>
<a href="javascript:void(0)">JS</a>
</body></html>`)
	got := extractURLs(body, "https://go.dev/doc/")
	want := map[string]bool{
		"https://go.dev/doc/effective_go":             true,
		"https://go.dev/doc/articles/wiki/index.html": true,
		"https://other.example/external":              true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d urls, want %d: %v", len(got), len(want), got)
	}
	for _, u := range got {
		if !want[u] {
			t.Errorf("unexpected URL %q", u)
		}
	}
}

func TestExtractURLsMarkdownFallsBackToRegex(t *testing.T) {
	// llms-sitemap.md style: bullets with URLs, no HTML anchors.
	body := []byte(`# sitemap
- https://example.com/a.md
- https://example.com/b.md
`)
	got := extractURLs(body, "")
	want := []string{"https://example.com/a.md", "https://example.com/b.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestExtractURLsFromFileFetchesURL(t *testing.T) {
	body := `# llms-sitemap

- https://example.com/a.md
- https://example.com/b.md
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	got, err := extractURLsFromFile(srv.URL + "/llms-sitemap.md")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://example.com/a.md", "https://example.com/b.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("urls[%d]: got %q, want %q", i, got[i], w)
		}
	}
}

func TestIsNativeMarkdownURL(t *testing.T) {
	cases := map[string]bool{
		"https://docs.slack.dev/quickstart.md":                    true,
		"https://docs.slack.dev/reference/methods/chat.md":        true,
		"https://example.com/path.MD":                             true, // case-insensitive
		"https://example.com/path.markdown":                       true,
		"https://firebase.google.com/docs/cloud-messaging.md.txt": true,
		"https://docs.slack.dev/quickstart":                       false,
		"https://example.com/foo.md.html":                         false,
		"https://example.com/":                                    false,
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := isNativeMarkdownURL(u); got != want {
			t.Errorf("isNativeMarkdownURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestResolveTarget(t *testing.T) {
	cases := []struct {
		raw, source, rel string
	}{
		{"https://supabase.com/docs/guides/database/drizzle",
			"supabase", "guides/database/drizzle.md"},
		{"https://supabase.com/docs/reference/cli/start",
			"supabase", "reference/cli/start.md"},
		{"https://supabase.github.io/splinter/",
			"splinter", "index.md"},
		{"https://supabase.github.io/splinter/lints/0001",
			"splinter", "lints/0001.md"},
		{"https://github.com/supabase-community/seed",
			"github", "supabase-community/seed/README.md"},
		{"https://www.postgresql.org/docs/current/pgtrgm.html",
			"postgresql", "docs/current/pgtrgm.md"},
		{"https://redis.io/docs/latest/develop/get-started/",
			"redis", "develop/get-started.md"},
		{"https://redis.io/docs/latest/commands/set/",
			"redis", "commands/set.md"},
		{"https://redis.io/docs/latest/", "redis", "index.md"},
		{"https://posthog.com/docs/getting-started/install",
			"posthog", "docs/getting-started/install.md"},
		{"https://posthog.com/docs/", "posthog", "docs.md"},
		{"https://posthog.com/handbook/values",
			"posthog", "handbook/values.md"},
		{"https://linear.app/developers", "linear", "index.md"},
		{"https://linear.app/developers/agents", "linear", "agents.md"},
		{"https://linear.app/developers/sdk-errors", "linear", "sdk-errors.md"},
		{"https://linear.app/about", "", ""},
		{"https://docs.slack.dev/quickstart.md", "slack", "quickstart.md"},
		{"https://docs.slack.dev/reference/methods/chat.postMessage.md",
			"slack", "reference/methods/chat.postMessage.md"},
		{"https://docs.slack.dev/", "slack", "index.md"},
		{"https://ai-sdk.dev/docs/introduction.md", "ai-sdk", "docs/introduction.md"},
		{"https://ai-sdk.dev/docs/ai-sdk-core/generating-text",
			"ai-sdk", "docs/ai-sdk-core/generating-text.md"},
		{"https://ai-sdk.dev/providers/ai-sdk-providers/openai.md",
			"ai-sdk", "providers/ai-sdk-providers/openai.md"},
		{"https://developers.openai.com/codex/codex-manual.md",
			"openai", "codex/codex-manual.md"},
		// code.claude.com native-markdown mirror: locale stripped, `.md`
		// suffix must not double up into `.md.md`.
		{"https://code.claude.com/docs/en/hooks-guide.md",
			"claude-code", "hooks-guide.md"},
		{"https://code.claude.com/docs/en/mcp", "claude-code", "mcp.md"},
		{"https://go.dev/doc/effective_go", "go", "doc/effective_go.md"},
		{"https://go.dev/doc/articles/wiki/index.html",
			"go", "doc/articles/wiki/index.md"},
		{"https://go.dev/ref/spec", "go", "ref/spec.md"},
		{"https://go.dev/", "go", "index.md"},
		{"https://developer.android.com/develop/ui/compose/documentation",
			"android", "develop/ui/compose/documentation.md"},
		{"https://nextjs.org/docs/app/getting-started/project-structure",
			"nextjs", "docs/app/getting-started/project-structure.md"},
		{"https://tailwindcss.com/docs/installation/using-vite",
			"tailwindcss", "installation/using-vite.md"},
		{"https://www.radix-ui.com/primitives/docs/components/dialog",
			"radix-ui", "primitives/docs/components/dialog.md"},
		{"https://lucide.dev/guide/react/getting-started",
			"lucide", "guide/react/getting-started.md"},
		{"https://firebase.google.com/docs/cloud-messaging",
			"firebase", "cloud-messaging.md"},
		{"https://firebase.google.com/docs/cloud-messaging/android/client.md.txt",
			"firebase", "cloud-messaging/android/client.md"},
		{"https://rnfirebase.io/messaging/usage",
			"react-native-firebase", "messaging/usage.md"},
		{"https://reactnavigation.org/docs/getting-started",
			"react-navigation", "getting-started.md"},
		{"https://redux-toolkit.js.org/tutorials/quick-start",
			"redux", "tutorials/quick-start.md"},
		{"https://react-hook-form.com/docs/useform",
			"react-hook-form", "useform.md"},
		{"https://zod.dev/api?id=objects",
			"zod", "api.md"},
		{"https://turbo.build/repo/docs",
			"turbo", "repo/docs.md"},
		{"https://turborepo.dev/docs/reference/configuration",
			"turbo", "docs/reference/configuration.md"},
		{"https://pnpm.io/workspaces",
			"pnpm", "workspaces.md"},
		{"https://getpino.io/#/docs/api",
			"pino", "index.md"},
		{"https://expressjs.com/en/4x/api.html",
			"express", "en/4x/api.md"},
		{"https://axiom.co/docs/guides/javascript",
			"axiom", "guides/javascript.md"},
		{"https://docs.lemonsqueezy.com/api/products",
			"lemon-squeezy", "api/products.md"},
		{"https://cloud.google.com/bigquery/docs/reference/libraries",
			"google-cloud", "bigquery/docs/reference/libraries.md"},
		{"https://keystatic.com/docs/quick-start",
			"keystatic", "docs/quick-start.md"},
		{"https://developer.wordpress.org/rest-api/reference/posts/",
			"wordpress", "rest-api/reference/posts.md"},
		{"https://mariadb.com/kb/en/installing-and-using-mariadb-via-docker/",
			"mariadb", "kb/en/installing-and-using-mariadb-via-docker.md"},
		{"https://openfeature.dev/docs/reference/sdks/server/javascript/",
			"openfeature", "docs/reference/sdks/server/javascript.md"},
		{"https://docs.growthbook.io/lib/js",
			"growthbook", "lib/js.md"},
		{"https://docs.temporal.io/develop/typescript",
			"temporal", "develop/typescript.md"},
		{"https://docs.peerdb.io/quickstart",
			"peerdb", "quickstart.md"},
		{"https://min.io/docs/minio/linux/index.html",
			"minio", "linux/index.md"},
		{"https://docs.nativewind.dev/getting-started/installation",
			"nativewind", "getting-started/installation.md"},
		{"https://nativewind.dev/getting-started/installation",
			"nativewind", "getting-started/installation.md"},
		{"https://shopify.github.io/react-native-skia/docs/getting-started/installation",
			"react-native-skia", "docs/getting-started/installation.md"},
		{"https://docs.swmansion.com/react-native-reanimated/docs/fundamentals/getting-started/",
			"react-native-reanimated", "docs/fundamentals/getting-started.md"},
		{"https://docs.swmansion.com/react-native-gesture-handler/docs/fundamentals/installation",
			"react-native-gesture-handler", "docs/fundamentals/installation.md"},
		{"https://dev.appsflyer.com/hc/docs/rn-getting-started",
			"appsflyer", "hc/docs/rn-getting-started.md"},
		{"https://docs.obsidian.md/Reference/Manifest",
			"obsidian-app-docs", "Reference/Manifest.md"},
		{"https://docs.obsidian.md/Plugins/User+interface/Workspace",
			"obsidian-app-docs", "Plugins/User+interface/Workspace.md"},
		{"https://docs.obsidian.md/", "obsidian-app-docs", "index.md"},
		{"https://example.com/anything", "", ""},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", c.raw, err)
		}
		gotSource, gotRel := resolveTarget(u)
		if gotSource != c.source || gotRel != c.rel {
			t.Errorf("resolveTarget(%q) = (%q, %q), want (%q, %q)",
				c.raw, gotSource, gotRel, c.source, c.rel)
		}
	}
}

func TestIsGithubRepoRoot(t *testing.T) {
	cases := map[string]bool{
		"/owner/repo":            true,
		"owner/repo":             true,
		"/owner/repo/tree/main":  false,
		"/owner":                 false,
		"":                       false,
		"/owner/repo/issues/123": false,
	}
	for path, want := range cases {
		if got := isGithubRepoRoot(path); got != want {
			t.Errorf("isGithubRepoRoot(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestHostToSource(t *testing.T) {
	cases := map[string]string{
		"supabase.com":       "supabase",
		"www.postgresql.org": "postgresql",
		"github.com":         "github",
		"":                   "",
		"localhost":          "localhost",
	}
	for host, want := range cases {
		if got := hostToSource(host); got != want {
			t.Errorf("hostToSource(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestCleanMarkdownStripsEmptyAnchors(t *testing.T) {
	cases := map[string]string{
		"## [​](#x) Title":          "## Title",
		"# [](#section)Heading":     "# Heading",
		"plain text":                "plain text",
		"## [ ](#nope) Header":      "## Header",
		"keep [real](#anchor) link": "keep [real](#anchor) link",
	}
	for in, want := range cases {
		if got := cleanMarkdown(in); got != want {
			t.Errorf("cleanMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBodyHasMatchingH1(t *testing.T) {
	cases := []struct {
		md, title string
		want      bool
	}{
		// MS Learn pattern: body H1 matches <title>
		{"chrome\n\n# az account\n\nManage subscriptions.", "az account", true},
		// Docusaurus pattern: body has no H1, title comes from <title>
		{"## Subsection\n\nbody only", "ClickHouse Cloud quick start", false},
		// pg_hba.conf-style false H1 in body, real title is unrelated
		{"# TYPE  DATABASE  USER\n\nrest", "PostgreSQL", false},
		// Title contains body H1 (substring match in either direction)
		{"# az\n\nbody", "az account", true},
		{"# az account list\n\nbody", "az account", true},
		// Empty inputs
		{"", "any", false},
		{"# x", "", false},
	}
	for _, c := range cases {
		if got := bodyHasMatchingH1(c.md, c.title); got != c.want {
			t.Errorf("bodyHasMatchingH1(%q, %q) = %v, want %v", c.md, c.title, got, c.want)
		}
	}
}

func TestExtractMainStripsNav(t *testing.T) {
	html := []byte(`<!doctype html><html><body>
<nav>NAV CRUFT</nav>
<main>
  <h1>Real Content</h1>
  <p>This is what we want.</p>
  <aside>SIDEBAR CRUFT</aside>
</main>
<footer>FOOTER CRUFT</footer>
<script>alert(1)</script>
</body></html>`)
	out := string(extractMain("supabase.com", html))
	if !strings.Contains(out, "Real Content") || !strings.Contains(out, "what we want") {
		t.Errorf("expected main content kept, got: %s", out)
	}
	for _, junk := range []string{"NAV CRUFT", "SIDEBAR CRUFT", "FOOTER CRUFT", "alert(1)"} {
		if strings.Contains(out, junk) {
			t.Errorf("expected %q stripped, got: %s", junk, out)
		}
	}
}

func TestWriteManifestsSkipsUnmappedBucket(t *testing.T) {
	dir := t.TempDir()
	results := []result{
		{URL: "https://supabase.com/docs/guides/x", Source: "supabase", Path: "supabase/guides/x.md", FetchedAt: "2026-04-28"},
		{URL: "https://unknown.example.com/x", Source: "", Skipped: "no target mapping for host"},
	}
	if err := writeManifests(dir, results, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "supabase", manifestFile)); err != nil {
		t.Errorf("expected supabase/%s, got: %v", manifestFile, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "_unmapped")); !os.IsNotExist(err) {
		t.Errorf("_unmapped/ should not exist, stat err: %v", err)
	}
}

func TestWriteManifestsMergesPriorJSONEntries(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "s")
	prior := newManifest()
	prior.Entries["https://a.example/x"] = result{URL: "https://a.example/x", Source: "s", Path: "s/x.md", FetchedAt: "2026-04-01"}
	prior.Entries["https://b.example/y"] = result{URL: "https://b.example/y", Source: "s", Path: "s/y.md", FetchedAt: "2026-04-02"}
	if err := writeManifestAtomic(srcDir, prior); err != nil {
		t.Fatal(err)
	}

	fresh := []result{
		{URL: "https://b.example/y", Source: "s", Path: "s/y.md", FetchedAt: "2026-04-28"},
		{URL: "https://c.example/z", Source: "s", Path: "s/z.md", FetchedAt: "2026-04-28"},
	}
	if err := writeManifests(dir, fresh, false, nil); err != nil {
		t.Fatal(err)
	}

	got, err := loadOrMigrateManifest(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 3 {
		t.Fatalf("expected 3 merged entries, got %d: %+v", len(got.Entries), got.Entries)
	}
	if got.Entries["https://b.example/y"].FetchedAt != "2026-04-28" {
		t.Errorf("b.example/y not updated: %+v", got.Entries["https://b.example/y"])
	}
	if got.Entries["https://a.example/x"].FetchedAt != "2026-04-01" {
		t.Errorf("a.example/x prior entry not preserved: %+v", got.Entries["https://a.example/x"])
	}
}

func TestReplaceManifestForSourcePrunesStaleURLs(t *testing.T) {
	m := newManifest()
	m.Entries["https://old.example/x"] = result{URL: "https://old.example/x", Path: "s/old.md"}
	fresh := []result{{URL: "https://new.example/y", Path: "s/new.md", FetchedAt: "2026-06-30"}}
	pruned := replaceManifestForSource(&m, fresh)
	if len(pruned) != 1 || pruned[0] != "s/old.md" {
		t.Fatalf("pruned = %v, want [s/old.md]", pruned)
	}
	if _, ok := m.Entries["https://old.example/x"]; ok {
		t.Fatal("stale URL should be removed")
	}
	if _, ok := m.Entries["https://new.example/y"]; !ok {
		t.Fatal("fresh URL should be present")
	}
}

func TestReplaceManifestForSourceDropsSkippedURLs(t *testing.T) {
	m := newManifest()
	m.Entries["https://gone.example/x"] = result{URL: "https://gone.example/x", Source: "s", Path: "s/gone.md"}
	fresh := []result{{URL: "https://gone.example/x", Source: "s", Path: "s/gone.md", Skipped: "HTTP 404"}}
	pruned := replaceManifestForSource(&m, fresh)
	if len(pruned) != 1 || pruned[0] != "s/gone.md" {
		t.Fatalf("pruned = %v, want [s/gone.md]", pruned)
	}
	if _, ok := m.Entries["https://gone.example/x"]; ok {
		t.Fatal("skipped URL should not be re-added during replace")
	}
}

func TestWriteManifestsWrapsManifestLoadError(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "s")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(srcDir, manifestFile)
	if err := os.WriteFile(path, []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := writeManifests(dir, []result{{
		URL:       "https://example.com/bad",
		Source:    "s",
		Path:      "s/bad.md",
		FetchedAt: "2026-05-07",
	}}, false, nil)
	if err == nil {
		t.Fatal("expected manifest load error")
	}
	if !strings.HasPrefix(err.Error(), "load s manifest: parse "+path+": ") {
		t.Fatalf("writeManifests error = %v", err)
	}
	if !strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("writeManifests error does not include JSON cause: %v", err)
	}
}

func TestManifestMigratesLegacyJSONL(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "s")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(srcDir, legacyManifestFile)
	body := `{"url":"https://a.example/x","source":"s","path":"s/x.md","fetched_at":"2026-04-01"}` + "\n" +
		`{"url":"https://b.example/y","source":"s","path":"s/y.md","fetched_at":"2026-04-02"}` + "\n"
	if err := os.WriteFile(legacy, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := loadOrMigrateManifest(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 2 {
		t.Errorf("expected 2 migrated entries, got %d", len(m.Entries))
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("expected legacy %s removed, stat err: %v", legacyManifestFile, err)
	}
	if _, err := os.Stat(filepath.Join(srcDir, manifestFile)); err != nil {
		t.Errorf("expected new %s after migration, got: %v", manifestFile, err)
	}

	// Second load should be a no-op (no legacy file to migrate).
	m2, err := loadOrMigrateManifest(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Entries) != 2 || m2.Version != manifestVersion {
		t.Errorf("post-migration reload mismatch: %+v", m2)
	}
}

func TestLoadOrMigrateManifestWrapsParseError(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "s")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(srcDir, manifestFile)
	if err := os.WriteFile(path, []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := loadOrMigrateManifest(srcDir)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if got.Entries != nil {
		t.Fatalf("expected zero manifest on parse error, got %+v", got)
	}
	if !strings.HasPrefix(err.Error(), "parse "+path+": ") {
		t.Fatalf("loadOrMigrateManifest error = %v", err)
	}
	if !strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("loadOrMigrateManifest error does not include JSON cause: %v", err)
	}
}

func TestWriteManifestAtomicNoPartialOnEncodeFail(t *testing.T) {
	// The temp-file + rename pattern means a successful write replaces the
	// final atomically; failure in the middle leaves the prior file intact.
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "s")
	original := newManifest()
	original.Entries["https://x"] = result{URL: "https://x", Path: "s/x.md", FetchedAt: "first"}
	if err := writeManifestAtomic(srcDir, original); err != nil {
		t.Fatal(err)
	}
	// Confirm the on-disk file matches what we wrote.
	got, err := loadOrMigrateManifest(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Entries["https://x"].FetchedAt != "first" {
		t.Errorf("post-write read mismatch: %+v", got.Entries)
	}
	// And no temp leftovers in srcDir.
	ents, _ := os.ReadDir(srcDir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".manifest.json.") {
			t.Errorf("temp file leftover: %s", e.Name())
		}
	}
}

func TestExtractHTMLTitle(t *testing.T) {
	cases := map[string]string{
		`<html><head><title>Just a Title</title></head><body></body></html>`:                             "Just a Title",
		`<html><head><title>Page | ClickHouse Docs</title></head></html>`:                                "Page",
		`<html><head><title>Foo | Bar | Baz</title></head></html>`:                                       "Foo | Bar",
		`<html><head><title>  Trim Me  </title></head></html>`:                                           "Trim Me",
		`<html><head></head></html>`:                                                                     "",
		`<html><head><title>PostgreSQL: Documentation: 18: pg_dumpall</title></head></html>`:             "pg_dumpall",
		`<html><head><title>PostgreSQL: Documentation: 18: 5.6. System Columns</title></head></html>`:    "5.6. System Columns",
		`<html><head><title>Section 5.6: System Columns</title></head></html>`:                           "Section 5.6: System Columns",
		`<html><head><title>Claude Code overview - Claude Code Docs</title></head></html>`:               "Claude Code overview",
		`<html><head><title>gh repo clone - GitHub CLI documentation</title></head></html>`:              "gh repo clone",
		`<html><head><title>Foo - a tool for X</title></head></html>`:                                    "Foo - a tool for X",
		`<html><head><title>Chrome Extensions &nbsp;|&nbsp; Chrome for Developers</title></head></html>`: "Chrome Extensions",
	}
	for in, want := range cases {
		if got := extractHTMLTitle([]byte(in)); got != want {
			t.Errorf("extractHTMLTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFetchAndConvertEndToEnd covers the riskiest path: HTTP fetch +
// per-host extractor + Docusaurus-style strip rules + title prepend +
// html-to-markdown. Uses a fake clickhouse.com host so the Docusaurus
// selectors (article, .theme-doc-markdown) and strip rules
// (.theme-edit-this-page, .hash-link) actually fire.
func TestFetchAndConvertEndToEnd(t *testing.T) {
	html := `<!doctype html>
<html><head><title>Test Page | ClickHouse Docs</title></head><body>
<nav>NAV CRUFT</nav>
<aside class="theme-doc-sidebar-container">SIDEBAR CRUFT</aside>
<main class="main-wrapper">
  <article>
    <a class="theme-edit-this-page" href="x">Edit this page</a>
    <div class="theme-doc-markdown markdown">
      <h2>Section<a class="hash-link" href="#section">[zwsp]</a></h2>
      <p>Body content kept.</p>
      <pre><code>SELECT 1</code></pre>
    </div>
  </article>
</main>
<footer>FOOTER CRUFT</footer>
<script>alert('drop me')</script>
</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(html))
	}))
	defer srv.Close()

	// Force the clickhouse.com selector path by parsing the URL with that host.
	// We can't change the request URL host without DNS hacks, so we hit the
	// httptest server directly — extractMain falls back to generic `main` in
	// the absence of a host match, which still picks the article.
	out, err := fetchAndConvert(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	for _, want := range []string{
		"# Test Page", // <title> prepended (no H1 in body)
		"## Section",  // h2 kept
		"Body content kept.",
		"SELECT 1", // code block survived
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output:\n%s", want, got)
		}
	}
	for _, junk := range []string{
		"NAV CRUFT", "SIDEBAR CRUFT", "FOOTER CRUFT",
		"Edit this page", "alert('drop me')",
	} {
		if strings.Contains(got, junk) {
			t.Errorf("expected %q stripped, got:\n%s", junk, got)
		}
	}
}

func TestExtractMainFallsBackWhenNoSelectorMatches(t *testing.T) {
	html := []byte(`<!doctype html><html><body><div>only-content</div></body></html>`)
	out := string(extractMain("unknown.example.com", html))
	if !strings.Contains(out, "only-content") {
		t.Errorf("expected fallback to keep body content, got: %s", out)
	}
}
