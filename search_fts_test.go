package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFTSBuildQuery(t *testing.T) {
	cases := map[string]string{
		// Tokens lowercase'd before quoting (FTS5 with porter stemmer is
		// case-insensitive anyway, and lowercasing makes synonym lookup work).
		"row level security": `"row" "level" "security"`,
		"RAG":                `"rag"`,
		"  multi   space  ":  `"multi" "space"`,
		"":                   "",
		// Strip injection of FTS5 operators.
		`"phrase" NEAR(x y)`: `"phrase" "near" "x" "y"`,
		// Hyphens preserved (legitimate in CLI flags / identifiers).
		"--my-flag": `"--my-flag"`,
		"foo*":      `"foo"`, // prefix operator stripped
	}
	for in, want := range cases {
		if got := ftsBuildQuery(in, false); got != want {
			t.Errorf("ftsBuildQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFTSBuildQueryExpandsSynonyms(t *testing.T) {
	// "spec" is in a synonym class; output should be a parenthesized OR group
	// joined with explicit AND (FTS5 needs AND when mixing parens + bare terms).
	got := ftsBuildQuery("go language spec", false)
	for _, want := range []string{`"go"`, `"language"`, `("spec" OR `, `"specification"`, " AND "} {
		if !strings.Contains(got, want) {
			t.Errorf("ftsBuildQuery(go language spec) missing %q in %q", want, got)
		}
	}
	// Queries without any synonym tokens keep the original implicit-AND form.
	if got := ftsBuildQuery("rate limit", false); got != `"rate" "limit"` {
		t.Errorf("ftsBuildQuery(rate limit) = %q, want plain quoted phrases", got)
	}
}

func TestFTSBuildQueryDropsNaturalLanguageStopWords(t *testing.T) {
	got := ftsBuildQuery("how do I upload files to Supabase storage", false)
	for _, want := range []string{`"upload"`, `"files"`, `"supabase"`, `"storage"`} {
		if !strings.Contains(got, want) {
			t.Errorf("ftsBuildQuery natural language missing %q in %q", want, got)
		}
	}
	for _, notWant := range []string{`"how"`, `"do"`, `"i"`, `"to"`} {
		if strings.Contains(got, notWant) {
			t.Errorf("ftsBuildQuery natural language kept stop word %q in %q", notWant, got)
		}
	}
}

func TestFTSBuildTitleQueryDropsStopWordsBeforeSourceStrip(t *testing.T) {
	got, src := ftsBuildTitleQuery("how do I upload files to Supabase storage", false)
	if src != "supabase" {
		t.Fatalf("inferred source = %q, want supabase", src)
	}
	for _, want := range []string{`"upload"`, `"files"`, `"storage"`} {
		if !strings.Contains(got, want) {
			t.Errorf("title query missing %q in %q", want, got)
		}
	}
	for _, notWant := range []string{`"how"`, `"do"`, `"i"`, `"to"`, `"supabase"`} {
		if strings.Contains(got, notWant) {
			t.Errorf("title query kept stripped token %q in %q", notWant, got)
		}
	}
}

func TestFTSScoringTokensDropNaturalLanguageStopWords(t *testing.T) {
	got := ftsScoringTokens("how do I upload files to Supabase storage", false)
	want := []string{"upload", "files", "supabase", "storage"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("scoring tokens = %v, want %v", got, want)
	}
}

func TestFTSBuildQueryDropsActionStopWords(t *testing.T) {
	got := ftsBuildQuery("how do I write an Obsidian plugin manifest", false)
	if strings.Contains(got, `"write"`) {
		t.Fatalf("natural-language action word stayed in query: %q", got)
	}
	for _, want := range []string{`"obsidian"`, `"plugin"`, `"manifest"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("ftsBuildQuery missing %q in %q", want, got)
		}
	}
}

func TestFTSBuildQueryRewritesInspectQueryPlan(t *testing.T) {
	got := ftsBuildQuery("how do I inspect a PostgreSQL query plan", false)
	if strings.Contains(got, `"inspect"`) {
		t.Fatalf("inspect query-plan intent kept generic verb: %q", got)
	}
	for _, want := range []string{`"explain"`, `"postgresql"`, `"query"`, `"plan"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("ftsBuildQuery missing %q in %q", want, got)
		}
	}
}

func TestFTSBuildQueryRewritesTechStackNaturalLanguage(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{"how do I configure row level security on a postgres table in supabase", `"supabase" "row" "level" "security"`},
		{"what's the easiest way to upload a file from my web app to supabase storage", `"supabase" "storage" "standard" "uploads"`},
		{"how do I find out why a postgres query is slow", `"postgres" "using" "explain"`},
		{"how do I get faster inserts in clickhouse without losing data", `"clickhouse" "async" "insert"`},
		{"how do I render thousands of items in a react native list without lag", `"react" "native" "flatlist" "performance"`},
		{"what are the rules for writing typescript that narrows a union type", `("typescript" OR "ts") AND "narrowing" AND "union" AND "type"`},
		{"how do I run an openai-compatible chat completion against the xai grok api", `"xai" "rest" "api" "chat"`},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			if got := ftsBuildQuery(c.query, false); got != c.want {
				t.Fatalf("ftsBuildQuery = %q, want %q", got, c.want)
			}
		})
	}
}

func TestFTSBuildQueryKeepsCanonicalSupabaseRLS(t *testing.T) {
	got := ftsBuildQuery("supabase row level security", false)
	want := `"supabase" "row" "level" "security"`
	if got != want {
		t.Fatalf("canonical RLS query = %q, want %q", got, want)
	}
}

func TestFTSBuildQueryRewritesWeakLibrarySearchMisses(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{"how do I store and update local state in a react function component", `"react" "usestate"`},
		{"how do I run side effects when a react component mounts", `"react" "useeffect"`},
		{"react context", `"react" "createcontext"`},
		{"installed MCPs list", `"mcps"`},
		{"how do I see logs for an azure web app", `"azure" "app" "log" "diagnostics"`},
		{"how do I check what queries are currently running on my clickhouse server", `"clickhouse" "introspection"`},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			if got := ftsBuildQuery(c.query, false); got != c.want {
				t.Fatalf("ftsBuildQuery = %q, want %q", got, c.want)
			}
		})
	}
}

func TestFTSBuildTitleQueryStripsSourcePhrase(t *testing.T) {
	got, src := ftsBuildTitleQuery("how do I render thousands of items in a react native list without lag", false)
	if src != "react-native" {
		t.Fatalf("inferred source = %q, want react-native", src)
	}
	want := `title:("flatlist" "performance")`
	if got != want {
		t.Fatalf("title query = %q, want %q", got, want)
	}
}

