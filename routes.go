package main

import (
	"net/url"
	"path/filepath"
	"strings"
)

type targetRoute struct {
	name  string
	match func(*url.URL) (source, rel string, ok bool)
}

var targetRoutes = []targetRoute{
	{name: "supabase-docs", match: routeSupabaseDocs},
	{name: "supabase-pages", match: routeSupabasePages},
	{name: "github-readme", match: routeGithubReadme},
	{name: "postgresql", match: routePostgresql},
	{name: "sqlite", match: routeSQLite},
	{name: "clickhouse", match: routeClickHouse},
	{name: "microsoft-learn", match: routeMicrosoftLearn},
	{name: "openai", match: routeOpenAI},
	{name: "claude-code", match: routeClaudeCode},
	simpleRoute("cli.github.com", "gh-cli", relTrimmedPath),
	simpleRoute("code.visualstudio.com", "vscode", relTrimmedPath),
	simpleRoute("reactnative.dev", "react-native", relTrimmedPath),
	simpleRoute("docs.expo.dev", "expo", relTrimmedPath),
	simpleRoute("developer.android.com", "android", relTrimmedPath),
	simpleRoute("nextjs.org", "nextjs", relTrimmedPath),
	prefixedRoute("tailwindcss.com", "tailwindcss", "/docs", relAfterPrefix),
	simpleRoute("www.radix-ui.com", "radix-ui", relTrimmedPath),
	simpleRoute("lucide.dev", "lucide", relTrimmedPath),
	prefixedRoute("firebase.google.com", "firebase", "/docs", relMarkdownTextAfterPrefix),
	simpleRoute("rnfirebase.io", "react-native-firebase", relTrimmedPath),
	prefixedRoute("reactnavigation.org", "react-navigation", "/docs", relAfterPrefix),
	simpleRoute("redux-toolkit.js.org", "redux", relTrimmedPath),
	prefixedRoute("react-hook-form.com", "react-hook-form", "/docs", relAfterPrefix),
	simpleRoute("zod.dev", "zod", relTrimmedPath),
	simpleRoute("turbo.build", "turbo", relTrimmedPath),
	simpleRoute("turborepo.dev", "turbo", relTrimmedPath),
	simpleRoute("pnpm.io", "pnpm", relTrimmedPath),
	simpleRoute("getpino.io", "pino", relTrimmedPath),
	simpleRoute("expressjs.com", "express", relHTMLPath),
	prefixedRoute("axiom.co", "axiom", "/docs", relAfterPrefix),
	simpleRoute("docs.axiom.co", "axiom", relTrimmedPath),
	simpleRoute("docs.lemonsqueezy.com", "lemon-squeezy", relTrimmedPath),
	simpleRoute("cloud.google.com", "google-cloud", relTrimmedPath),
	simpleRoute("keystatic.com", "keystatic", relTrimmedPath),
	simpleRoute("developer.wordpress.org", "wordpress", relTrimmedPath),
	simpleRoute("mariadb.com", "mariadb", relTrimmedPath),
	simpleRoute("openfeature.dev", "openfeature", relTrimmedPath),
	simpleRoute("docs.growthbook.io", "growthbook", relTrimmedPath),
	simpleRoute("docs.temporal.io", "temporal", relTrimmedPath),
	simpleRoute("docs.peerdb.io", "peerdb", relTrimmedPath),
	prefixedRoute("min.io", "minio", "/docs/minio", relHTMLAfterPrefix),
	simpleRoute("docs.nativewind.dev", "nativewind", relTrimmedPath),
	simpleRoute("nativewind.dev", "nativewind", relTrimmedPath),
	simpleRoute("www.nativewind.dev", "nativewind", relTrimmedPath),
	prefixedRoute("shopify.github.io", "react-native-skia", "/react-native-skia", relAfterPrefix),
	{name: "software-mansion", match: routeSoftwareMansionDocs},
	simpleRoute("dev.appsflyer.com", "appsflyer", relTrimmedPath),
	simpleRoute("docs.sentry.io", "sentry", relTrimmedPath),
	simpleRoute("developer.chrome.com", "chrome", relTrimmedPath),
	simpleRoute("posthog.com", "posthog", relTrimmedPath),
	{name: "redis", match: routeRedis},
	{name: "lua", match: routeLua},
	{name: "hammerspoon", match: routeHammerspoon},
	{name: "go", match: routeGo},
	simpleRoute("docs.obsidian.md", "obsidian-app-docs", relTrimmedPath),
	simpleRoute("docs.slack.dev", "slack", relMarkdownMirror),
	simpleRoute("docs.x.ai", "xai", relMarkdownMirror),
	simpleRoute("docs.langchain.com", "langchain", relMarkdownMirror),
	simpleRoute("ai-sdk.dev", "ai-sdk", relMarkdownMirror),
	simpleRoute("react.dev", "react", relMarkdownMirror),
	prefixedRoute("bun.com", "bun", "/docs", relMarkdownMirrorAfterPrefix),
	simpleRoute("playwright.dev", "playwright", relTrimmedPath),
	prefixedRoute("hono.dev", "hono", "/docs", relAfterPrefix),
	prefixedRoute("orm.drizzle.team", "drizzle", "/docs", relAfterPrefix),
	simpleRoute("developers.cloudflare.com", "cloudflare", relTrimmedPath),
	simpleRoute("docs.stripe.com", "stripe", relMarkdownMirror),
	simpleRoute("tanstack.com", "tanstack", relMarkdownMirror),
	prefixedRoute("vercel.com", "vercel", "/docs", relMarkdownMirrorAfterPrefix),
	{name: "anthropic", match: routeAnthropic},
	{name: "sqlc", match: routeSQLC},
	simpleRoute("docs.n8n.io", "n8n", relTrimmedPath),
	{name: "linear", match: routeLinear},
}

