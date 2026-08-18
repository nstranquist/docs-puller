package main

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzDedupeLocaleVariants drives the sitemap URL post-processing that collapses
// per-locale variants (?hl=, ?lang=, ?locale=, ?lr=, ?uselang=) of one page down
// to a single canonical URL. It runs on every <loc> harvested from a remote,
// untrusted sitemap.xml — parsing each entry as a URL and choosing a winner — so
// it must survive arbitrary, malformed, or adversarial URL lists.
//
// Invariants: no panic, and dedup never grows the input (it only ever removes
// variants, never invents URLs). Idempotence is deliberately NOT asserted:
// canonicalPullURL strips a single trailing "/index" segment by design, so a
// path like "/p/index/index" only fully normalizes over repeated passes — but
// the pipeline applies dedupe exactly once per sitemap (via filterURLs), so
// single-strip is the intended, shipped behavior.
func FuzzDedupeLocaleVariants(f *testing.F) {
	seeds := []string{
		"https://example.com/a\nhttps://example.com/a?hl=fr\nhttps://example.com/b",
		"https://x/p/index https://x/p",
		"https://x/p/index/index https://x/p", // nested /index: dedupe is intentionally not idempotent here
		"https://a?lang=de https://a?locale=es https://a",
		"not a url :::: https://ok/",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, blob string) {
		urls := strings.Fields(blob)
		out := dedupeLocaleVariants(urls)
		if len(out) > len(urls) {
			t.Fatalf("dedupe grew the list: %d in, %d out", len(urls), len(out))
		}
		seen := make(map[string]bool, len(out))
		for _, raw := range out {
			parsed, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("dedupe emitted an unparsable URL %q: %v", raw, err)
			}
			key := parsed.Scheme + "://" + parsed.Host + parsed.Path
			if seen[key] {
				t.Fatalf("dedupe emitted canonical key %q more than once: %v", key, out)
			}
			seen[key] = true
		}
	})
}