func TestFTSScoringTokensKeepUseForTitleAndPathScoring(t *testing.T) {
	got := ftsScoringTokens("how does Anthropic tool use work", false)
	want := []string{"anthropic", "tool", "use", "work"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("scoring tokens = %v, want %v", got, want)
	}
}

func TestFTSScoringTokensDropUseOutsideToolUsePhrase(t *testing.T) {
	got := ftsScoringTokens("how do I use Go modules for a new project", false)
	want := []string{"go", "modules", "new"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("scoring tokens = %v, want %v", got, want)
	}
}

func TestFTSScoringTokensExactKeepsStopWords(t *testing.T) {
	got := ftsScoringTokens("how do I upload files", true)
	want := []string{"how", "do", "i", "upload", "files"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("exact scoring tokens = %v, want %v", got, want)
	}
}

// TestFTSBuildQueryExpandsAcronymToPhrase verifies acronym tokens like
// "rls" emit an OR group containing both the token AND the multi-word
// phrase form ("row level security"). FTS5 matches the phrase as
// adjacent-in-order, so the canonical doc whose title/body contains the
// expanded phrase wins on tier-qualification even when its title doesn't
// contain the bare acronym.
func TestFTSBuildQueryExpandsAcronymToPhrase(t *testing.T) {
	got := ftsBuildQuery("supabase rls", false)
	for _, want := range []string{`"rls"`, `"row level security"`, " OR "} {
		if !strings.Contains(got, want) {
			t.Errorf("ftsBuildQuery(supabase rls) missing %q in %q", want, got)
		}
	}
	// Tokens not in phraseSynonyms shouldn't get an acronym expansion.
	if got := ftsBuildQuery("supabase storage", false); strings.Contains(got, " OR ") {
		t.Errorf("ftsBuildQuery(supabase storage) should NOT have OR group, got %q", got)
	}
}

// TestFTSBuildQueryExact verifies the --exact mode collapses the input into
// a single quoted FTS5 phrase and skips both synonym-class expansion AND
// phraseSynonyms acronym expansion. Exact mode means literal phrase — any
// expansion would change the semantic.
func TestFTSBuildQueryExact(t *testing.T) {
	cases := map[string]string{
		"row level security": `"row level security"`,
		"  spec  ":           `"spec"`, // synonym class ignored when exact
		"go language spec":   `"go language spec"`,
		"supabase rls":       `"supabase rls"`,    // phraseSynonyms ignored when exact
		`"phrase" NEAR(x y)`: `"phrase near x y"`, // FTS5 operators stripped to spaces
		"":                   "",
	}
	for in, want := range cases {
		if got := ftsBuildQuery(in, true); got != want {
			t.Errorf("ftsBuildQuery(%q, exact=true) = %q, want %q", in, got, want)
		}
	}
}

// TestFTSExactPhraseRejectsDisjointTokens proves the practical value of
// --exact: a doc that contains every query token but never adjacent matches
// in default mode (token AND) but NOT in exact mode (phrase requires
// adjacency in source order).
func TestFTSExactPhraseRejectsDisjointTokens(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "x")
	writeFTSDoc(t, src, "disjoint.md",
		"# Disjoint\n\nRow counters are common. The level of detail varies.\nSecurity is a separate concern.\n")
	writeFTSDoc(t, src, "adjacent.md",
		"# Adjacent\n\nRow Level Security policies in Postgres.\n")

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	// Default mode: token AND finds both docs.
	defHits, err := idx.search("row level security", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(defHits) != 2 {
		t.Errorf("default mode hits = %d, want 2 (both docs contain all tokens)", len(defHits))
	}

	// Exact mode: only the adjacent doc matches.
	exHits, err := idx.search("row level security", "", 10, true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(exHits) != 1 {
		t.Fatalf("exact mode hits = %d, want 1 (disjoint must be rejected)", len(exHits))
	}
	if !strings.HasSuffix(exHits[0].Path, "adjacent.md") {
		t.Errorf("exact mode top = %q, want adjacent.md", exHits[0].Path)
	}
}

func writeFTSDoc(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFTSIndexBuildAndSearch(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "supabase")
	writeFTSDoc(t, src, "guides/rls.md",
		"---\ntitle: 'Row Level Security'\n---\n\nRow Level Security policies in Postgres.\n")
	writeFTSDoc(t, src, "guides/auth.md",
		"# Auth\n\nAuthentication and authorization.\n")
	m := newManifest()
	m.Entries["https://supabase.com/docs/rls"] = result{
		URL: "https://supabase.com/docs/rls", Source: "supabase",
		Path: "supabase/guides/rls.md", FetchedAt: "now",
	}
	if err := writeManifestAtomic(src, m); err != nil {
		t.Fatal(err)
	}

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	n, _ := idx.totalDocs()
	if n != 2 {
		t.Errorf("totalDocs = %d, want 2", n)
	}

	hits, err := idx.search("row level security", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits for 'row level security'")
	}
	if !strings.HasSuffix(hits[0].Path, "rls.md") {
		t.Errorf("top result = %q, want rls.md", hits[0].Path)
	}
	if hits[0].Title != "Row Level Security" {
		t.Errorf("title = %q, want Row Level Security", hits[0].Title)
	}
	if hits[0].URL != "https://supabase.com/docs/rls" {
		t.Errorf("URL = %q, want from manifest", hits[0].URL)
	}
	if len(hits[0].Snippets) == 0 {
		t.Error("expected snippets")
	}
}

func TestFTSUpdateFromMemoryIndexesWithoutReadingCopiedFile(t *testing.T) {
	out := t.TempDir()
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()

	doc := ftsMemoryDoc{
		Path:    "local/guide.md",
		AbsPath: filepath.Join(out, "local", "guide.md"),
		Body:    []byte("unique in-memory phrase"),
		URL:     "file:///source/guide.md",
		Title:   "Memory Title",
	}
	if err := idx.updateFTSFromMemory(out, []string{doc.Path}, []ftsMemoryDoc{doc}, true); err != nil {
		t.Fatal(err)
	}

	total, err := idx.totalDocs()
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("totalDocs = %d, want 1", total)
	}
	hits, err := idx.search("unique in-memory phrase", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Path != "local/guide.md" {
		t.Fatalf("hits = %+v, want local/guide.md", hits)
	}
	if hits[0].Title != "Memory Title" {
		t.Fatalf("title = %q, want Memory Title", hits[0].Title)
	}
	if hits[0].URL != "file:///source/guide.md" {
		t.Fatalf("URL = %q, want file URL", hits[0].URL)
	}
}

func TestFTSUpdateFromMemoryUsesKnownHubFlag(t *testing.T) {
	out := t.TempDir()
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()

	doc := ftsMemoryDoc{
		Path:     "local/guide.md",
		AbsPath:  filepath.Join(out, "missing", "guide.md"),
		Body:     []byte("known hub metadata phrase"),
		Title:    "Known Hub",
		HubKnown: true,
		IsHub:    true,
	}
	if err := idx.updateFTSFromMemory(out, []string{doc.Path}, []ftsMemoryDoc{doc}, true); err != nil {
		t.Fatal(err)
	}

	var isHub int
	if err := idx.db.QueryRow("SELECT is_hub FROM docs WHERE path = ?", doc.Path).Scan(&isHub); err != nil {
		t.Fatal(err)
	}
	if isHub != 1 {
		t.Fatalf("is_hub = %d, want 1 from memory metadata", isHub)
	}
}

func TestFTSRebuildSkipsIndexClutter(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "kb")
	writeFTSDoc(t, src, "keep.md", "# Keep\n\ncanonical alpha content\n")
	writeFTSDoc(t, src, "short.md", "# Short\n\nselector missed content\n")
	writeFTSDoc(t, src, "de/keep.md", "# Deutsch\n\nlocalized duplicate alpha\n")
	writeFTSDoc(t, src, "local-dev/docs-puller-retrieval-bench.md", "# Bench\n\nself referential alpha benchmark\n")
	m := newManifest()
	m.Entries["https://example.com/keep"] = result{URL: "https://example.com/keep", Source: "kb", Path: "kb/keep.md", FetchedAt: "2026-05-01T00:00:00Z"}
	m.Entries["https://example.com/short"] = result{URL: "https://example.com/short", Source: "kb", Path: "kb/short.md", FetchedAt: "2026-05-01T00:00:00Z", Warning: "low-content (31 bytes)"}
	if err := writeManifestAtomic(src, m); err != nil {
		t.Fatal(err)
	}

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	total, err := idx.totalDocs()
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("indexed docs = %d, want 1", total)
	}
	hits, err := idx.search("alpha", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "kb/keep.md" {
		t.Fatalf("hits = %+v, want only kb/keep.md", hits)
	}
}

