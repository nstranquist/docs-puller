package main

import (
	"strings"
	"testing"
)

func TestRankDelta(t *testing.T) {
	cases := []struct {
		base, cur, want int
	}{
		{1, 1, 0},
		{1, 3, +2},
		{3, 1, -2},
		{0, 5, -95}, // miss → top-5 = improvement
		{5, 0, +95}, // top-5 → miss = regression
		{0, 0, 0},   // both miss = no change
	}
	for _, c := range cases {
		got := rankDelta(c.base, c.cur)
		if got != c.want {
			t.Errorf("rankDelta(%d,%d) = %d, want %d", c.base, c.cur, got, c.want)
		}
	}
}

func TestNewlyDisplacing(t *testing.T) {
	cases := []struct {
		name           string
		baseTop        []string
		curTop         []string
		expect         []string
		wantDisplacers []string
	}{
		{
			name:           "BM25 had canonical at 1; hybrid demoted to 3 with two new docs ahead",
			baseTop:        []string{"src/canonical.md", "src/a.md", "src/b.md"},
			curTop:         []string{"src/new1.md", "src/new2.md", "src/canonical.md"},
			expect:         []string{"src/canonical.md"},
			wantDisplacers: []string{"src/new1.md", "src/new2.md"},
		},
		{
			name:           "no regression — canonical still at top",
			baseTop:        []string{"src/canonical.md", "src/a.md"},
			curTop:         []string{"src/canonical.md", "src/new.md"},
			expect:         []string{"src/canonical.md"},
			wantDisplacers: nil,
		},
		{
			name:           "displacing docs ranked above canonical only",
			baseTop:        []string{"src/canonical.md", "src/a.md", "src/b.md", "src/c.md", "src/d.md"},
			curTop:         []string{"src/new1.md", "src/canonical.md", "src/new2.md"},
			expect:         []string{"src/canonical.md"},
			wantDisplacers: []string{"src/new1.md"},
		},
	}
	for _, c := range cases {
		got := newlyDisplacing(c.baseTop, c.curTop, c.expect)
		if !equalStrings(got, c.wantDisplacers) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.wantDisplacers)
		}
	}
}

func TestSuspectKind(t *testing.T) {
	cases := []struct {
		name    string
		baseTop []string
		curTop  []string
		expect  []string
		want    string
	}{
		{
			name:    "cross-source bleed — displacer from different source",
			baseTop: []string{"clickhouse/canonical.md", "clickhouse/a.md"},
			curTop:  []string{"posthog/blog.md", "clickhouse/canonical.md"},
			expect:  []string{"clickhouse/canonical.md"},
			want:    "cross-source-bleed",
		},
		{
			name:    "vendor-generic-page-promoted — same source, shallower path",
			baseTop: []string{"clickhouse/engines/family/specific.md"},
			curTop:  []string{"clickhouse/overview.md", "clickhouse/engines/family/specific.md"},
			expect:  []string{"clickhouse/engines/family/specific.md"},
			want:    "vendor-generic-page-promoted",
		},
		{
			name:    "same-source thematic adjacency — same depth peer",
			baseTop: []string{"src/a/x.md"},
			curTop:  []string{"src/a/y.md", "src/a/x.md"},
			expect:  []string{"src/a/x.md"},
			want:    "same-source-thematic-adjacency",
		},
		{
			name:    "canonical fully missed — not in current results",
			baseTop: []string{"src/canonical.md", "src/a.md"},
			curTop:  []string{"src/x.md", "src/y.md", "src/z.md"},
			expect:  []string{"src/canonical.md"},
			want:    "canonical-fully-missed",
		},
		{
			name:    "no displacers — empty kind",
			baseTop: []string{"src/canonical.md", "src/a.md"},
			curTop:  []string{"src/canonical.md", "src/a.md"},
			expect:  []string{"src/canonical.md"},
			want:    "",
		},
	}
	for _, c := range cases {
		got := suspectKind(c.baseTop, c.curTop, c.expect, "")
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestComputeRegressions(t *testing.T) {
	base := []evalCaseResult{
		{Query: "q1", Expect: []string{"src/a.md"}, FirstHitRank: 1, GotPaths: []string{"src/a.md", "src/b.md"}},
		{Query: "q2", Expect: []string{"src/c.md"}, FirstHitRank: 3, GotPaths: []string{"src/x.md", "src/y.md", "src/c.md"}},
		{Query: "q3", Expect: []string{"src/d.md"}, FirstHitRank: 0, GotPaths: []string{"src/m.md", "src/n.md"}},
	}
	cur := []evalCaseResult{
		{Query: "q1", Expect: []string{"src/a.md"}, FirstHitRank: 5, GotPaths: []string{"src/x.md", "src/y.md", "src/z.md", "src/w.md", "src/a.md"}}, // regressed 1→5
		{Query: "q2", Expect: []string{"src/c.md"}, FirstHitRank: 1, GotPaths: []string{"src/c.md", "src/x.md"}},                                     // improved 3→1
		{Query: "q3", Expect: []string{"src/d.md"}, FirstHitRank: 0, GotPaths: []string{"src/m.md", "src/n.md"}},                                     // unchanged miss
	}
	regs := computeRegressions(base, cur, evalDiagnoseOpts{minDelta: 1})
	if len(regs) != 1 {
		t.Fatalf("got %d regressions, want 1", len(regs))
	}
	if regs[0].Query != "q1" {
		t.Errorf("regression query = %q, want q1", regs[0].Query)
	}
	if regs[0].BaselineRank != 1 || regs[0].CurrentRank != 5 {
		t.Errorf("ranks = %d→%d, want 1→5", regs[0].BaselineRank, regs[0].CurrentRank)
	}
	imp, reg := countDirections(base, cur, evalDiagnoseOpts{})
	if imp != 1 || reg != 1 {
		t.Errorf("improved/regressed = %d/%d, want 1/1", imp, reg)
	}
}

func TestSourceFilter(t *testing.T) {
	base := []evalCaseResult{
		{Query: "in-source", Expect: []string{"clickhouse/x.md"}, FirstHitRank: 1, GotPaths: []string{"clickhouse/x.md"}},
		{Query: "not-in-source", Expect: []string{"supabase/y.md"}, FirstHitRank: 1, GotPaths: []string{"supabase/y.md"}},
	}
	cur := []evalCaseResult{
		{Query: "in-source", Expect: []string{"clickhouse/x.md"}, FirstHitRank: 5, GotPaths: []string{"a", "b", "c", "d", "clickhouse/x.md"}},
		{Query: "not-in-source", Expect: []string{"supabase/y.md"}, FirstHitRank: 5, GotPaths: []string{"a", "b", "c", "d", "supabase/y.md"}},
	}
	regs := computeRegressions(base, cur, evalDiagnoseOpts{minDelta: 1, source: "clickhouse"})
	if len(regs) != 1 {
		t.Fatalf("got %d regressions with source filter, want 1", len(regs))
	}
	if !strings.HasPrefix(regs[0].Expect[0], "clickhouse/") {
		t.Errorf("filtered to wrong source: %v", regs[0].Expect)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
