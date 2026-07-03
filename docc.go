package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nstranquist/docs-puller/searchruntime"
)

// DocC ingestion: developer.apple.com (and other DocC-published archives —
// Swift Package Manager, third-party Swift libraries) is a JSON-driven SPA.
// The HTML at /documentation/<bundle>/<path> is a 600-byte shell; the real
// content is at /tutorials/data/documentation/<bundle>/<path>.json (DocC
// reference render JSON, schema 0.3).
//
// Scope: works on any DocC archive. The doc:// identifier scheme uses
// reverse-DNS namespaces (com.apple.<bundle>, org.swift.<bundle>, or any
// third-party publisher's NS); bundleFromIdentifier accepts all of them.
// Identifiers without a recognizable namespace fall back to inferring the
// bundle from the page URL path. Does NOT work on Apple marketing pages
// (developer.apple.com/macos/, WWDC session pages, etc.) — those aren't DocC.
//
// Discovery: BFS over topicSections.identifiers, resolving each identifier
// via the page's references dict. seeAlso/relationships sections render
// inline but are not crawled — they spill cross-bundle (e.g. Swift Array's
// 43 "Conforms To" entries).
//
// Output: one markdown file per node under <out>/<source>/<rel>.md. Source
// name is `apple-<bundle>` for com.apple.* identifiers, otherwise just
// `<bundle>`; --name overrides. Relative path is the human URL minus
// `/documentation/<bundle>/`.

const (
	doccDataPrefix = "/tutorials/data"
	doccBundleNS   = "com.apple."
)

// doccNode is the top-level DocC reference render JSON. Fields not used by
// the converter are omitted; json.Unmarshal silently drops the rest.
type doccNode struct {
	SchemaVersion struct {
		Major, Minor, Patch int
	} `json:"schemaVersion"`
	Identifier struct {
		URL               string `json:"url"`
		InterfaceLanguage string `json:"interfaceLanguage"`
	} `json:"identifier"`
	Kind     string       `json:"kind"`
	Metadata doccMetadata `json:"metadata"`
	Abstract []doccInline `json:"abstract"`

	PrimaryContentSections []doccPrimary    `json:"primaryContentSections"`
	TopicSections          []doccTopicGroup `json:"topicSections"`
	SeeAlsoSections        []doccTopicGroup `json:"seeAlsoSections"`
	RelationshipsSections  []doccTopicGroup `json:"relationshipsSections"`

	References map[string]doccReference `json:"references"`
}

type doccMetadata struct {
	Title          string         `json:"title"`
	NavigatorTitle []doccInline   `json:"navigatorTitle"`
	Role           string         `json:"role"`
	RoleHeading    string         `json:"roleHeading"`
	SymbolKind     string         `json:"symbolKind"`
	ExternalID     string         `json:"externalID"`
	Modules        []doccModule   `json:"modules"`
	Platforms      []doccPlatform `json:"platforms"`
}

type doccModule struct {
	Name string `json:"name"`
}

type doccPlatform struct {
	Name         string `json:"name"`
	IntroducedAt string `json:"introducedAt"`
	DeprecatedAt string `json:"deprecatedAt"`
	Beta         bool   `json:"beta"`
	Unavailable  bool   `json:"unavailable"`
	Deprecated   bool   `json:"deprecated"`
}

type doccTopicGroup struct {
	Title       string       `json:"title"`
	Anchor      string       `json:"anchor"`
	Abstract    []doccInline `json:"abstract"`
	Identifiers []string     `json:"identifiers"`
}

type doccReference struct {
	Type     string       `json:"type"`
	Kind     string       `json:"kind"`
	Title    string       `json:"title"`
	URL      string       `json:"url"`
	Role     string       `json:"role"`
	Abstract []doccInline `json:"abstract"`
}

// doccPrimary is one of: declarations, content, properties, parameters,
// returns, mentions, restBody, restEndpoint. The unmarshaler is tagged-union;
// unused fields stay zero.
type doccPrimary struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	// declarations
	Declarations []doccDeclaration `json:"declarations,omitempty"`
	// content / returns
	Content []doccBlock `json:"content,omitempty"`
	// properties / parameters
	Items []doccPropItem `json:"items,omitempty"`
}

type doccDeclaration struct {
	Languages []string    `json:"languages"`
	Platforms []string    `json:"platforms"`
	Tokens    []doccToken `json:"tokens"`
}

type doccToken struct {
	Text       string `json:"text"`
	Kind       string `json:"kind"`
	Identifier string `json:"identifier"`
}

type doccPropItem struct {
	Name              string      `json:"name"`
	Type              []doccToken `json:"type"`
	Content           []doccBlock `json:"content"`
	Attributes        []doccAttr  `json:"attributes"`
	Required          bool        `json:"required"`
	Deprecated        bool        `json:"deprecated"`
	IntroducedVersion string      `json:"introducedVersion"`
}

type doccAttr struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	Title string `json:"title"`
}