// TestFTSRebuildIsAtomicForConcurrentReaders pins the failure-mode-#4 fix:
// rebuild() now wraps DELETE + INSERT in a single transaction, so a
// concurrent read-only opener that takes a snapshot during the rebuild
// either sees the FULL OLD state or the FULL NEW state — never an empty
// or partial intermediate. Without this, the DELETE FROM ran outside the
// txn and committed immediately, leaving readers in a `count=0` window
// for the entire INSERT phase. Symptom in production: 85/137 eval
// queries returned `backend_mode="fts5"` with empty hit lists.
func TestFTSRebuildIsAtomicForConcurrentReaders(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "demo")
	// Seed corpus: 5 docs.
	for i := 0; i < 5; i++ {
		writeFTSDoc(t, src, fmt.Sprintf("doc%d.md", i),
			fmt.Sprintf("# Doc %d\n\nbody content matchword\n", i))
	}
	// Build initial index.
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	idx.close()
	// Verify the seeded count.
	roBefore, err := openFTSIndexReadOnly(out)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := roBefore.totalDocs()
	roBefore.close()
	if n != 5 {
		t.Fatalf("seed count = %d, want 5", n)
	}
	// Open a R/W index to drive a rebuild, but pause mid-rebuild by
	// holding the txn open via direct SQL — easier than racing goroutines.
	rwIdx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer rwIdx.close()
	tx, err := rwIdx.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// Mid-rebuild simulation: DELETE then INSERT one row, but DON'T commit.
	if _, err := tx.Exec("DELETE FROM docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(
		"INSERT INTO docs(path, source, title, body, url) VALUES('demo/new.md', 'demo', 'New', 'matchword', '')"); err != nil {
		t.Fatal(err)
	}
	// While the writer txn is uncommitted, a concurrent read-only opener
	// must see the OLD state (5 docs) — not 0, not 1.
	roDuring, err := openFTSIndexReadOnly(out)
	if err != nil {
		t.Fatal(err)
	}
	nDuring, err := roDuring.totalDocs()
	roDuring.close()
	if err != nil {
		t.Fatalf("totalDocs during rebuild: %v", err)
	}
	if nDuring != 5 {
		t.Errorf("count during uncommitted rebuild = %d, want 5 (snapshot of OLD)", nDuring)
	}
	// Commit and verify NEW state visible.
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	roAfter, err := openFTSIndexReadOnly(out)
	if err != nil {
		t.Fatal(err)
	}
	nAfter, _ := roAfter.totalDocs()
	roAfter.close()
	if nAfter != 1 {
		t.Errorf("count after commit = %d, want 1 (NEW state)", nAfter)
	}
}

func TestOpenFTSIndexReadOnlyRejectsWrites(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "demo")
	writeFTSDoc(t, src, "doc.md", "# Doc\n\nreadonly marker\n")

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	idx.close()

	roIdx, err := openFTSIndexReadOnly(out)
	if err != nil {
		t.Fatal(err)
	}
	defer roIdx.close()

	hits, err := roIdx.search("readonly marker", "", 5, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "demo/doc.md" {
		t.Fatalf("readonly search hits = %+v, want demo/doc.md", hits)
	}
	if _, err := roIdx.db.Exec("INSERT INTO docs(path, source, title, path_tokens, body, url, is_hub) VALUES('demo/new.md', 'demo', 'New', 'demo/new.md', 'body', '', 0)"); err == nil {
		t.Fatal("read-only FTS index accepted a write")
	}
}

func TestOpenFTSIndexReadOnlyDoesNotCreateCacheDir(t *testing.T) {
	out := t.TempDir()
	if _, err := openFTSIndexReadOnly(out); err == nil {
		t.Fatal("read-only FTS open succeeded without an existing index")
	}
	if _, err := os.Stat(filepath.Join(out, ".cache")); !os.IsNotExist(err) {
		t.Fatalf("read-only FTS open created .cache or returned unexpected stat error: %v", err)
	}
}

func TestOpenFTSIndexReadModeRejectsUnsupportedMode(t *testing.T) {
	idx, err := openFTSIndexReadMode(t.TempDir(), "fast")
	if err == nil {
		idx.close()
		t.Fatal("unsupported FTS read mode succeeded")
	}
	if err.Error() != `unsupported FTS read mode "fast"` {
		t.Fatalf("openFTSIndexReadMode error = %v", err)
	}
}

func TestOpenFTSIndexImmutableFailsWhenWALExists(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "demo")
	writeFTSDoc(t, src, "doc.md", "# Doc\n\nimmutable marker\n")

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	idx.close()

	immutableIdx, err := openFTSIndexImmutable(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := immutableIdx.db.Exec("INSERT INTO docs(path, source, title, path_tokens, body, url, is_hub) VALUES('demo/new.md', 'demo', 'New', 'demo/new.md', 'body', '', 0)"); err == nil {
		t.Fatal("immutable FTS index accepted a write")
	}
	immutableIdx.close()

	if err := os.WriteFile(ftsDBPath(out)+"-wal", []byte("pending"), 0o644); err != nil {
		t.Fatal(err)
	}
	blockedIdx, err := openFTSIndexImmutable(out)
	if err == nil {
		blockedIdx.close()
		t.Fatal("immutable FTS open succeeded with a non-empty WAL file")
	}
	if !strings.Contains(err.Error(), "immutable FTS read requires checkpointed index") {
		t.Fatalf("immutable FTS WAL error = %v", err)
	}
}

