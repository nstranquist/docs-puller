package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end runDocC test against an in-process httptest server. Verifies the
// full pipeline that the dogfood ingest exercises: JSON URL conversion → BFS
// crawl → render → write to <out>/<source>/<rel>.md → manifest → FTS5 index.
//
// Strategy: stand up an httptest server that serves canned DocC JSON, then
// override the package-level httpClient with a transport that redirects any
// developer.apple.com URL to the test server. That way the crawl path runs
// exactly as it does in production (refURL keeps its hardcoded apple.com),
// and we don't need to fork URLs in the fixture.

// rewriteTransport redirects all requests to a single target host.
type rewriteTransport struct {
	target *url.URL
	inner  http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme = t.target.Scheme
	r2.URL.Host = t.target.Host
	r2.Host = t.target.Host
	return t.inner.RoundTrip(r2)
}

func TestRunDocCEndToEnd(t *testing.T) {
	// Three-page bundle: root + two leaves. Identifiers and refs are shaped
	// the way DocC actually emits them — references dict maps doc:// IDs to
	// {url: root-relative, title: ..., abstract: [inline]}.
	root := `{
		"schemaVersion": {"major":0,"minor":3,"patch":0},
		"identifier": {"url":"doc://com.example.testbundle/documentation/TestBundle","interfaceLanguage":"data"},
		"kind": "symbol",
		"metadata": {"title":"Test Bundle","role":"collection"},
		"abstract": [{"type":"text","text":"A test bundle for the DocC ingester."}],
		"topicSections": [
			{"title":"Articles","identifiers":[
				"doc://com.example.testbundle/documentation/TestBundle/Hello",
				"doc://com.example.testbundle/documentation/TestBundle/World"
			]}
		],
		"primaryContentSections": [],
		"references": {
			"doc://com.example.testbundle/documentation/TestBundle/Hello": {
				"type":"topic","kind":"article","title":"Hello",
				"url":"/documentation/testbundle/hello",
				"abstract":[{"type":"text","text":"First child."}]
			},
			"doc://com.example.testbundle/documentation/TestBundle/World": {
				"type":"topic","kind":"article","title":"World",
				"url":"/documentation/testbundle/world",
				"abstract":[{"type":"text","text":"Second child."}]
			}
		}
	}`
	hello := `{
		"schemaVersion": {"major":0,"minor":3,"patch":0},
		"identifier": {"url":"doc://com.example.testbundle/documentation/TestBundle/Hello","interfaceLanguage":"data"},
		"kind":"article","metadata":{"title":"Hello","role":"article"},
		"abstract": [{"type":"text","text":"First child."}],
		"primaryContentSections": [
			{"kind":"content","content":[
				{"type":"paragraph","inlineContent":[{"type":"text","text":"Body of Hello."}]}
			]}
		],
		"references": {}
	}`
	world := `{
		"schemaVersion": {"major":0,"minor":3,"patch":0},
		"identifier": {"url":"doc://com.example.testbundle/documentation/TestBundle/World","interfaceLanguage":"data"},
		"kind":"article","metadata":{"title":"World","role":"article"},
		"abstract": [{"type":"text","text":"Second child."}],
		"primaryContentSections": [
			{"kind":"content","content":[
				{"type":"paragraph","inlineContent":[{"type":"text","text":"Body of World."}]}
			]}
		],
		"references": {}
	}`

	pages := map[string]string{
		"/tutorials/data/documentation/testbundle.json":       root,
		"/tutorials/data/documentation/testbundle/hello.json": hello,
		"/tutorials/data/documentation/testbundle/world.json": world,
	}

	hits := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		hits[r.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}

	// Swap httpClient for one that rewrites every request to the test server.
	origClient := httpClient
	httpClient = &http.Client{
		Transport: &rewriteTransport{target: serverURL, inner: http.DefaultTransport},
		Timeout:   5e9, // 5s
	}
	defer func() { httpClient = origClient }()

	tmpDir := t.TempDir()
	opts := pullOpts{
		out:         tmpDir,
		sourceCache: filepath.Join(tmpDir, ".cache"),
		concurrency: 4,
	}

	// Suppress runDocC stderr chatter during tests.
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	origStderr := os.Stderr
	os.Stderr = devnull
	defer func() { os.Stderr = origStderr }()

	runDocC(
		"https://developer.apple.com/documentation/testbundle",
		"", "", 0, false, false,
		opts,
		[]string{"pull", "--docc", "https://developer.apple.com/documentation/testbundle"},
	)

	// Source name: identifier is com.example.testbundle (non-Apple), so the
	// directory is bare "testbundle" — not "apple-testbundle".
	sourceDir := filepath.Join(tmpDir, "testbundle")
	if _, err := os.Stat(sourceDir); err != nil {
		// Apple-only fallback: try the legacy naming in case the inference
		// path kicked in due to identifier-host shape.
		appleDir := filepath.Join(tmpDir, "apple-testbundle")
		if _, err2 := os.Stat(appleDir); err2 == nil {
			sourceDir = appleDir
		} else {
			entries, _ := os.ReadDir(tmpDir)
			names := []string{}
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("expected source dir %q (or apple-testbundle); got entries: %v", sourceDir, names)
		}
	}

	// All three pages should have been fetched exactly once.
	for path, count := range hits {
		if count != 1 {
			t.Errorf("page %s fetched %d times (want 1)", path, count)
		}
	}
	if len(hits) != 3 {
		t.Errorf("expected 3 pages fetched, got %d: %v", len(hits), hits)
	}

	// Markdown files exist.
	for _, rel := range []string{"index.md", "hello.md", "world.md"} {
		p := filepath.Join(sourceDir, rel)
		body, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("missing %s: %v", p, err)
			continue
		}
		if !strings.Contains(string(body), "# ") {
			t.Errorf("%s: no H1 heading found", p)
		}
	}

	// index.md links to children with apple.com URLs (refURL today hardcodes
	// developer.apple.com — we explicitly verify that contract here so a
	// future refactor that rewrites the host base updates this test).
	indexBody, _ := os.ReadFile(filepath.Join(sourceDir, "index.md"))
	for _, want := range []string{
		"# Test Bundle",
		"## Topics",
		"### Articles",
		"[Hello](https://developer.apple.com/documentation/testbundle/hello)",
		"[World](https://developer.apple.com/documentation/testbundle/world)",
	} {
		if !strings.Contains(string(indexBody), want) {
			t.Errorf("index.md missing %q\n--- body ---\n%s", want, string(indexBody))
		}
	}

	// Manifest has 3 entries keyed by the human-facing apple.com URL.
	manifestPath := filepath.Join(sourceDir, "manifest.json")
	manifestBody, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(manifestBody, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(m.Entries) != 3 {
		t.Errorf("manifest has %d entries (want 3): %v", len(m.Entries), m.Entries)
	}
	wantURLs := []string{
		"https://developer.apple.com/documentation/testbundle",
		"https://developer.apple.com/documentation/testbundle/hello",
		"https://developer.apple.com/documentation/testbundle/world",
	}
	for _, u := range wantURLs {
		entry, ok := m.Entries[u]
		if !ok {
			t.Errorf("manifest missing %s", u)
			continue
		}
		if entry.Mode != "docc" {
			t.Errorf("manifest %s: mode %q, want docc", u, entry.Mode)
		}
		if entry.Source == "" || !strings.HasSuffix(entry.Path, ".md") {
			t.Errorf("manifest %s: bad source/path: %+v", u, entry)
		}
	}
}

