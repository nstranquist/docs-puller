package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffEvalSuiteRowsMatchesByFixtureBasename(t *testing.T) {
	baseline := []evalSuiteRow{
		{Fixture: "/old/fixture.yaml", Summary: evalSummary{
			N: 2, HitAt1: 0.5, HitAt5: 1, MRR: 0.75, P50MS: 100, P99MS: 200,
			BySource: map[string]float64{"supabase": 1, "stripe": 1},
		}},
	}
	current := []evalSuiteRow{
		{Fixture: "/new/fixture.yaml", Summary: evalSummary{
			N: 2, HitAt1: 1, HitAt5: 0.5, MRR: 0.9, P50MS: 80, P99MS: 250,
			BySource: map[string]float64{"supabase": 0.5, "stripe": 1},
		}},
	}

	diff := diffEvalSuiteRows(baseline, current)
	if len(diff.Fixtures) != 1 {
		t.Fatalf("fixtures = %+v", diff.Fixtures)
	}
	got := diff.Fixtures[0]
	if got.Fixture != "fixture.yaml" || got.BaselineN != 2 || got.CurrentN != 2 {
		t.Fatalf("unexpected fixture diff: %+v", got)
	}
	if got.HitAt1Delta != 50 || got.HitAt5Delta != -50 || math.Abs(got.MRRDelta-0.15) > 0.000001 || got.P50MSDelta != -20 || got.P99MSDelta != 50 {
		t.Fatalf("unexpected metric deltas: %+v", got)
	}
	if len(got.SourceDrift) != 1 || got.SourceDrift[0].Source != "supabase" || got.SourceDrift[0].DeltaPP != -50 {
		t.Fatalf("unexpected source drift: %+v", got.SourceDrift)
	}
}

func TestLoadAndDiffEvalSuiteAcceptsRowsArrayAndReportObject(t *testing.T) {
	dir := t.TempDir()
	rowsPath := filepath.Join(dir, "rows.json")
	reportPath := filepath.Join(dir, "report.json")
	baseline := []evalSuiteRow{{Fixture: "/old/a.yaml", Summary: evalSummary{N: 1, HitAt1: 1, HitAt5: 1, MRR: 1}}}
	writeTestJSON(t, rowsPath, baseline)
	writeTestJSON(t, reportPath, evalSuiteReport{Rows: baseline})
	current := []evalSuiteRow{{Fixture: "/new/a.yaml", Summary: evalSummary{N: 1, HitAt1: 0, HitAt5: 1, MRR: 0}}}

	for _, path := range []string{rowsPath, reportPath} {
		diff, err := loadAndDiffEvalSuite(path, current)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(diff.Fixtures) != 1 || diff.Fixtures[0].HitAt1Delta != -100 {
			t.Fatalf("%s: unexpected diff %+v", path, diff)
		}
	}
}

func TestDiffEvalSuiteRowsReportsMissingFixtures(t *testing.T) {
	diff := diffEvalSuiteRows(
		[]evalSuiteRow{{Fixture: "/old/only-baseline.yaml"}, {Fixture: "/old/shared.yaml"}},
		[]evalSuiteRow{{Fixture: "/new/only-current.yaml"}, {Fixture: "/new/shared.yaml"}},
	)
	if len(diff.MissingCurrent) != 1 || diff.MissingCurrent[0] != "only-baseline.yaml" {
		t.Fatalf("missing current = %+v", diff.MissingCurrent)
	}
	if len(diff.MissingBaseline) != 1 || diff.MissingBaseline[0] != "only-current.yaml" {
		t.Fatalf("missing baseline = %+v", diff.MissingBaseline)
	}
}

func TestEvalSuiteFixturePathsForSelection(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"fixture.yaml", "natural-language.yaml", "nl-questions.yaml", "production-telemetry.yaml"} {
		writeEvalSuiteFixture(t, filepath.Join(dir, name))
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.yaml"), []byte("notes: hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	include, err := parseEvalSuiteFixtureSelection("fixture.yaml,"+filepath.Join(dir, "nl-questions.yaml"), "")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := evalSuiteFixturePathsForSelection(dir, include)
	if err != nil {
		t.Fatal(err)
	}
	if got := fixturePathBases(paths); strings.Join(got, ",") != "fixture.yaml,nl-questions.yaml" {
		t.Fatalf("included fixture paths = %v", got)
	}

	exclude, err := parseEvalSuiteFixtureSelection("", "production-telemetry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	paths, err = evalSuiteFixturePathsForSelection(dir, exclude)
	if err != nil {
		t.Fatal(err)
	}
	if got := fixturePathBases(paths); strings.Join(got, ",") != "fixture.yaml,natural-language.yaml,nl-questions.yaml" {
		t.Fatalf("excluded fixture paths = %v", got)
	}
}

func TestEvalSuiteFixtureSelectionErrors(t *testing.T) {
	dir := t.TempDir()
	writeEvalSuiteFixture(t, filepath.Join(dir, "fixture.yaml"))

	conflict, err := parseEvalSuiteFixtureSelection("fixture.yaml", "fixture.yaml")
	if err == nil {
		t.Fatalf("expected include/exclude conflict, got selection %+v", conflict)
	}
	if want := "fixture fixture.yaml appears in both --include-fixture and --exclude-fixture"; err.Error() != want {
		t.Fatalf("conflict error = %q, want %q", err.Error(), want)
	}

	unknown, err := parseEvalSuiteFixtureSelection("missing.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = evalSuiteFixturePathsForSelection(dir, unknown)
	if err == nil {
		t.Fatal("expected unknown include fixture error")
	}
	if want := "--include-fixture missing.yaml did not match a fixture in " + dir; err.Error() != want {
		t.Fatalf("unknown include error = %q, want %q", err.Error(), want)
	}
}

func TestDefaultFixturesDirFromRoot(t *testing.T) {
	tests := []struct {
		name   string
		root   string
		exists map[string]bool
		want   string
	}{
		{
			name: "monorepo layout wins",
			root: "/repo",
			exists: map[string]bool{
				filepath.Join("/repo", "cmd", "docs-puller", "eval"): true,
				filepath.Join("/repo", "eval"):                       true,
			},
			want: filepath.Join("/repo", "cmd", "docs-puller", "eval"),
		},
		{
			name: "standalone repo layout",
			root: "/repo",
			exists: map[string]bool{
				filepath.Join("/repo", "eval"): true,
			},
			want: filepath.Join("/repo", "eval"),
		},
		{
			name: "working directory standalone fallback",
			root: ".",
			exists: map[string]bool{
				filepath.Join("eval"): true,
			},
			want: filepath.Join("eval"),
		},
		{
			name:   "missing returns monorepo default",
			root:   "/repo",
			exists: map[string]bool{},
			want:   filepath.Join("/repo", "cmd", "docs-puller", "eval"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultFixturesDirFromRoot(tt.root, func(path string) bool {
				return tt.exists[path]
			})
			if got != tt.want {
				t.Fatalf("defaultFixturesDirFromRoot() = %q, want %q", got, tt.want)
			}
		})
	}
}

func writeTestJSON(t *testing.T, path string, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeEvalSuiteFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("queries:\n  - q: hello\n    expect: [a/b.md]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixturePathBases(paths []string) []string {
	out := make([]string, len(paths))
	for i, path := range paths {
		out[i] = filepath.Base(path)
	}
	return out
}