// TestIsHubDoc covers the hub-detection contract: a doc at
// `<dir>/<basename>.md` is a hub iff `<dir>/<basename>/` exists as a
// directory with content. Drives the hub-page boost in rerank.
func TestIsHubDoc(t *testing.T) {
	root := t.TempDir()
	// Hub case: manifest.md alongside manifest/ dir with sub-pages.
	manifestMD := filepath.Join(root, "manifest.md")
	if err := os.WriteFile(manifestMD, []byte("# Manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "manifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest", "background.md"), []byte("# bg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isHubDoc(manifestMD) {
		t.Errorf("isHubDoc(manifest.md w/ sibling manifest/ dir) = false; want true")
	}
	// Non-hub case: leaf doc with no sibling subdir.
	leaf := filepath.Join(root, "manifest", "background.md")
	if isHubDoc(leaf) {
		t.Errorf("isHubDoc(leaf, no sibling background/ dir) = true; want false")
	}
	// Non-md path: defensive — never a hub.
	notMD := filepath.Join(root, "manifest")
	if isHubDoc(notMD) {
		t.Errorf("isHubDoc(non-.md path) = true; want false")
	}
}

// TestFTSWordBoundary verifies the "RAG vs storage" failure mode is fixed.
// FTS5 with porter+unicode61 tokenizes on word boundaries — "RAG" must not
// match inside "stoRAGe".
func TestFTSWordBoundary(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "azure")
	writeFTSDoc(t, src, "storage.md",
		"# az storage\n\nManage Azure Cloud Storage.\nStorage account commands.\nMore storage.\n")
	writeFTSDoc(t, src, "rag.md",
		"# RAG with Permissions\n\nRetrieval augmented generation patterns.\n")

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.search("RAG", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1 (storage must NOT match)", len(hits))
	}
	if !strings.HasSuffix(hits[0].Path, "rag.md") {
		t.Errorf("hit = %q, want rag.md", hits[0].Path)
	}
}

// TestFTSStemming verifies the porter stemmer is active — "row" should match
// "rows" / "rowing" in body text.
func TestFTSStemming(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "x")
	writeFTSDoc(t, src, "doc.md", "# Title\n\nThe rows are inserted.\n")

	idx, _ := openFTSIndex(out)
	defer idx.close()
	idx.rebuild(out)

	hits, err := idx.search("row", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("expected stemming match for 'row' → 'rows', got %d hits", len(hits))
	}
}

// TestFTSTitleTieBreaker verifies that when two docs would otherwise tie at
// the BM25 level, the doc whose title contains the query token wins. Real
// example: "row level security" should rank `row-level-security.md` above
// `secure-data.md`, even though both mention RLS prominently in body text.
func TestFTSTitleTieBreaker(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "x")
	// Both docs mention "configuration" in body the same number of times.
	// configuration.md has it in the title; off-topic.md doesn't.
	writeFTSDoc(t, src, "off-topic.md",
		"# General Reference\n\nThis page covers configuration. configuration matters. configuration is important.\n")
	writeFTSDoc(t, src, "configuration.md",
		"# Configuration Reference\n\nThis page is the configuration guide. configuration is here. configuration explained.\n")

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.search("configuration", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if !strings.HasSuffix(hits[0].Path, "configuration.md") {
		t.Errorf("rank 0 = %q (score %d) vs %q (score %d) — title-match should win",
			hits[0].Path, hits[0].Score, hits[1].Path, hits[1].Score)
	}
}

// TestFTSExactTitleMatch verifies the canonical doc wins when its title
// matches the query exactly, even at --limit 1. Regression case: "row level
// security" should top-rank a doc titled "Row Level Security" decisively over
// a doc titled "Securing your data" that has higher raw BM25 due to body
// keyword density.
func TestFTSExactTitleMatch(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "x")
	// Loser: heavy body keyword density, off-target title.
	writeFTSDoc(t, src, "secure-data.md",
		"---\ntitle: 'Securing your data'\n---\n\n"+
			strings.Repeat("Row level security is important. ", 40))
	// Winner: exact title match, modest body.
	writeFTSDoc(t, src, "rls.md",
		"---\ntitle: 'Row Level Security'\n---\n\nRLS overview.\n")

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	// Critical: at limit=1, the exact-title doc must still win even though SQL
	// might pull the keyword-dense doc first. This catches the over-fetch bug.
	hits, err := idx.search("row level security", "", 1, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits at limit 1", len(hits))
	}
	if !strings.HasSuffix(hits[0].Path, "rls.md") {
		t.Errorf("rank 0 = %q, want rls.md (exact-title-match should win at limit 1)", hits[0].Path)
	}
}

// TestFTSShortCanonicalTitleWinsOverLongVendorRedundant covers the Phase A1
// regression: a query like "supabase edge functions" should rank the
// canonical short-titled doc ("Edge Functions") above an off-canonical doc
// whose long title redundantly contains the vendor name ("Consuming
// Supabase Queue Messages with Edge Functions"). The fix: skip the
// per-token title boost for source-keyword tokens whose source matches the
// candidate's source, since the source-keyword boost (+30) already credited
// that match.
func TestFTSShortCanonicalTitleWinsOverLongVendorRedundant(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "supabase")
	// Canonical short title — does NOT redundantly contain "supabase".
	writeFTSDoc(t, src, "guides/functions.md",
		"---\ntitle: 'Edge Functions'\n---\n\nSupabase Edge Functions overview. Edge functions run at the edge.\n")
	// Off-canonical long title — title literally contains "supabase".
	writeFTSDoc(t, src, "guides/queues/consuming.md",
		"---\ntitle: 'Consuming Supabase Queue Messages with Edge Functions'\n---\n\nQueue consumers using Edge Functions. Supabase queues can be consumed via edge functions.\n")

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	hits, err := idx.search("supabase edge functions", "", 5, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if !strings.HasSuffix(hits[0].Path, "functions.md") {
		t.Errorf("rank 0 = %q (score %d) vs %q (score %d) — canonical short-title doc should win",
			hits[0].Path, hits[0].Score, hits[1].Path, hits[1].Score)
	}
}

