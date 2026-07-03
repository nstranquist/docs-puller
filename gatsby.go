package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/nstranquist/docs-puller/searchruntime"
)

// loadGatsbyPageData fetches a Gatsby page-data.json (e.g.
// https://posthog.com/page-data/docs/page-data.json), walks every
// staticQueryHash it references, and returns the slugs from whichever
// query has the largest allMdx.nodes[].slug array.
//
// Why: Gatsby sites often ship a near-empty sitemap.xml because the
// real URL inventory lives inside hashed static-query JSONs. The
// page-data root references every query the page consumes, so finding
// the one with allMdx is a reliable, host-agnostic discovery path.
//
// The returned slugs are repository-relative (e.g. "docs/getting-started/install");
// callers are responsible for joining them with the site origin.
func loadGatsbyPageData(rawURL string) ([]string, error) {
	pd, err := fetchGatsbyJSON(rawURL)
	if err != nil {
		return nil, searchruntime.GatsbyFetchError(rawURL, err)
	}
	var root struct {
		StaticQueryHashes []string `json:"staticQueryHashes"`
	}
	if err := json.Unmarshal(pd, &root); err != nil {
		return nil, searchruntime.GatsbyParseError(rawURL, err)
	}
	if len(root.StaticQueryHashes) == 0 {
		return nil, searchruntime.GatsbyNoStaticQueryHashesError(rawURL)
	}

	base, err := url.Parse(rawURL)
	if err != nil {
		return nil, searchruntime.GatsbyBaseURLParseError(err)
	}

	var best []string
	for _, h := range root.StaticQueryHashes {
		sqURL := fmt.Sprintf("%s://%s/page-data/sq/d/%s.json", base.Scheme, base.Host, h)
		body, err := fetchGatsbyJSON(sqURL)
		if err != nil {
			// One missing/slow query JSON shouldn't fail the whole discovery.
			continue
		}
		slugs := extractGatsbySlugs(body)
		if len(slugs) > len(best) {
			best = slugs
		}
	}
	if len(best) == 0 {
		return nil, searchruntime.GatsbyNoAllMdxSlugsError(len(root.StaticQueryHashes))
	}
	return best, nil
}

// extractGatsbySlugs pulls .data.allMdx.nodes[].slug out of a static-query JSON.
// Returns nil when the JSON has no allMdx.nodes path.
func extractGatsbySlugs(body []byte) []string {
	var sq struct {
		Data struct {
			AllMdx struct {
				Nodes []struct {
					Slug string `json:"slug"`
				} `json:"nodes"`
			} `json:"allMdx"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &sq); err != nil {
		return nil
	}
	if len(sq.Data.AllMdx.Nodes) == 0 {
		return nil
	}
	out := make([]string, 0, len(sq.Data.AllMdx.Nodes))
	seen := make(map[string]bool, len(sq.Data.AllMdx.Nodes))
	for _, n := range sq.Data.AllMdx.Nodes {
		s := strings.TrimSpace(n.Slug)
		if s == "" || seen[s] || hasPrivateSegment(s) {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// gatsbySlugsToURLs joins discovered slugs with the page-data URL's origin
// to produce absolute page URLs.
func gatsbySlugsToURLs(pageDataURL string, slugs []string) ([]string, error) {
	base, err := url.Parse(pageDataURL)
	if err != nil {
		return nil, err
	}
	origin := base.Scheme + "://" + base.Host
	out := make([]string, 0, len(slugs))
	for _, s := range slugs {
		s = strings.TrimPrefix(s, "/")
		out = append(out, origin+"/"+s)
	}
	return out, nil
}

// hasPrivateSegment reports whether any path segment in the slug starts with
// an underscore. Gatsby/MDX projects use this to mark private include
// partials (e.g. _snippets/foo) that aren't routed publicly and would 404 if
// fetched via HTTP.
func hasPrivateSegment(slug string) bool {
	for _, seg := range strings.Split(slug, "/") {
		if strings.HasPrefix(seg, "_") {
			return true
		}
	}
	return false
}

// fetchGatsbyJSON wraps httpGet for Gatsby JSON endpoints. Identical to a
// plain GET today but kept separate so retry/UA tweaks specific to JSON
// endpoints can land here without affecting page fetches.
func fetchGatsbyJSON(rawURL string) ([]byte, error) {
	return httpGet(rawURL)
}
