package searchdispatch

import (
	"errors"
	"testing"
	"time"
)

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

func TestRequestAndResultCarryDispatchState(t *testing.T) {
	req := Request[string, int]{Query: "needle", Opts: "opts", SharedIdx: 7}
	if req.Query != "needle" || req.Opts != "opts" || req.SharedIdx != 7 {
		t.Fatalf("unexpected request: %+v", req)
	}

	res := Result[string]{Hits: []string{"hit"}, Scanned: 3, Mode: "fts5"}
	if len(res.Hits) != 1 || res.Hits[0] != "hit" || res.Scanned != 3 || res.Mode != "fts5" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestPlanCandidateLimits(t *testing.T) {
	tests := []struct {
		name string
		plan CandidateLimitPlan
		want CandidateLimits
	}{
		{
			name: "rerank uses rerankK when larger than user limit",
			plan: CandidateLimitPlan{UserLimit: 10, CurrentLimit: 10, Rerank: true, RerankK: 50, HygieneLimit: 60},
			want: CandidateLimits{UserLimit: 10, BM25Limit: 50},
		},
		{
			name: "rerank keeps user limit when rerankK is smaller",
			plan: CandidateLimitPlan{UserLimit: 10, CurrentLimit: 10, RerankLLM: true, RerankK: 5, HygieneLimit: 60},
			want: CandidateLimits{UserLimit: 10, BM25Limit: 10},
		},
		{
			name: "non-rerank applies hygiene overfetch",
			plan: CandidateLimitPlan{UserLimit: 10, CurrentLimit: 10, HygieneLimit: 50},
			want: CandidateLimits{UserLimit: 10, BM25Limit: 50},
		},
		{
			name: "non-rerank preserves already wider limit",
			plan: CandidateLimitPlan{UserLimit: 10, CurrentLimit: 75, HygieneLimit: 50},
			want: CandidateLimits{UserLimit: 10, BM25Limit: 75},
		},
		{
			name: "wide version policy expands after rerank planning",
			plan: CandidateLimitPlan{UserLimit: 5, CurrentLimit: 5, RerankLLM: true, RerankK: 20, HygieneLimit: 25, NeedsWideCandidatePool: true},
			want: CandidateLimits{UserLimit: 5, BM25Limit: 200},
		},
		{
			name: "wide version policy preserves wider candidate pool",
			plan: CandidateLimitPlan{UserLimit: 5, CurrentLimit: 250, HygieneLimit: 25, NeedsWideCandidatePool: true},
			want: CandidateLimits{UserLimit: 5, BM25Limit: 250},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanCandidateLimits(tt.plan); got != tt.want {
				t.Fatalf("PlanCandidateLimits() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanFirstStageSource(t *testing.T) {
	tests := []struct {
		name string
		plan FirstStageSourcePlan
		want FirstStageSourceDecision
	}{
		{
			name: "keeps current source without policy source",
			plan: FirstStageSourcePlan{CurrentSource: "supabase"},
			want: FirstStageSourceDecision{Source: "supabase"},
		},
		{
			name: "uses policy source",
			plan: FirstStageSourcePlan{CurrentSource: "react-native", PolicySourceID: "react-native-v079"},
			want: FirstStageSourceDecision{Source: "react-native-v079"},
		},
		{
			name: "empty source stays global",
			plan: FirstStageSourcePlan{},
			want: FirstStageSourceDecision{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanFirstStageSource(tt.plan); got != tt.want {
				t.Fatalf("PlanFirstStageSource() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanFTSIndex(t *testing.T) {
	tests := []struct {
		name string
		plan FTSIndexPlan
		want FTSIndexDecision
	}{
		{
			name: "explicit scan skips fts",
			plan: FTSIndexPlan{UseScan: true},
			want: FTSIndexDecision{},
		},
		{
			name: "shared index tries fts without local open",
			plan: FTSIndexPlan{SharedIndexProvided: true},
			want: FTSIndexDecision{TryFTS: true},
		},
		{
			name: "cli path opens local index",
			plan: FTSIndexPlan{},
			want: FTSIndexDecision{TryFTS: true, OpenLocal: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanFTSIndex(tt.plan); got != tt.want {
				t.Fatalf("PlanFTSIndex() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanFTSOpen(t *testing.T) {
	tests := []struct {
		name string
		plan FTSOpenPlan
		want FTSOpenAttempts
	}{
		{
			name: "normal search tries once",
			plan: FTSOpenPlan{},
			want: FTSOpenAttempts{Attempts: 1, RetryDelay: 100 * time.Millisecond},
		},
		{
			name: "ftsOnly retries rebuild window",
			plan: FTSOpenPlan{FTSOnly: true},
			want: FTSOpenAttempts{Attempts: 5, RetryDelay: 100 * time.Millisecond},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanFTSOpen(tt.plan); got != tt.want {
				t.Fatalf("PlanFTSOpen() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanFTSOpenAttempt(t *testing.T) {
	tests := []struct {
		name string
		plan FTSOpenAttemptPlan
		want FTSOpenAttemptDecision
	}{
		{
			name: "opened index is accepted without sleeping",
			plan: FTSOpenAttemptPlan{Attempt: 0, Attempts: 5, Opened: true},
			want: FTSOpenAttemptDecision{UseOpened: true},
		},
		{
			name: "failed middle attempt sleeps before retry",
			plan: FTSOpenAttemptPlan{Attempt: 2, Attempts: 5},
			want: FTSOpenAttemptDecision{SleepBeforeRetry: true},
		},
		{
			name: "failed final attempt does not sleep",
			plan: FTSOpenAttemptPlan{Attempt: 4, Attempts: 5},
			want: FTSOpenAttemptDecision{},
		},
		{
			name: "opened final attempt is accepted",
			plan: FTSOpenAttemptPlan{Attempt: 4, Attempts: 5, Opened: true},
			want: FTSOpenAttemptDecision{UseOpened: true},
		},
		{
			name: "single failed attempt does not sleep",
			plan: FTSOpenAttemptPlan{Attempts: 1},
			want: FTSOpenAttemptDecision{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanFTSOpenAttempt(tt.plan); got != tt.want {
				t.Fatalf("PlanFTSOpenAttempt() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanFTSIndexSelection(t *testing.T) {
	tests := []struct {
		name string
		plan FTSIndexSelectionPlan
		want FTSIndexSelectionDecision
	}{
		{
			name: "no available index",
			plan: FTSIndexSelectionPlan{},
			want: FTSIndexSelectionDecision{},
		},
		{
			name: "shared index is used without ownership",
			plan: FTSIndexSelectionPlan{SharedIndexProvided: true},
			want: FTSIndexSelectionDecision{UseIndex: true},
		},
		{
			name: "local index is used and owned",
			plan: FTSIndexSelectionPlan{LocalIndexOpened: true},
			want: FTSIndexSelectionDecision{UseIndex: true, UseLocalIndex: true, OwnsIndex: true},
		},
		{
			name: "shared index wins when both are present",
			plan: FTSIndexSelectionPlan{SharedIndexProvided: true, LocalIndexOpened: true},
			want: FTSIndexSelectionDecision{UseIndex: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanFTSIndexSelection(tt.plan); got != tt.want {
				t.Fatalf("PlanFTSIndexSelection() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanFTSScannedCount(t *testing.T) {
	tests := []struct {
		name string
		plan FTSScannedCountPlan
		want FTSScannedCountDecision
	}{
		{
			name: "positive cached total is used",
			plan: FTSScannedCountPlan{CachedTotalDocs: 123},
			want: FTSScannedCountDecision{UseCached: true, Scanned: 123},
		},
		{
			name: "zero cached total falls back to index count",
			plan: FTSScannedCountPlan{},
			want: FTSScannedCountDecision{},
		},
		{
			name: "negative cached total falls back to index count",
			plan: FTSScannedCountPlan{CachedTotalDocs: -1},
			want: FTSScannedCountDecision{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanFTSScannedCount(tt.plan); got != tt.want {
				t.Fatalf("PlanFTSScannedCount() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanFTSQueryOutcome(t *testing.T) {
	tests := []struct {
		name string
		plan FTSQueryOutcomePlan
		want FTSQueryOutcomeDecision
	}{
		{
			name: "missing index does nothing",
			plan: FTSQueryOutcomePlan{CachedTotalDocs: 123},
			want: FTSQueryOutcomeDecision{},
		},
		{
			name: "query error records opened index without usable hits",
			plan: FTSQueryOutcomePlan{IndexAvailable: true, QueryErrored: true, CachedTotalDocs: 123},
			want: FTSQueryOutcomeDecision{IndexOpened: true},
		},
		{
			name: "success with cached total publishes fts mode and cached scanned count",
			plan: FTSQueryOutcomePlan{IndexAvailable: true, CachedTotalDocs: 123},
			want: FTSQueryOutcomeDecision{IndexOpened: true, UseHits: true, UseCachedScanned: true, Scanned: 123, Mode: "fts5"},
		},
		{
			name: "success without cached total requests fallback scanned count",
			plan: FTSQueryOutcomePlan{IndexAvailable: true},
			want: FTSQueryOutcomeDecision{IndexOpened: true, UseHits: true, Mode: "fts5"},
		},
		{
			name: "negative cached total requests fallback scanned count",
			plan: FTSQueryOutcomePlan{IndexAvailable: true, CachedTotalDocs: -1},
			want: FTSQueryOutcomeDecision{IndexOpened: true, UseHits: true, Mode: "fts5"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanFTSQueryOutcome(tt.plan); got != tt.want {
				t.Fatalf("PlanFTSQueryOutcome() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanBackendFallback(t *testing.T) {
	tests := []struct {
		name string
		plan BackendFallbackPlan
		want BackendFallbackDecision
	}{
		{
			name: "explicit scan runs scan without warnings",
			plan: BackendFallbackPlan{UseScan: true},
			want: BackendFallbackDecision{RunScan: true},
		},
		{
			name: "resolved fts mode does nothing",
			plan: BackendFallbackPlan{IndexAvailable: true, Mode: "fts5"},
			want: BackendFallbackDecision{},
		},
		{
			name: "missing index warns and scans",
			plan: BackendFallbackPlan{},
			want: BackendFallbackDecision{WarnMissingIndex: true, RunScan: true},
		},
		{
			name: "missing shared index scans without missing-index warning",
			plan: BackendFallbackPlan{SharedIndexProvided: true},
			want: BackendFallbackDecision{RunScan: true},
		},
		{
			name: "query error warns and scans",
			plan: BackendFallbackPlan{IndexAvailable: true, QueryErrored: true},
			want: BackendFallbackDecision{WarnQueryError: true, RunScan: true},
		},
		{
			name: "ftsOnly missing index returns degraded",
			plan: BackendFallbackPlan{FTSOnly: true},
			want: BackendFallbackDecision{ReturnDegraded: true},
		},
		{
			name: "ftsOnly query error returns degraded without warning",
			plan: BackendFallbackPlan{FTSOnly: true, IndexAvailable: true, QueryErrored: true},
			want: BackendFallbackDecision{ReturnDegraded: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanBackendFallback(tt.plan); got != tt.want {
				t.Fatalf("PlanBackendFallback() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRunBackendRetrieval(t *testing.T) {
	t.Run("explicit scan skips fts callbacks", func(t *testing.T) {
		ftsCalled := false
		got := RunBackendRetrieval(BackendRetrieval[string, string]{
			Plan: BackendRetrievalPlan{UseScan: true},
			OpenLocal: func() (string, bool) {
				ftsCalled = true
				return "local", true
			},
			Query: func(index string) ([]string, error) {
				ftsCalled = true
				return []string{"fts"}, nil
			},
			Scan: func() ([]string, int) {
				return []string{"scan"}, 7
			},
		})
		if ftsCalled {
			t.Fatalf("RunBackendRetrieval() called FTS callbacks in scan mode")
		}
		if got.Mode != "scan" || got.Scanned != 7 || !equalStrings(got.Hits, []string{"scan"}) {
			t.Fatalf("RunBackendRetrieval() = %+v", got)
		}
	})

	t.Run("missing local index warns and scans", func(t *testing.T) {
		got := RunBackendRetrieval(BackendRetrieval[string, string]{
			OpenLocal: func() (string, bool) {
				return "", false
			},
			Scan: func() ([]string, int) {
				return []string{"scan"}, 11
			},
		})
		if !got.WarnMissingIndex || got.WarnQueryError || got.Degraded {
			t.Fatalf("RunBackendRetrieval() warning state = %+v", got)
		}
		if got.Mode != "scan" || got.Scanned != 11 || !equalStrings(got.Hits, []string{"scan"}) {
			t.Fatalf("RunBackendRetrieval() = %+v", got)
		}
	})

	t.Run("ftsOnly missing local index returns degraded result", func(t *testing.T) {
		scanCalled := false
		got := RunBackendRetrieval(BackendRetrieval[string, string]{
			Plan: BackendRetrievalPlan{FTSOnly: true},
			OpenLocal: func() (string, bool) {
				return "", false
			},
			Scan: func() ([]string, int) {
				scanCalled = true
				return []string{"scan"}, 11
			},
		})
		if scanCalled {
			t.Fatalf("RunBackendRetrieval() scanned despite ftsOnly degraded result")
		}
		if !got.Degraded || got.WarnMissingIndex || got.Mode != "" || len(got.Hits) != 0 {
			t.Fatalf("RunBackendRetrieval() = %+v", got)
		}
	})

	t.Run("local fts success uses fallback count and closes owned index", func(t *testing.T) {
		closed := false
		got := RunBackendRetrieval(BackendRetrieval[string, string]{
			OpenLocal: func() (string, bool) {
				return "local-index", true
			},
			Query: func(index string) ([]string, error) {
				if index != "local-index" {
					t.Fatalf("Query() index = %q", index)
				}
				return []string{"fts-a", "fts-b"}, nil
			},
			Count: func(index string) int {
				if index != "local-index" {
					t.Fatalf("Count() index = %q", index)
				}
				return 42
			},
			Close: func(index string) {
				if index != "local-index" {
					t.Fatalf("Close() index = %q", index)
				}
				closed = true
			},
			Scan: func() ([]string, int) {
				t.Fatalf("Scan() should not run after FTS success")
				return nil, 0
			},
		})
		if !closed {
			t.Fatalf("RunBackendRetrieval() did not close owned local index")
		}
		if got.Mode != "fts5" || got.Scanned != 42 || !equalStrings(got.Hits, []string{"fts-a", "fts-b"}) {
			t.Fatalf("RunBackendRetrieval() = %+v", got)
		}
	})

	t.Run("shared fts success uses cached count without ownership", func(t *testing.T) {
		openCalled := false
		countCalled := false
		closeCalled := false
		got := RunBackendRetrieval(BackendRetrieval[string, string]{
			Plan:        BackendRetrievalPlan{SharedIndexProvided: true, CachedTotalDocs: 99},
			SharedIndex: "shared-index",
			OpenLocal: func() (string, bool) {
				openCalled = true
				return "local-index", true
			},
			Query: func(index string) ([]string, error) {
				if index != "shared-index" {
					t.Fatalf("Query() index = %q", index)
				}
				return []string{"shared-hit"}, nil
			},
			Count: func(index string) int {
				countCalled = true
				return 0
			},
			Close: func(index string) {
				closeCalled = true
			},
			Scan: func() ([]string, int) {
				t.Fatalf("Scan() should not run after shared FTS success")
				return nil, 0
			},
		})
		if openCalled || countCalled || closeCalled {
			t.Fatalf("RunBackendRetrieval() ownership callbacks open=%v count=%v close=%v", openCalled, countCalled, closeCalled)
		}
		if got.Mode != "fts5" || got.Scanned != 99 || !equalStrings(got.Hits, []string{"shared-hit"}) {
			t.Fatalf("RunBackendRetrieval() = %+v", got)
		}
	})

	t.Run("fts query error warns and scans", func(t *testing.T) {
		queryErr := errors.New("fts unavailable")
		got := RunBackendRetrieval(BackendRetrieval[string, string]{
			Plan:        BackendRetrievalPlan{SharedIndexProvided: true},
			SharedIndex: "shared-index",
			Query: func(index string) ([]string, error) {
				return nil, queryErr
			},
			Scan: func() ([]string, int) {
				return []string{"scan"}, 5
			},
		})
		if !got.WarnQueryError || got.WarnMissingIndex || !errors.Is(got.FTSQueryErr, queryErr) {
			t.Fatalf("RunBackendRetrieval() warning state = %+v", got)
		}
		if got.Mode != "scan" || got.Scanned != 5 || !equalStrings(got.Hits, []string{"scan"}) {
			t.Fatalf("RunBackendRetrieval() = %+v", got)
		}
	})
}

func TestPlanStrictProfileFilter(t *testing.T) {
	tests := []struct {
		name string
		plan StrictProfileFilterPlan
		want StrictProfileFilterDecision
	}{
		{
			name: "non strict profile search does not filter",
			plan: StrictProfileFilterPlan{ProfileActive: true},
			want: StrictProfileFilterDecision{},
		},
		{
			name: "strict without profile does not filter",
			plan: StrictProfileFilterPlan{Strict: true},
			want: StrictProfileFilterDecision{},
		},
		{
			name: "strict with profile filters",
			plan: StrictProfileFilterPlan{Strict: true, ProfileActive: true},
			want: StrictProfileFilterDecision{Apply: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanStrictProfileFilter(tt.plan); got != tt.want {
				t.Fatalf("PlanStrictProfileFilter() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanProfileProcessing(t *testing.T) {
	tests := []struct {
		name string
		plan ProfileProcessingPlan
		want ProfileProcessingDecision
	}{
		{
			name: "no profile skips annotation and filtering",
			plan: ProfileProcessingPlan{},
			want: ProfileProcessingDecision{},
		},
		{
			name: "active profile annotates without strict filter",
			plan: ProfileProcessingPlan{ProfileActive: true},
			want: ProfileProcessingDecision{Annotate: true},
		},
		{
			name: "strict without profile is still a no-op",
			plan: ProfileProcessingPlan{Strict: true},
			want: ProfileProcessingDecision{},
		},
		{
			name: "strict active profile annotates and filters",
			plan: ProfileProcessingPlan{Strict: true, ProfileActive: true},
			want: ProfileProcessingDecision{Annotate: true, ApplyStrictFilter: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanProfileProcessing(tt.plan); got != tt.want {
				t.Fatalf("PlanProfileProcessing() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestApplyProfileProcessing(t *testing.T) {
	tests := []struct {
		name     string
		decision ProfileProcessingDecision
		want     []string
	}{
		{
			name:     "skips profile passes",
			decision: ProfileProcessingDecision{},
			want:     []string{"hit"},
		},
		{
			name:     "annotates without strict filter",
			decision: ProfileProcessingDecision{Annotate: true},
			want:     []string{"hit", "annotated"},
		},
		{
			name:     "strict filter runs after annotation",
			decision: ProfileProcessingDecision{Annotate: true, ApplyStrictFilter: true},
			want:     []string{"hit", "annotated", "filtered"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyProfileProcessing(
				[]string{"hit"},
				tt.decision,
				func(hits []string) []string {
					return append(hits, "annotated")
				},
				func(hits []string) []string {
					return append(hits, "filtered")
				},
			)
			if len(got) != len(tt.want) {
				t.Fatalf("ApplyProfileProcessing() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("ApplyProfileProcessing() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestPlanSourceHygieneWindow(t *testing.T) {
	tests := []struct {
		name string
		plan SourceHygieneWindowPlan
		want SourceHygieneWindowDecision
	}{
		{
			name: "all durable uses user limit",
			plan: SourceHygieneWindowPlan{UserLimit: 2, Penalties: []int{0, 0, 0}},
			want: SourceHygieneWindowDecision{Limit: 2},
		},
		{
			name: "durable prefix truncates before first penalty",
			plan: SourceHygieneWindowPlan{UserLimit: 5, Penalties: []int{0, 0, 10, 20}},
			want: SourceHygieneWindowDecision{Limit: 2},
		},
		{
			name: "first penalized keeps normal window",
			plan: SourceHygieneWindowPlan{UserLimit: 2, Penalties: []int{10, 20, 30}},
			want: SourceHygieneWindowDecision{Limit: 2},
		},
		{
			name: "zero user limit expands to usable window",
			plan: SourceHygieneWindowPlan{Penalties: []int{0, 0, 10}},
			want: SourceHygieneWindowDecision{Limit: 2},
		},
		{
			name: "oversized user limit expands to all sorted hits when no durable prefix",
			plan: SourceHygieneWindowPlan{UserLimit: 10, Penalties: []int{10, 20, 30}},
			want: SourceHygieneWindowDecision{Limit: 3},
		},
		{
			name: "empty penalties keep empty window",
			plan: SourceHygieneWindowPlan{UserLimit: 10},
			want: SourceHygieneWindowDecision{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanSourceHygieneWindow(tt.plan); got != tt.want {
				t.Fatalf("PlanSourceHygieneWindow() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestApplySourceHygiene(t *testing.T) {
	type hit struct {
		id      string
		penalty int
	}
	tests := []struct {
		name      string
		hits      []hit
		userLimit int
		wantIDs   []string
	}{
		{
			name:      "durable prefix wins after stable penalty sort",
			hits:      []hit{{id: "durable-a"}, {id: "penalty-b", penalty: 10}, {id: "durable-c"}, {id: "penalty-d", penalty: 20}},
			userLimit: 10,
			wantIDs:   []string{"durable-a", "durable-c"},
		},
		{
			name:      "first penalized keeps normal window",
			hits:      []hit{{id: "a", penalty: 10}, {id: "b", penalty: 20}, {id: "c", penalty: 30}},
			userLimit: 2,
			wantIDs:   []string{"a", "b"},
		},
		{
			name:      "oversized user limit expands to all sorted hits without durable prefix",
			hits:      []hit{{id: "a", penalty: 20}, {id: "b", penalty: 10}, {id: "c", penalty: 30}},
			userLimit: 10,
			wantIDs:   []string{"b", "a", "c"},
		},
		{
			name:      "equal penalties preserve input order",
			hits:      []hit{{id: "a", penalty: 10}, {id: "b", penalty: 10}, {id: "c", penalty: 10}},
			userLimit: 3,
			wantIDs:   []string{"a", "b", "c"},
		},
		{
			name:      "empty input stays empty",
			hits:      nil,
			userLimit: 3,
			wantIDs:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplySourceHygiene(tt.hits, tt.userLimit, func(h hit) int { return h.penalty })
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("ApplySourceHygiene() len = %d, want %d; got %+v", len(got), len(tt.wantIDs), got)
			}
			for i, want := range tt.wantIDs {
				if got[i].id != want {
					t.Fatalf("ApplySourceHygiene()[%d] = %q, want %q; got %+v", i, got[i].id, want, got)
				}
			}
		})
	}
}

func TestPlanVersionPolicyProcessing(t *testing.T) {
	tests := []struct {
		name string
		plan VersionPolicyProcessingPlan
		want VersionPolicyProcessingDecision
	}{
		{
			name: "no policy skips both passes",
			plan: VersionPolicyProcessingPlan{SourceID: "react-native-v079"},
			want: VersionPolicyProcessingDecision{},
		},
		{
			name: "soft policy only applies pre-rerank pass",
			plan: VersionPolicyProcessingPlan{PolicyActive: true},
			want: VersionPolicyProcessingDecision{ApplyPreRerank: true},
		},
		{
			name: "source constraint applies both passes",
			plan: VersionPolicyProcessingPlan{PolicyActive: true, SourceID: "react-native-v079"},
			want: VersionPolicyProcessingDecision{ApplyPreRerank: true, ApplyPostRerankFilter: true},
		},
		{
			name: "latest constraint applies both passes",
			plan: VersionPolicyProcessingPlan{PolicyActive: true, LatestOnly: true},
			want: VersionPolicyProcessingDecision{ApplyPreRerank: true, ApplyPostRerankFilter: true},
		},
		{
			name: "explicit version applies both passes",
			plan: VersionPolicyProcessingPlan{PolicyActive: true, Version: "0.79"},
			want: VersionPolicyProcessingDecision{ApplyPreRerank: true, ApplyPostRerankFilter: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanVersionPolicyProcessing(tt.plan); got != tt.want {
				t.Fatalf("PlanVersionPolicyProcessing() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanPostRerankVersionFilter(t *testing.T) {
	tests := []struct {
		name string
		plan PostRerankVersionFilterPlan
		want PostRerankVersionFilterDecision
	}{
		{
			name: "no policy skips filter",
			plan: PostRerankVersionFilterPlan{SourceID: "react-native-v079"},
			want: PostRerankVersionFilterDecision{},
		},
		{
			name: "soft policy skips filter",
			plan: PostRerankVersionFilterPlan{PolicyActive: true},
			want: PostRerankVersionFilterDecision{},
		},
		{
			name: "source id applies filter",
			plan: PostRerankVersionFilterPlan{PolicyActive: true, SourceID: "react-native-v079"},
			want: PostRerankVersionFilterDecision{Apply: true},
		},
		{
			name: "latest only applies filter",
			plan: PostRerankVersionFilterPlan{PolicyActive: true, LatestOnly: true},
			want: PostRerankVersionFilterDecision{Apply: true},
		},
		{
			name: "version applies filter",
			plan: PostRerankVersionFilterPlan{PolicyActive: true, Version: "0.79"},
			want: PostRerankVersionFilterDecision{Apply: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanPostRerankVersionFilter(tt.plan); got != tt.want {
				t.Fatalf("PlanPostRerankVersionFilter() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestApplyPostRerankVersionFilter(t *testing.T) {
	tests := []struct {
		name string
		plan PostRerankVersionFilterApplication[string]
		want []string
	}{
		{
			name: "skipped filter preserves hits",
			plan: PostRerankVersionFilterApplication[string]{
				Hits:         []string{"current-a", "current-b"},
				FilteredHits: []string{"filtered-a"},
				Decision:     PostRerankVersionFilterDecision{},
			},
			want: []string{"current-a", "current-b"},
		},
		{
			name: "applied filter uses filtered hits",
			plan: PostRerankVersionFilterApplication[string]{
				Hits:         []string{"current-a", "current-b"},
				FilteredHits: []string{"filtered-a"},
				Decision:     PostRerankVersionFilterDecision{Apply: true},
			},
			want: []string{"filtered-a"},
		},
		{
			name: "applied filter can return empty hits",
			plan: PostRerankVersionFilterApplication[string]{
				Hits:     []string{"current-a"},
				Decision: PostRerankVersionFilterDecision{Apply: true},
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyPostRerankVersionFilter(tt.plan)
			if len(got) != len(tt.want) {
				t.Fatalf("ApplyPostRerankVersionFilter() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("ApplyPostRerankVersionFilter() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestPlanRerankStages(t *testing.T) {
	tests := []struct {
		name string
		plan RerankStagePlan
		want RerankStageDecision
	}{
		{
			name: "no rerank skips both optional stages",
			plan: RerankStagePlan{RerankHybrid: true, HitCount: 10},
			want: RerankStageDecision{},
		},
		{
			name: "bm25 confidence skips hybrid and rerank",
			plan: RerankStagePlan{RerankLLM: true, RerankHybrid: true, BM25Confident: true, HitCount: 10},
			want: RerankStageDecision{},
		},
		{
			name: "llm rerank runs hybrid and rerank when uncertain with enough hits",
			plan: RerankStagePlan{RerankLLM: true, RerankHybrid: true, HitCount: 10},
			want: RerankStageDecision{ApplyHybrid: true, ApplyRerank: true},
		},
		{
			name: "embedding rerank can run without hybrid",
			plan: RerankStagePlan{Rerank: true, HitCount: 2},
			want: RerankStageDecision{ApplyRerank: true},
		},
		{
			name: "single candidate can still apply hybrid before rerank decision is replanned",
			plan: RerankStagePlan{RerankLLM: true, RerankHybrid: true, HitCount: 1},
			want: RerankStageDecision{ApplyHybrid: true},
		},
		{
			name: "post-hybrid replanning enables rerank after candidate expansion",
			plan: RerankStagePlan{RerankLLM: true, RerankHybrid: true, HitCount: 3},
			want: RerankStageDecision{ApplyHybrid: true, ApplyRerank: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanRerankStages(tt.plan); got != tt.want {
				t.Fatalf("PlanRerankStages() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanHybridOutcome(t *testing.T) {
	tests := []struct {
		name string
		plan HybridOutcomePlan
		want HybridOutcomeDecision
	}{
		{
			name: "success accepts expanded hits",
			plan: HybridOutcomePlan{},
			want: HybridOutcomeDecision{UseExpandedHits: true, HybridApplied: true},
		},
		{
			name: "error warns and preserves bm25 hits",
			plan: HybridOutcomePlan{HybridErrored: true},
			want: HybridOutcomeDecision{WarnFallback: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanHybridOutcome(tt.plan); got != tt.want {
				t.Fatalf("PlanHybridOutcome() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanRerankCall(t *testing.T) {
	tests := []struct {
		name string
		plan RerankCallPlan
		want RerankCallDecision
	}{
		{
			name: "no rerank selects no implementation",
			plan: RerankCallPlan{},
			want: RerankCallDecision{},
		},
		{
			name: "embedding rerank selects embedding implementation",
			plan: RerankCallPlan{Rerank: true},
			want: RerankCallDecision{UseEmbedding: true},
		},
		{
			name: "llm rerank selects llm implementation",
			plan: RerankCallPlan{RerankLLM: true},
			want: RerankCallDecision{UseLLM: true},
		},
		{
			name: "llm wins when both rerank flags are set",
			plan: RerankCallPlan{Rerank: true, RerankLLM: true},
			want: RerankCallDecision{UseLLM: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanRerankCall(tt.plan); got != tt.want {
				t.Fatalf("PlanRerankCall() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanRerankModeTag(t *testing.T) {
	tests := []struct {
		name string
		plan RerankModeTagPlan
		want string
	}{
		{
			name: "llm",
			plan: RerankModeTagPlan{RerankLLM: true},
			want: "llm",
		},
		{
			name: "hybrid llm",
			plan: RerankModeTagPlan{RerankLLM: true, HybridApplied: true},
			want: "hybrid-llm",
		},
		{
			name: "embedding",
			plan: RerankModeTagPlan{},
			want: "embed",
		},
		{
			name: "chunked embedding",
			plan: RerankModeTagPlan{RerankChunkSize: 256},
			want: "embed-chunked-256",
		},
		{
			name: "hybrid chunked embedding",
			plan: RerankModeTagPlan{RerankChunkSize: 256, HybridApplied: true},
			want: "hybrid-embed-chunked-256",
		},
		{
			name: "llm ignores chunk size",
			plan: RerankModeTagPlan{RerankLLM: true, RerankChunkSize: 256},
			want: "llm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanRerankModeTag(tt.plan); got != tt.want {
				t.Fatalf("PlanRerankModeTag() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPlanRerankOutcome(t *testing.T) {
	tests := []struct {
		name string
		plan RerankOutcomePlan
		want RerankOutcomeDecision
	}{
		{
			name: "error falls back to base mode",
			plan: RerankOutcomePlan{RerankErrored: true, BaseMode: "fts5", RerankLLM: true, HybridApplied: true},
			want: RerankOutcomeDecision{WarnFallback: true, Mode: "fts5"},
		},
		{
			name: "llm success appends rerank mode",
			plan: RerankOutcomePlan{BaseMode: "fts5", RerankLLM: true},
			want: RerankOutcomeDecision{UseRerankedHits: true, Mode: "fts5+rerank-llm"},
		},
		{
			name: "hybrid chunked embedding success appends full mode",
			plan: RerankOutcomePlan{BaseMode: "scan", RerankChunkSize: 512, HybridApplied: true},
			want: RerankOutcomeDecision{UseRerankedHits: true, Mode: "scan+rerank-hybrid-embed-chunked-512"},
		},
		{
			name: "empty base mode preserves old concatenation behavior",
			plan: RerankOutcomePlan{},
			want: RerankOutcomeDecision{UseRerankedHits: true, Mode: "+rerank-embed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanRerankOutcome(tt.plan); got != tt.want {
				t.Fatalf("PlanRerankOutcome() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestApplyRerankOutcome(t *testing.T) {
	tests := []struct {
		name string
		plan RerankOutcomeApplication[string]
		want RerankOutcomeApplicationResult[string]
	}{
		{
			name: "failure keeps current hits and mode",
			plan: RerankOutcomeApplication[string]{
				CurrentHits:  []string{"bm25-a", "bm25-b"},
				RerankedHits: []string{"reranked-a"},
				CurrentMode:  "fts5",
				Outcome:      RerankOutcomeDecision{WarnFallback: true, Mode: "fts5"},
			},
			want: RerankOutcomeApplicationResult[string]{
				Hits:         []string{"bm25-a", "bm25-b"},
				Mode:         "fts5",
				WarnFallback: true,
			},
		},
		{
			name: "success replaces hits and mode",
			plan: RerankOutcomeApplication[string]{
				CurrentHits:  []string{"bm25-a", "bm25-b"},
				RerankedHits: []string{"reranked-a", "reranked-b"},
				CurrentMode:  "scan",
				Outcome:      RerankOutcomeDecision{UseRerankedHits: true, Mode: "scan+rerank-llm"},
			},
			want: RerankOutcomeApplicationResult[string]{
				Hits: []string{"reranked-a", "reranked-b"},
				Mode: "scan+rerank-llm",
			},
		},
		{
			name: "noop outcome preserves current hits and mode",
			plan: RerankOutcomeApplication[string]{
				CurrentHits: []string{"bm25-a"},
				CurrentMode: "fts5",
			},
			want: RerankOutcomeApplicationResult[string]{
				Hits: []string{"bm25-a"},
				Mode: "fts5",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyRerankOutcome(tt.plan)
			if got.Mode != tt.want.Mode || got.WarnFallback != tt.want.WarnFallback {
				t.Fatalf("ApplyRerankOutcome() = %+v, want %+v", got, tt.want)
			}
			if len(got.Hits) != len(tt.want.Hits) {
				t.Fatalf("ApplyRerankOutcome() hits = %v, want %v", got.Hits, tt.want.Hits)
			}
			for i := range got.Hits {
				if got.Hits[i] != tt.want.Hits[i] {
					t.Fatalf("ApplyRerankOutcome() hits = %v, want %v", got.Hits, tt.want.Hits)
				}
			}
		})
	}
}

func TestRunRerankProcessing(t *testing.T) {
	t.Run("confident bm25 skips hybrid and rerank", func(t *testing.T) {
		called := false
		got := RunRerankProcessing(RerankProcessing[string]{
			Hits: []string{"bm25-a", "bm25-b"},
			Mode: "fts5",
			Plan: RerankProcessingPlan{
				Rerank:        true,
				RerankHybrid:  true,
				BM25Confident: true,
			},
			Hybrid: func(hits []string) ([]string, error) {
				called = true
				return hits, nil
			},
			EmbeddingRerank: func(hits []string) ([]string, error) {
				called = true
				return hits, nil
			},
		})
		if called {
			t.Fatalf("RunRerankProcessing() called optional stages for confident BM25")
		}
		if got.Mode != "fts5" || !equalStrings(got.Hits, []string{"bm25-a", "bm25-b"}) {
			t.Fatalf("RunRerankProcessing() = %+v", got)
		}
	})

	t.Run("hybrid expansion is replanned before rerank", func(t *testing.T) {
		expanded := []string{"expanded-a", "expanded-b"}
		got := RunRerankProcessing(RerankProcessing[string]{
			Hits: []string{"bm25-a"},
			Mode: "fts5",
			Plan: RerankProcessingPlan{
				Rerank:       true,
				RerankHybrid: true,
			},
			Hybrid: func(hits []string) ([]string, error) {
				if !equalStrings(hits, []string{"bm25-a"}) {
					t.Fatalf("Hybrid() hits = %v", hits)
				}
				return expanded, nil
			},
			EmbeddingRerank: func(hits []string) ([]string, error) {
				if !equalStrings(hits, expanded) {
					t.Fatalf("EmbeddingRerank() hits = %v, want %v", hits, expanded)
				}
				return []string{"expanded-b", "expanded-a"}, nil
			},
		})
		if got.Mode != "fts5+rerank-hybrid-embed" || !got.HybridApplied {
			t.Fatalf("RunRerankProcessing() = %+v", got)
		}
		if !equalStrings(got.Hits, []string{"expanded-b", "expanded-a"}) {
			t.Fatalf("RunRerankProcessing() hits = %v", got.Hits)
		}
	})

	t.Run("hybrid failure keeps bm25 candidates and still reranks", func(t *testing.T) {
		hybridErr := errors.New("hybrid unavailable")
		got := RunRerankProcessing(RerankProcessing[string]{
			Hits: []string{"bm25-a", "bm25-b"},
			Mode: "scan",
			Plan: RerankProcessingPlan{
				Rerank:       true,
				RerankHybrid: true,
			},
			Hybrid: func(hits []string) ([]string, error) {
				return nil, hybridErr
			},
			EmbeddingRerank: func(hits []string) ([]string, error) {
				if !equalStrings(hits, []string{"bm25-a", "bm25-b"}) {
					t.Fatalf("EmbeddingRerank() hits = %v", hits)
				}
				return []string{"bm25-b", "bm25-a"}, nil
			},
		})
		if !got.WarnHybridFallback || !errors.Is(got.HybridErr, hybridErr) || got.HybridApplied {
			t.Fatalf("RunRerankProcessing() hybrid state = %+v", got)
		}
		if got.Mode != "scan+rerank-embed" || !equalStrings(got.Hits, []string{"bm25-b", "bm25-a"}) {
			t.Fatalf("RunRerankProcessing() = %+v", got)
		}
	})

	t.Run("llm rerank wins when both rerank flags are set", func(t *testing.T) {
		embeddingCalled := false
		got := RunRerankProcessing(RerankProcessing[string]{
			Hits: []string{"bm25-a", "bm25-b"},
			Mode: "fts5",
			Plan: RerankProcessingPlan{
				Rerank:    true,
				RerankLLM: true,
			},
			EmbeddingRerank: func(hits []string) ([]string, error) {
				embeddingCalled = true
				return hits, nil
			},
			LLMRerank: func(hits []string) ([]string, error) {
				return []string{"llm-a", "llm-b"}, nil
			},
		})
		if embeddingCalled {
			t.Fatalf("RunRerankProcessing() called embedding reranker despite LLM flag")
		}
		if got.Mode != "fts5+rerank-llm" || !equalStrings(got.Hits, []string{"llm-a", "llm-b"}) {
			t.Fatalf("RunRerankProcessing() = %+v", got)
		}
	})

	t.Run("rerank failure falls back to current hits and mode", func(t *testing.T) {
		rerankErr := errors.New("rerank unavailable")
		got := RunRerankProcessing(RerankProcessing[string]{
			Hits: []string{"bm25-a", "bm25-b"},
			Mode: "fts5",
			Plan: RerankProcessingPlan{Rerank: true},
			EmbeddingRerank: func(hits []string) ([]string, error) {
				return nil, rerankErr
			},
		})
		if !got.WarnRerankFallback || !errors.Is(got.RerankErr, rerankErr) {
			t.Fatalf("RunRerankProcessing() rerank state = %+v", got)
		}
		if got.Mode != "fts5" || !equalStrings(got.Hits, []string{"bm25-a", "bm25-b"}) {
			t.Fatalf("RunRerankProcessing() = %+v", got)
		}
	})
}

func TestRunPostRetrievalProcessing(t *testing.T) {
	t.Run("runs post-backend stages in order", func(t *testing.T) {
		got := RunPostRetrievalProcessing(PostRetrievalProcessing[string]{
			Hits: []string{"base"},
			Mode: "fts5",
			Plan: PostRetrievalProcessingPlan{
				VersionPolicy: VersionPolicyProcessingPlan{PolicyActive: true, SourceID: "react-native-v079"},
				Profile:       ProfileProcessingPlan{Strict: true, ProfileActive: true},
				Rerank:        true,
			},
			ApplyVersionPolicy: func(hits []string) []string {
				return append(hits, "version-pre")
			},
			AnnotateProfile: func(hits []string) []string {
				if !equalStrings(hits, []string{"base", "version-pre"}) {
					t.Fatalf("AnnotateProfile() hits = %v", hits)
				}
				return append(hits, "profile-annotate")
			},
			StrictProfileFilter: func(hits []string) []string {
				if !equalStrings(hits, []string{"base", "version-pre", "profile-annotate"}) {
					t.Fatalf("StrictProfileFilter() hits = %v", hits)
				}
				return append(hits, "profile-strict")
			},
			BM25Confident: func(hits []string) bool {
				if !equalStrings(hits, []string{"base", "version-pre", "profile-annotate", "profile-strict"}) {
					t.Fatalf("BM25Confident() hits = %v", hits)
				}
				return false
			},
			EmbeddingRerank: func(hits []string) ([]string, error) {
				if !equalStrings(hits, []string{"base", "version-pre", "profile-annotate", "profile-strict"}) {
					t.Fatalf("EmbeddingRerank() hits = %v", hits)
				}
				return append(hits, "rerank"), nil
			},
			PostRerankVersionFilter: func(hits []string) []string {
				if !equalStrings(hits, []string{"base", "version-pre", "profile-annotate", "profile-strict", "rerank"}) {
					t.Fatalf("PostRerankVersionFilter() hits = %v", hits)
				}
				return append(hits, "version-post")
			},
			SourceHygiene: func(hits []string) []string {
				if !equalStrings(hits, []string{"base", "version-pre", "profile-annotate", "profile-strict", "rerank", "version-post"}) {
					t.Fatalf("SourceHygiene() hits = %v", hits)
				}
				return append(hits, "hygiene")
			},
		})
		want := []string{"base", "version-pre", "profile-annotate", "profile-strict", "rerank", "version-post", "hygiene"}
		if got.Mode != "fts5+rerank-embed" || !equalStrings(got.Hits, want) {
			t.Fatalf("RunPostRetrievalProcessing() = %+v, want hits %v", got, want)
		}
	})

	t.Run("soft version policy skips post-rerank filter", func(t *testing.T) {
		postFilterCalled := false
		got := RunPostRetrievalProcessing(PostRetrievalProcessing[string]{
			Hits: []string{"base"},
			Mode: "scan",
			Plan: PostRetrievalProcessingPlan{
				VersionPolicy: VersionPolicyProcessingPlan{PolicyActive: true},
				Profile:       ProfileProcessingPlan{ProfileActive: true},
			},
			ApplyVersionPolicy: func(hits []string) []string {
				return append(hits, "version-pre")
			},
			AnnotateProfile: func(hits []string) []string {
				return append(hits, "profile-annotate")
			},
			PostRerankVersionFilter: func(hits []string) []string {
				postFilterCalled = true
				return append(hits, "version-post")
			},
		})
		if postFilterCalled {
			t.Fatalf("RunPostRetrievalProcessing() called post filter for soft version policy")
		}
		if got.Mode != "scan" || !equalStrings(got.Hits, []string{"base", "version-pre", "profile-annotate"}) {
			t.Fatalf("RunPostRetrievalProcessing() = %+v", got)
		}
	})

	t.Run("bm25 confidence skips rerank but keeps hygiene", func(t *testing.T) {
		rerankCalled := false
		got := RunPostRetrievalProcessing(PostRetrievalProcessing[string]{
			Hits: []string{"base-a", "base-b"},
			Mode: "fts5",
			Plan: PostRetrievalProcessingPlan{
				Rerank: true,
			},
			BM25Confident: func(hits []string) bool {
				return true
			},
			EmbeddingRerank: func(hits []string) ([]string, error) {
				rerankCalled = true
				return hits, nil
			},
			SourceHygiene: func(hits []string) []string {
				return append(hits, "hygiene")
			},
		})
		if rerankCalled {
			t.Fatalf("RunPostRetrievalProcessing() reranked confident BM25")
		}
		if got.Mode != "fts5" || !equalStrings(got.Hits, []string{"base-a", "base-b", "hygiene"}) {
			t.Fatalf("RunPostRetrievalProcessing() = %+v", got)
		}
	})

	t.Run("propagates rerank fallback warning", func(t *testing.T) {
		rerankErr := errors.New("rerank failed")
		got := RunPostRetrievalProcessing(PostRetrievalProcessing[string]{
			Hits: []string{"base-a", "base-b"},
			Mode: "fts5",
			Plan: PostRetrievalProcessingPlan{
				Rerank: true,
			},
			EmbeddingRerank: func(hits []string) ([]string, error) {
				return nil, rerankErr
			},
		})
		if !got.WarnRerankFallback || !errors.Is(got.RerankErr, rerankErr) {
			t.Fatalf("RunPostRetrievalProcessing() warning state = %+v", got)
		}
		if got.Mode != "fts5" || !equalStrings(got.Hits, []string{"base-a", "base-b"}) {
			t.Fatalf("RunPostRetrievalProcessing() = %+v", got)
		}
	})
}

func TestPlanBM25Confidence(t *testing.T) {
	tests := []struct {
		name string
		plan BM25ConfidencePlan
		want bool
	}{
		{
			name: "gate disabled",
			plan: BM25ConfidencePlan{HitCount: 2, TopScore: 100, SecondScore: 50},
			want: false,
		},
		{
			name: "single hit is not confident",
			plan: BM25ConfidencePlan{Gate: 0.10, HitCount: 1, TopScore: 100},
			want: false,
		},
		{
			name: "non-positive top score is not confident",
			plan: BM25ConfidencePlan{Gate: 0.10, HitCount: 2, TopScore: 0, SecondScore: -10},
			want: false,
		},
		{
			name: "gap below gate",
			plan: BM25ConfidencePlan{Gate: 0.10, HitCount: 2, TopScore: 100, SecondScore: 91},
			want: false,
		},
		{
			name: "gap equals gate",
			plan: BM25ConfidencePlan{Gate: 0.10, HitCount: 2, TopScore: 100, SecondScore: 90},
			want: true,
		},
		{
			name: "gap above gate",
			plan: BM25ConfidencePlan{Gate: 0.10, HitCount: 2, TopScore: 100, SecondScore: 80},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanBM25Confidence(tt.plan); got != tt.want {
				t.Fatalf("PlanBM25Confidence() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlanCompactOutputPreset(t *testing.T) {
	tests := []struct {
		name string
		plan CompactOutputPresetPlan
		want CompactOutputPresetDecision
	}{
		{
			name: "non compact keeps existing values",
			plan: CompactOutputPresetPlan{
				SnippetLen:         120,
				MaxSnippets:        2,
				DefaultSnippetLen:  80,
				DefaultMaxSnippets: 1,
			},
			want: CompactOutputPresetDecision{SnippetLen: 120, MaxSnippets: 2},
		},
		{
			name: "compact fills unset values",
			plan: CompactOutputPresetPlan{
				Compact:            true,
				DefaultSnippetLen:  80,
				DefaultMaxSnippets: 1,
			},
			want: CompactOutputPresetDecision{SnippetLen: 80, MaxSnippets: 1},
		},
		{
			name: "compact preserves explicit snippet length",
			plan: CompactOutputPresetPlan{
				Compact:            true,
				SnippetLen:         120,
				DefaultSnippetLen:  80,
				DefaultMaxSnippets: 1,
			},
			want: CompactOutputPresetDecision{SnippetLen: 120, MaxSnippets: 1},
		},
		{
			name: "compact preserves explicit snippet count",
			plan: CompactOutputPresetPlan{
				Compact:            true,
				MaxSnippets:        2,
				DefaultSnippetLen:  80,
				DefaultMaxSnippets: 1,
			},
			want: CompactOutputPresetDecision{SnippetLen: 80, MaxSnippets: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanCompactOutputPreset(tt.plan); got != tt.want {
				t.Fatalf("PlanCompactOutputPreset() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanOutputOverrides(t *testing.T) {
	tests := []struct {
		name string
		plan OutputOverridePlan
		want OutputOverrideDecision
	}{
		{
			name: "default does nothing",
			plan: OutputOverridePlan{},
			want: OutputOverrideDecision{},
		},
		{
			name: "compact drops url",
			plan: OutputOverridePlan{Compact: true},
			want: OutputOverrideDecision{Apply: true, DropURL: true},
		},
		{
			name: "no snippets drops snippets",
			plan: OutputOverridePlan{NoSnippets: true},
			want: OutputOverrideDecision{Apply: true, DropSnippets: true},
		},
		{
			name: "snippet length retunes snippets",
			plan: OutputOverridePlan{SnippetLen: 80},
			want: OutputOverrideDecision{Apply: true, RetuneSnippets: true},
		},
		{
			name: "max snippets retunes snippets",
			plan: OutputOverridePlan{MaxSnippets: 1},
			want: OutputOverrideDecision{Apply: true, RetuneSnippets: true},
		},
		{
			name: "no snippets wins over retune",
			plan: OutputOverridePlan{NoSnippets: true, MaxSnippets: 1, SnippetLen: 80},
			want: OutputOverrideDecision{Apply: true, DropSnippets: true},
		},
		{
			name: "compact combines with retune",
			plan: OutputOverridePlan{Compact: true, MaxSnippets: 2},
			want: OutputOverrideDecision{Apply: true, RetuneSnippets: true, DropURL: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanOutputOverrides(tt.plan); got != tt.want {
				t.Fatalf("PlanOutputOverrides() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
