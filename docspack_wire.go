package main

// Optional docspack conversion path for fetchAndConvert (main.go).
//
// Default OFF. Set DOCS_PULLER_DOCSPACK=1 to route HTML -> Markdown
// conversion through internal/docspack (segment-allowlist cleaning with
// <main>/<article> landmark preference) instead of the default
// extractMain (extract.go) + html-to-markdown path. The docspack path
// emits heading-structured plain Markdown from cleaned segments; it
// intentionally does not preserve inline links/images the way
// html-to-markdown does, which is why it is opt-in until a corpus diff
// (see internal/docspack/INTEGRATION.md) justifies flipping the default.
//
// Transplanted 2026-11-09 from the Hyperbrowser-harvest docspack
// (hyperbrowserai/examples ragzip recipe, MIT) — branch docspack-transplant.

import (
	"os"
	"strings"

	"github.com/nstranquist/docs-puller/internal/docspack"
)

// docspackEnabled reports whether the docspack conversion path is on.
func docspackEnabled() bool {
	return os.Getenv("DOCS_PULLER_DOCSPACK") == "1"
}

// convertWithDocspack cleans raw HTML with docspack.CleanHTML and renders
// the surviving segments as Markdown: headings keep their level, <pre>
// blocks become fenced code blocks, list items become bullets.
func convertWithDocspack(html []byte) string {
	segments := docspack.CleanHTML(string(html))
	var b strings.Builder
	for _, s := range segments {
		switch s.Tag {
		case "h1", "h2", "h3", "h4":
			level := int(s.Tag[1]-'0')
			b.WriteString(strings.Repeat("#", level) + " " + s.Text + "\n\n")
		case "pre":
			b.WriteString("```\n" + s.Text + "\n```\n\n")
		case "li":
			b.WriteString("- " + s.Text + "\n")
		default:
			b.WriteString(s.Text + "\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}
