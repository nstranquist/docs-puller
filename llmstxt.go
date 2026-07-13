package main

import (
	"fmt"
	"net/url"
	pathpkg "path"
	"regexp"
	"strings"
)

// Some vendors publish a single combined llms.txt corpus rather than the
// llmstxt.org link-index shape. xAI uses one native-Markdown document per
// ===/path=== section. We still fetch each vendor .md URL through the normal
// pipeline so manifests, hashes, retries, routing, and FTS updates retain one
// shared implementation.
var combinedLLMsTxtHeaderRE = regexp.MustCompile(`(?m)^===\s*(/[^=\r\n]*)\s*===\r?$`)

// loadLLMsTxt resolves an upstream llms.txt into the document URLs consumed by
// the regular pull pipeline. It supports both absolute links in a conventional
// llms.txt and xAI-style combined section markers.
func loadLLMsTxt(rawURL string) ([]string, error) {
	body, err := httpGet(rawURL)
	if err != nil {
		return nil, fmt.Errorf("fetch llms.txt %s: %w", rawURL, err)
	}

	base, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse llms.txt URL %s: %w", rawURL, err)
	}

	matches := combinedLLMsTxtHeaderRE.FindAllSubmatch(body, -1)
	if len(matches) > 0 {
		urls := make([]string, 0, len(matches))
		for _, match := range matches {
			p := pathpkg.Clean(strings.TrimSpace(string(match[1])))
			if p == "." || p == "/" {
				p = "/index"
			}
			if !strings.HasSuffix(strings.ToLower(p), ".md") &&
				!strings.HasSuffix(strings.ToLower(p), ".markdown") {
				p += ".md"
			}
			ref := &url.URL{Path: p}
			urls = append(urls, base.ResolveReference(ref).String())
		}
		return dedupeURLs(urls), nil
	}

	urls := extractURLs(body, rawURL)
	if len(urls) == 0 {
		return nil, fmt.Errorf("llms.txt %s contains no document URLs or ===/path=== sections", rawURL)
	}
	return urls, nil
}