// resolveTarget maps a URL to (sourceDir, relativePath under that source).
func resolveTarget(u *url.URL) (source, rel string) {
	for _, route := range targetRoutes {
		if source, rel, ok := route.match(u); ok {
			return source, rel
		}
	}
	return "", ""
}

func simpleRoute(host, source string, relFn func(*url.URL) string) targetRoute {
	return targetRoute{
		name: host,
		match: func(u *url.URL) (string, string, bool) {
			if u.Host != host {
				return "", "", false
			}
			return source, relFn(u), true
		},
	}
}

func prefixedRoute(host, source, prefix string, relFn func(*url.URL, string) string) targetRoute {
	return targetRoute{
		name: host,
		match: func(u *url.URL) (string, string, bool) {
			if u.Host != host || !strings.HasPrefix(u.Path, prefix) {
				return "", "", false
			}
			return source, relFn(u, prefix), true
		},
	}
}

func relTrimmedPath(u *url.URL) string {
	p := strings.Trim(u.Path, "/")
	return relWithIndex(p)
}

func relHTMLPath(u *url.URL) string {
	p := strings.Trim(u.Path, "/")
	p = strings.TrimSuffix(p, ".html")
	return relWithIndex(p)
}

func relMarkdownMirror(u *url.URL) string {
	p := strings.TrimPrefix(u.Path, "/")
	p = strings.TrimSuffix(p, ".md")
	p = strings.Trim(p, "/")
	return relWithIndex(p)
}

func relAfterPrefix(u *url.URL, prefix string) string {
	p := strings.TrimPrefix(u.Path, prefix)
	p = strings.Trim(p, "/")
	return relWithIndex(p)
}

func relHTMLAfterPrefix(u *url.URL, prefix string) string {
	p := strings.TrimPrefix(u.Path, prefix)
	p = strings.Trim(p, "/")
	p = strings.TrimSuffix(p, ".html")
	return relWithIndex(p)
}

func relMarkdownMirrorAfterPrefix(u *url.URL, prefix string) string {
	p := strings.TrimPrefix(u.Path, prefix)
	p = strings.TrimSuffix(p, ".md")
	p = strings.Trim(p, "/")
	return relWithIndex(p)
}

func relMarkdownTextAfterPrefix(u *url.URL, prefix string) string {
	p := strings.TrimPrefix(u.Path, prefix)
	p = strings.TrimSuffix(p, ".md.txt")
	p = strings.TrimSuffix(p, ".md")
	p = strings.Trim(p, "/")
	return relWithIndex(p)
}

func relWithIndex(p string) string {
	if p == "" {
		p = "index"
	}
	return p + ".md"
}

func routeSupabaseDocs(u *url.URL) (string, string, bool) {
	if u.Host != supabaseHost || !strings.HasPrefix(u.Path, "/docs/") {
		return "", "", false
	}
	return "supabase", strings.TrimPrefix(u.Path, "/docs/") + ".md", true
}

func routeSupabasePages(u *url.URL) (string, string, bool) {
	if u.Host != "supabase.github.io" {
		return "", "", false
	}
	p := strings.Trim(u.Path, "/")
	if p == "" {
		return "supabase-pages", "index.md", true
	}
	parts := strings.SplitN(p, "/", 2)
	project := parts[0]
	sub := "index"
	if len(parts) == 2 && parts[1] != "" {
		sub = parts[1]
	}
	return project, sub + ".md", true
}

