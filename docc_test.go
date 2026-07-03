package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadDocCFixture parses a real DocC JSON file from testdata/docc.
func loadDocCFixture(t *testing.T, name string) *doccNode {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "docc", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var n doccNode
	if err := json.Unmarshal(data, &n); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return &n
}

// ─────────────── URL & path mapping ───────────────

func TestPageURLToJSONURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			"https://developer.apple.com/documentation/devicemanagement/desktop",
			"https://developer.apple.com/tutorials/data/documentation/devicemanagement/desktop.json",
		},
		{
			"https://developer.apple.com/documentation/devicemanagement",
			"https://developer.apple.com/tutorials/data/documentation/devicemanagement.json",
		},
		{
			"https://developer.apple.com/documentation/devicemanagement/desktop/",
			"https://developer.apple.com/tutorials/data/documentation/devicemanagement/desktop.json",
		},
		{
			// idempotent on already-JSON URLs
			"https://developer.apple.com/tutorials/data/documentation/devicemanagement/desktop.json",
			"https://developer.apple.com/tutorials/data/documentation/devicemanagement/desktop.json",
		},
	}
	for _, c := range cases {
		got, err := pageURLToJSONURL(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s\n got: %s\nwant: %s", c.in, got, c.want)
		}
	}
}

func TestPageURLToJSONURLRejectsRelativeURL(t *testing.T) {
	got, err := pageURLToJSONURL("/documentation/devicemanagement/desktop")
	if err == nil {
		t.Fatal("expected relative URL error")
	}
	if got != "" {
		t.Fatalf("url = %q, want empty on error", got)
	}
	if want := "not an absolute URL: /documentation/devicemanagement/desktop"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestCrawlDocCWrapsRootURLError(t *testing.T) {
	got, err := crawlDocC(doccCrawlOpts{rootURL: "%"})
	if err == nil || !strings.HasPrefix(err.Error(), "root url: ") {
		t.Fatalf("crawlDocC root error = results=%+v err=%v, want root url wrapper", got, err)
	}
}

func TestCrawlDocCRejectsRootOutsideFilter(t *testing.T) {
	got, err := crawlDocC(doccCrawlOpts{
		rootURL: "https://developer.apple.com/documentation/devicemanagement/desktop",
		filter:  "/documentation/swift",
	})
	if err == nil {
		t.Fatal("expected root filter rejection")
	}
	if len(got) != 0 {
		t.Fatalf("results = %+v, want none on root rejection", got)
	}
	if want := "root URL https://developer.apple.com/documentation/devicemanagement/desktop rejected by filter or already visited"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestCrawlDocCWrapsNodeParseError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		n, err := w.Write([]byte(`{`))
		if err != nil || n != 1 {
			return
		}
	}))
	defer server.Close()

	got, err := crawlDocC(doccCrawlOpts{
		rootURL:     server.URL + "/documentation/example",
		concurrency: 1,
	})
	if err != nil {
		t.Fatalf("crawlDocC returned top-level error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("crawlDocC results = %+v, want one parse-error result", got)
	}
	if got[0].err == nil || !strings.HasPrefix(got[0].err.Error(), "parse: ") {
		t.Fatalf("crawlDocC result error = %v, want parse wrapper", got[0].err)
	}
}

func TestJSONURLToPageURL(t *testing.T) {
	in := "https://developer.apple.com/tutorials/data/documentation/devicemanagement/desktop.json"
	want := "https://developer.apple.com/documentation/devicemanagement/desktop"
	if got := jsonURLToPageURL(in); got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestBundleFromIdentifier(t *testing.T) {
	cases := []struct{ id, want string }{
		// Apple bundles
		{"doc://com.apple.devicemanagement/documentation/DeviceManagement/Desktop", "devicemanagement"},
		{"doc://com.apple.swift/documentation/Swift/Array", "swift"},
		{"doc://com.apple.documentation/documentation/technologies", "documentation"},
		// Third-party DocC archives (Swift Package Manager + custom NS)
		{"doc://org.swift.swift-syntax/documentation/SwiftSyntax/Trivia", "swift-syntax"},
		{"doc://other.vendor.tool/documentation/Foo", "tool"},
		{"doc://MyPackage/documentation/MyPackage/Symbol", "MyPackage"},
		// Negative cases
		{"not-an-identifier", ""},
		{"doc://", ""},
	}
	for _, c := range cases {
		if got := bundleFromIdentifier(c.id); got != c.want {
			t.Errorf("%s: got %q want %q", c.id, got, c.want)
		}
	}
}

func TestSourceNameForBundle(t *testing.T) {
	cases := []struct {
		identifier, bundle, want string
	}{
		// com.apple.* gets the apple- prefix.
		{"doc://com.apple.devicemanagement/...", "devicemanagement", "apple-devicemanagement"},
		{"doc://com.apple.swift/...", "swift", "apple-swift"},
		// Non-Apple bundles use the bare name.
		{"doc://org.swift.swift-syntax/...", "swift-syntax", "swift-syntax"},
		{"doc://MyPackage/...", "MyPackage", "mypackage"}, // sanitize lowercases
		// Empty bundle → empty source.
		{"doc://com.apple.x/", "", ""},
	}
	for _, c := range cases {
		if got := sourceNameForBundle(c.identifier, c.bundle); got != c.want {
			t.Errorf("sourceNameForBundle(%q, %q): got %q want %q", c.identifier, c.bundle, got, c.want)
		}
	}
}

func TestPageURLToRelPath(t *testing.T) {
	cases := []struct {
		page, bundle, want string
	}{
		{"https://developer.apple.com/documentation/devicemanagement/desktop", "devicemanagement", "desktop.md"},
		{"https://developer.apple.com/documentation/devicemanagement", "devicemanagement", "index.md"},
		{"https://developer.apple.com/documentation/devicemanagement/", "devicemanagement", "index.md"},
		{"https://developer.apple.com/documentation/devicemanagement/profile-specific-payload-keys", "devicemanagement", "profile-specific-payload-keys.md"},
		{"https://developer.apple.com/documentation/swift/array/append(_:)", "swift", "array/append(_:).md"},
	}
	for _, c := range cases {
		got := pageURLToRelPath(c.page, c.bundle)
		if got != c.want {
			t.Errorf("%s [%s]: got %s want %s", c.page, c.bundle, got, c.want)
		}
	}
}

// ─────────────── Inline rendering ───────────────

func TestRenderInlines(t *testing.T) {
	refs := map[string]doccReference{
		"doc://com.apple.devicemanagement/documentation/DeviceManagement/Dock": {
			Type: "topic", Kind: "symbol", Title: "Dock",
			URL: "/documentation/devicemanagement/dock",
		},
	}
	in := []doccInline{
		{Type: "text", Text: "Use "},
		{Type: "codeVoice", Code: "com.apple.dock"},
		{Type: "text", Text: " or see "},
		{Type: "reference", Identifier: "doc://com.apple.devicemanagement/documentation/DeviceManagement/Dock", IsActive: true},
		{Type: "text", Text: "."},
	}
	got := renderInlines(in, refs)
	want := "Use `com.apple.dock` or see [Dock](https://developer.apple.com/documentation/devicemanagement/dock)."
	if got != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}
}

