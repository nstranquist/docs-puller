// Package evidence reads bounded, content-addressed passages from a local corpus.
package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const MaxDocumentBytes = 1 << 20

type Passage struct {
	ID          string   `json:"id"`
	DocumentID  string   `json:"document_id"`
	ContentHash string   `json:"content_hash"`
	Path        string   `json:"path"`
	Source      string   `json:"source"`
	URL         string   `json:"url,omitempty"`
	StartLine   int      `json:"start_line"`
	EndLine     int      `json:"end_line"`
	Headings    []string `json:"headings,omitempty"`
	Text        string   `json:"text"`
}

type Request struct {
	Path  string
	URL   string
	Line  int
	Lines int
}

type Pack struct {
	SchemaVersion   int       `json:"schema_version"`
	Passages        []Passage `json:"passages"`
	ContentBytes    int       `json:"content_bytes"`
	MaxContentBytes int       `json:"max_content_bytes"`
	Skipped         []string  `json:"skipped,omitempty"`
}

// Build bounds passage text bytes, excluding JSON metadata. No tokenizer is
// assumed. Reads are confined by os.Root, including symlink traversal.
func Build(ctx context.Context, corpus string, requests []Request, maxBytes int) (Pack, error) {
	p := Pack{SchemaVersion: 1, Passages: []Passage{}, MaxContentBytes: maxBytes}
	if maxBytes <= 0 {
		return p, fmt.Errorf("max content bytes must be positive")
	}
	root, err := os.OpenRoot(corpus)
	if err != nil {
		return p, err
	}
	defer root.Close()
	seen := map[string]bool{}
	for _, req := range requests {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		passage, err := read(root, req)
		if err != nil {
			p.Skipped = append(p.Skipped, req.Path+": "+err.Error())
			continue
		}
		if seen[passage.ID] {
			continue
		}
		seen[passage.ID] = true
		if len(passage.Text) > maxBytes-p.ContentBytes {
			p.Skipped = append(p.Skipped, req.Path+": passage exceeds remaining content budget")
			continue
		}
		p.Passages = append(p.Passages, passage)
		p.ContentBytes += len(passage.Text)
	}
	return p, nil
}

func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func read(root *os.Root, req Request) (Passage, error) {
	var p Passage
	path := filepath.ToSlash(filepath.Clean(req.Path))
	if !filepath.IsLocal(req.Path) || !strings.HasSuffix(path, ".md") || strings.HasPrefix(path, ".") {
		return p, fmt.Errorf("expected a corpus-relative Markdown path")
	}
	f, err := root.Open(path)
	if err != nil {
		return p, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return p, err
	}
	if !info.Mode().IsRegular() {
		return p, fmt.Errorf("not a regular document")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxDocumentBytes+1))
	if err != nil {
		return p, err
	}
	if len(b) > MaxDocumentBytes {
		return p, fmt.Errorf("document exceeds %d bytes", MaxDocumentBytes)
	}
	if !utf8.Valid(b) {
		return p, fmt.Errorf("document is not UTF-8")
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	start := 0
	if req.Line > 0 {
		start = req.Line - 1
	}
	if start >= len(lines) {
		return p, fmt.Errorf("line outside document")
	}
	n := req.Lines
	if n <= 0 {
		n = 60
	}
	end := start + min(n, len(lines)-start)
	// Expand boundaries around fenced blocks. A too-large complete block is
	// skipped by Build instead of returning malformed or misleading code.
	fenceStart := -1
	marker := ""
	width := 0
	headings := []string{}
	headingLevels := []int{}
	for i, line := range lines {
		t := strings.TrimSpace(line)
		ch := ""
		w := 0
		if len(t) > 0 && (t[0] == '`' || t[0] == '~') {
			for w < len(t) && t[w] == t[0] {
				w++
			}
			if w >= 3 {
				ch = t[:1]
			}
		}
		if marker == "" {
			if ch != "" {
				marker = ch
				width = w
				fenceStart = i
			} else if i <= start && strings.HasPrefix(t, "#") {
				level := 0
				for level < len(t) && t[level] == '#' {
					level++
				}
				if level <= 6 && level < len(t) && t[level] == ' ' {
					for len(headingLevels) > 0 && headingLevels[len(headingLevels)-1] >= level {
						headings = headings[:len(headings)-1]
						headingLevels = headingLevels[:len(headingLevels)-1]
					}
					headings = append(headings, strings.TrimSpace(t[level:]))
					headingLevels = append(headingLevels, level)
				}
			}
		} else if ch == marker && w >= width && strings.TrimSpace(t[w:]) == "" {
			if fenceStart < start && i >= start {
				start = fenceStart
			}
			if fenceStart < end && i >= end {
				end = i + 1
			}
			marker = ""
			fenceStart = -1
		}
	}
	if marker != "" && fenceStart < end {
		if fenceStart < start {
			start = fenceStart
		}
		end = len(lines)
	}
	p = Passage{Path: path, Source: strings.Split(path, "/")[0], URL: req.URL, StartLine: start + 1, EndLine: end, Headings: headings, Text: strings.Join(lines[start:end], "\n"), ContentHash: hash(string(b)), DocumentID: hash(path)}
	p.ID = hash(fmt.Sprintf("%s:%s:%d:%d", p.DocumentID, p.ContentHash, p.StartLine, p.EndLine))
	return p, nil
}
