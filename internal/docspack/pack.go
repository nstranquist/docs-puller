package docspack

// Cross-page pack selection — Go port of pack.py: TF-IDF rank -> Jaccard
// dedupe (>= 0.7) -> greedy global budget fill (default 8000 tokens).

import (
	"math"
	"regexp"
	"sort"
)

const (
	JaccardThreshold = 0.7
	DefaultBudget    = 8000
)

var wordRe = regexp.MustCompile(`[a-z0-9]+`)

func words(text string) []string {
	return wordRe.FindAllString(toLowerASCII(text), -1)
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// TFIDFScores returns the mean TF-IDF salience per chunk (same formula as
// the Python implementation).
func TFIDFScores(chunks []Chunk) []float64 {
	docs := make([][]string, len(chunks))
	n := len(docs)
	if n == 0 {
		return nil
	}
	df := map[string]int{}
	for i, c := range chunks {
		docs[i] = words(c.Text)
		seen := map[string]bool{}
		for _, w := range docs[i] {
			if !seen[w] {
				seen[w] = true
				df[w]++
			}
		}
	}
	scores := make([]float64, n)
	for i, doc := range docs {
		if len(doc) == 0 {
			scores[i] = 0
			continue
		}
		tf := map[string]int{}
		for _, w := range doc {
			tf[w]++
		}
		total := 0.0
		for w, cnt := range tf {
			total += (float64(cnt) / float64(len(doc))) *
				(math.Log(float64(n+1)/float64(df[w]+1)) + 1.0)
		}
		denom := len(tf)
		if denom < 1 {
			denom = 1
		}
		scores[i] = total / float64(denom)
	}
	return scores
}

// Jaccard similarity of two word sets.
func Jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	return float64(inter) / float64(union)
}

// Stats mirrors the Python stats dict (same JSON keys).
type Stats struct {
	ChunksIn          int      `json:"chunks_in"`
	ChunksSelected    int      `json:"chunks_selected"`
	ChunksDroppedDupl int      `json:"chunks_dropped_duplicate"`
	DedupeRate        float64  `json:"dedupe_rate"`
	TokensIn          int      `json:"tokens_in"`
	TokensSelected    int      `json:"tokens_selected"`
	Budget            int      `json:"budget"`
	TokenReduction    float64  `json:"token_reduction"`
	Sources           []string `json:"sources,omitempty"`
}

func round4(x float64) float64 { return math.Round(x*10000) / 10000 }

// SelectPack ranks, dedupes, and budget-fills chunks (mirrors Python
// select_pack; stable ranking so score ties keep input order).
func SelectPack(chunks []Chunk, budget int) ([]Chunk, Stats) {
	scores := TFIDFScores(chunks)
	ranked := make([]int, len(chunks))
	for i := range ranked {
		ranked[i] = i
	}
	sort.SliceStable(ranked, func(a, b int) bool {
		return scores[ranked[a]] > scores[ranked[b]]
	})
	var selected []Chunk
	var selectedSets []map[string]bool
	used, dropped := 0, 0
	for _, i := range ranked {
		c := chunks[i]
		wset := map[string]bool{}
		for _, w := range words(c.Text) {
			wset[w] = true
		}
		dup := false
		for _, s := range selectedSets {
			if Jaccard(wset, s) >= JaccardThreshold {
				dup = true
				break
			}
		}
		if dup {
			dropped++
			continue
		}
		if used+c.Tokens > budget {
			continue
		}
		selected = append(selected, c)
		selectedSets = append(selectedSets, wset)
		used += c.Tokens
	}
	totalIn := 0
	for _, c := range chunks {
		totalIn += c.Tokens
	}
	st := Stats{
		ChunksIn:          len(chunks),
		ChunksSelected:    len(selected),
		ChunksDroppedDupl: dropped,
		Budget:            budget,
		TokensIn:          totalIn,
		TokensSelected:    used,
	}
	if len(chunks) > 0 {
		st.DedupeRate = round4(float64(dropped) / float64(len(chunks)))
	}
	if totalIn > 0 {
		st.TokenReduction = round4(1 - float64(used)/float64(totalIn))
	}
	return selected, st
}

// SourceDoc is one (sourceID, html) pair.
type SourceDoc struct {
	ID   string
	HTML string
}

// BuildPack chunks all sources and selects a pack (mirrors Python
// build_pack). Returns (selected, stats, allChunks).
func BuildPack(sources []SourceDoc, budget int) ([]Chunk, Stats, []Chunk) {
	var all []Chunk
	ids := make([]string, 0, len(sources))
	for _, s := range sources {
		all = append(all, ChunkHTML(s.HTML, s.ID)...)
		ids = append(ids, s.ID)
	}
	selected, st := SelectPack(all, budget)
	st.Sources = ids
	return selected, st, all
}
