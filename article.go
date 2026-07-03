package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nstranquist/docs-puller/searchruntime"
)

// Single-article ingestion. Articles land under <out>/articles/<host>/<slug>.md
// so the existing per-source manifest + _INDEX + FTS path applies unchanged.
// Host bucketing gives the index a natural categorization (dev.to, reddit.com,
// substack.com ...) without forcing the user to declare a category up front.
//
// Reddit threads can't be scraped via the normal HTML path — Reddit 403s any
// non-browser UA. We rewrite to the public `<url>.json` endpoint and render
// the post body + top-level comments as Markdown ourselves.

const articleSource = "articles"

// articleSlugRE strips characters that are illegal or ugly in path segments.
var articleSlugRE = regexp.MustCompile(`[^a-z0-9._-]+`)

func cmdPullArticle(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("pull-article", flag.ExitOnError)
	name := fs.String("name", "", "override the slug (default: derived from URL path)")
	bindOpts(fs, &o)
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "pull-article: URL required")
		os.Exit(2)
	}
	raw := fs.Arg(0)

	start := time.Now()
	now := start.UTC().Format(time.RFC3339)
	r := processArticle(raw, *name, o, now)

	if err := withWriteLock(o.out, func() error {
		if err := writeManifests(o.out, []result{r}, false, nil); err != nil {
			return err
		}
		var changed []string
		if r.Skipped == "" && r.Path != "" {
			changed = []string{r.Path}
		}
		if err := regenerateIndex(o.out, []string{articleSource}); err != nil {
			return err
		}
		if idx, err := openFTSIndex(o.out); err == nil {
			if rerr := idx.updateFTS(o.out, changed); rerr != nil {
				fmt.Fprintf(os.Stderr, "fts5: update failed: %v (search will fall back to scan)\n", rerr)
			}
			idx.close()
		}
		finished := time.Now().UTC()
		entry := logEntry{
			StartedAt:  now,
			FinishedAt: finished.Format(time.RFC3339),
			ElapsedMs:  finished.Sub(start.UTC()).Milliseconds(),
			Mode:       "pull-article",
			Args:       os.Args[1:],
			Sources:    []string{articleSource},
			URLs:       1,
		}
		if r.Skipped != "" {
			entry.Skipped = 1
		} else {
			entry.Pulled = 1
		}
		if r.Warning != "" {
			entry.Warned = 1
		}
		return appendIngestLog(o.out, entry)
	}); err != nil {
		die(err)
	}

	switch {
	case r.Skipped != "":
		fmt.Printf("SKIP %s — %s\n", r.URL, r.Skipped)
		os.Exit(1)
	case r.Warning != "":
		fmt.Printf("WARN %s — %s\n  → %s\n", r.URL, r.Warning, r.Path)
	default:
		fmt.Printf("pulled %s\n  → %s (%s)\n", r.URL, r.Path, r.Mode)
	}
}

func processArticle(raw, nameOverride string, o pullOpts, now string) result {
	u, err := url.Parse(raw)
	if err != nil {
		return result{URL: raw, FetchedAt: now, Skipped: "parse error: " + err.Error()}
	}
	if u.Scheme == "" || u.Host == "" {
		return result{URL: raw, FetchedAt: now, Skipped: "URL needs scheme + host"}
	}

	host := articleHost(u.Host)
	slug := nameOverride
	if slug == "" {
		slug = articleSlug(u.Path)
	}
	if slug == "" {
		slug = "index"
	}
	rel := filepath.Join(host, slug+".md")

	data, mode, ferr := fetchArticle(u)
	if ferr != nil {
		return result{URL: raw, Source: articleSource, FetchedAt: now, Skipped: ferr.Error()}
	}

	outPath := filepath.Join(o.out, articleSource, rel)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return result{URL: raw, Source: articleSource, FetchedAt: now, Skipped: err.Error()}
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return result{URL: raw, Source: articleSource, FetchedAt: now, Skipped: err.Error()}
	}
	sum := sha256.Sum256(data)
	r := result{
		URL: raw, Source: articleSource,
		Path: filepath.Join(articleSource, rel), Mode: mode,
		SHA256: hex.EncodeToString(sum[:]), FetchedAt: now,
	}
	if mode != "reddit-json" && len(strings.TrimSpace(string(data))) < thinContentThreshold {
		r.Warning = fmt.Sprintf("low-content (%d bytes) — selector may have missed real content or page is client-rendered", len(data))
	}
	return r
}

// articleHost normalizes a hostname for use as a directory name. Strips www.
// and lowercases. Keeps dots so "dev.to" and "reddit.com" stay readable.
func articleHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimPrefix(h, "www.")
	h = strings.TrimPrefix(h, "old.")
	h = strings.TrimPrefix(h, "m.")
	return h
}

