package main

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/nstranquist/docs-puller/searchruntime"
)

// loadSitemap fetches a sitemap.xml (or sitemap_index.xml) and returns every
// page URL it points at. Recursively follows sitemap-index entries.
//
// Handles:
//   - <urlset><url><loc>...</loc></url>...</urlset>            (page sitemap)
//   - <sitemapindex><sitemap><loc>...</loc></sitemap></sitemapindex>  (index)
//   - .gz-suffixed sitemaps via gzip decompression
//
// Errors on a single nested sitemap are logged but don't fail the whole load.
func loadSitemap(rawURL string) ([]string, error) {
	body, err := fetchSitemap(rawURL)
	if err != nil {
		return nil, searchruntime.SitemapFetchError(rawURL, err)
	}

	// Probe root element to decide between index and page sitemap.
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	root := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, searchruntime.SitemapParseError(rawURL, err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			root = se.Name.Local
			break
		}
	}

	switch root {
	case "sitemapindex":
		var idx sitemapIndex
		if err := xml.Unmarshal(body, &idx); err != nil {
			return nil, err
		}
		var all []string
		for _, sm := range idx.Sitemaps {
			loc := strings.TrimSpace(sm.Loc)
			if loc == "" {
				continue
			}
			urls, err := loadSitemap(loc)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  sitemap %s: %v\n", loc, err)
				continue
			}
			all = append(all, urls...)
		}
		return all, nil
	case "urlset":
		var us urlSet
		if err := xml.Unmarshal(body, &us); err != nil {
			return nil, err
		}
		// Resolve relative <loc> values (the spec requires absolute, but some
		// sites — notably cli.github.com — ship root-relative paths).
		base, _ := url.Parse(rawURL)
		out := make([]string, 0, len(us.URLs))
		for _, u := range us.URLs {
			loc := strings.TrimSpace(u.Loc)
			if loc == "" {
				continue
			}
			if base != nil && !strings.HasPrefix(loc, "http") {
				if abs, err := base.Parse(loc); err == nil {
					loc = abs.String()
				}
			}
			out = append(out, loc)
		}
		return out, nil
	default:
		return nil, searchruntime.SitemapUnexpectedRootError(root, rawURL)
	}
}

// fetchSitemap reuses httpGet so the sitemap fetch itself benefits from
// retry on 5xx/429. Gunzips when the URL has a .gz suffix (servers serving
// `Content-Type: application/gzip` without the suffix are rare; users can
// rename the URL to .gz if needed).
func fetchSitemap(rawURL string) ([]byte, error) {
	body, err := httpGet(rawURL)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(rawURL, ".gz") {
		return body, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, searchruntime.SitemapGunzipError(err)
	}
	defer gz.Close()
	return io.ReadAll(gz)
}

type urlEntry struct {
	Loc string `xml:"loc"`
}

type urlSet struct {
	URLs []urlEntry `xml:"url"`
}

type sitemapEntry struct {
	Loc string `xml:"loc"`
}

type sitemapIndex struct {
	Sitemaps []sitemapEntry `xml:"sitemap"`
}

// filterURLs keeps only URLs starting with prefix (when prefix is non-empty)
// and caps the slice to maxN (when maxN > 0). Order is preserved.
//
// Also dedupes locale-query variants — Google DevSite (chrome, android) and
// some other sites emit `<loc>` entries for every locale via ?hl=<lang> on
// the same path. Without dedup we'd pull every variant; the last write would
// win and clobber English content with whatever locale comes last in the
// sitemap. Server's Accept-Language is ignored when ?hl= is set, so
// English-only requires dropping locale-query URLs at the sitemap level.
func filterURLs(urls []string, prefix string, maxN int) []string {
	// Don't reuse the input slice's backing array — `out := urls; out = out[:0]`
	// would mutate the caller's data and break callers that pass the same
	// slice to filterURLs twice.
	var filtered []string
	if prefix != "" {
		filtered = make([]string, 0, len(urls))
		for _, u := range urls {
			if strings.HasPrefix(u, prefix) {
				filtered = append(filtered, u)
			}
		}
	} else {
		filtered = urls
	}
	deduped := dedupeLocaleVariants(filtered)
	if maxN > 0 && len(deduped) > maxN {
		deduped = deduped[:maxN]
	}
	return deduped
}

// dedupeLocaleVariants groups URLs by canonical path (scheme+host+path) and
// picks the variant without a locale query param. Locale params we recognize:
// hl, lang, locale, lr (Google), uselang (MediaWiki).
func dedupeLocaleVariants(urls []string) []string {
	type slot struct {
		idx int    // first-seen position, for stable ordering
		url string // chosen URL (prefer no-locale-query)
	}
	chosen := make(map[string]*slot, len(urls))
	for i, u := range urls {
		u = canonicalPullURL(u)
		parsed, err := url.Parse(u)
		if err != nil {
			continue
		}
		key := parsed.Scheme + "://" + parsed.Host + parsed.Path
		hasLocaleQuery := urlHasLocaleParam(parsed)
		s, ok := chosen[key]
		if !ok {
			chosen[key] = &slot{idx: i, url: u}
			continue
		}
		// Already seen this canonical. Replace if current is no-locale and
		// existing is locale-tagged.
		if !hasLocaleQuery {
			existingParsed, err := url.Parse(s.url)
			if err == nil && urlHasLocaleParam(existingParsed) {
				s.url = u
			}
		}
	}
	type entry struct {
		idx int
		url string
	}
	entries := make([]entry, 0, len(chosen))
	for _, s := range chosen {
		entries = append(entries, entry{s.idx, s.url})
	}
	// Stable sort by first-seen index so output order roughly mirrors sitemap order.
	sort.Slice(entries, func(i, j int) bool { return entries[i].idx < entries[j].idx })
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.url
	}
	return out
}

func canonicalPullURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if strings.HasSuffix(parsed.Path, "/index") {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/index")
		if parsed.Path == "" {
			parsed.Path = "/"
		}
	}
	return parsed.String()
}

func urlHasLocaleParam(u *url.URL) bool {
	q := u.Query()
	for _, k := range []string{"hl", "lang", "locale", "lr", "uselang"} {
		if q.Get(k) != "" {
			return true
		}
	}
	return false
}
