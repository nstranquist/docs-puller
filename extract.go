package main

import (
	"bytes"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// hostSelectors lists per-host content selectors, tried in order.
var hostSelectors = map[string][]string{
	"supabase.github.io": {"article.md-content__inner", ".md-content"},
	"www.postgresql.org": {"#docContent", "#pgContentWrap", "div.SECT1"},
	"postgresql.org":     {"#docContent", "#pgContentWrap", "div.SECT1"},
	"supabase.com":       {"main"},
	// Docusaurus (used by clickhouse.com/docs and many others). The article
	// wraps title + content; .theme-doc-markdown is the content body alone.
	"clickhouse.com": {"article", ".theme-doc-markdown"},
	// Microsoft Learn uses [data-bi-name="content"] for the article body.
	// `main` is too broad here (includes header controls + ToC).
	"learn.microsoft.com": {"[data-bi-name='content']", "main"},
	// OpenAI Developers (Codex docs etc.) — Next.js with prose article.
	"developers.openai.com": {"article#mainContent", "article.prose", "main"},
	// Anthropic Claude Code docs — Mintlify-style with mdx-content div.
	"code.claude.com": {"#content.mdx-content", "#content", "article", "main"},
	// GitHub CLI manual — markdown-body class is GitHub's own convention.
	"cli.github.com": {"main.markdown-body", "main", "article"},
	// VS Code docs — main#main-content is the article container; .docs-main-content body class.
	"code.visualstudio.com": {"main#main-content", "main"},
	// React Native docs — Docusaurus.
	"reactnative.dev": {"article", ".theme-doc-markdown", "main"},
	// Expo docs — Mintlify-style.
	"docs.expo.dev": {"article", "main"},
	// Android developer docs — main#main-content is standard.
	"developer.android.com": {"main#main-content", "main", "article"},
	// Sentry docs — Docusaurus + custom.
	"docs.sentry.io": {"main", "article"},
	// Google DevSite (Android + Chrome) — main#main-content.devsite-main-content.
	"developer.chrome.com": {"main#main-content", "article.devsite-article", "main"},
	// Redis docs — `.prose` is the article body inside `main.docs`. Selecting
	// `.prose` directly avoids dragging in sidebar + version picker chrome.
	"redis.io": {".prose", "main.docs", "main"},
	// PostHog docs — Gatsby site, single article wrapper with reader-view +
	// prose classes. Must be selected explicitly because the page also has
	// `.prose` blocks inside the sidebar that would match generic selectors.
	"posthog.com": {"article.reader-view-content-container", "article.prose", "article"},
	// Linear developer docs — Next.js SPA but pages are prerendered. The
	// `<main>` wrapper includes the sidebar nav, so prefer `article` (which
	// is the DocsArticle body). Class names carry CSS-module hashes that
	// would rot, so match the bare tag.
	"linear.app": {"article", "main"},
	// Go.dev — Hugo-based site. <article class="Doc Article"> wraps the
	// main content; <main id="main-content"> includes the sidebar nav too.
	"go.dev": {"article.Doc", "main#main-content", "article", "main"},
	// Obsidian docs — Hugo-based, content lives inside <main> with a markdown
	// body and sidebar siblings. Prefer the article wrapper when present.
	"docs.obsidian.md": {"article", "main"},
	// Hono docs — VitePress site. `<main class="main">` wraps the content
	// body; the sidebar lives outside. The class hash (`data-v-...`) rotates
	// on every build, so match the bare class.
	"hono.dev": {"main.main", "main", "article"},
	// Drizzle ORM — Astro site. `<main class="documentation-container">`
	// wraps the content; sidebar is sibling. `data-astro-cid-*` hash rotates,
	// so match the class.
	"orm.drizzle.team": {"main.documentation-container", "main", "article"},
	// Cloudflare developer docs — Starlight (Astro). Content body is the
	// `<main>` element; the bare-tag fallback works because Starlight keeps
	// nav outside <main>.
	"developers.cloudflare.com": {"main", "article"},
	// React.dev — Next.js, but we use the .md mirrors so HTML selectors
	// rarely fire. Kept for completeness against the rare html-fallback path.
	"react.dev": {"article", "main"},
	// Bun docs — Astro/Starlight-style; bare main + article.
	"bun.com": {"article", "main"},
	// Playwright docs — Docusaurus (uses /docs/<lang>/...). Same selectors
	// as ClickHouse (also Docusaurus).
	"playwright.dev": {"article", ".theme-doc-markdown", "main"},
	// sqlc docs — Sphinx + RTD theme. Content lives inside
	// <div role="main" class="document">; the bodywrapper holds the article.
	"docs.sqlc.dev": {"div[role='main']", ".document", ".body", "main"},
}

// genericSelectors are the fallback content containers.
var genericSelectors = []string{"main", "article", "[role='main']", "#content", ".content", ".markdown-body"}

// stripWithin are removed from the chosen content container as nav chrome.
var stripWithin = strings.Join([]string{
	// Generic semantic chrome. NOTE: bare ".toc" is intentionally not
	// stripped — postgresql.org chapter intros use <dl class="toc"> for
	// the actual content. Framework-specific TOC selectors below cover
	// Docusaurus / MS Learn / mkdocs without false positives.
	"nav", "aside", "footer", "header",
	".sidebar", ".breadcrumb", ".pagination",
	"[aria-label='breadcrumb']",
	// Docusaurus.
	".theme-doc-toc-desktop", ".theme-doc-toc-mobile",
	".theme-doc-breadcrumbs", ".theme-doc-sidebar-container", ".pagination-nav",
	".theme-edit-this-page", ".theme-edit-this-page-wrapper",
	".theme-doc-version-banner", ".hash-link",
	// Microsoft Learn — page chrome inside [data-bi-name='content'].
	"[data-bi-name='content-header']", "[data-bi-name='pageactions']",
	"[data-bi-name='copy-markdown']", "[data-bi-name='print']",
	"[data-bi-name='language-toggle']", "[data-bi-name='theme']",
	"[data-bi-name='select-locale']", "[data-bi-name='focus-mode-entry']",
	"[data-bi-name='plan']", "[data-bi-name='contents-expand']",
	"[data-bi-name='intopic toc']",
	"[data-bi-name='aiDisclaimer']",
	"[data-bi-name='site-feedback-section']",
	"[data-bi-name='site-feedback-right-rail']",
	"[data-bi-name='feedback-suggest']",
	"[data-bi-name='ask-learn-assistant-entry']",
	"[data-bi-name='ask-learn-assistant-entry-troubleshoot']",
	"[data-bi-name='feedback-unhelpful-popover']",
	"[data-bi-name='open-source-feedback-section']",
	"[data-bi-name='archivelink']", "[data-bi-name='bloglink']",
	"[data-bi-name='contributorGuide']", "[data-bi-name='editMember']",
	"[data-bi-name='button-rating-yes']", "[data-bi-name='button-rating-no']",
	".authentication-determined", // MS Learn auth-required nag (p.authentication-determined)
	// PostgreSQL.org docs — chapter-level prev/up/home/next nav inside
	// #docContent, plus invisible <a class="indexterm"> anchors that the
	// converter emits as empty `[]()` links.
	".navheader", ".navfooter", "a.indexterm",
}, ", ")

// extractHTMLTitle returns the trimmed <title> element text with site/section
// prefixes/suffixes removed. Handles three common SEO conventions:
//
//   - Pipe suffix:  "Page Title | Site Name"          -> "Page Title"
//   - Dash suffix:  "Page Title - Site Name"          -> "Page Title"
//   - Colon prefix: "Site: Section: Page Title"       -> "Page Title"
//
// The colon-prefix variant only triggers on 2+ ": " separators so legitimate
// single-colon titles (e.g. "Section 5.6: System Columns") aren't truncated.
// The dash-suffix variant only triggers when the trailing segment looks like
// a site name (contains "Docs", "Documentation", or a known short site
// suffix) so legitimate em-dash-style titles like "Foo - a tool for X" aren't
// truncated. Empty string if <title> is absent.
func extractHTMLTitle(html []byte) string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return ""
	}
	t := strings.TrimSpace(doc.Find("title").First().Text())
	// Normalize non-breaking spaces (U+00A0, often from &nbsp;) into regular
	// spaces so separator detection below works on titles like
	// "Chrome Extensions [nbsp]|[nbsp] Chrome for Developers".
	t = strings.ReplaceAll(t, " ", " ")
	t = strings.Join(strings.Fields(t), " ")
	if i := strings.LastIndex(t, " | "); i > 0 {
		t = strings.TrimSpace(t[:i])
	}
	if i := strings.LastIndex(t, " - "); i > 0 {
		tail := strings.ToLower(t[i+3:])
		if strings.Contains(tail, "docs") || strings.Contains(tail, "documentation") {
			t = strings.TrimSpace(t[:i])
		}
	}
	if strings.Count(t, ": ") >= 2 {
		if i := strings.LastIndex(t, ": "); i > 0 {
			t = strings.TrimSpace(t[i+2:])
		}
	}
	return t
}

// extractMain selects the most likely main-content element for the given host
// and returns its outer HTML stripped of script/style/nav. Falls back to the
// whole document if no selector matches.
func extractMain(host string, html []byte) []byte {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return html
	}

	doc.Find("script, style, noscript, link, meta").Remove()

	selectors := append([]string{}, hostSelectors[host]...)
	selectors = append(selectors, genericSelectors...)

	for _, sel := range selectors {
		s := doc.Find(sel).First()
		if s.Length() == 0 {
			continue
		}
		s.Find(stripWithin).Remove()
		out, err := goquery.OuterHtml(s)
		if err == nil && len(out) > 0 {
			return []byte(out)
		}
	}
	out, err := doc.Html()
	if err != nil {
		return html
	}
	return []byte(out)
}