// doccBlock and doccInline are tagged-union nodes. We unmarshal into a
// permissive struct that holds every field used across the dispatch in
// the converter — JSON decoding cost is similar to using json.RawMessage +
// type-switch and the code is far simpler.
type doccBlock struct {
	Type string `json:"type"`

	// heading
	Level  int    `json:"level"`
	Text   string `json:"text"`
	Anchor string `json:"anchor"`

	// paragraph
	InlineContent []doccInline `json:"inlineContent"`

	// codeListing
	Syntax string   `json:"syntax"`
	Code   []string `json:"code"`

	// aside
	Style   string      `json:"style"`
	Name    string      `json:"name"`
	Content []doccBlock `json:"content"`

	// orderedList / unorderedList
	Items []doccListItem `json:"items"`

	// table
	Header string          `json:"header"`
	Rows   [][][]doccBlock `json:"rows"`

	// links (style: list, compactGrid, detailedGrid)
	// reuses Items for identifiers list when style != ""

	// termList
	// items reused with TermList semantics
}

type doccListItem struct {
	Content []doccBlock `json:"content"`
	// termList items
	Term struct {
		InlineContent []doccInline `json:"inlineContent"`
	} `json:"term"`
	Definition struct {
		Content []doccBlock `json:"content"`
	} `json:"definition"`
}

type doccInline struct {
	Type string `json:"type"`

	// text
	Text string `json:"text"`

	// codeVoice
	Code string `json:"code"`

	// reference / image
	Identifier string `json:"identifier"`
	IsActive   bool   `json:"isActive"`

	// link
	Title       string `json:"title"`
	Destination string `json:"destination"`

	// emphasis / strong / inlineHead
	InlineContent []doccInline `json:"inlineContent"`
}

// ─────────────────────────── URL & source mapping ───────────────────────────

// pageURLToJSONURL converts a developer-facing URL to its DocC JSON URL.
//
//	https://developer.apple.com/documentation/devicemanagement/desktop
//	  → https://developer.apple.com/tutorials/data/documentation/devicemanagement/desktop.json
//
// Returns the input unchanged when it already points at a /tutorials/data/
// JSON URL, so callers can pass either form.
func pageURLToJSONURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", searchruntime.DocCNotAbsoluteURLError(rawURL)
	}
	p := u.Path
	if strings.HasPrefix(p, doccDataPrefix+"/") {
		// Already a JSON URL (or close to it). Ensure trailing .json.
		if !strings.HasSuffix(p, ".json") {
			p += ".json"
		}
	} else {
		p = strings.TrimSuffix(p, "/")
		p = doccDataPrefix + p + ".json"
	}
	u2 := *u
	u2.Path = p
	u2.RawQuery = ""
	u2.Fragment = ""
	return u2.String(), nil
}

// jsonURLToPageURL is the inverse of pageURLToJSONURL — used to derive the
// citation-friendly URL stored in the manifest from a JSON fetch URL.
func jsonURLToPageURL(jsonURL string) string {
	u, err := url.Parse(jsonURL)
	if err != nil {
		return jsonURL
	}
	p := u.Path
	p = strings.TrimSuffix(p, ".json")
	p = strings.TrimPrefix(p, doccDataPrefix)
	if p == "" {
		p = "/"
	}
	u.Path = p
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// bundleFromIdentifier extracts the bundle name from a doc:// identifier.
// DocC uses reverse-DNS namespaces — Apple uses com.apple.<bundle>, Swift
// Package Manager uses org.swift.<bundle> or any third-party publisher's NS.
// The bundle is the last dot-separated segment of the namespace authority.
//
//	doc://com.apple.devicemanagement/documentation/DeviceManagement/Desktop
//	  → "devicemanagement"
//	doc://com.apple.swift/documentation/Swift/Array
//	  → "swift"
//	doc://com.apple.documentation/...
//	  → "documentation"  (Apple's catch-all "Apple Documentation Archive")
//	doc://org.swift.swift-syntax/documentation/SwiftSyntax/...
//	  → "swift-syntax"
//	doc://MyPackage/documentation/MyPackage/...
//	  → "MyPackage"  (single-segment namespace, common for SwiftPM docs)
//
// Returns "" when the identifier doesn't have a doc:// scheme at all.
func bundleFromIdentifier(id string) string {
	const scheme = "doc://"
	if !strings.HasPrefix(id, scheme) {
		return ""
	}
	rest := id[len(scheme):]
	// Authority ends at the first '/'.
	authority := rest
	if i := strings.IndexByte(rest, '/'); i > 0 {
		authority = rest[:i]
	}
	if authority == "" {
		return ""
	}
	// Bundle is the last dot-separated segment ("com.apple.devicemanagement"
	// → "devicemanagement"; bare "MyPackage" → "MyPackage").
	if j := strings.LastIndexByte(authority, '.'); j >= 0 {
		return authority[j+1:]
	}
	return authority
}

// sourceNameForBundle picks the on-disk directory name for a bundle. Apple
// bundles get an `apple-` prefix to disambiguate (e.g. `apple-swift` vs the
// hypothetical Swift-Package-Manager `swift` source); other publishers use
// the bare bundle name.
func sourceNameForBundle(identifier, bundle string) string {
	if bundle == "" {
		return ""
	}
	if strings.HasPrefix(identifier, "doc://"+doccBundleNS) {
		return sanitizeSourceName("apple-" + bundle)
	}
	return sanitizeSourceName(bundle)
}

// pageURLToRelPath maps a page URL to a stable file path under the source
// dir. /documentation/<bundle>/<rest> → <rest>.md. The root collection
// (/documentation/<bundle>) → index.md.
func pageURLToRelPath(pageURL, bundle string) string {
	u, err := url.Parse(pageURL)
	if err != nil {
		return "index.md"
	}
	p := strings.TrimSuffix(u.Path, "/")
	p = strings.TrimPrefix(p, "/documentation/"+bundle)
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "index.md"
	}
	return p + ".md"
}

