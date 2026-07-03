package main

import (
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLoadSitemapURLSet(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/a</loc></url>
  <url><loc>https://example.com/b</loc></url>
  <url><loc>https://example.com/c</loc></url>
</urlset>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(xml))
	}))
	defer srv.Close()

	urls, err := loadSitemap(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://example.com/a", "https://example.com/b", "https://example.com/c"}
	if !reflect.DeepEqual(urls, want) {
		t.Errorf("got %v, want %v", urls, want)
	}
}

func TestLoadSitemapIndexFollows(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		base := "http://" + r.Host
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>` + base + `/sub1.xml</loc></sitemap>
  <sitemap><loc>` + base + `/sub2.xml</loc></sitemap>
</sitemapindex>`))
	})
	mux.HandleFunc("/sub1.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/x1</loc></url>
  <url><loc>https://example.com/x2</loc></url>
</urlset>`))
	})
	mux.HandleFunc("/sub2.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/y1</loc></url>
</urlset>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	urls, err := loadSitemap(srv.URL + "/sitemap.xml")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://example.com/x1",
		"https://example.com/x2",
		"https://example.com/y1",
	}
	if !reflect.DeepEqual(urls, want) {
		t.Errorf("got %v, want %v", urls, want)
	}
}

func TestLoadSitemapGzipped(t *testing.T) {
	xml := `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/gz</loc></url>
</urlset>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		gw := gzip.NewWriter(w)
		gw.Write([]byte(xml))
		gw.Close()
	}))
	defer srv.Close()

	urls, err := loadSitemap(srv.URL + "/sitemap.xml.gz")
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 || urls[0] != "https://example.com/gz" {
		t.Errorf("got %v", urls)
	}
}

func TestLoadSitemapRejectsUnexpectedRoot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(`<feed><entry>not a sitemap</entry></feed>`))
	}))
	defer srv.Close()

	urls, err := loadSitemap(srv.URL + "/sitemap.xml")
	if err == nil {
		t.Fatalf("expected unexpected-root error, got urls=%v", urls)
	}
	want := "unexpected sitemap root <feed> at " + srv.URL + "/sitemap.xml"
	if err.Error() != want {
		t.Fatalf("loadSitemap error = %v, want %q", err, want)
	}
}

// TestLoadSitemapResolvesRelative verifies that root-relative <loc> values
// (used by cli.github.com) get resolved against the sitemap URL, so
// downstream filter+pull see absolute URLs.
func TestLoadSitemapResolvesRelative(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>/manual/gh_repo</loc></url>
  <url><loc>/manual/gh_pr</loc></url>
  <url><loc>https://other.example/full</loc></url>
</urlset>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(xml))
	}))
	defer srv.Close()

	urls, err := loadSitemap(srv.URL + "/sitemap.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 3 {
		t.Fatalf("got %d urls, want 3", len(urls))
	}
	if !strings.HasPrefix(urls[0], srv.URL+"/manual/") {
		t.Errorf("urls[0] = %q, want resolved to %s/manual/...", urls[0], srv.URL)
	}
	if urls[2] != "https://other.example/full" {
		t.Errorf("urls[2] = %q, want preserved absolute URL", urls[2])
	}
}

// TestFilterURLsDedupsLocaleVariants reproduces the Chrome devsite case:
// the sitemap emits one <loc> per locale via ?hl=<code>. Without dedup we'd
// pull every locale; with last-write-wins the file would end up in Indonesian
// or Arabic. The dedup picks the no-query canonical URL.
func TestFilterURLsDedupsLocaleVariants(t *testing.T) {
	in := []string{
		"https://developer.chrome.com/docs/extensions/oauth",
		"https://developer.chrome.com/docs/extensions/oauth?hl=zh-TW",
		"https://developer.chrome.com/docs/extensions/oauth?hl=ar",
		"https://developer.chrome.com/docs/extensions/oauth?hl=de",
		"https://developer.chrome.com/docs/extensions/storage",
		"https://developer.chrome.com/docs/extensions/storage?hl=fr",
	}
	got := filterURLs(in, "", 0)
	if len(got) != 2 {
		t.Fatalf("got %d urls, want 2 (deduped to canonical paths): %v", len(got), got)
	}
	for _, u := range got {
		if strings.Contains(u, "hl=") {
			t.Errorf("locale-tagged URL leaked through dedup: %s", u)
		}
	}
}

// When only locale variants exist (no canonical), pick one of them so we
// don't drop the page entirely.
func TestFilterURLsKeepsLocaleVariantWhenNoCanonical(t *testing.T) {
	in := []string{
		"https://example.com/page?hl=zh-TW",
		"https://example.com/page?hl=de",
	}
	got := filterURLs(in, "", 0)
	if len(got) != 1 {
		t.Errorf("expected 1 URL kept (locale-only), got %d: %v", len(got), got)
	}
}

func TestFilterURLsDedupesFragments(t *testing.T) {
	in := []string{
		"https://zod.dev/api?id=objects",
		"https://zod.dev/api?id=strings",
		"https://zod.dev/api#arrays",
		"https://zod.dev/basics",
	}
	got := filterURLs(in, "", 0)
	want := []string{
		"https://zod.dev/api?id=objects",
		"https://zod.dev/basics",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fragment/query dedupe: got %v, want %v", got, want)
	}
}

func TestFilterURLsCanonicalizesIndexAliases(t *testing.T) {
	in := []string{
		"https://turborepo.dev/docs/core-concepts/index",
		"https://turborepo.dev/docs/reference/run",
	}
	got := filterURLs(in, "", 0)
	want := []string{
		"https://turborepo.dev/docs/core-concepts",
		"https://turborepo.dev/docs/reference/run",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("index alias canonicalization: got %v, want %v", got, want)
	}
}

func TestFilterURLs(t *testing.T) {
	in := []string{
		"https://example.com/docs/a",
		"https://example.com/blog/x",
		"https://example.com/docs/b",
		"https://example.com/docs/c",
	}
	got := filterURLs(in, "https://example.com/docs/", 2)
	want := []string{"https://example.com/docs/a", "https://example.com/docs/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filter+max: got %v, want %v", got, want)
	}
	got = filterURLs(in, "", 0)
	if len(got) != 4 {
		t.Errorf("no filter, no max: got %d, want 4", len(got))
	}
}

func TestHTTPGetRetriesOn5xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	body, err := httpGet(srv.URL)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("hits = %d, want 3", got)
	}
}

func TestHTTPGetGivesUpAfterMax(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := httpGet(srv.URL)
	if err == nil {
		t.Fatal("expected failure after max retries")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("expected 502 in error, got %v", err)
	}
}

func TestHTTPGetDoesNotRetryOn404(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := httpGet(srv.URL)
	if err == nil {
		t.Fatal("expected 404 to surface")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d, want 1 (no retry on 404)", got)
	}
}
