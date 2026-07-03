package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func syntheticLeaderboardReport(n int) evalSuiteOverviewReport {
	return evalSuiteOverviewReport{
		GeneratedAt: "2026-06-08T00:00:00Z",
		DocsRoot:    "/tmp/corpus",
		Limit:       10,
		Overall:     evalOverviewMetric{Name: "overall", N: n, HitAt1: 0.64, HitAt5: 0.717, MRR: 0.672, P50MS: 1.2, P95MS: 4.0, P99MS: 9.0},
		Fixtures: []evalSuiteRow{
			{Fixture: "supabase<script>.yaml", Summary: evalSummary{N: 100, HitAt1: 0.7, HitAt5: 0.8, MRR: 0.74, P50MS: 1, P99MS: 8}},
		},
	}
}

func TestLeaderboardPublishableFailsClosed(t *testing.T) {
	if err := leaderboardPublishable(syntheticLeaderboardReport(0)); err == nil {
		t.Fatal("zero-query report must be unpublishable (fail closed)")
	}
	if !strings.Contains(mustErr(t, leaderboardPublishable(syntheticLeaderboardReport(0))), "fail closed") {
		t.Fatal("fail-closed error should say so")
	}
	if err := leaderboardPublishable(syntheticLeaderboardReport(258)); err != nil {
		t.Fatalf("non-empty report should publish: %v", err)
	}
}

func TestRenderLeaderboardHTMLEscapedAndSelfContained(t *testing.T) {
	out := renderLeaderboardHTML(syntheticLeaderboardReport(258), 4)

	// Self-contained: no external scripts/styles/images.
	for _, bad := range []string{`src="http`, `href="http`, `<script src`, `<link rel="stylesheet"`} {
		if strings.Contains(out, bad) {
			t.Fatalf("leaderboard must be self-contained; found %q", bad)
		}
	}
	// Headline numbers present.
	for _, want := range []string{"64.0%", "71.7%", "0.672", "Methodology", "Reproduce this", "not yet measured", "BM25-only"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered HTML missing %q", want)
		}
	}
	// XSS-y fixture name is escaped, not raw.
	if strings.Contains(out, "<script>") {
		t.Fatal("fixture name must be HTML-escaped")
	}
	if !strings.Contains(out, "supabase&lt;script&gt;") {
		t.Fatal("expected escaped fixture name in output")
	}
}

func TestRenderLeaderboardDoesNotLeakHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	r := syntheticLeaderboardReport(258)
	r.DocsRoot = filepath.Join(home, "code", "docs")
	out := renderLeaderboardHTML(r, 5)
	if strings.Contains(out, home) {
		t.Fatalf("public leaderboard must not leak the absolute home path %q", home)
	}
	if !strings.Contains(out, "~/code/docs") {
		t.Fatalf("expected ~-relative corpus path in output")
	}
}

func TestLeaderboardFixturePathsExcludeInternalByDefault(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"fixture.yaml", "fixture-acme-internal.yaml", "vendor-internal.yaml"} {
		writeEvalSuiteFixture(t, filepath.Join(dir, name))
	}

	paths, err := leaderboardFixturePaths(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fixturePathBases(paths), ","); got != "fixture.yaml" {
		t.Fatalf("public leaderboard fixtures = %q, want fixture.yaml", got)
	}

	paths, err = leaderboardFixturePaths(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fixturePathBases(paths), ","); got != "fixture-acme-internal.yaml,fixture.yaml,vendor-internal.yaml" {
		t.Fatalf("internal leaderboard fixtures = %q", got)
	}
}

func mustErr(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	return err.Error()
}

func TestWriteLeaderboardHTMLFileCreatesParentDir(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "docs", "human", "docs-puller-leaderboard.html")
	if err := writeLeaderboardHTMLFile(outPath, "<html>ok</html>"); err != nil {
		t.Fatalf("writeLeaderboardHTMLFile returned error: %v", err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("leaderboard file was not written: %v", err)
	}
	if string(body) != "<html>ok</html>" {
		t.Fatalf("leaderboard file body = %q", string(body))
	}
}