// TestFTSPathSegmentBoost covers the Phase A3 fix: when a query token
// appears in the path (basename or any segment after the source dir),
// the candidate gets +searchPathBoost. Targets cases where the canonical
// doc has a short title that doesn't match every query token but the
// URL clearly identifies it — e.g. a doc at `slack/auth/tokens.md`
// titled "Tokens" should win over `slack/legacy/legacy-auth.md` for
// the query "slack auth tokens" because the path matches more tokens.
func TestFTSPathSegmentBoost(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "slack")
	// Canonical doc: short title, but path has "auth" + "tokens". Body
	// must mention all three query terms (slack, auth, tokens) so it
	// makes the body-tier candidate set; ranking is what we're testing.
	writeFTSDoc(t, src, "authentication/tokens.md",
		"---\ntitle: 'Tokens'\n---\n\nSlack tokens are the auth credentials for your app. Multiple types of tokens exist for different authentication scenarios.\n")
	// Off-canonical: title has "auth" but path doesn't have "tokens".
	writeFTSDoc(t, src, "legacy/legacy-authentication.md",
		"---\ntitle: 'Legacy authentication'\n---\n\n"+
			strings.Repeat("Slack legacy authentication tokens used a different oauth flow. ", 10))

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	hits, err := idx.search("slack auth tokens", "", 5, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if !strings.HasSuffix(hits[0].Path, "authentication/tokens.md") {
		t.Errorf("rank 0 = %q (score %d) vs %q (score %d) — path-segment match should win",
			hits[0].Path, hits[0].Score, hits[1].Path, hits[1].Score)
	}
}

func TestFTSTitleBasenameAlignmentBoostsCanonicalGuide(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "slack")
	writeFTSDoc(t, src, "authentication/verifying-requests-from-slack.md",
		"# Verifying requests from Slack\n\nSlack request signatures let apps verify requests from Slack with a signing secret.\n")
	writeFTSDoc(t, src, "tools/python-slack-sdk/reference/signature.md",
		"# Module slack_sdk.signature\n\n"+
			strings.Repeat("Slack request signature verifier can verify request signatures. ", 8))

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	hits, err := idx.search("slack verify request signature", "", 5, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if !strings.HasSuffix(hits[0].Path, "authentication/verifying-requests-from-slack.md") {
		t.Errorf("rank 0 = %q (score %d) vs %q (score %d) — title/basename-aligned canonical guide should win",
			hits[0].Path, hits[0].Score, hits[1].Path, hits[1].Score)
	}
}

func TestFTSTitleBasenameTieBreaksExactSingleTopic(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "apple-devicemanagement")
	writeFTSDoc(t, src, "passcode.md",
		"# Passcode\n\nThe payload that configures a passcode policy for MDM.\n")
	writeFTSDoc(t, src, "implementing-device-management.md",
		"# Implementing Device Management\n\n"+
			strings.Repeat("MDM device management includes profiles, passcode payloads, and policy deployment. ", 8))

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	hits, err := idx.search("passcode policy mdm", "", 5, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if !strings.HasSuffix(hits[0].Path, "passcode.md") {
		t.Errorf("rank 0 = %q (score %d) vs %q (score %d) — exact title/basename topic should win ties",
			hits[0].Path, hits[0].Score, hits[1].Path, hits[1].Score)
	}
}

// TestFTSBasenameExactBoost covers the Phase A5 fix: when a query token
// equals the path basename verbatim, give a tiebreaker boost. Targets
// canonical short-named docs (e.g. `tokens.md`) over body-keyword-dense
// adjacent docs.
func TestFTSBasenameExactBoost(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "slack")
	writeFTSDoc(t, src, "authentication/tokens.md",
		"---\ntitle: 'Tokens'\n---\n\nSlack tokens are credentials. Multiple types of tokens.\n")
	writeFTSDoc(t, src, "authentication/installing-with-oauth.md",
		"---\ntitle: 'Installing with OAuth'\n---\n\n"+
			strings.Repeat("Slack tokens via oauth installation. ", 20))

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	hits, err := idx.search("slack tokens", "", 5, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if !strings.HasSuffix(hits[0].Path, "authentication/tokens.md") {
		t.Errorf("rank 0 = %q (score %d) vs %q (score %d) — basename-exact match should win",
			hits[0].Path, hits[0].Score, hits[1].Path, hits[1].Score)
	}
}

// TestBasenameStemMatch verifies the stem-aware basename comparison covers
// plurals (jwt/jwts, hyperloglog/hyperloglogs) and Y-IES (policy/policies)
// without false positives like token/tokenize.
func TestBasenameStemMatch(t *testing.T) {
	cases := []struct {
		basename, tok string
		want          bool
	}{
		{"jwts", "jwt", true},
		{"jwt", "jwts", true},
		{"hyperloglogs", "hyperloglog", true},
		{"tokens", "tokens", true},
		{"policies", "policy", true},
		{"policy", "policies", true},
		{"boxes", "box", true},
		// Strict: shouldn't match unrelated suffixes.
		{"tokenize", "token", false},
		{"performance", "perform", false},
		{"build", "builds", true},   // commutative
		{"buildup", "build", false}, // strict
	}
	for _, c := range cases {
		if got := basenameStemMatch(c.basename, c.tok); got != c.want {
			t.Errorf("basenameStemMatch(%q, %q) = %v, want %v", c.basename, c.tok, got, c.want)
		}
	}
}

// TestFTSBasenameExactSkipsSynonymTokens verifies basename-exact does NOT
// fire on synonym-class tokens like "auth" — otherwise `auth.md` would
// steal from canonical `tokens.md` for "slack auth tokens", since the
// query mentions auth but means the authentication concept (already
// handled by synonym expansion) rather than a doc named exactly "auth".
func TestFTSBasenameExactSkipsSynonymTokens(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "slack")
	writeFTSDoc(t, src, "authentication/tokens.md",
		"---\ntitle: 'Tokens'\n---\n\nSlack auth tokens overview.\n")
	writeFTSDoc(t, src, "tools/python-slack-sdk/legacy/auth.md",
		"---\ntitle: 'Auth'\n---\n\n"+
			strings.Repeat("Slack auth tokens for legacy SDK. ", 30))

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	hits, err := idx.search("slack auth tokens", "", 5, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if !strings.HasSuffix(hits[0].Path, "authentication/tokens.md") {
		t.Errorf("rank 0 = %q (score %d) vs %q (score %d) — synonym-class basename match should be skipped",
			hits[0].Path, hits[0].Score, hits[1].Path, hits[1].Score)
	}
}

func TestFTSSourceFilter(t *testing.T) {
	out := t.TempDir()
	writeFTSDoc(t, filepath.Join(out, "a"), "doc.md", "# x\n\nfoo content\n")
	writeFTSDoc(t, filepath.Join(out, "b"), "doc.md", "# y\n\nfoo content\n")

	idx, _ := openFTSIndex(out)
	defer idx.close()
	idx.rebuild(out)

	hits, _ := idx.search("foo", "a", 10, false, nil, false)
	if len(hits) != 1 || hits[0].Source != "a" {
		t.Errorf("source filter failed: %+v", hits)
	}
}

