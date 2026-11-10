package docspack

// Minimal HTML tokenizer (stdlib only). This is the one deliberate
// simplification vs the Python implementation, which uses html.parser:
// the tokenizer handles well-formed documents, comments, doctypes, and
// raw-text elements (script/style/textarea/title), but does not perform
// html.parser's error recovery on badly malformed markup. See PARITY.md.

import (
	"html"
	"strings"
)

type tokenKind int

const (
	tokText tokenKind = iota
	tokStart
	tokEnd
)

type token struct {
	kind       tokenKind
	tag        string            // lowercased tag name (start/end)
	attrs      map[string]string // start tags only
	text       string            // text tokens (entities already unescaped)
	start, end int               // byte offsets in the source
	selfClose  bool
}

var voidTags = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

var rawTextTags = map[string]bool{
	"script": true, "style": true, "textarea": true, "title": true,
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

func tokenize(src string) []token {
	var toks []token
	i, n := 0, len(src)
	for i < n {
		if src[i] != '<' {
			j := strings.IndexByte(src[i:], '<')
			var raw string
			if j == -1 {
				raw = src[i:]
				toks = append(toks, token{kind: tokText, text: html.UnescapeString(raw), start: i, end: n})
				break
			}
			raw = src[i : i+j]
			toks = append(toks, token{kind: tokText, text: html.UnescapeString(raw), start: i, end: i + j})
			i += j
			continue
		}
		// comment
		if strings.HasPrefix(src[i:], "<!--") {
			end := strings.Index(src[i+4:], "-->")
			if end == -1 {
				break
			}
			i += 4 + end + 3
			continue
		}
		// doctype / processing instruction
		if strings.HasPrefix(src[i:], "<!") || strings.HasPrefix(src[i:], "<?") {
			end := strings.IndexByte(src[i:], '>')
			if end == -1 {
				break
			}
			i += end + 1
			continue
		}
		// end tag
		if strings.HasPrefix(src[i:], "</") {
			end := strings.IndexByte(src[i:], '>')
			if end == -1 {
				break
			}
			name := strings.Fields(strings.TrimSpace(src[i+2 : i+end]))
			tag := ""
			if len(name) > 0 {
				tag = strings.ToLower(name[0])
			}
			toks = append(toks, token{kind: tokEnd, tag: tag, start: i, end: i + end + 1})
			i += end + 1
			continue
		}
		// start tag
		j := i + 1
		for j < n && !isSpaceByte(src[j]) && src[j] != '>' && src[j] != '/' {
			j++
		}
		tag := strings.ToLower(src[i+1 : j])
		if tag == "" {
			// stray '<' — treat as text
			toks = append(toks, token{kind: tokText, text: "<", start: i, end: i + 1})
			i++
			continue
		}
		attrs := map[string]string{}
		selfClose := false
		k := j
		for k < n {
			for k < n && isSpaceByte(src[k]) {
				k++
			}
			if k >= n {
				break
			}
			if src[k] == '>' {
				k++
				break
			}
			if src[k] == '/' {
				if k+1 < n && src[k+1] == '>' {
					selfClose = true
					k += 2
					break
				}
				k++
				continue
			}
			// attribute name
			a := k
			for k < n && !isSpaceByte(src[k]) && src[k] != '=' && src[k] != '>' && src[k] != '/' {
				k++
			}
			name := strings.ToLower(src[a:k])
			for k < n && isSpaceByte(src[k]) {
				k++
			}
			val := ""
			if k < n && src[k] == '=' {
				k++
				for k < n && isSpaceByte(src[k]) {
					k++
				}
				if k < n && (src[k] == '"' || src[k] == '\'') {
					q := src[k]
					k++
					e := strings.IndexByte(src[k:], q)
					if e == -1 {
						val = src[k:]
						k = n
					} else {
						val = src[k : k+e]
						k += e + 1
					}
				} else {
					e := k
					for e < n && !isSpaceByte(src[e]) && src[e] != '>' {
						e++
					}
					val = src[k:e]
					k = e
				}
			}
			if name != "" {
				attrs[name] = html.UnescapeString(val)
			}
		}
		toks = append(toks, token{kind: tokStart, tag: tag, attrs: attrs, start: i, end: k, selfClose: selfClose})
		i = k
		// raw-text element: swallow content up to its close tag as one text token
		if rawTextTags[tag] && !selfClose {
			closeTag := "</" + tag
			idx := strings.Index(strings.ToLower(src[i:]), closeTag)
			if idx == -1 {
				if i < n {
					toks = append(toks, token{kind: tokText, text: src[i:], start: i, end: n})
				}
				break
			}
			if idx > 0 {
				toks = append(toks, token{kind: tokText, text: src[i : i+idx], start: i, end: i + idx})
			}
			i += idx
		}
	}
	return toks
}
