package docspack

// Segment-allowlist HTML cleaning — Go port of clean.py.
//
// Based on the MIT-licensed ragzip recipe from
// https://github.com/hyperbrowserai/examples (ragzip/ragzip.ts cleanHtml),
// with the same two improvements as the Python implementation:
//  1. <main>/<article> landmark preference (fixes skip-link/chrome leaks).
//  2. Raised per-segment cap for <pre> so long code blocks survive.
//
// Divergence note: lengths/caps are BYTE counts here vs Python's unicode
// codepoint counts — identical on ASCII, slightly stricter on multibyte
// text. The segment extractor uses an element stack instead of Python's
// drop counters; results match on well-formed input (see PARITY.md).

import (
	"strings"
)

var keepTags = map[string]bool{
	"h1": true, "h2": true, "h3": true, "h4": true, "p": true,
	"li": true, "blockquote": true, "pre": true, "code": true,
}

var headingTags = map[string]bool{"h1": true, "h2": true, "h3": true, "h4": true}

var dropTags = map[string]bool{
	"script": true, "style": true, "nav": true, "footer": true,
	"aside": true, "form": true, "button": true, "iframe": true,
	"noscript": true, "svg": true, "header": true,
}

var dropClasses = map[string]bool{
	"sidebar": true, "nav": true, "footer": true, "header": true,
	"menu": true, "cookie": true, "banner": true, "popup": true, "modal": true,
}

const (
	MinSegmentChars = 20
	MaxSegmentChars = 2000  // ragzip's cap — kept for prose
	MaxPreChars     = 20000 // improvement: code blocks survive
)

// Segment is one (tag, text) content segment.
type Segment struct {
	Tag  string
	Text string
}

func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func hasDropClass(attrs map[string]string) bool {
	cls, ok := attrs["class"]
	if !ok {
		return false
	}
	for _, c := range strings.Fields(cls) {
		if dropClasses[c] {
			return true
		}
	}
	return false
}

// ExtractLandmarkHTML returns the first <main>/<article> subtree if
// present, else the full html (mirrors Python extract_landmark_html).
func ExtractLandmarkHTML(src string) string {
	toks := tokenize(src)
	target := ""
	depth := 0
	start := 0
	for _, t := range toks {
		if target == "" {
			if t.kind == tokStart && (t.tag == "main" || t.tag == "article") {
				target = t.tag
				depth = 1
				start = t.start
				if t.selfClose {
					return src[t.start:t.end]
				}
			}
			continue
		}
		if t.kind == tokStart && t.tag == target && !t.selfClose {
			depth++
		}
		if t.kind == tokEnd && t.tag == target {
			depth--
			if depth == 0 {
				return src[start:t.end]
			}
		}
	}
	return src
}

type openElem struct {
	tag     string
	dropped bool
}

// CleanHTML converts HTML to ordered content segments, boilerplate removed
// (mirrors Python clean_html, including exact dedupe on lowercase text).
func CleanHTML(src string) []Segment {
	scoped := ExtractLandmarkHTML(src)
	toks := tokenize(scoped)

	var segments []Segment
	var stack []openElem
	dropDepth := 0

	var curTag string
	var curParts []string
	innerDrop := 0

	flush := func() {
		if curTag == "" {
			return
		}
		raw := strings.Join(curParts, "")
		tag := curTag
		curTag, curParts, innerDrop = "", nil, 0
		if tag == "pre" {
			text := strings.TrimSpace(strings.Trim(raw, "\n"))
			if len(text) >= MinSegmentChars {
				if len(text) > MaxPreChars {
					text = text[:MaxPreChars]
				}
				segments = append(segments, Segment{tag, text})
			}
			return
		}
		text := collapseWS(raw)
		floor := MinSegmentChars
		if headingTags[tag] {
			floor = 1 // headings bypass the floor (section structure)
		}
		if len(text) >= floor {
			if len(text) > MaxSegmentChars {
				text = text[:MaxSegmentChars]
			}
			segments = append(segments, Segment{tag, text})
		}
	}

	for _, t := range toks {
		switch t.kind {
		case tokStart:
			dropped := dropDepth > 0 || dropTags[t.tag] || hasDropClass(t.attrs)
			if t.selfClose || voidTags[t.tag] {
				continue
			}
			stack = append(stack, openElem{t.tag, dropped})
			if dropped {
				dropDepth++
				continue
			}
			if curTag == "" && keepTags[t.tag] {
				curTag = t.tag
				curParts = nil
			} else if curTag != "" && dropTags[t.tag] {
				innerDrop++
			}
		case tokEnd:
			// pop the matching open element (and anything above it)
			poppedDropped := false
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i].tag == t.tag {
					for _, e := range stack[i:] {
						if e.dropped {
							poppedDropped = true
						}
					}
					if stack[i].dropped {
						dropDepth--
					}
					stack = stack[:i]
					break
				}
			}
			_ = poppedDropped
			if dropDepth > 0 {
				continue
			}
			if curTag != "" {
				if innerDrop > 0 && dropTags[t.tag] {
					innerDrop--
					continue
				}
				if t.tag == curTag || ((t.tag == "pre" || t.tag == "code") && (curTag == "pre" || curTag == "code")) {
					flush()
				}
			}
		case tokText:
			if dropDepth > 0 || innerDrop > 0 {
				continue
			}
			if curTag != "" {
				curParts = append(curParts, t.text)
			}
		}
	}
	flush()

	seen := map[string]bool{}
	out := segments[:0]
	for _, s := range segments {
		key := strings.ToLower(s.Text)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}

// CleanToText renders segments as markdown-ish text (headings get #
// prefixes, pre gets code fences) — mirrors Python clean_to_text.
func CleanToText(src string) string {
	var lines []string
	for _, s := range CleanHTML(src) {
		switch {
		case headingTags[s.Tag]:
			lines = append(lines, strings.Repeat("#", int(s.Tag[1]-'0'))+" "+s.Text)
		case s.Tag == "pre":
			lines = append(lines, "```\n"+s.Text+"\n```")
		case s.Tag == "li":
			lines = append(lines, "- "+s.Text)
		case s.Tag == "blockquote":
			lines = append(lines, "> "+s.Text)
		default:
			lines = append(lines, s.Text)
		}
	}
	return strings.Join(lines, "\n\n")
}
