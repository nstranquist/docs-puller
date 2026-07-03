package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildEvalSuiteOverviewGroupsLibraryAndQueryType(t *testing.T) {
	rows := []evalSuiteRow{
		{
			Fixture: "fixture.yaml",
			Summary: evalSummary{N: 2},
			Results: []evalCaseResult{
				{
					Query:                       "azure cli login",
					QueryType:                   "library_search",
					ExpectedSource:              "microsoft-learn",
					FirstHitRank:                1,
					HitAt1:                      true,
					HitAt3:                      true,
					HitAt5:                      true,
					HitAt10:                     true,
					RecallAt5:                   1,
					LatencyMS:                   20,
					TokensReturned:              120,
					TokenBudgetHit:              true,
					AnswerContextTokensReturned: 800,
					AnswerContextBudgetHit:      true,
				},
				{
					Query:                       "react native skia path",
					QueryType:                   "tech_stack_search",
					Expect:                      []string{"react-native-skia/docs/path.md"},
					FirstHitRank:                2,
					HitAt3:                      true,
					HitAt5:                      true,
					HitAt10:                     true,
					RecallAt5:                   1,
					LatencyMS:                   50,
					TokensReturned:              240,
					AnswerContextTokensReturned: 1600,
				},
			},
		},
		{
			Fixture: "bad.yaml",
			Error:   "parse failed",
		},
	}
	report := buildEvalSuiteOverview(rows, "/docs", 10, 4000, true, time.Date(2026, 5, 12, 17, 0, 0, 0, time.UTC))
	if report.GeneratedAt != "2026-05-12T17:00:00Z" || report.Overall.N != 2 {
		t.Fatalf("overview basics = %+v", report)
	}
	if got := findOverviewMetric(report.ByLibrary, "microsoft-learn"); got.N != 1 || got.HitAt1 != 1 || got.P95AnswerContextTokens != 800 {
		t.Fatalf("microsoft-learn metric = %+v", got)
	}
	if got := findOverviewMetric(report.ByLibrary, "react-native-skia"); got.N != 1 || got.HitAt5 != 1 || got.P95AnswerContextTokens != 1600 {
		t.Fatalf("react-native-skia metric = %+v", got)
	}
	if got := findOverviewMetric(report.ByQueryType, "library_search"); got.N != 1 || got.AnswerContextBudgetHitRate != 1 {
		t.Fatalf("library query type metric = %+v", got)
	}
	if got := findOverviewMetric(report.ByQueryType, "tech_stack_search"); got.N != 1 || got.AnswerContextBudgetHitRate != 0 {
		t.Fatalf("tech-stack query type metric = %+v", got)
	}
	if report.SlowQueries[0].Query != "react native skia path" || report.TokenHeavyQueries[0].Query != "react native skia path" {
		t.Fatalf("top queries = slow:%+v heavy:%+v", report.SlowQueries, report.TokenHeavyQueries)
	}
}

func TestRenderEvalSuiteOverviewMarkdownAndHTML(t *testing.T) {
	report := buildEvalSuiteOverview([]evalSuiteRow{{
		Fixture: "fixture.yaml",
		Summary: evalSummary{N: 1, HitAt1: 1, HitAt5: 1, MRR: 1, P50MS: 12, P99MS: 12, P50AnswerContextTokens: 400, P99AnswerContextTokens: 400},
		Results: []evalCaseResult{{
			Query:                       "supabase | rls",
			QueryType:                   "library_search",
			ExpectedSource:              "supabase",
			FirstHitRank:                1,
			HitAt1:                      true,
			HitAt3:                      true,
			HitAt5:                      true,
			HitAt10:                     true,
			RecallAt5:                   1,
			LatencyMS:                   12,
			TokensReturned:              90,
			AnswerContextTokensReturned: 400,
		}},
	}}, "/docs", 10, 4000, true, time.Date(2026, 5, 12, 17, 0, 0, 0, time.UTC))
	md := renderEvalSuiteOverviewMarkdown(report)
	for _, want := range []string{"# Docs-puller Retrieval Metrics Overview", "By Docs Library", "Library search", "supabase \\| rls"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
	html := renderEvalSuiteOverviewHTML(report)
	for _, want := range []string{"docs-puller retrieval metrics", "Per-library", "supabase | rls"} {
		if !strings.Contains(html, want) {
			t.Fatalf("html missing %q:\n%s", want, html)
		}
	}
	out := filepath.Join(t.TempDir(), "nested", "overview.md")
	if err := writeEvalSuiteOverviewMarkdown(out, report); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("overview markdown not written: %v", err)
	}
}

func findOverviewMetric(metrics []evalOverviewMetric, name string) evalOverviewMetric {
	for _, metric := range metrics {
		if metric.Name == name {
			return metric
		}
	}
	return evalOverviewMetric{}
}