// TestLiveFTSIndexReopensOnMtimeAdvance verifies the persistent serve
// connection picks up out-of-process index rebuilds. Symptom this guards
// against: a `pull-url` (which rewrites search.db) leaves serve returning
// stale results until restart.
func TestLiveFTSIndexReopensOnMtimeAdvance(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "s")
	writeFTSDoc(t, src, "a.md", "# A\n\nalpha content\n")
	idx0, _ := openFTSIndex(out)
	if err := idx0.rebuild(out); err != nil {
		t.Fatal(err)
	}
	idx0.close()

	live, err := newLiveFTSIndex(out)
	if err != nil || live == nil {
		t.Fatalf("newLiveFTSIndex: live=%v err=%v", live, err)
	}
	defer live.close()

	var hits []searchHit
	live.withSearch(func(i *ftsIndex) { hits, _ = i.search("alpha", "", 10, false, nil, false) })
	if len(hits) != 1 {
		t.Fatalf("first 'alpha' search: %d hits, want 1", len(hits))
	}
	live.withSearch(func(i *ftsIndex) { hits, _ = i.search("beta", "", 10, false, nil, false) })
	if len(hits) != 0 {
		t.Fatalf("'beta' before rebuild: %d hits, want 0", len(hits))
	}

	// Out-of-process rebuild adds a new doc.
	writeFTSDoc(t, src, "b.md", "# B\n\nbeta content\n")
	idx2, _ := openFTSIndex(out)
	if err := idx2.rebuild(out); err != nil {
		t.Fatal(err)
	}
	idx2.close()

	// Force mtime advance: some filesystems have second-granularity mtime
	// and back-to-back rebuilds may not visibly bump it. Real-world reindex
	// happens after enough wall clock for mtime to differ; we simulate that.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(ftsDBPath(out), future, future); err != nil {
		t.Fatal(err)
	}

	live.withSearch(func(i *ftsIndex) { hits, _ = i.search("beta", "", 10, false, nil, false) })
	if len(hits) != 1 {
		t.Errorf("after rebuild + mtime advance: 'beta' hits = %d, want 1 (live should reopen)", len(hits))
	}
}

func TestFTSIndexExistsFalseWhenEmpty(t *testing.T) {
	out := t.TempDir()
	if ftsIndexExists(out) {
		t.Error("ftsIndexExists should be false before any pull")
	}
	// Create empty index.
	idx, _ := openFTSIndex(out)
	idx.close()
	if ftsIndexExists(out) {
		t.Error("ftsIndexExists should be false for an empty index (0 docs)")
	}
}

// TestFTSUpsertReplacesRowByPath verifies the incremental update path:
// upserting a previously-indexed path replaces the body, doesn't duplicate
// it, and updates the title.
func TestFTSUpsertReplacesRowByPath(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "supabase")
	writeFTSDoc(t, src, "guides/x.md", "# Old Title\n\nold body content\n")
	m := newManifest()
	m.Entries["https://x"] = result{
		URL: "https://x", Source: "supabase",
		Path: "supabase/guides/x.md", FetchedAt: "now",
	}
	if err := writeManifestAtomic(src, m); err != nil {
		t.Fatal(err)
	}

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	// Mutate the file and upsert the changed path.
	writeFTSDoc(t, src, "guides/x.md", "# New Title\n\nfresh body content keyword\n")
	if err := idx.updateFTS(out, []string{"supabase/guides/x.md"}); err != nil {
		t.Fatal(err)
	}

	n, _ := idx.totalDocs()
	if n != 1 {
		t.Errorf("totalDocs after upsert = %d, want 1 (no duplication)", n)
	}
	hits, err := idx.search("keyword", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search 'keyword' got %d hits, want 1", len(hits))
	}
	if hits[0].Title != "New Title" {
		t.Errorf("title = %q, want 'New Title' (upsert should refresh metadata)", hits[0].Title)
	}
	// Old body should no longer match.
	old, _ := idx.search("old", "", 10, false, nil, false)
	if len(old) != 0 {
		t.Errorf("expected old body to be evicted, got %+v", old)
	}
}

// TestFTSDocsPathStaysInSync verifies the side table tracks every row in
// docs across rebuild + upsert cycles. Drift here would mean DELETE-by-path
// silently misses rows, leaving stale snippets in search results.
func TestFTSDocsPathStaysInSync(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "s")
	writeFTSDoc(t, src, "a.md", "# A\n\na body\n")
	writeFTSDoc(t, src, "b.md", "# B\n\nb body\n")

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	checkSync := func(label string, wantDocs int) {
		t.Helper()
		var docs, paths int
		idx.db.QueryRow("SELECT count(*) FROM docs").Scan(&docs)
		idx.db.QueryRow("SELECT count(*) FROM docs_path").Scan(&paths)
		if docs != wantDocs || paths != wantDocs {
			t.Errorf("%s: docs=%d docs_path=%d, want both=%d", label, docs, paths, wantDocs)
		}
	}
	checkSync("after rebuild", 2)

	// Upsert one existing path: counts should stay at 2.
	writeFTSDoc(t, src, "a.md", "# A2\n\nfresh a\n")
	if err := idx.updateFTS(out, []string{"s/a.md"}); err != nil {
		t.Fatal(err)
	}
	checkSync("after upsert existing", 2)

	// Upsert a new path: counts should rise to 3.
	writeFTSDoc(t, src, "c.md", "# C\n\nc body\n")
	if err := idx.updateFTS(out, []string{"s/c.md"}); err != nil {
		t.Fatal(err)
	}
	checkSync("after upsert new", 3)

	// Upsert a path whose file has been deleted: counts should fall to 2.
	if err := os.Remove(filepath.Join(src, "b.md")); err != nil {
		t.Fatal(err)
	}
	if err := idx.updateFTS(out, []string{"s/b.md"}); err != nil {
		t.Fatal(err)
	}
	checkSync("after upsert deleted-file", 2)
}