// ─────────────────────────── Markdown conversion ───────────────────────────

// bufPool reuses *bytes.Buffer instances across renderDocC calls. bytes.Buffer
// keeps its backing array on Reset (unlike strings.Builder which nils it), so
// the warm path only allocates the output []byte copy.
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

func getBuf(reserve int) *bytes.Buffer {
	buf, ok := bufPool.Get().(*bytes.Buffer)
	if !ok || buf == nil {
		buf = new(bytes.Buffer)
	}
	buf.Reset()
	if reserve > 0 && buf.Cap() < reserve {
		buf.Grow(reserve - buf.Cap())
	}
	return buf
}

func putBuf(buf *bytes.Buffer) {
	// Don't pool runaway buffers — one giant Swift Array doc (340 KB) shouldn't
	// pin a megabyte of scratch memory in the pool for every later render.
	if buf.Cap() > 1<<20 {
		return
	}
	bufPool.Put(buf)
}

// writeIntDecimal emits a non-negative int as decimal without going through
// strconv.Itoa (one alloc) or fmt.Fprintf. Used for ordered-list numbers.
func writeIntDecimal(b *bytes.Buffer, n int) {
	if n == 0 {
		b.WriteByte('0')
		return
	}
	if n < 0 {
		b.WriteByte('-')
		n = -n
	}
	var arr [20]byte
	i := len(arr)
	for n > 0 {
		i--
		arr[i] = byte('0' + n%10)
		n /= 10
	}
	b.Write(arr[i:])
}

// writeMDLink emits `[title](url)` without fmt.Fprintf's interface boxing or
// format-string parsing. Hot path — every topic and reference inline.
func writeMDLink(b *bytes.Buffer, title, url string) {
	b.WriteByte('[')
	b.WriteString(title)
	b.WriteString("](")
	b.WriteString(url)
	b.WriteByte(')')
}

// writeHeading emits an n-level markdown heading with trailing blank line:
// "## Title\n\n". Avoids fmt.Fprintf's format-string parsing.
func writeHeading(b *bytes.Buffer, level int, text string) {
	if level < 1 {
		level = 1
	}
	if level > 6 {
		level = 6
	}
	for i := 0; i < level; i++ {
		b.WriteByte('#')
	}
	b.WriteByte(' ')
	b.WriteString(text)
	b.WriteString("\n\n")
}

// estimateRenderSize gives sync.Pool's Grow a lower bound based on the input
// shape. Empirically the markdown is ~1.5× the JSON node size; we
// conservatively use 1× JSON size + a fixed budget for headers/topics. This
// avoids the 4× capacity-doubling growth that strings.Builder does for large
// outputs.
func estimateRenderSize(n *doccNode) int {
	size := 256
	for _, sec := range n.PrimaryContentSections {
		size += 64
		size += len(sec.Content) * 80
		size += len(sec.Items) * 200
	}
	for _, g := range n.TopicSections {
		size += 64 + len(g.Identifiers)*80
	}
	for _, g := range n.SeeAlsoSections {
		size += 64 + len(g.Identifiers)*80
	}
	for _, g := range n.RelationshipsSections {
		size += 64 + len(g.Identifiers)*80
	}
	if size > 64*1024 {
		size = 64 * 1024
	}
	return size
}

