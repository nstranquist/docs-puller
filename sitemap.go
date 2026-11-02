package main

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
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
	kept, _ := filterURLsCounted(urls, prefix, maxN)
	return kept
}

// filterURLsCounted is filterURLs that also reports how many URLs matched
// the filter (after locale dedupe) before the --max cap applied. Callers use
// matched-len(kept) to warn loudly about cap truncation: a silent --max once
// left the claude-code mirror 28 pages short while its log line read
// "llms.txt → 200 URLs" as if that were the whole site.
func filterURLsCounted(urls []string, prefix string, maxN int) (kept []string, matched int) {
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
	deduped := dropPathVersionTwins(dropPathLocaleTwins(dedupeLocaleVariants(filtered)))
	matched = len(deduped)
	if maxN > 0 && len(deduped) > maxN {
		deduped = deduped[:maxN]
	}
	return deduped, matched
}

// pathLocales are locale path segments used by docs i18n (Docusaurus, Mintlify,
// Nextra, ...). Compared case-insensitively.
var pathLocales = map[string]bool{
	"ar": true, "bn": true, "cs": true, "da": true, "de": true, "el": true, "es": true,
	"fa": true, "fi": true, "fr": true, "he": true, "hi": true, "hu": true, "id": true,
	"it": true, "ja": true, "jp": true, "ko": true, "nl": true, "pl": true, "pt": true,
	"pt-br": true, "pt-pt": true, "ro": true, "ru": true, "sv": true, "th": true,
	"tr": true, "uk": true, "vi": true, "zh": true, "zh-cn": true, "zh-tw": true,
	"zh-hans": true, "zh-hant": true, "es-419": true,
}

// dropPathLocaleTwins removes translated copies addressed by a path segment,
// such as https://x/docs/zh/guide when https://x/docs/guide is also present.
// A URL is dropped only when its untranslated twin is in the same set, so a
// page that exists only in one language is kept, and a path word that looks
// like a locale code cannot drop a page without a twin. Without this, an
// uncapped ClickHouse sitemap pull was 24,951 URLs, about 21,000 of them
// translations of the English pages.
func dropPathLocaleTwins(urls []string) []string {
	present := make(map[string]bool, len(urls))
	for _, u := range urls {
		present[u] = true
	}
	out := urls[:0:0]
	for _, u := range urls {
		if twin, ok := pathLocaleTwin(u); ok && present[twin] {
			continue
		}
		out = append(out, u)
	}
	return out
}

// pathLocaleTwin returns u with its first locale path segment removed.
func pathLocaleTwin(u string) (string, bool) {
	parsed, err := url.Parse(u)
	if err != nil {
		return "", false
	}
	segs := strings.Split(parsed.Path, "/")
	for i, seg := range segs {
		if seg != "" && pathLocales[strings.ToLower(seg)] {
			rest := append(append([]string{}, segs[:i]...), segs[i+1:]...)
			twin := *parsed
			twin.Path = strings.Join(rest, "/")
			if twin.Path == "" {
				twin.Path = "/"
			}
			return twin.String(), true
		}
	}
	return "", false
}

// pathVersionRE matches a docs-version path segment: 0.85, 3.2.x, v58.0.0,
// v2, or next (the unreleased line).
var pathVersionRE = regexp.MustCompile(`^(?:v?\d+(?:\.\d+){0,3}(?:\.x)?|next)$`)

// dropPathVersionTwins removes copies of a page published for other docs
// versions, such as https://x/docs/0.85/intro or https://x/docs/next/intro
// when https://x/docs/intro is also listed, or /versions/v58.0.0/sdk/camera
// when /versions/latest/sdk/camera is listed. A URL is dropped only when its
// twin (version segment removed, or replaced by "latest") is in the same set,
// so pages that exist only under a version stay. Without this, an uncapped
// React Native sitemap pull was 2,670 URLs for 225 current pages, and Expo
// added about 950 old-SDK copies.
func dropPathVersionTwins(urls []string) []string {
	present := make(map[string]bool, len(urls))
	for _, u := range urls {
		present[u] = true
	}
	out := urls[:0:0]
	for _, u := range urls {
		if hasPresentVersionTwin(u, present) {
			continue
		}
		out = append(out, u)
	}
	return out
}

func hasPresentVersionTwin(u string, present map[string]bool) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	segs := strings.Split(parsed.Path, "/")
	for i, seg := range segs {
		if seg == "" || !pathVersionRE.MatchString(strings.ToLower(seg)) {
			continue
		}
		for _, replacement := range []string{"", "latest"} {
			var rest []string
			rest = append(rest, segs[:i]...)
			if replacement != "" {
				rest = append(rest, replacement)
			}
			rest = append(rest, segs[i+1:]...)
			twin := *parsed
			twin.Path = strings.Join(rest, "/")
			if twin.Path == "" {
				twin.Path = "/"
			}
			if t := twin.String(); t != u && present[t] {
				return true
			}
		}
		return false // only the first version-like segment is considered
	}
	return false
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