// articleSlug picks the most informative segment of a URL path. Prefers the
// last non-numeric segment (so reddit's `/r/X/comments/<id>/<title>/` resolves
// to `<title>` rather than `<id>`). Falls back to joining segments with `_`
// when nothing slug-like surfaces.
func articleSlug(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		cleaned = append(cleaned, p)
	}
	if len(cleaned) == 0 {
		return ""
	}
	pick := func(s string) string {
		s = strings.ToLower(s)
		s = articleSlugRE.ReplaceAllString(s, "-")
		s = strings.Trim(s, "-")
		if len(s) > 120 {
			s = s[:120]
		}
		return s
	}
	for i := len(cleaned) - 1; i >= 0; i-- {
		seg := cleaned[i]
		if isNumericish(seg) || len(seg) < 4 {
			continue
		}
		if s := pick(seg); s != "" {
			return s
		}
	}
	return pick(strings.Join(cleaned, "_"))
}

func isNumericish(s string) bool {
	if s == "" {
		return false
	}
	digits := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits*2 >= len(s) // mostly digits, e.g. "1opxf9f" or "12345"
}

// fetchArticle dispatches to the right strategy for the host. Reddit uses the
// JSON endpoint; everything else goes through the generic HTML→Markdown path.
func fetchArticle(u *url.URL) ([]byte, string, error) {
	host := strings.ToLower(u.Host)
	switch {
	case strings.HasSuffix(host, "reddit.com"):
		md, err := fetchRedditThread(u)
		if err != nil {
			return nil, "", err
		}
		return md, "reddit-json", nil
	default:
		data, err := fetchAndConvert(u.String())
		if err != nil {
			return nil, "", err
		}
		return data, "http", nil
	}
}

// fetchRedditThread loads a thread via Reddit's public JSON endpoint and
// renders post body + top-level comments as Markdown. Reddit 403s the
// browser-imitating UA we'd otherwise need to scrape HTML; the JSON endpoint
// returns content for any UA.
func fetchRedditThread(u *url.URL) ([]byte, error) {
	jsonURL := *u
	jsonURL.Host = "www.reddit.com"
	jsonURL.Path = strings.TrimSuffix(jsonURL.Path, "/") + ".json"
	jsonURL.RawQuery = "raw_json=1"
	body, err := httpGet(jsonURL.String())
	if err != nil {
		return nil, searchruntime.ArticleRedditJSONFetchError(err)
	}
	var listings []struct {
		Data struct {
			Children []struct {
				Kind string          `json:"kind"`
				Data json.RawMessage `json:"data"`
			} `json:"children"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listings); err != nil {
		return nil, searchruntime.ArticleRedditJSONParseError(err)
	}
	if len(listings) < 2 || len(listings[0].Data.Children) == 0 {
		return nil, searchruntime.ArticleRedditEmptyListingsError()
	}

	var post struct {
		Title       string  `json:"title"`
		Selftext    string  `json:"selftext"`
		Author      string  `json:"author"`
		Subreddit   string  `json:"subreddit_name_prefixed"`
		CreatedUTC  float64 `json:"created_utc"`
		URL         string  `json:"url"`
		Permalink   string  `json:"permalink"`
		Score       int     `json:"score"`
		NumComments int     `json:"num_comments"`
	}
	if err := json.Unmarshal(listings[0].Data.Children[0].Data, &post); err != nil {
		return nil, searchruntime.ArticleRedditPostParseError(err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", strings.TrimSpace(post.Title))
	created := time.Unix(int64(post.CreatedUTC), 0).UTC().Format("2006-01-02")
	fmt.Fprintf(&b, "_Posted by u/%s in %s on %s • %d points • %d comments_\n\n",
		post.Author, post.Subreddit, created, post.Score, post.NumComments)
	if post.URL != "" && !strings.Contains(post.URL, "reddit.com") {
		fmt.Fprintf(&b, "Link: <%s>\n\n", post.URL)
	}
	if strings.TrimSpace(post.Selftext) != "" {
		b.WriteString(strings.TrimSpace(post.Selftext))
		b.WriteString("\n\n")
	}

	b.WriteString("## Comments\n\n")
	renderRedditComments(&b, listings[1].Data.Children, 0)
	return []byte(b.String()), nil
}

func renderRedditComments(b *strings.Builder, children []struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}, depth int) {
	for _, c := range children {
		if c.Kind != "t1" {
			continue
		}
		var cm struct {
			Author  string          `json:"author"`
			Body    string          `json:"body"`
			Score   int             `json:"score"`
			Replies json.RawMessage `json:"replies"`
		}
		if err := json.Unmarshal(c.Data, &cm); err != nil {
			continue
		}
		if cm.Author == "" || strings.TrimSpace(cm.Body) == "" {
			continue
		}
		indent := strings.Repeat("  ", depth)
		fmt.Fprintf(b, "%s- **u/%s** (%d): %s\n", indent, cm.Author, cm.Score, strings.ReplaceAll(strings.TrimSpace(cm.Body), "\n", "\n"+indent+"  "))
		// Recurse only one level deep — top-level + first replies is plenty
		// for indexing; deep threads pollute the doc with low-signal banter.
		if depth >= 1 || len(cm.Replies) == 0 || string(cm.Replies) == `""` {
			continue
		}
		var reply struct {
			Data struct {
				Children []struct {
					Kind string          `json:"kind"`
					Data json.RawMessage `json:"data"`
				} `json:"children"`
			} `json:"data"`
		}
		if err := json.Unmarshal(cm.Replies, &reply); err == nil {
			renderRedditComments(b, reply.Data.Children, depth+1)
		}
	}
}