// renderDocC converts a parsed DocC node to markdown. The shape is fixed so
// agents and search hits are predictable across symbol/article/collection
// kinds: title H1, abstract blockquote, role-heading, declarations, primary
// content, properties/parameters/returns, topics, see also, relationships.
func renderDocC(n *doccNode) []byte {
	b := getBuf(estimateRenderSize(n))
	defer putBuf(b)

	title := strings.TrimSpace(n.Metadata.Title)
	if title == "" {
		title = lastPathSegment(n.Identifier.URL)
	}
	if title != "" {
		b.WriteString("# ")
		b.WriteString(title)
		b.WriteString("\n\n")
	}

	if heading := strings.TrimSpace(n.Metadata.RoleHeading); heading != "" {
		b.WriteByte('*')
		b.WriteString(heading)
		b.WriteString("*\n\n")
	}

	if abs := renderInlines(n.Abstract, n.References); abs != "" {
		b.WriteString("> ")
		b.WriteString(strings.ReplaceAll(abs, "\n", "\n> "))
		b.WriteString("\n\n")
	}

	mods := moduleNames(n.Metadata.Modules)
	if mods != "" {
		b.WriteString("**Module:** ")
		b.WriteString(mods)
		b.WriteString("  \n")
	}
	if plats := platformLine(n.Metadata.Platforms); plats != "" {
		b.WriteString("**Platforms:** ")
		b.WriteString(plats)
		b.WriteString("\n\n")
	} else if mods != "" {
		b.WriteByte('\n')
	}

	for _, sec := range n.PrimaryContentSections {
		switch sec.Kind {
		case "declarations":
			renderDeclarations(b, sec.Declarations)
		case "content":
			renderBlocks(b, sec.Content, n.References, 2)
		case "properties":
			renderPropItems(b, "Properties", sec.Items, n.References)
		case "parameters":
			renderPropItems(b, "Parameters", sec.Items, n.References)
		case "returns":
			b.WriteString("## Returns\n\n")
			renderBlocks(b, sec.Content, n.References, 3)
		case "mentions":
			if len(sec.Content) > 0 {
				b.WriteString("## Mentioned In\n\n")
				renderBlocks(b, sec.Content, n.References, 3)
			}
		default:
			if sec.Title != "" {
				writeHeading(b, 2, sec.Title)
			}
			renderBlocks(b, sec.Content, n.References, 3)
		}
	}

	renderTopics(b, "Topics", n.TopicSections, n.References)
	renderTopics(b, "Relationships", n.RelationshipsSections, n.References)
	renderTopics(b, "See Also", n.SeeAlsoSections, n.References)

	// Copy out — caller's []byte must not alias the pooled buffer.
	out := make([]byte, b.Len())
	copy(out, b.Bytes())
	return out
}

// titleCase uppercases the first rune of s. strings.Title was deprecated in
// Go 1.18; we don't need its full Unicode-aware word-boundary semantics.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	if c := s[0]; c >= 'a' && c <= 'z' {
		return string(c-32) + s[1:]
	}
	return s
}