// ─────────────── End-to-end render: real DocC JSON ───────────────

func TestRenderDesktop(t *testing.T) {
	n := loadDocCFixture(t, "desktop.json")
	md := string(renderDocC(n))

	mustContain(t, md, "# Desktop")
	mustContain(t, md, "> The payload that configures the desktop wallpaper.")
	// dictionary symbols use the empty-language code fence (we map "data" → "")
	mustContain(t, md, "object Desktop")
	// Properties section with named property + tags
	mustContain(t, md, "## Properties")
	mustContain(t, md, "### `locked`")
	mustContain(t, md, "deprecated")
	mustContain(t, md, "since 10.10.0")
	// Discussion heading from primaryContentSections.content
	mustContain(t, md, "Discussion")
	// See Also section is preserved
	mustContain(t, md, "## See Also")
	mustContain(t, md, "User Experience")
	mustContain(t, md, "[Dock](https://developer.apple.com/documentation/devicemanagement/dock)")
}

func TestRenderRootCollection(t *testing.T) {
	n := loadDocCFixture(t, "devicemanagement-root.json")
	md := string(renderDocC(n))

	mustContain(t, md, "# Device Management")
	mustContain(t, md, "## Topics")
	mustContain(t, md, "### Configuration Profiles")
	mustContain(t, md, "### MDM Protocol")
	mustContain(t, md, "### Declarative Management")
	// Topic links resolve to absolute developer.apple.com URLs
	mustContain(t, md, "https://developer.apple.com/documentation/")
}

func TestRenderArticleCollection(t *testing.T) {
	n := loadDocCFixture(t, "profile-specific-payload-keys.json")
	md := string(renderDocC(n))

	mustContain(t, md, "# ")
	mustContain(t, md, "## Topics")
	// 30 topic groups — sample a few we expect
	mustContain(t, md, "### Top Level")
	mustContain(t, md, "### Accounts")
	mustContain(t, md, "### AirPlay")
}

func TestRenderSwiftSymbol(t *testing.T) {
	n := loadDocCFixture(t, "swift-array.json")
	md := string(renderDocC(n))

	mustContain(t, md, "# Array")
	// Swift declarations carry "swift" language
	mustContain(t, md, "```swift")
	mustContain(t, md, "## Relationships")
	// 43 conformances — sanity check one
	mustContain(t, md, "Conforms To")
	// Has many topics
	mustContain(t, md, "## Topics")
}

func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		// Show a small window to keep test output readable.
		preview := body
		if len(preview) > 800 {
			preview = preview[:800]
		}
		t.Errorf("missing %q\n--- preview ---\n%s\n", want, preview)
	}
}

// ─────────────── Crawl URL extraction ───────────────

// TestCrawlIdentifierExtraction asserts that BFS picks up the in-bundle
// topic identifiers from a real fixture. We don't run the full crawl (no
// network) — we just verify the identifier-walking shape used by crawlDocC.
func TestCrawlExpansion(t *testing.T) {
	n := loadDocCFixture(t, "devicemanagement-root.json")
	bundle := bundleFromIdentifier(n.Identifier.URL)
	if bundle != "devicemanagement" {
		t.Fatalf("bundle: got %q want devicemanagement", bundle)
	}

	// Build the same expansion the crawler does for topicSections.
	var enqueued []string
	for _, g := range n.TopicSections {
		for _, id := range g.Identifiers {
			ref, ok := n.References[id]
			if !ok || ref.URL == "" {
				continue
			}
			if bundleFromIdentifier(id) != bundle {
				continue
			}
			pageURL := refURL(ref)
			jsonURL, err := pageURLToJSONURL(pageURL)
			if err != nil {
				continue
			}
			enqueued = append(enqueued, jsonURL)
		}
	}
	if len(enqueued) == 0 {
		t.Fatalf("expected non-empty enqueue list from root collection")
	}
	for _, u := range enqueued {
		if !strings.HasPrefix(u, "https://developer.apple.com/tutorials/data/documentation/devicemanagement/") {
			t.Errorf("enqueued URL outside bundle: %s", u)
		}
		if !strings.HasSuffix(u, ".json") {
			t.Errorf("enqueued URL missing .json suffix: %s", u)
		}
	}
}