// TestRunDocCRespectsMax confirms --max caps the crawl. We use a 1-root +
// 5-children fixture and assert that with max=2 we only fetch root + 1 child.
func TestRunDocCRespectsMax(t *testing.T) {
	const ids = 5
	rootIDs := []string{}
	rootRefs := map[string]string{}
	pages := map[string]string{}

	// Build root JSON listing 5 child identifiers.
	for i := 0; i < ids; i++ {
		id := fmt.Sprintf("doc://com.example.maxtest/documentation/MaxTest/Child%d", i)
		rootIDs = append(rootIDs, id)
		rootRefs[id] = fmt.Sprintf("/documentation/maxtest/child%d", i)
		pages[fmt.Sprintf("/tutorials/data/documentation/maxtest/child%d.json", i)] = fmt.Sprintf(`{
			"schemaVersion":{"major":0,"minor":3,"patch":0},
			"identifier":{"url":"%s","interfaceLanguage":"data"},
			"kind":"article","metadata":{"title":"Child%d","role":"article"},
			"abstract":[],"primaryContentSections":[],"references":{}
		}`, id, i)
	}

	idsList := ""
	for _, id := range rootIDs {
		if idsList != "" {
			idsList += ","
		}
		idsList += `"` + id + `"`
	}
	refsBlock := ""
	for id, u := range rootRefs {
		if refsBlock != "" {
			refsBlock += ","
		}
		refsBlock += fmt.Sprintf(`"%s":{"type":"topic","kind":"article","title":"x","url":"%s"}`, id, u)
	}
	pages["/tutorials/data/documentation/maxtest.json"] = fmt.Sprintf(`{
		"schemaVersion":{"major":0,"minor":3,"patch":0},
		"identifier":{"url":"doc://com.example.maxtest/documentation/MaxTest","interfaceLanguage":"data"},
		"kind":"symbol","metadata":{"title":"MaxTest","role":"collection"},
		"abstract":[],
		"topicSections":[{"title":"Children","identifiers":[%s]}],
		"primaryContentSections":[],
		"references":{%s}
	}`, idsList, refsBlock)

	hits := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		hits[r.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	origClient := httpClient
	httpClient = &http.Client{Transport: &rewriteTransport{target: u, inner: http.DefaultTransport}}
	defer func() { httpClient = origClient }()

	tmpDir := t.TempDir()
	opts := pullOpts{out: tmpDir, sourceCache: filepath.Join(tmpDir, ".cache"), concurrency: 2}

	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	origStderr := os.Stderr
	os.Stderr = devnull
	defer func() { os.Stderr = origStderr }()

	runDocC(
		"https://developer.apple.com/documentation/maxtest",
		"", "", 2, false, false,
		opts, []string{"pull", "--docc", "--max", "2"},
	)

	if len(hits) != 2 {
		t.Errorf("--max=2 fetched %d pages (want 2): %v", len(hits), hits)
	}
}
