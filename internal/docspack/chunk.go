package docspack

// Heading-aware chunker — Go port of chunk.py. Splits at h2/h3 boundaries
// first (a chunk never spans two h2 sections), then greedily packs
// segments within a section to <= MaxTokens. Token estimator: (chars+3)/4
// in bytes here vs codepoints in Python — identical on ASCII.

import "strings"

const MaxTokens = 800

// EstimateTokens is the ragzip-compatible chars/4 estimator.
func EstimateTokens(text string) int {
	return (len(text) + 3) / 4
}

// Chunk is one cited chunk of a source document.
type Chunk struct {
	Citation    string   `json:"citation"` // url#chunkN
	Source      string   `json:"source"`
	HeadingPath []string `json:"heading_path"`
	Text        string   `json:"text"`
	Tokens      int      `json:"tokens"`
}

func newChunk(citation, source string, path []string, text string) Chunk {
	p := make([]string, len(path))
	copy(p, path)
	return Chunk{Citation: citation, Source: source, HeadingPath: p,
		Text: text, Tokens: EstimateTokens(text)}
}

type section struct {
	path []string
	segs []Segment
}

func splitSections(segments []Segment) []section {
	var sections []section
	var path []string
	var current []Segment
	flush := func() {
		if len(current) > 0 {
			p := make([]string, len(path))
			copy(p, path)
			c := make([]Segment, len(current))
			copy(c, current)
			sections = append(sections, section{p, c})
			current = nil
		}
	}
	for _, s := range segments {
		if s.Tag == "h1" || s.Tag == "h2" || s.Tag == "h3" {
			depth := int(s.Tag[1]-'0') - 1 // h1->0, h2->1, h3->2
			if s.Tag == "h2" || s.Tag == "h3" {
				flush()
			}
			if len(path) > depth {
				path = path[:depth]
			}
			path = append(path, s.Text)
			current = append(current, s)
		} else {
			current = append(current, s)
		}
	}
	flush()
	return sections
}

// ChunkSegments chunks cleaned segments for one source (mirrors Python
// chunk_segments; oversized segments get their own chunk, untruncated).
func ChunkSegments(segments []Segment, source string) []Chunk {
	var chunks []Chunk
	n := 0
	for _, sec := range splitSections(segments) {
		var buf []string
		bufTokens := 0
		emit := func() {
			if len(buf) > 0 {
				n++
				chunks = append(chunks, newChunk(
					source+"#chunk"+itoa(n), source, sec.path,
					strings.Join(buf, "\n\n")))
				buf, bufTokens = nil, 0
			}
		}
		for _, s := range sec.segs {
			t := EstimateTokens(s.Text)
			if t > MaxTokens {
				emit()
				n++
				chunks = append(chunks, newChunk(
					source+"#chunk"+itoa(n), source, sec.path, s.Text))
				continue
			}
			if bufTokens+t > MaxTokens {
				emit()
			}
			buf = append(buf, s.Text)
			bufTokens += t
		}
		emit()
	}
	return chunks
}

// ChunkHTML cleans then chunks one HTML document.
func ChunkHTML(html, source string) []Chunk {
	return ChunkSegments(CleanHTML(html), source)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