func routeGithubReadme(u *url.URL) (string, string, bool) {
	if u.Host != "github.com" {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	return "github", filepath.Join(parts[0], parts[1], "README.md"), true
}

func routePostgresql(u *url.URL) (string, string, bool) {
	if u.Host != "www.postgresql.org" && u.Host != "postgresql.org" {
		return "", "", false
	}
	return "postgresql", relHTMLPath(u), true
}

func routeSQLite(u *url.URL) (string, string, bool) {
	if u.Host != "sqlite.org" && u.Host != "www.sqlite.org" {
		return "", "", false
	}
	return "sqlite", relHTMLPath(u), true
}

func routeClickHouse(u *url.URL) (string, string, bool) {
	if u.Host != "clickhouse.com" {
		return "", "", false
	}
	return "clickhouse", relAfterPrefix(u, "/docs"), true
}

func routeSoftwareMansionDocs(u *url.URL) (string, string, bool) {
	if u.Host != "docs.swmansion.com" {
		return "", "", false
	}
	switch {
	case strings.HasPrefix(u.Path, "/react-native-reanimated"):
		return "react-native-reanimated", relAfterPrefix(u, "/react-native-reanimated"), true
	case strings.HasPrefix(u.Path, "/react-native-gesture-handler"):
		return "react-native-gesture-handler", relAfterPrefix(u, "/react-native-gesture-handler"), true
	default:
		return "", "", false
	}
}

func routeMicrosoftLearn(u *url.URL) (string, string, bool) {
	if u.Host != "learn.microsoft.com" {
		return "", "", false
	}
	p := strings.TrimPrefix(u.Path, "/")
	if i := strings.IndexByte(p, '/'); i > 0 && len(p[:i]) <= 6 && strings.Contains(p[:i], "-") {
		p = p[i+1:]
	}
	p = strings.Trim(p, "/")
	return "microsoft-learn", relWithIndex(p), true
}

func routeOpenAI(u *url.URL) (string, string, bool) {
	if u.Host != "developers.openai.com" {
		return "", "", false
	}
	p := strings.TrimPrefix(u.Path, "/")
	if p == "" {
		return "openai", "index.md", true
	}
	if strings.HasSuffix(p, "/") {
		p += "index"
	}
	p = strings.TrimSuffix(p, ".md")
	return "openai", p + ".md", true
}

func routeClaudeCode(u *url.URL) (string, string, bool) {
	if u.Host != "code.claude.com" {
		return "", "", false
	}
	p := strings.TrimPrefix(u.Path, "/docs/")
	p = strings.Trim(p, "/")
	if i := strings.IndexByte(p, '/'); i > 0 && len(p[:i]) <= 6 {
		p = p[i+1:]
	}
	// code.claude.com serves a `<page>.md` native-markdown mirror; strip the
	// suffix so it doesn't become `<page>.md.md` (mirrors routeAnthropic).
	p = strings.TrimSuffix(p, ".md")
	return "claude-code", relWithIndex(p), true
}

func routeRedis(u *url.URL) (string, string, bool) {
	if u.Host != "redis.io" {
		return "", "", false
	}
	p := strings.TrimPrefix(u.Path, "/docs/latest/")
	p = strings.TrimPrefix(p, "/docs/")
	p = strings.Trim(p, "/")
	return "redis", relWithIndex(p), true
}

func routeLua(u *url.URL) (string, string, bool) {
	if u.Host != "www.lua.org" {
		return "", "", false
	}
	return "lua", relHTMLPath(u), true
}

func routeHammerspoon(u *url.URL) (string, string, bool) {
	if u.Host != "www.hammerspoon.org" || !strings.HasPrefix(u.Path, "/docs") {
		return "", "", false
	}
	p := strings.TrimPrefix(u.Path, "/docs")
	p = strings.TrimSuffix(p, ".html")
	p = strings.Trim(p, "/")
	return "hammerspoon", relWithIndex(p), true
}

func routeGo(u *url.URL) (string, string, bool) {
	if u.Host != "go.dev" {
		return "", "", false
	}
	return "go", relHTMLPath(u), true
}

func routeAnthropic(u *url.URL) (string, string, bool) {
	if u.Host != "platform.claude.com" || !strings.HasPrefix(u.Path, "/docs/") {
		return "", "", false
	}
	p := strings.TrimPrefix(u.Path, "/docs/")
	if i := strings.IndexByte(p, '/'); i > 0 {
		locale := p[:i]
		if len(locale) <= 6 && (len(locale) == 2 || strings.Contains(locale, "-")) {
			p = p[i+1:]
		}
	}
	p = strings.TrimSuffix(p, ".md")
	p = strings.Trim(p, "/")
	return "anthropic", relWithIndex(p), true
}

func routeSQLC(u *url.URL) (string, string, bool) {
	if u.Host != "docs.sqlc.dev" {
		return "", "", false
	}
	p := strings.TrimPrefix(u.Path, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 && p[:i] == "en" {
		p = p[i+1:]
		if j := strings.IndexByte(p, '/'); j >= 0 {
			p = p[j+1:]
		}
	}
	p = strings.TrimSuffix(p, ".html")
	p = strings.Trim(p, "/")
	return "sqlc", relWithIndex(p), true
}

func routeLinear(u *url.URL) (string, string, bool) {
	if u.Host != "linear.app" || (u.Path != "/developers" && !strings.HasPrefix(u.Path, "/developers/")) {
		return "", "", false
	}
	p := strings.TrimPrefix(u.Path, "/developers")
	p = strings.Trim(p, "/")
	return "linear", relWithIndex(p), true
}