func lastPathSegment(s string) string {
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func moduleNames(mods []doccModule) string {
	if len(mods) == 0 {
		return ""
	}
	names := make([]string, 0, len(mods))
	for _, m := range mods {
		if m.Name != "" {
			names = append(names, m.Name)
		}
	}
	return strings.Join(names, ", ")
}

func platformLine(plats []doccPlatform) string {
	if len(plats) == 0 {
		return ""
	}
	out := make([]string, 0, len(plats))
	for _, p := range plats {
		if p.Name == "" || p.Unavailable {
			continue
		}
		s := p.Name
		if p.IntroducedAt != "" {
			s += " " + p.IntroducedAt + "+"
		}
		if p.Deprecated {
			s += " (deprecated"
			if p.DeprecatedAt != "" {
				s += " in " + p.DeprecatedAt
			}
			s += ")"
		} else if p.Beta {
			s += " (beta)"
		}
		out = append(out, s)
	}
	return strings.Join(out, " · ")
}

func renderDeclarations(b *bytes.Buffer, decls []doccDeclaration) {
	for _, d := range decls {
		lang := "swift"
		if len(d.Languages) > 0 {
			lang = d.Languages[0]
		}
		// DocC uses "data" for Property List/JSON dictionary symbols.
		if lang == "data" {
			lang = ""
		}
		b.WriteString("```")
		b.WriteString(lang)
		b.WriteByte('\n')
		for _, t := range d.Tokens {
			b.WriteString(t.Text)
		}
		b.WriteString("\n```\n\n")
	}
}

func renderPropItems(b *bytes.Buffer, header string, items []doccPropItem, refs map[string]doccReference) {
	if len(items) == 0 {
		return
	}
	writeHeading(b, 2, header)
	for _, it := range items {
		b.WriteString("### `")
		b.WriteString(it.Name)
		b.WriteByte('`')
		if typ := tokensToString(it.Type); typ != "" {
			b.WriteString(" : ")
			b.WriteString(typ)
		}
		b.WriteString("\n\n")
		var tags []string
		if it.Required {
			tags = append(tags, "required")
		} else {
			tags = append(tags, "optional")
		}
		if it.Deprecated {
			tags = append(tags, "deprecated")
		}
		if it.IntroducedVersion != "" {
			tags = append(tags, "since "+it.IntroducedVersion)
		}
		for _, a := range it.Attributes {
			if a.Kind == "default" && a.Value != "" {
				tags = append(tags, "default: "+a.Value)
			}
		}
		if len(tags) > 0 {
			b.WriteByte('*')
			b.WriteString(strings.Join(tags, " · "))
			b.WriteString("*\n\n")
		}
		renderBlocks(b, it.Content, refs, 4)
	}
}

func tokensToString(toks []doccToken) string {
	if len(toks) == 0 {
		return ""
	}
	if len(toks) == 1 {
		return strings.TrimSpace(toks[0].Text)
	}
	var s strings.Builder
	total := 0
	for _, t := range toks {
		total += len(t.Text)
	}
	s.Grow(total)
	for _, t := range toks {
		s.WriteString(t.Text)
	}
	return strings.TrimSpace(s.String())
}

func renderTopics(b *bytes.Buffer, header string, groups []doccTopicGroup, refs map[string]doccReference) {
	hasContent := false
	for _, g := range groups {
		if len(g.Identifiers) > 0 {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return
	}
	writeHeading(b, 2, header)
	for _, g := range groups {
		if len(g.Identifiers) == 0 {
			continue
		}
		if g.Title != "" {
			writeHeading(b, 3, g.Title)
		}
		for _, id := range g.Identifiers {
			ref, ok := refs[id]
			if !ok {
				b.WriteString("- ")
				b.WriteString(lastPathSegment(id))
				b.WriteByte('\n')
				continue
			}
			b.WriteString("- ")
			writeMDLink(b, refTitle(ref, id), refURL(ref))
			if abs := renderInlines(ref.Abstract, refs); abs != "" {
				b.WriteString(" — ")
				b.WriteString(abs)
			}
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
}

func refTitle(r doccReference, id string) string {
	if t := strings.TrimSpace(r.Title); t != "" {
		return t
	}
	return lastPathSegment(id)
}

// refURL returns an absolute developer.apple.com URL for a reference. The
// references dict gives root-relative paths like /documentation/foo.
func refURL(r doccReference) string {
	if r.URL == "" {
		return ""
	}
	if strings.HasPrefix(r.URL, "http://") || strings.HasPrefix(r.URL, "https://") {
		return r.URL
	}
	if strings.HasPrefix(r.URL, "/") {
		return "https://developer.apple.com" + r.URL
	}
	return "https://developer.apple.com/" + r.URL
}

// renderBlocks emits markdown for a list of block-level content nodes. The
// `headingBase` arg lets callers shift heading levels so a "## Returns" block
// containing a level-2 heading doesn't outrank the section header.
func renderBlocks(b *bytes.Buffer, blocks []doccBlock, refs map[string]doccReference, headingBase int) {
	for _, blk := range blocks {
		switch blk.Type {
		case "paragraph":
			if s := renderInlines(blk.InlineContent, refs); s != "" {
				b.WriteString(s)
				b.WriteString("\n\n")
			}
		case "heading":
			level := blk.Level
			if level < 1 {
				level = 1
			}
			level += headingBase - 1
			writeHeading(b, level, blk.Text)
		case "codeListing":
			b.WriteString("```")
			b.WriteString(blk.Syntax)
			b.WriteByte('\n')
			for _, line := range blk.Code {
				b.WriteString(line)
				b.WriteByte('\n')
			}
			b.WriteString("```\n\n")
		case "aside":
			renderAside(b, blk, refs, headingBase)
		case "unorderedList":
			renderListItems(b, blk.Items, refs, "- ", headingBase)
		case "orderedList":
			for k, item := range blk.Items {
				writeIntDecimal(b, k+1)
				b.WriteString(". ")
				renderListItemContent(b, item.Content, refs, headingBase)
			}
			b.WriteByte('\n')
		case "termList":
			for _, item := range blk.Items {
				term := renderInlines(item.Term.InlineContent, refs)
				b.WriteString("**")
				b.WriteString(term)
				b.WriteString("**\n")
				renderBlocks(b, item.Definition.Content, refs, headingBase+1)
			}
		case "table":
			renderTable(b, blk, refs)
		case "links":
			// links sections reuse Items as a list of {content: [paragraph(reference)]}
			// in the DocC schema; render as bullet list.
			renderListItems(b, blk.Items, refs, "- ", headingBase)
		case "":
			// Empty type — happens when a sub-array slot is empty. Skip.
		default:
			// Unknown block kind — render inline content if any.
			if s := renderInlines(blk.InlineContent, refs); s != "" {
				b.WriteString(s)
				b.WriteString("\n\n")
			}
		}
	}
}

// renderAside writes a DocC aside as a GFM blockquote. We pool the inner
// buffer rather than using a fresh strings.Builder per call.
func renderAside(b *bytes.Buffer, blk doccBlock, refs map[string]doccReference, headingBase int) {
	label := blk.Name
	if label == "" {
		label = blk.Style
	}
	if label == "" {
		label = "Note"
	}
	sub := getBuf(256)
	defer putBuf(sub)
	renderBlocks(sub, blk.Content, refs, headingBase+1)
	body := bytes.TrimRight(sub.Bytes(), "\n")

	b.WriteString("> **")
	b.WriteString(titleCase(label))
	b.WriteString(".** ")
	for j, line := range bytes.Split(body, []byte{'\n'}) {
		if j > 0 {
			b.WriteString("> ")
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
}

func renderListItems(b *bytes.Buffer, items []doccListItem, refs map[string]doccReference, prefix string, headingBase int) {
	for _, item := range items {
		b.WriteString(prefix)
		renderListItemContent(b, item.Content, refs, headingBase)
	}
	b.WriteByte('\n')
}

// renderListItemContent emits a single list item. When the item's content is
// just one paragraph we keep it inline with the bullet; otherwise we drop a
// blank line and indent the rest as continuation.
func renderListItemContent(b *bytes.Buffer, content []doccBlock, refs map[string]doccReference, headingBase int) {
	if len(content) == 0 {
		b.WriteByte('\n')
		return
	}
	first := content[0]
	if first.Type == "paragraph" {
		b.WriteString(renderInlines(first.InlineContent, refs))
		b.WriteByte('\n')
		if len(content) > 1 {
			sub := getBuf(256)
			renderBlocks(sub, content[1:], refs, headingBase+1)
			indented := bytes.ReplaceAll(bytes.TrimRight(sub.Bytes(), "\n"), []byte{'\n'}, []byte("\n  "))
			b.WriteString("  ")
			b.Write(indented)
			b.WriteByte('\n')
			putBuf(sub)
		}
		return
	}
	b.WriteByte('\n')
	sub := getBuf(256)
	renderBlocks(sub, content, refs, headingBase+1)
	indented := bytes.ReplaceAll(bytes.TrimRight(sub.Bytes(), "\n"), []byte{'\n'}, []byte("\n  "))
	b.WriteString("  ")
	b.Write(indented)
	b.WriteByte('\n')
	putBuf(sub)
}

// renderTable emits a GFM pipe table. DocC table rows are [][][]block, so each
// cell is itself a list of blocks; we flatten via a recursive text render and
// strip newlines so the cell stays on one row.
func renderTable(b *bytes.Buffer, blk doccBlock, refs map[string]doccReference) {
	if len(blk.Rows) == 0 {
		return
	}
	cellBuf := getBuf(128)
	defer putBuf(cellBuf)
	cellStr := func(cell []doccBlock) string {
		cellBuf.Reset()
		renderBlocks(cellBuf, cell, refs, 6)
		out := bytes.TrimSpace(cellBuf.Bytes())
		// Replace newlines + escape pipes; do both in one pass over the
		// trimmed bytes to avoid two ReplaceAll allocations.
		dst := make([]byte, 0, len(out))
		for _, c := range out {
			switch c {
			case '\n':
				dst = append(dst, ' ')
			case '|':
				dst = append(dst, '\\', '|')
			default:
				dst = append(dst, c)
			}
		}
		return string(dst)
	}
	cols := 0
	for _, row := range blk.Rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	if cols == 0 {
		return
	}
	for i, row := range blk.Rows {
		b.WriteByte('|')
		for c := 0; c < cols; c++ {
			b.WriteByte(' ')
			if c < len(row) {
				b.WriteString(cellStr(row[c]))
			}
			b.WriteString(" |")
		}
		b.WriteByte('\n')
		if i == 0 {
			b.WriteByte('|')
			for c := 0; c < cols; c++ {
				b.WriteString(" --- |")
			}
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
}

// renderInlines flattens a sequence of inline nodes to a markdown string.
// Resolves `reference` inlines via the refs dict; falls back to identifier
// last segment when the dict lacks a matching entry (cross-bundle ref).
//
// Hot path: called once per paragraph + once per topic-section reference
// abstract. We pool the working buffer to avoid per-call allocations.
func renderInlines(items []doccInline, refs map[string]doccReference) string {
	if len(items) == 0 {
		return ""
	}
	// Better initial-size estimate than 64*len(items): sum the text length
	// plus an overhead per node. Keeps trivial paragraphs from over-allocating.
	hint := 16
	for _, in := range items {
		hint += len(in.Text) + len(in.Code) + len(in.Title) + 8
	}
	b := getBuf(hint)
	defer putBuf(b)
	writeInlines(b, items, refs)
	return strings.TrimSpace(b.String())
}

// writeInlines is the shared implementation that writes inline content into
// an existing buffer. renderInlines wraps it for the string-returning callers.
func writeInlines(b *bytes.Buffer, items []doccInline, refs map[string]doccReference) {
	for _, in := range items {
		switch in.Type {
		case "text":
			b.WriteString(in.Text)
		case "codeVoice":
			b.WriteByte('`')
			b.WriteString(in.Code)
			b.WriteByte('`')
		case "reference":
			r, ok := refs[in.Identifier]
			if !ok {
				b.WriteString(lastPathSegment(in.Identifier))
				continue
			}
			if href := refURL(r); href != "" && in.IsActive {
				writeMDLink(b, refTitle(r, in.Identifier), href)
			} else {
				b.WriteString(refTitle(r, in.Identifier))
			}
		case "link":
			if in.Destination != "" {
				writeMDLink(b, in.Title, in.Destination)
			} else {
				b.WriteString(in.Title)
			}
		case "emphasis":
			b.WriteByte('*')
			writeInlines(b, in.InlineContent, refs)
			b.WriteByte('*')
		case "strong", "inlineHead", "newTerm":
			b.WriteString("**")
			writeInlines(b, in.InlineContent, refs)
			b.WriteString("**")
		case "image":
			// Images point at asset identifiers we'd need a separate fetch
			// for; we drop them rather than embed broken refs.
		case "superscript":
			b.WriteByte('^')
			writeInlines(b, in.InlineContent, refs)
			b.WriteByte('^')
		case "subscript":
			b.WriteByte('~')
			writeInlines(b, in.InlineContent, refs)
			b.WriteByte('~')
		default:
			// Unknown inline kind — drop silently rather than embed raw JSON.
		}
	}
}

// ─────────────────────────── BFS crawler ───────────────────────────

type doccCrawlOpts struct {
	rootURL             string
	filter              string
	maxNodes            int
	concurrency         int
	followSeeAlso       bool
	followRelationships bool
}

type doccCrawlResult struct {
	jsonURL    string
	pageURL    string
	relPath    string
	bundle     string
	identifier string // doc:// identifier — used to pick com.apple-vs-other source naming
	markdown   []byte
	title      string
	err        error
}

// crawlDocC walks the DocC graph starting from rootURL. It returns one entry
// per successfully-fetched node. Failures are recorded with err set and don't
// abort the whole crawl. A central frontier guarded by a mutex feeds workers
// and absorbs new identifiers as references resolve.
func crawlDocC(opts doccCrawlOpts) ([]doccCrawlResult, error) {
	rootJSON, err := pageURLToJSONURL(opts.rootURL)
	if err != nil {
		return nil, searchruntime.DocCRootURLError(err)
	}
	rootPage := jsonURLToPageURL(rootJSON)

	// Filter is evaluated against the page URL path (e.g. /documentation/...)
	// because that's the form references use. Default filter: keep everything
	// under the rootPage's path so we don't spill into another bundle when a
	// topic links into Foundation, Swift, etc.
	filter := opts.filter
	if filter == "" {
		ru, _ := url.Parse(rootPage)
		if ru != nil {
			filter = strings.TrimSuffix(ru.Path, "/")
		}
	}

	type frontierEntry struct {
		jsonURL string
		pageURL string
	}

	var (
		mu      sync.Mutex
		visited = map[string]bool{}
		results []doccCrawlResult
	)
	var stopped int32

	concurrency := opts.concurrency
	if concurrency < 1 {
		concurrency = 1
	}

	// Concurrency model: a buffered work channel feeds N workers; an atomic
	// `pending` counter tracks "in flight or in queue" items. When pending
	// hits zero, a closer goroutine closes the work channel so all workers
	// exit. The buffer is sized so a worker that just discovered 30 children
	// doesn't block while peers are idle.
	const chanCapPerWorker = 64
	work := make(chan frontierEntry, concurrency*chanCapPerWorker)
	var pending int64

	enqueue := func(e frontierEntry) bool {
		mu.Lock()
		if visited[e.jsonURL] {
			mu.Unlock()
			return false
		}
		if opts.maxNodes > 0 && len(visited) >= opts.maxNodes {
			atomic.StoreInt32(&stopped, 1)
			mu.Unlock()
			return false
		}
		if filter != "" {
			pu, err := url.Parse(e.pageURL)
			if err != nil || !strings.HasPrefix(pu.Path, filter) {
				mu.Unlock()
				return false
			}
		}
		visited[e.jsonURL] = true
		mu.Unlock()
		atomic.AddInt64(&pending, 1)
		work <- e
		return true
	}

	addResult := func(r doccCrawlResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	}

	// Closer: closes the work channel once `pending` settles at zero. A
	// dedicated goroutine avoids the classic "worker decrements, peer hasn't
	// checked yet" race that bites when each worker tries to close on its own.
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if atomic.LoadInt64(&pending) == 0 {
					close(work)
					return
				}
			}
		}
	}()

	// Seed the frontier with the root.
	if !enqueue(frontierEntry{rootJSON, rootPage}) {
		close(done)
		return nil, searchruntime.DocCRootRejectedError(rootPage)
	}

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range work {
				body, err := httpGet(e.jsonURL)
				if err != nil {
					addResult(doccCrawlResult{jsonURL: e.jsonURL, pageURL: e.pageURL, err: err})
					atomic.AddInt64(&pending, -1)
					continue
				}
				var node doccNode
				if err := json.Unmarshal(body, &node); err != nil {
					addResult(doccCrawlResult{jsonURL: e.jsonURL, pageURL: e.pageURL, err: searchruntime.DocCParseError(err)})
					atomic.AddInt64(&pending, -1)
					continue
				}
				bundle := bundleFromIdentifier(node.Identifier.URL)
				if bundle == "" {
					bundle = inferBundleFromPath(e.pageURL)
				}
				rel := pageURLToRelPath(e.pageURL, bundle)
				md := renderDocC(&node)
				addResult(doccCrawlResult{
					jsonURL:    e.jsonURL,
					pageURL:    e.pageURL,
					relPath:    rel,
					bundle:     bundle,
					identifier: node.Identifier.URL,
					markdown:   md,
					title:      node.Metadata.Title,
				})

				// Expand the frontier with same-bundle topicSection identifiers
				// (and optionally seeAlso/relationships). Cross-bundle refs
				// render inline but aren't followed.
				if atomic.LoadInt32(&stopped) == 0 {
					expand := func(groups []doccTopicGroup) {
						for _, g := range groups {
							for _, id := range g.Identifiers {
								ref, ok := node.References[id]
								if !ok || ref.URL == "" {
									continue
								}
								if bundleFromIdentifier(id) != bundle {
									continue
								}
								pageURL := refURL(ref)
								jsonURL, err := pageURLToJSONURL(pageURL)
								if err != nil {
									continue
								}
								enqueue(frontierEntry{jsonURL, pageURL})
							}
						}
					}
					expand(node.TopicSections)
					if opts.followSeeAlso {
						expand(node.SeeAlsoSections)
					}
					if opts.followRelationships {
						expand(node.RelationshipsSections)
					}
				}
				// Decrement AFTER expansion so the closer never sees pending=0
				// during the gap between popping an item and enqueueing its
				// children. This is the only ordering that makes the supervisor
				// correct without a separate counted-barrier.
				atomic.AddInt64(&pending, -1)
			}
		}()
	}

	wg.Wait()
	close(done)

	return results, nil
}

// inferBundleFromPath extracts the bundle from a /documentation/<bundle>/...
// URL when the JSON identifier is missing or unparseable.
func inferBundleFromPath(pageURL string) string {
	u, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "documentation" {
		return parts[1]
	}
	return ""
}

// runDocC drives a DocC crawl and merges the output into the corpus, sharing
// the same finalize pipeline (manifest write, _INDEX.md regen, FTS5 update,
// ingest log) as run() and ingest().
func runDocC(rootURL, filter, nameOverride string, maxNodes int, followSeeAlso, followRel bool, o pullOpts, cmdArgs []string) {
	start := time.Now()
	startedAt := start.UTC().Format(time.RFC3339)

	fmt.Fprintf(os.Stderr, "docc crawl: %s (filter=%q max=%d concurrency=%d)\n",
		rootURL, filter, maxNodes, o.concurrency)

	crawlRes, err := crawlDocC(doccCrawlOpts{
		rootURL:             rootURL,
		filter:              filter,
		maxNodes:            maxNodes,
		concurrency:         o.concurrency,
		followSeeAlso:       followSeeAlso,
		followRelationships: followRel,
	})
	if err != nil {
		die(err)
	}

	// Group by bundle → source name. A user can pin to one source via --name.
	// Most archives are single-bundle; cross-bundle is a misuse anyway since
	// we don't follow those edges.
	bundleToSource := map[string]string{}
	results := make([]result, 0, len(crawlRes))
	pulled, skipped, warned := 0, 0, 0
	now := startedAt

	for _, c := range crawlRes {
		if c.err != nil {
			results = append(results, result{
				URL: c.pageURL, FetchedAt: now,
				Skipped: c.err.Error(),
			})
			skipped++
			continue
		}
		source := nameOverride
		if source == "" {
			if cached, ok := bundleToSource[c.bundle]; ok {
				source = cached
			} else {
				source = sourceNameForBundle(c.identifier, c.bundle)
				bundleToSource[c.bundle] = source
			}
		}
		outPath := filepath.Join(o.out, source, c.relPath)
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			results = append(results, result{
				URL: c.pageURL, Source: source, FetchedAt: now,
				Skipped: err.Error(),
			})
			skipped++
			continue
		}
		if err := os.WriteFile(outPath, c.markdown, 0o644); err != nil {
			results = append(results, result{
				URL: c.pageURL, Source: source, FetchedAt: now,
				Skipped: err.Error(),
			})
			skipped++
			continue
		}
		sum := sha256.Sum256(c.markdown)
		r := result{
			URL: c.pageURL, Source: source,
			Path:      filepath.Join(source, c.relPath),
			Mode:      "docc",
			SHA256:    hex.EncodeToString(sum[:]),
			FetchedAt: now,
		}
		if len(c.markdown) < thinContentThreshold {
			r.Warning = fmt.Sprintf("low-content (%d bytes)", len(c.markdown))
			warned++
		}
		results = append(results, r)
		pulled++
	}

	// Stable order so manifest reads diff-friendly.
	sort.Slice(results, func(i, j int) bool { return results[i].Path < results[j].Path })

	if err := withWriteLock(o.out, func() error {
		if err := writeManifests(o.out, results, false, nil); err != nil {
			return err
		}
		var changedPaths []string
		for _, r := range results {
			if r.Skipped == "" && r.Path != "" {
				changedPaths = append(changedPaths, r.Path)
			}
		}
		sources := distinctSources(results)
		if err := regenerateIndex(o.out, sources); err != nil {
			return err
		}
		if idx, err := openFTSIndex(o.out); err == nil {
			if rerr := idx.updateFTS(o.out, changedPaths); rerr != nil {
				fmt.Fprintf(os.Stderr, "fts5: update failed: %v\n", rerr)
			}
			idx.close()
		}
		finished := time.Now().UTC()
		entry := logEntry{
			StartedAt:  startedAt,
			FinishedAt: finished.Format(time.RFC3339),
			ElapsedMs:  finished.Sub(start.UTC()).Milliseconds(),
			Mode:       "docc",
			Args:       cmdArgs,
			Sources:    sources,
			URLs:       len(results),
			Pulled:     pulled,
			Skipped:    skipped,
			Warned:     warned,
		}
		if err := appendIngestLog(o.out, entry); err != nil {
			fmt.Fprintf(os.Stderr, "ingest-log: append failed: %v\n", err)
		}
		return nil
	}); err != nil {
		die(err)
	}

	elapsed := time.Since(start)
	fmt.Printf("docc: pulled %d  skipped %d  warned %d  total %d  in %s\n",
		pulled, skipped, warned, len(results), elapsed.Round(time.Millisecond))
	for _, r := range results {
		switch {
		case r.Skipped != "":
			fmt.Printf("  SKIP %s — %s\n", r.URL, r.Skipped)
		case r.Warning != "":
			fmt.Printf("  WARN %s — %s\n", r.URL, r.Warning)
		}
	}
}
