package docspack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

var boilerPhrases = []string{
	"newsletter", "subscribe", "promotional", "Skip to main content",
	"global header branding", "Copyright Acme",
}

// --- Clean (mirrors Python CleanTests) ---

func TestBoilerplateRemoved(t *testing.T) {
	text := strings.ToLower(CleanToText(loadFixture(t, "nav_heavy.html")))
	for _, p := range boilerPhrases {
		if strings.Contains(text, strings.ToLower(p)) {
			t.Errorf("boilerplate leaked: %s", p)
		}
	}
}

func TestContentKept(t *testing.T) {
	text := CleanToText(loadFixture(t, "nav_heavy.html"))
	if !strings.Contains(text, "Widgets are small reusable components") {
		t.Error("main content missing")
	}
	if !strings.Contains(text, "npm install @acme/widgets") {
		t.Error("install command missing")
	}
}

func TestLongCodeBlockSurvives(t *testing.T) {
	segs := CleanHTML(loadFixture(t, "code_heavy.html"))
	var pres []string
	for _, s := range segs {
		if s.Tag == "pre" {
			pres = append(pres, s.Text)
		}
	}
	if len(pres) != 1 {
		t.Fatalf("want exactly 1 pre segment, got %d", len(pres))
	}
	if len(pres[0]) <= 2000 {
		t.Errorf("pre block truncated at ragzip cap: %d chars", len(pres[0]))
	}
}

// --- Chunk (mirrors Python ChunkTests) ---

func TestHeadingBoundariesRespected(t *testing.T) {
	chunks := ChunkHTML(loadFixture(t, "nav_heavy.html"), "http://x.test/widgets")
	byPath := map[string][]string{}
	for _, c := range chunks {
		k := strings.Join(c.HeadingPath, " > ")
		byPath[k] = append(byPath[k], c.Text)
	}
	config := "Widgets Guide > Configuration"
	texts, ok := byPath[config]
	if !ok {
		t.Fatalf("no chunks under %q; have %v", config, keys(byPath))
	}
	for _, text := range texts {
		if strings.Contains(text, "Install the widget toolkit") {
			t.Error("chunk crossed an h2 boundary")
		}
	}
}

func TestCitationFormat(t *testing.T) {
	chunks := ChunkHTML(loadFixture(t, "nav_heavy.html"), "http://x.test/widgets")
	if len(chunks) == 0 {
		t.Fatal("no chunks produced")
	}
	for i, c := range chunks {
		want := "http://x.test/widgets#chunk" + itoa(i+1)
		if c.Citation != want {
			t.Errorf("chunk %d citation = %q, want %q", i, c.Citation, want)
		}
		if c.Source != "http://x.test/widgets" {
			t.Errorf("chunk %d source = %q", i, c.Source)
		}
		if c.Tokens <= 0 {
			t.Errorf("chunk %d tokens = %d", i, c.Tokens)
		}
	}
}

// --- Pack (mirrors Python PackTests) ---

func TestDedupeAndBudget(t *testing.T) {
	base := "Every request must carry an API key in the x-api-key header " +
		"for access and keys can be rotated at any time from the " +
		"dashboard settings page safely and quickly"
	mk := func(citation, source string, path []string, text string) Chunk {
		return Chunk{Citation: citation, Source: source, HeadingPath: path,
			Text: text, Tokens: EstimateTokens(text)}
	}
	chunks := []Chunk{
		mk("http://x.test/a#chunk1", "http://x.test/a", []string{"API", "Authentication"}, base),
		mk("http://x.test/b#chunk1", "http://x.test/b", []string{"API", "Authentication"}, base+" indeed"),
		mk("http://x.test/b#chunk2", "http://x.test/b", []string{"API", "Webhooks"},
			"Webhooks deliver event notifications to your server over HTTPS POST calls whenever something changes"),
	}
	selected, stats := SelectPack(chunks, 800)
	auth := 0
	for _, c := range selected {
		if strings.Contains(c.Text, "Every request must carry an API key") {
			auth++
		}
	}
	if auth != 1 {
		t.Errorf("near-duplicate not deduped: %d auth chunks selected", auth)
	}
	if stats.ChunksDroppedDupl != 1 {
		t.Errorf("dropped_duplicate = %d, want 1", stats.ChunksDroppedDupl)
	}
}

func TestMultiPagePackBudget(t *testing.T) {
	sources := []SourceDoc{
		{"http://x.test/a", loadFixture(t, "dup_heavy_a.html")},
		{"http://x.test/b", loadFixture(t, "dup_heavy_b.html")},
	}
	_, stats, all := BuildPack(sources, 120)
	if len(all) == 0 {
		t.Fatal("no chunks built")
	}
	if stats.TokensSelected > 120 {
		t.Errorf("budget exceeded: %d > 120", stats.TokensSelected)
	}
}

func TestSelectPackRespectsSmallBudget(t *testing.T) {
	_, stats, _ := BuildPack([]SourceDoc{{"http://x.test/w", loadFixture(t, "nav_heavy.html")}}, 50)
	if stats.TokensSelected > 50 {
		t.Errorf("budget exceeded: %d > 50", stats.TokensSelected)
	}
}

// --- Sitemap (mirrors Python SitemapTests) ---

func TestParseURLSetAndIndex(t *testing.T) {
	xmlText := `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
		`<url><loc>https://ex.com/docs/a</loc></url>` +
		`<url><loc>https://ex.com/docs/b</loc></url></urlset>`
	pages, children := ParseSitemap(xmlText)
	if len(pages) != 2 || pages[0] != "https://ex.com/docs/a" || pages[1] != "https://ex.com/docs/b" {
		t.Errorf("pages = %v", pages)
	}
	if len(children) != 0 {
		t.Errorf("children = %v", children)
	}
	idx := `<?xml version="1.0"?><sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
		`<sitemap><loc>https://ex.com/sm1.xml</loc></sitemap></sitemapindex>`
	pages, children = ParseSitemap(idx)
	if len(children) != 1 || children[0] != "https://ex.com/sm1.xml" {
		t.Errorf("index children = %v", children)
	}
	if len(pages) != 0 {
		t.Errorf("index pages = %v", pages)
	}
}

func TestFilterExcludesPromo(t *testing.T) {
	urls := []string{"https://ex.com/docs/a", "https://ex.com/newsletter-signup", "https://ex.com/promo-sale"}
	got := FilterURLs(urls, nil, nil)
	if len(got) != 1 || got[0] != "https://ex.com/docs/a" {
		t.Errorf("filtered = %v", got)
	}
}

func TestFollowLinksFallback(t *testing.T) {
	fake := func(u string) (string, bool) {
		if strings.HasSuffix(u, "/sitemap.xml") {
			return "", false
		}
		return `<html><body><a href="/docs/one">One page of docs</a>` +
			`<a href="https://other.com/x">External link here</a>` +
			`<a href="/newsletter">Newsletter signup page</a></body></html>`, true
	}
	urls := DiscoverURLs("https://ex.com/start", 100, fake, nil, nil)
	contains := func(s string) bool {
		for _, u := range urls {
			if u == s {
				return true
			}
		}
		return false
	}
	if !contains("https://ex.com/docs/one") {
		t.Errorf("missing same-domain link: %v", urls)
	}
	if contains("https://other.com/x") {
		t.Error("external link not filtered")
	}
	if contains("https://ex.com/newsletter") {
		t.Error("newsletter link not excluded")
	}
}

func keys(m map[string][]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