func TestFTSRepeatPullRepairsMissingUnchangedPath(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "s")
	writeFTSDoc(t, src, "a.md", "# A\n\nalpha\n")
	writeFTSDoc(t, src, "b.md", "# B\n\nrepairabletoken\n")

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	rowID, err := idx.q.LookupDocRowIDByPath(context.Background(), "s/b.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.q.DeleteDocByRowID(context.Background(), rowID); err != nil {
		t.Fatal(err)
	}

	repaired, err := idx.updateFTSAndRepair(out, nil, []string{"s/a.md", "s/b.md"})
	if err != nil {
		t.Fatal(err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}
	hits, err := idx.search("repairabletoken", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "s/b.md" {
		t.Fatalf("repaired search hits = %+v, want s/b.md", hits)
	}

	// The inverse partial drift (orphaned FTS row with no side-table mapping)
	// must also repair without duplicating the document.
	if err := idx.q.DeletePathByPath(context.Background(), "s/b.md"); err != nil {
		t.Fatal(err)
	}
	repaired, err = idx.updateFTSAndRepair(out, nil, []string{"s/a.md", "s/b.md"})
	if err != nil {
		t.Fatal(err)
	}
	if repaired != 1 {
		t.Fatalf("orphan repair count = %d, want 1", repaired)
	}
	n, err := idx.totalDocs()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("total docs after orphan repair = %d, want 2", n)
	}
}

func TestFTSReplaceSourcesPrunesStalePinnedDocs(t *testing.T) {
	out := t.TempDir()
	pinned := filepath.Join(out, "react__v18")
	latest := filepath.Join(out, "react")
	writeFTSDoc(t, pinned, "index.md", "# Old pinned\n\noldpinnedtoken")
	writeFTSDoc(t, latest, "reference/react.md", "# Latest React\n\nlatesttoken")

	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	if hits, err := idx.search("oldpinnedtoken", "", 10, false, nil, false); err != nil || len(hits) != 1 {
		t.Fatalf("old pinned seed search hits=%+v err=%v, want one hit", hits, err)
	}

	if err := os.RemoveAll(pinned); err != nil {
		t.Fatal(err)
	}
	writeFTSDoc(t, pinned, "reference/react/useState.md", "# useState\n\nnewpinnedtoken")
	if err := idx.replaceSources(out, []string{"react__v18"}); err != nil {
		t.Fatal(err)
	}

	n, _ := idx.totalDocs()
	if n != 2 {
		t.Fatalf("totalDocs after replaceSources = %d, want 2", n)
	}
	if hits, err := idx.search("oldpinnedtoken", "", 10, false, nil, false); err != nil || len(hits) != 0 {
		t.Fatalf("old pinned token hits=%+v err=%v, want none", hits, err)
	}
	hits, err := idx.search("newpinnedtoken", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "react__v18/reference/react/useState.md" {
		t.Fatalf("new pinned token hits=%+v, want react__v18/reference/react/useState.md", hits)
	}
	hits, err = idx.search("latesttoken", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "react/reference/react.md" {
		t.Fatalf("latest source hits=%+v, want react/reference/react.md", hits)
	}
}

// TestFTSUpdateFallsBackToRebuildOnColdStart guarantees a fresh index gets
// fully populated from disk even when the caller passes an empty changed-paths
// list (e.g. an `init`-style flow that creates the index without a pull).
func TestFTSUpdateFallsBackToRebuildOnColdStart(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "supabase")
	writeFTSDoc(t, src, "a.md", "# A\n\nalpha\n")
	writeFTSDoc(t, src, "b.md", "# B\n\nbeta\n")

	idx, _ := openFTSIndex(out)
	defer idx.close()
	// Pass empty paths: cold-start fallback should rebuild from disk.
	if err := idx.updateFTS(out, nil); err != nil {
		t.Fatal(err)
	}
	n, _ := idx.totalDocs()
	if n != 2 {
		t.Errorf("totalDocs after cold-start updateFTS = %d, want 2", n)
	}
}

func TestExtractSnippetsFromText(t *testing.T) {
	body := "preamble\n\nfoo bar\nirrelevant\nfoo baz\nirrelevant\nfoo qux\nirrelevant\nfoo extra\n"
	got := extractSnippetsFromText(body, "foo")
	if len(got) != searchSnippetMax {
		t.Errorf("got %d snippets, want capped at %d", len(got), searchSnippetMax)
	}
	if got[0].Line != 3 {
		t.Errorf("first snippet line = %d, want 3", got[0].Line)
	}
}

// TestExtractSnippetsMultiTokenRanksByDensity covers the case that broke in
// the live ClickHouse "best practices large queries" demo: multi-word queries
// where no single line contains the full phrase. Lines should still match if
// they contain ANY token, and the line with the most distinct tokens wins.
func TestExtractSnippetsMultiTokenRanksByDensity(t *testing.T) {
	body := "intro line\n\n" +
		"this mentions best practices for queries\n" + // 3 tokens
		"single mention of practices alone\n" + // 1 token
		"covers best practices on large data\n" + // 3 tokens
		"another line with queries only\n" // 1 token
	got := extractSnippetsFromText(body, "best practices large queries")
	if len(got) == 0 {
		t.Fatal("expected snippets for multi-word query, got none")
	}
	// Highest-density lines (3 tokens) should come first.
	if !strings.Contains(got[0].Text, "best practices") {
		t.Errorf("rank 0 = %q, expected a 3-token line", got[0].Text)
	}
}

// TestFtsBuildTitleQuery pins the documented edge cases for source-keyword
// stripping in the title-tier query builder. The function has surprising
// fall-back behavior (all-source-keyword queries don't return "" — they
// fall back to all tokens) and exact-mode interacts with stripping in a
// non-obvious way; both are covered here so future tuning doesn't drift.
func TestFtsBuildTitleQuery(t *testing.T) {
	cases := []struct {
		name            string
		query           string
		exact           bool
		wantTitleQ      string
		wantInferredSrc string
	}{
		{
			name: "empty input returns empty",
		},
		{
			name:       "pure non-source query keeps all tokens",
			query:      "rate limit",
			wantTitleQ: `title:("rate" "limit")`,
		},
		{
			name:            "mixed source+content strips and infers",
			query:           "supabase storage",
			wantTitleQ:      `title:("storage")`,
			wantInferredSrc: "supabase",
		},
		{
			name:            "mixed exact mode quotes the post-strip phrase",
			query:           "supabase storage",
			exact:           true,
			wantTitleQ:      `title:("storage")`,
			wantInferredSrc: "supabase",
		},
		{
			name:       "bare source name falls back to all tokens (no inferred src)",
			query:      "slack",
			wantTitleQ: `title:("slack")`,
		},
		{
			name:       "all-source-keyword multi-token falls back to all tokens",
			query:      "supabase azure",
			wantTitleQ: `title:("supabase" "azure")`,
		},
		{
			name:            "trailing source keyword still strips and infers",
			query:           "row level security supabase",
			wantTitleQ:      `title:("row" "level" "security")`,
			wantInferredSrc: "supabase",
		},
		{
			// Phrase-only fallback: when a phraseSynonyms token is in the
			// query, OR-in the bare canonical phrase as a second
			// title-tier qualification path. Lets docs whose titles
			// contain the phrase qualify even when other query tokens
			// (e.g. "recursive") aren't in the title — covers the
			// `postgres recursive cte` → `queries-with.md` miss.
			name:            "phraseSynonyms token adds phrase-only fallback",
			query:           "postgres recursive cte",
			wantTitleQ:      `title:(("recursive" AND ("cte" OR "common table expression")) OR "common table expression")`,
			wantInferredSrc: "postgresql",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotTitleQ, gotSrc := ftsBuildTitleQuery(c.query, c.exact)
			if gotTitleQ != c.wantTitleQ {
				t.Errorf("titleQ = %q, want %q", gotTitleQ, c.wantTitleQ)
			}
			if gotSrc != c.wantInferredSrc {
				t.Errorf("inferredSource = %q, want %q", gotSrc, c.wantInferredSrc)
			}
		})
	}
}

