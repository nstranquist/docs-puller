package docspack

// Sitemap-first URL discovery with followLinks fallback — Go port of
// sitemap.py. Measured in Pilot 1: sitemaps existed on only 3/5 real docs
// sites, so the fallback is mandatory, not optional.

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Fetcher fetches a URL; ok=false on any failure (mirrors Python fetch
// returning None).
type Fetcher func(url string) (string, bool)

// DefaultFetcher is an HTTP GET with the pack's user agent.
func DefaultFetcher(rawurl string) (string, bool) {
	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("User-Agent", "docs-puller-pack/1.0 (+harvest reference implementation)")
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false
	}
	return string(body), true
}

var DefaultExcludes = []string{
	"newsletter", "subscribe", "promo", "promotion",
	"login", "signup", "sign-up", "cart", "checkout",
}

// ParseSitemap returns (pageURLs, childSitemapURLs) from a sitemap
// document, matching on element local names (namespace-agnostic), like the
// Python implementation.
func ParseSitemap(xmlText string) ([]string, []string) {
	var pages, children []string
	dec := xml.NewDecoder(strings.NewReader(xmlText))
	isIndex := false
	sawRoot := false
	var curLoc *strings.Builder
	inLoc := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !sawRoot {
				sawRoot = true
				isIndex = t.Name.Local == "sitemapindex"
			}
			if t.Name.Local == "loc" {
				curLoc = &strings.Builder{}
				inLoc = true
			}
		case xml.CharData:
			if inLoc && curLoc != nil {
				curLoc.WriteString(string(t))
			}
		case xml.EndElement:
			if t.Name.Local == "loc" && inLoc {
				v := strings.TrimSpace(curLoc.String())
				if v != "" {
					if isIndex {
						children = append(children, v)
					} else {
						pages = append(pages, v)
					}
				}
				inLoc = false
				curLoc = nil
			}
		}
	}
	return pages, children
}

// DiscoverSitemapURLs fetches <base>/sitemap.xml, following sitemap
// indexes one level.
func DiscoverSitemapURLs(baseURL string, maxPages int, fetch Fetcher) []string {
	if fetch == nil {
		fetch = DefaultFetcher
	}
	root := strings.TrimRight(baseURL, "/") + "/sitemap.xml"
	xmlText, ok := fetch(root)
	if !ok {
		return nil
	}
	pages, children := ParseSitemap(xmlText)
	for _, child := range children {
		if len(pages) >= maxPages {
			break
		}
		if sub, ok := fetch(child); ok {
			subPages, _ := ParseSitemap(sub)
			pages = append(pages, subPages...)
		}
	}
	if len(pages) > maxPages {
		pages = pages[:maxPages]
	}
	return pages
}

var assetExtRe = regexp.MustCompile(`(?i)\.(png|jpg|jpeg|gif|svg|css|js|zip|pdf|ico)$`)

// ExtractSameDomainLinks extracts same-domain http(s) links from a page.
func ExtractSameDomainLinks(htmlText, baseURL string) []string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, t := range tokenize(htmlText) {
		if t.kind != tokStart || t.tag != "a" {
			continue
		}
		href := t.attrs["href"]
		if href == "" || strings.HasPrefix(href, "#") ||
			strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "tel:") ||
			strings.HasPrefix(href, "javascript:") {
			continue
		}
		ref, err := url.Parse(href)
		if err != nil {
			continue
		}
		full := base.ResolveReference(ref)
		full.Fragment = ""
		if full.Host != base.Host || !strings.HasPrefix(full.Scheme, "http") {
			continue
		}
		if assetExtRe.MatchString(full.Path) {
			continue
		}
		s := full.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// FilterURLs applies include (allowlist, if non-empty) and exclude
// (substring, case-insensitive) filters.
func FilterURLs(urls []string, includes, excludes []string) []string {
	if excludes == nil {
		excludes = DefaultExcludes
	}
	var out []string
	for _, u := range urls {
		low := strings.ToLower(u)
		skip := false
		for _, e := range excludes {
			if strings.Contains(low, e) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		if len(includes) > 0 {
			ok := false
			for _, i := range includes {
				if strings.Contains(low, i) {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		out = append(out, u)
	}
	return out
}

// DiscoverURLs is sitemap-first with a followLinks fallback from the seed
// page (mirrors Python discover_urls). Pass fetch=nil for DefaultFetcher.
func DiscoverURLs(seedURL string, maxPages int, fetch Fetcher, includes, excludes []string) []string {
	if fetch == nil {
		fetch = DefaultFetcher
	}
	seed, err := url.Parse(seedURL)
	if err != nil {
		return nil
	}
	origin := seed.Scheme + "://" + seed.Host
	urls := DiscoverSitemapURLs(origin, maxPages, fetch)
	if len(urls) == 0 {
		if htmlText, ok := fetch(seedURL); ok {
			urls = ExtractSameDomainLinks(htmlText, seedURL)
		}
		found := false
		for _, u := range urls {
			if u == seedURL {
				found = true
			}
		}
		if !found {
			urls = append([]string{seedURL}, urls...)
		}
	}
	if len(urls) > maxPages {
		urls = urls[:maxPages]
	}
	return FilterURLs(urls, includes, excludes)
}