// TestDepthPenalty covers the Phase B10 path-depth penalty math. Verifies
// per-segment penalty and cap saturation. Boundary cases matter because the
// constants gate which canonicals win their own subtree.
func TestDepthPenalty(t *testing.T) {
	cases := []struct {
		pathSansSource string
		want           int
	}{
		{"manifest.md", 0},                                   // depth 0 (basename only)
		{"reference/manifest.md", -2},                        // depth 1
		{"docs/reference/manifest.md", -4},                   // depth 2
		{"docs/extensions/reference/manifest.md", -6},        // depth 3 (chrome canonical)
		{"docs/extensions/reference/manifest/author.md", -8}, // depth 4 (chrome sub-page)
		{"a/b/c/d/e/f/g/h/leaf.md", -16},                     // depth 8 — at cap
		{"a/b/c/d/e/f/g/h/i/leaf.md", -16},                   // depth 9 — capped
		{"", 0},                                              // empty
	}
	for _, c := range cases {
		t.Run(c.pathSansSource, func(t *testing.T) {
			got := depthPenalty(c.pathSansSource)
			if got != c.want {
				t.Errorf("depthPenalty(%q) = %d, want %d", c.pathSansSource, got, c.want)
			}
		})
	}
}

// TestFTSBuildPathQuery mirrors TestFTSBuildTitleQuery for the path-tier
// analog. Same source-stripping + phrase-synonym + exact-mode semantics
// because both paths run through buildColumnQuery — keeping a separate
// test guards against accidental column-name divergence.
func TestFTSBuildPathQuery(t *testing.T) {
	cases := []struct {
		name            string
		query           string
		exact           bool
		wantPathQ       string
		wantInferredSrc string
	}{
		{
			name:            "basic — chrome stripped, source inferred",
			query:           "chrome extensions manifest",
			wantPathQ:       `path_tokens:("extensions" "manifest")`,
			wantInferredSrc: "chrome",
		},
		{
			name:            "no source keyword — no inference",
			query:           "extensions manifest",
			wantPathQ:       `path_tokens:("extensions" "manifest")`,
			wantInferredSrc: "",
		},
		{
			name:            "phrase synonym appended for acronym",
			query:           "supabase rls",
			wantPathQ:       `path_tokens:((("rls" OR "row level security")) OR "row level security")`,
			wantInferredSrc: "supabase",
		},
		{
			name:            "exact mode wraps as one phrase",
			query:           "chrome extensions manifest",
			exact:           true,
			wantPathQ:       `path_tokens:("extensions manifest")`,
			wantInferredSrc: "chrome",
		},
		{
			name:            "all tokens are source keywords — fall back to all tokens, no inference",
			query:           "supabase",
			wantPathQ:       `path_tokens:("supabase")`,
			wantInferredSrc: "",
		},
		{
			name:            "empty query",
			query:           "",
			wantPathQ:       "",
			wantInferredSrc: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPathQ, gotSrc := ftsBuildPathQuery(c.query, c.exact)
			if gotPathQ != c.wantPathQ {
				t.Errorf("pathQ = %q, want %q", gotPathQ, c.wantPathQ)
			}
			if gotSrc != c.wantInferredSrc {
				t.Errorf("inferredSource = %q, want %q", gotSrc, c.wantInferredSrc)
			}
		})
	}
}

// TestFTSPathTierLiftsShortTitleCanonical guards the chrome smoking-gun
// Phase B10 fix: a doc whose title doesn't carry every query token but
// whose path_tokens do should still qualify for the title-tier base-score
// floor via path-tier. Without this the canonical hub doc with a short
// title gets buried under sub-pages whose long titles repeat all query
// tokens.
func TestFTSPathTierLiftsShortTitleCanonical(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "chrome")
	// Canonical hub doc — short title missing query token "extensions",
	// but path_tokens cover [extensions, manifest].
	writeFTSDoc(t, src, "docs/extensions/reference/manifest.md",
		"---\ntitle: 'Manifest file format'\n---\n\nManifest documents Chrome extensions metadata. Defines all extension fields.\n")
	// Sub-page — long title contains all query tokens; without path-tier
	// this would dominate via title-tier alone.
	writeFTSDoc(t, src, "docs/extensions/reference/manifest/author.md",
		"---\ntitle: 'Manifest - Author | Chrome Extensions'\n---\n\n"+
			strings.Repeat("Author field of Chrome extensions manifest. ", 10))
	idx, _ := openFTSIndex(out)
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.search("chrome extensions manifest", "", 5, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	// Canonical at rank 0 — beats descendant via path-tier floor + hub
	// boost + basename-exact + smaller depth penalty.
	if !strings.HasSuffix(hits[0].Path, "reference/manifest.md") {
		t.Errorf("rank 0 = %q (score %d) vs %q (score %d) — canonical hub should win on path-tier qualification",
			hits[0].Path, hits[0].Score, hits[1].Path, hits[1].Score)
	}
}

// TestChromeSourceKeywordRegistered locks in the 2026-04-30 `chrome` →
// sourceKeywordPairs addition. Without this entry, "chrome" is treated as
// a regular query token, gets counted in per-token title boost for sub-
// pages whose titles redundantly contain "Chrome Extensions", and no
// source-keyword stripping happens for title-tier/path-tier queries.
func TestChromeSourceKeywordRegistered(t *testing.T) {
	srcs := sourcesFromQueryTokens(tokenizeForFTS("chrome extensions manifest"))
	if !srcs["chrome"] {
		t.Fatalf("chrome query should source-boost chrome; got %+v — was 'chrome' removed from sourceKeywordPairs?", srcs)
	}
	// Sanity: ftsBuildTitleQuery / ftsBuildPathQuery should strip chrome.
	tq, inferred := ftsBuildTitleQuery("chrome extensions manifest", false)
	if strings.Contains(tq, "chrome") {
		t.Errorf("ftsBuildTitleQuery(%q) = %q — chrome should be stripped", "chrome extensions manifest", tq)
	}
	if inferred != "chrome" {
		t.Errorf("inferredSource = %q, want chrome", inferred)
	}
}
