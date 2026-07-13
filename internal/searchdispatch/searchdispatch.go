package searchdispatch

import (
	"errors"
	"sort"
	"strconv"
	"time"
)

// Request is the structured input to a docs-puller search dispatch run.
// Type parameters keep this package independent from the root CLI's option,
// hit, and shared-index types.
type Request[Opts any, Shared any] struct {
	Query     string
	Opts      Opts
	SharedIdx Shared
}

// Result is the structured output from a docs-puller search dispatch run.
type Result[Hit any] struct {
	Hits    []Hit
	Scanned int
	Mode    string
}

// CandidateLimitPlan captures the first dispatch decision: how many
// candidates BM25 should retrieve before downstream filtering or reranking.
type CandidateLimitPlan struct {
	UserLimit              int
	CurrentLimit           int
	Rerank                 bool
	RerankLLM              bool
	RerankK                int
	HygieneLimit           int
	NeedsWideCandidatePool bool
}

// CandidateLimits is the planned user-facing limit plus the BM25 candidate
// limit used for first-stage retrieval.
type CandidateLimits struct {
	UserLimit int
	BM25Limit int
}

// PlanCandidateLimits preserves docs-puller's historical candidate-limit
// rules while making the decision testable outside package main.
func PlanCandidateLimits(plan CandidateLimitPlan) CandidateLimits {
	userLimit := plan.UserLimit
	bm25Limit := plan.CurrentLimit
	if plan.Rerank || plan.RerankLLM {
		if plan.RerankK > userLimit {
			bm25Limit = plan.RerankK
		} else {
			bm25Limit = userLimit
		}
	} else if plan.HygieneLimit > bm25Limit {
		bm25Limit = plan.HygieneLimit
	}
	if wideLimit := userLimit*20 + 100; plan.NeedsWideCandidatePool && bm25Limit < wideLimit {
		bm25Limit = wideLimit
	}
	return CandidateLimits{
		UserLimit: userLimit,
		BM25Limit: bm25Limit,
	}
}

// FirstStageSourcePlan captures the source override used by version-scoped
// searches before first-stage retrieval. The caller still owns version policy
// resolution and option mutation.
type FirstStageSourcePlan struct {
	CurrentSource  string
	PolicySourceID string
}

// FirstStageSourceDecision returns the source to use for first-stage
// retrieval.
type FirstStageSourceDecision struct {
	Source string
}

// PlanFirstStageSource preserves docs-puller's version-policy behavior:
// a resolved policy source ID narrows first-stage retrieval to that source,
// otherwise the existing user-provided source filter is preserved.
func PlanFirstStageSource(plan FirstStageSourcePlan) FirstStageSourceDecision {
	if plan.PolicySourceID != "" {
		return FirstStageSourceDecision{Source: plan.PolicySourceID}
	}
	return FirstStageSourceDecision{Source: plan.CurrentSource}
}

// FTSIndexPlan captures the first routing choice for the FTS stage. The caller
// still owns index existence checks, opening, closing, and querying.
type FTSIndexPlan struct {
	UseScan             bool
	SharedIndexProvided bool
}

// FTSIndexDecision tells the caller whether to try FTS and whether a local
// per-query index open is needed.
type FTSIndexDecision struct {
	TryFTS    bool
	OpenLocal bool
}

// PlanFTSIndex preserves docs-puller's backend routing: explicit scan skips
// FTS; otherwise dispatch uses a shared index when present or opens one locally.
func PlanFTSIndex(plan FTSIndexPlan) FTSIndexDecision {
	if plan.UseScan {
		return FTSIndexDecision{}
	}
	return FTSIndexDecision{
		TryFTS:    true,
		OpenLocal: !plan.SharedIndexProvided,
	}
}

// FTSOpenPlan captures the dispatch decision for how aggressively to retry
// opening the local FTS index.
type FTSOpenPlan struct {
	FTSOnly bool
}

// FTSOpenAttempts is the concrete retry plan for an FTS open attempt.
type FTSOpenAttempts struct {
	Attempts   int
	RetryDelay time.Duration
}

// PlanFTSOpen preserves docs-puller's historical FTS retry policy: normal
// searches try once, while eval's ftsOnly mode retries briefly to ride out
// rebuild windows where the index exists but is temporarily unavailable.
func PlanFTSOpen(plan FTSOpenPlan) FTSOpenAttempts {
	attempts := 1
	if plan.FTSOnly {
		attempts = 5
	}
	return FTSOpenAttempts{
		Attempts:   attempts,
		RetryDelay: 100 * time.Millisecond,
	}
}

// FTSOpenAttemptPlan captures a single local FTS open attempt after the caller
// has performed any index existence and open checks.
type FTSOpenAttemptPlan struct {
	Attempt  int
	Attempts int
	Opened   bool
}

// FTSOpenAttemptDecision tells the caller whether to accept the opened local
// index and whether to sleep before the next retry.
type FTSOpenAttemptDecision struct {
	UseOpened        bool
	SleepBeforeRetry bool
}

// PlanFTSOpenAttempt preserves docs-puller's local FTS retry-loop behavior:
// use the first successfully opened index, otherwise sleep between attempts
// but never after the final attempt.
func PlanFTSOpenAttempt(plan FTSOpenAttemptPlan) FTSOpenAttemptDecision {
	if plan.Opened {
		return FTSOpenAttemptDecision{UseOpened: true}
	}
	return FTSOpenAttemptDecision{SleepBeforeRetry: plan.Attempt < plan.Attempts-1}
}

// FTSIndexSelectionPlan captures the available FTS index handles after any
// local open attempts. The caller still owns the actual index values.
type FTSIndexSelectionPlan struct {
	SharedIndexProvided bool
	LocalIndexOpened    bool
}

// FTSIndexSelectionDecision tells the caller which FTS handle class to use and
// whether it owns that handle.
type FTSIndexSelectionDecision struct {
	UseIndex      bool
	UseLocalIndex bool
	OwnsIndex     bool
}

// PlanFTSIndexSelection preserves docs-puller's ownership rules: shared
// service indexes win when provided and are not closed by dispatch, while
// locally-opened CLI indexes are owned by the dispatch call.
func PlanFTSIndexSelection(plan FTSIndexSelectionPlan) FTSIndexSelectionDecision {
	if plan.SharedIndexProvided {
		return FTSIndexSelectionDecision{UseIndex: true}
	}
	if plan.LocalIndexOpened {
		return FTSIndexSelectionDecision{UseIndex: true, UseLocalIndex: true, OwnsIndex: true}
	}
	return FTSIndexSelectionDecision{}
}

// FTSScannedCountPlan captures the cached total-doc count returned by the FTS
// query path. The caller still owns any fallback index count call.
type FTSScannedCountPlan struct {
	CachedTotalDocs int
}

// FTSScannedCountDecision tells the caller whether the cached total-doc count
// is usable or whether it must ask the index for a fresh count.
type FTSScannedCountDecision struct {
	UseCached bool
	Scanned   int
}

// PlanFTSScannedCount preserves docs-puller's scanned-count behavior: positive
// cached totals are authoritative, while zero/negative totals fall back to an
// index count.
func PlanFTSScannedCount(plan FTSScannedCountPlan) FTSScannedCountDecision {
	if plan.CachedTotalDocs > 0 {
		return FTSScannedCountDecision{UseCached: true, Scanned: plan.CachedTotalDocs}
	}
	return FTSScannedCountDecision{}
}

// FTSQueryOutcomePlan captures the FTS stage state after an index was
// selected and, when available, queried. The caller still owns the query,
// fallback count lookup, hit assignment, and index lifetime.
type FTSQueryOutcomePlan struct {
	IndexAvailable  bool
	QueryErrored    bool
	CachedTotalDocs int
}

// FTSQueryOutcomeDecision tells the caller whether the FTS stage opened,
// whether query hits are usable, and how to populate the mode/scanned count.
type FTSQueryOutcomeDecision struct {
	IndexOpened      bool
	UseHits          bool
	UseCachedScanned bool
	Scanned          int
	Mode             string
}

// PlanFTSQueryOutcome preserves docs-puller's FTS success semantics: an
// available index counts as opened even when the query fails, and only
// successful queries publish hits/mode.
func PlanFTSQueryOutcome(plan FTSQueryOutcomePlan) FTSQueryOutcomeDecision {
	if !plan.IndexAvailable {
		return FTSQueryOutcomeDecision{}
	}
	decision := FTSQueryOutcomeDecision{IndexOpened: true}
	if plan.QueryErrored {
		return decision
	}
	scanned := PlanFTSScannedCount(FTSScannedCountPlan{
		CachedTotalDocs: plan.CachedTotalDocs,
	})
	decision.UseHits = true
	decision.UseCachedScanned = scanned.UseCached
	decision.Scanned = scanned.Scanned
	decision.Mode = "fts5"
	return decision
}

// BackendFallbackPlan captures the dispatch decision after the optional FTS
// attempt has either produced a mode or left the query unresolved.
type BackendFallbackPlan struct {
	UseScan             bool
	FTSOnly             bool
	SharedIndexProvided bool
	IndexAvailable      bool
	QueryErrored        bool
	Mode                string
}

// BackendFallbackDecision tells the caller how to handle an unresolved FTS
// path without pulling package-main search or logging dependencies into this
// package.
type BackendFallbackDecision struct {
	WarnMissingIndex bool
	WarnQueryError   bool
	ReturnDegraded   bool
	RunScan          bool
}

// PlanBackendFallback preserves docs-puller's FTS fallback rules:
//   - explicit scan mode goes straight to scan
//   - ftsOnly returns an empty/degraded result when FTS did not produce a mode
//   - non-ftsOnly falls back to scan, with the same warning class as before
func PlanBackendFallback(plan BackendFallbackPlan) BackendFallbackDecision {
	if plan.UseScan {
		return BackendFallbackDecision{RunScan: plan.Mode == ""}
	}
	if plan.Mode != "" {
		return BackendFallbackDecision{}
	}
	if plan.FTSOnly {
		return BackendFallbackDecision{ReturnDegraded: true}
	}
	if !plan.IndexAvailable {
		return BackendFallbackDecision{
			WarnMissingIndex: !plan.SharedIndexProvided,
			RunScan:          true,
		}
	}
	if plan.QueryErrored {
		return BackendFallbackDecision{
			WarnQueryError: true,
			RunScan:        true,
		}
	}
	return BackendFallbackDecision{RunScan: true}
}

var (
	errFTSQueryFunctionRequired = errors.New("searchdispatch: FTS query function is required")
	errFTSCountFunctionRequired = errors.New("searchdispatch: FTS count function is required")
	errScanFunctionRequired     = errors.New("searchdispatch: scan function is required")
)

// FTSLocalOpener opens a local FTS index handle. The bool reports whether a
// usable handle was opened; errors are intentionally swallowed by callers today
// so the generic runner only needs the opened/not-opened state.
type FTSLocalOpener[Index any] func() (Index, bool)

// FTSQuery runs the FTS query against the selected index handle.
type FTSQuery[Index any, Hit any] func(Index) ([]Hit, error)

// FTSCount returns the total docs scanned for the selected index handle.
type FTSCount[Index any] func(Index) int

// FTSClose closes an owned local index handle.
type FTSClose[Index any] func(Index)

// BackendScan runs the scan fallback.
type BackendScan[Hit any] func() ([]Hit, int)

// BackendRetrievalPlan captures first-stage backend routing knobs. The caller
// still owns concrete index handles, concrete query options, and warning text.
type BackendRetrievalPlan struct {
	UseScan             bool
	FTSOnly             bool
	SharedIndexProvided bool
	CachedTotalDocs     int
}

// BackendRetrieval wires concrete backend callbacks into the generic
// first-stage retrieval runner.
type BackendRetrieval[Hit any, Index any] struct {
	Plan        BackendRetrievalPlan
	SharedIndex Index
	OpenLocal   FTSLocalOpener[Index]
	Query       FTSQuery[Index, Hit]
	Count       FTSCount[Index]
	Close       FTSClose[Index]
	Scan        BackendScan[Hit]
}

// BackendRetrievalResult is the effective first-stage backend state plus
// warning signals for the caller to render.
type BackendRetrievalResult[Hit any] struct {
	Hits             []Hit
	Scanned          int
	Mode             string
	FTSQueryErr      error
	WarnMissingIndex bool
	WarnQueryError   bool
	Degraded         bool
}

// RunBackendRetrieval preserves docs-puller's first-stage retrieval order:
// try FTS unless scan is forced, open/retry a local index when needed, query
// the selected index, close owned local handles, and fall back to scan or
// degraded ftsOnly output using the existing fallback planner.
func RunBackendRetrieval[Hit any, Index any](input BackendRetrieval[Hit, Index]) BackendRetrievalResult[Hit] {
	result := BackendRetrievalResult[Hit]{}
	ftsOpened := false
	ftsPlan := PlanFTSIndex(FTSIndexPlan{
		UseScan:             input.Plan.UseScan,
		SharedIndexProvided: input.Plan.SharedIndexProvided,
	})
	if ftsPlan.TryFTS {
		idx := input.SharedIndex
		var localIdx Index
		localOpened := false
		if ftsPlan.OpenLocal {
			openPlan := PlanFTSOpen(FTSOpenPlan{FTSOnly: input.Plan.FTSOnly})
			for i := 0; i < openPlan.Attempts; i++ {
				var opened Index
				openedOK := false
				if input.OpenLocal != nil {
					opened, openedOK = input.OpenLocal()
				}
				attempt := PlanFTSOpenAttempt(FTSOpenAttemptPlan{
					Attempt:  i,
					Attempts: openPlan.Attempts,
					Opened:   openedOK,
				})
				if attempt.UseOpened {
					localIdx = opened
					localOpened = true
					break
				}
				if attempt.SleepBeforeRetry {
					time.Sleep(openPlan.RetryDelay)
				}
			}
		}
		indexSelection := PlanFTSIndexSelection(FTSIndexSelectionPlan{
			SharedIndexProvided: input.Plan.SharedIndexProvided,
			LocalIndexOpened:    localOpened,
		})
		if indexSelection.UseLocalIndex {
			idx = localIdx
		}
		if indexSelection.UseIndex {
			var hits []Hit
			if input.Query == nil {
				result.FTSQueryErr = errFTSQueryFunctionRequired
			} else {
				var err error
				hits, err = input.Query(idx)
				result.FTSQueryErr = err
			}
			queryOutcome := PlanFTSQueryOutcome(FTSQueryOutcomePlan{
				IndexAvailable:  true,
				QueryErrored:    result.FTSQueryErr != nil,
				CachedTotalDocs: input.Plan.CachedTotalDocs,
			})
			ftsOpened = queryOutcome.IndexOpened
			if queryOutcome.UseHits {
				totalDocs := queryOutcome.Scanned
				if !queryOutcome.UseCachedScanned {
					if input.Count == nil {
						result.FTSQueryErr = errFTSCountFunctionRequired
					} else {
						totalDocs = input.Count(idx)
						result.Hits, result.Scanned, result.Mode = hits, totalDocs, queryOutcome.Mode
					}
				} else {
					result.Hits, result.Scanned, result.Mode = hits, totalDocs, queryOutcome.Mode
				}
			}
			if indexSelection.OwnsIndex && input.Close != nil {
				input.Close(idx)
			}
		}
	}
	fallback := PlanBackendFallback(BackendFallbackPlan{
		UseScan:             input.Plan.UseScan,
		FTSOnly:             input.Plan.FTSOnly,
		SharedIndexProvided: input.Plan.SharedIndexProvided,
		IndexAvailable:      ftsOpened,
		QueryErrored:        result.FTSQueryErr != nil,
		Mode:                result.Mode,
	})
	result.WarnMissingIndex = fallback.WarnMissingIndex
	result.WarnQueryError = fallback.WarnQueryError
	if fallback.ReturnDegraded {
		result.Degraded = true
		return result
	}
	if fallback.RunScan {
		if input.Scan == nil {
			result.FTSQueryErr = errScanFunctionRequired
			result.WarnQueryError = true
			return result
		}
		result.Hits, result.Scanned = input.Scan()
		result.Mode = "scan"
	}
	return result
}

// StrictProfileFilterPlan captures whether dispatch should hard-filter hits
// after profile membership has been annotated. The caller still owns profile
// loading, membership checks, and the actual hit filtering.
type StrictProfileFilterPlan struct {
	Strict        bool
	ProfileActive bool
}

// StrictProfileFilterDecision tells the caller whether strict profile
// filtering should be applied.
type StrictProfileFilterDecision struct {
	Apply bool
}

// PlanStrictProfileFilter preserves docs-puller's --strict behavior:
// strict only has an effect when an active profile was successfully loaded.
func PlanStrictProfileFilter(plan StrictProfileFilterPlan) StrictProfileFilterDecision {
	return StrictProfileFilterDecision{Apply: plan.Strict && plan.ProfileActive}
}

// ProfileProcessingPlan captures the dispatch decisions that depend on an
// active profile. The caller still owns profile loading, annotation, and
// filtering.
type ProfileProcessingPlan struct {
	Strict        bool
	ProfileActive bool
}

// ProfileProcessingDecision tells the caller which profile post-processing
// steps should run.
type ProfileProcessingDecision struct {
	Annotate          bool
	ApplyStrictFilter bool
}

// PlanProfileProcessing keeps profile annotation and strict filtering aligned:
// profile metadata is only meaningful when a profile is active, and strict
// filtering only applies after that annotation step.
func PlanProfileProcessing(plan ProfileProcessingPlan) ProfileProcessingDecision {
	strict := PlanStrictProfileFilter(StrictProfileFilterPlan{
		Strict:        plan.Strict,
		ProfileActive: plan.ProfileActive,
	})
	return ProfileProcessingDecision{
		Annotate:          plan.ProfileActive,
		ApplyStrictFilter: strict.Apply,
	}
}

// HitProcessor applies one post-processing pass to a hit set.
type HitProcessor[Hit any] func([]Hit) []Hit

// ApplyProfileProcessing preserves docs-puller's profile post-processing
// sequence: annotate profile membership first, then apply strict filtering
// only after annotation.
func ApplyProfileProcessing[Hit any](
	hits []Hit,
	decision ProfileProcessingDecision,
	annotate HitProcessor[Hit],
	strictFilter HitProcessor[Hit],
) []Hit {
	if decision.Annotate && annotate != nil {
		hits = annotate(hits)
	}
	if decision.ApplyStrictFilter && strictFilter != nil {
		hits = strictFilter(hits)
	}
	return hits
}

// SourceHygieneWindowPlan captures the already-sorted source-hygiene penalties
// plus the user-facing result limit. The caller still owns penalty scoring,
// stable sorting, and hit slicing.
type SourceHygieneWindowPlan struct {
	UserLimit int
	Penalties []int
}

// SourceHygieneWindowDecision tells the caller how many sorted hits to keep.
type SourceHygieneWindowDecision struct {
	Limit int
}

// PlanSourceHygieneWindow preserves docs-puller's source-hygiene window rule:
// when durable hits exist before the first penalized hit, only durable hits are
// usable; otherwise keep the normal limit window. A non-positive or oversized
// user limit expands to the usable window.
func PlanSourceHygieneWindow(plan SourceHygieneWindowPlan) SourceHygieneWindowDecision {
	usable := len(plan.Penalties)
	for i, penalty := range plan.Penalties {
		if penalty != 0 {
			if i > 0 {
				usable = i
			}
			break
		}
	}
	limit := plan.UserLimit
	if limit <= 0 || limit > usable {
		limit = usable
	}
	return SourceHygieneWindowDecision{Limit: limit}
}

type sourceHygieneRankedItem[T any] struct {
	item    T
	penalty int
	index   int
}

// ApplySourceHygiene applies source-hygiene penalty ordering and the
// post-sort result window to arbitrary hit values. The caller owns penalty
// scoring so this package stays independent of path/URL-specific rules.
func ApplySourceHygiene[T any](items []T, userLimit int, penalty func(T) int) []T {
	if len(items) == 0 {
		return items
	}
	ranked := make([]sourceHygieneRankedItem[T], 0, len(items))
	for i, item := range items {
		ranked = append(ranked, sourceHygieneRankedItem[T]{
			item:    item,
			penalty: penalty(item),
			index:   i,
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].penalty != ranked[j].penalty {
			return ranked[i].penalty < ranked[j].penalty
		}
		return ranked[i].index < ranked[j].index
	})
	penalties := make([]int, 0, len(ranked))
	for _, item := range ranked {
		penalties = append(penalties, item.penalty)
	}
	window := PlanSourceHygieneWindow(SourceHygieneWindowPlan{
		UserLimit: userLimit,
		Penalties: penalties,
	})
	out := make([]T, 0, window.Limit)
	for _, item := range ranked[:window.Limit] {
		out = append(out, item.item)
	}
	return out
}

// VersionPolicyProcessingPlan captures the dispatch decisions that depend on
// a resolved version-search policy. The caller still owns policy resolution,
// matching, scoring, and filtering.
type VersionPolicyProcessingPlan struct {
	PolicyActive bool
	SourceID     string
	LatestOnly   bool
	Version      string
}

// VersionPolicyProcessingDecision tells the caller which version-policy passes
// should run.
type VersionPolicyProcessingDecision struct {
	ApplyPreRerank        bool
	ApplyPostRerankFilter bool
}

// PlanVersionPolicyProcessing keeps the two version-policy passes aligned:
// active policies run the pre-rerank scoring/filtering pass, while only hard
// version constraints run the order-preserving post-rerank filter.
func PlanVersionPolicyProcessing(plan VersionPolicyProcessingPlan) VersionPolicyProcessingDecision {
	post := PlanPostRerankVersionFilter(PostRerankVersionFilterPlan{
		PolicyActive: plan.PolicyActive,
		SourceID:     plan.SourceID,
		LatestOnly:   plan.LatestOnly,
		Version:      plan.Version,
	})
	return VersionPolicyProcessingDecision{
		ApplyPreRerank:        plan.PolicyActive,
		ApplyPostRerankFilter: post.Apply,
	}
}

// PostRerankVersionFilterPlan captures the hard version constraint state after
// reranking. The caller still owns version-policy matching and hit filtering.
type PostRerankVersionFilterPlan struct {
	PolicyActive bool
	SourceID     string
	LatestOnly   bool
	Version      string
}

// PostRerankVersionFilterDecision tells the caller whether to run the
// order-preserving version filter after rerank/hybrid expansion.
type PostRerankVersionFilterDecision struct {
	Apply bool
}

// PlanPostRerankVersionFilter preserves docs-puller's rerank-safe version
// policy rule: only hard version constraints need the post-rerank filter;
// soft policies are handled by the pre-rerank scoring/filtering pass.
func PlanPostRerankVersionFilter(plan PostRerankVersionFilterPlan) PostRerankVersionFilterDecision {
	return PostRerankVersionFilterDecision{
		Apply: plan.PolicyActive && (plan.SourceID != "" || plan.LatestOnly || plan.Version != ""),
	}
}

// PostRerankVersionFilterApplication captures the current hit set and the
// already-filtered hit set after the caller has run version-policy matching.
type PostRerankVersionFilterApplication[Hit any] struct {
	Hits         []Hit
	FilteredHits []Hit
	Decision     PostRerankVersionFilterDecision
}

// ApplyPostRerankVersionFilter preserves docs-puller's assignment behavior:
// only hard post-rerank version constraints replace the current hit set.
func ApplyPostRerankVersionFilter[Hit any](plan PostRerankVersionFilterApplication[Hit]) []Hit {
	if plan.Decision.Apply {
		return plan.FilteredHits
	}
	return plan.Hits
}

// RerankStagePlan captures the dispatch decision for the optional hybrid
// retrieval and rerank stages. The caller still owns confidence scoring,
// candidate retrieval, and the actual reranker implementation.
type RerankStagePlan struct {
	Rerank        bool
	RerankLLM     bool
	RerankHybrid  bool
	BM25Confident bool
	HitCount      int
}

// RerankStageDecision tells the caller which rerank-adjacent stages to run.
type RerankStageDecision struct {
	ApplyHybrid bool
	ApplyRerank bool
}

// PlanRerankStages preserves docs-puller's historical rerank flow:
// hybrid expansion runs before rerank when reranking is requested and BM25
// is not confident; the reranker itself additionally requires at least two
// candidates after any hybrid expansion.
func PlanRerankStages(plan RerankStagePlan) RerankStageDecision {
	want := plan.Rerank || plan.RerankLLM
	if !want || plan.BM25Confident {
		return RerankStageDecision{}
	}
	return RerankStageDecision{
		ApplyHybrid: plan.RerankHybrid,
		ApplyRerank: plan.HitCount > 1,
	}
}

// HybridOutcomePlan captures the result of a hybrid retrieval attempt. The
// caller still owns the retrieval call, warning text, and hit mutation.
type HybridOutcomePlan struct {
	HybridErrored bool
}

// HybridOutcomeDecision tells the caller whether to keep expanded hybrid hits
// or warn and preserve the existing BM25 candidates.
type HybridOutcomeDecision struct {
	UseExpandedHits bool
	WarnFallback    bool
	HybridApplied   bool
}

// PlanHybridOutcome preserves docs-puller's hybrid fallback behavior: failed
// hybrid retrieval keeps the BM25 candidate pool, while successful retrieval
// replaces the candidates and marks hybrid as applied for downstream mode tags.
func PlanHybridOutcome(plan HybridOutcomePlan) HybridOutcomeDecision {
	if plan.HybridErrored {
		return HybridOutcomeDecision{WarnFallback: true}
	}
	return HybridOutcomeDecision{UseExpandedHits: true, HybridApplied: true}
}

// RerankCallPlan captures which reranker implementation dispatch should call
// after stage planning has already decided rerank should run.
type RerankCallPlan struct {
	Rerank    bool
	RerankLLM bool
}

// RerankCallDecision tells the caller which reranker implementation to invoke.
type RerankCallDecision struct {
	UseLLM       bool
	UseEmbedding bool
}

// PlanRerankCall preserves docs-puller's existing reranker precedence: the
// LLM cross-encoder wins when requested; otherwise explicit embedding rerank
// uses the embedding reranker.
func PlanRerankCall(plan RerankCallPlan) RerankCallDecision {
	if plan.RerankLLM {
		return RerankCallDecision{UseLLM: true}
	}
	if plan.Rerank {
		return RerankCallDecision{UseEmbedding: true}
	}
	return RerankCallDecision{}
}

// RerankModeTagPlan captures the small mode-label decision after a reranker
// succeeds. The caller owns the reranker call and only applies this on success.
type RerankModeTagPlan struct {
	RerankLLM       bool
	RerankChunkSize int
	HybridApplied   bool
}

// PlanRerankModeTag preserves docs-puller's existing mode suffixes:
// `llm`, `embed`, `embed-chunked-N`, optionally prefixed with `hybrid-`.
func PlanRerankModeTag(plan RerankModeTagPlan) string {
	tag := "embed"
	if plan.RerankLLM {
		tag = "llm"
	} else if plan.RerankChunkSize > 0 {
		tag = "embed-chunked-" + strconv.Itoa(plan.RerankChunkSize)
	}
	if plan.HybridApplied {
		tag = "hybrid-" + tag
	}
	return tag
}

// RerankOutcomePlan captures the dispatch outcome after a reranker attempt.
// The caller still owns the reranker call, warning text, and hit mutation.
type RerankOutcomePlan struct {
	RerankErrored   bool
	BaseMode        string
	RerankLLM       bool
	RerankChunkSize int
	HybridApplied   bool
}

// RerankOutcomeDecision tells the caller whether to keep reranked hits, warn
// about fallback, and which mode string to expose.
type RerankOutcomeDecision struct {
	UseRerankedHits bool
	WarnFallback    bool
	Mode            string
}

// PlanRerankOutcome preserves docs-puller's rerank outcome behavior: failed
// rerank attempts fall back to the prior mode/hits, while successful attempts
// append the rerank mode suffix.
func PlanRerankOutcome(plan RerankOutcomePlan) RerankOutcomeDecision {
	if plan.RerankErrored {
		return RerankOutcomeDecision{WarnFallback: true, Mode: plan.BaseMode}
	}
	tag := PlanRerankModeTag(RerankModeTagPlan{
		RerankLLM:       plan.RerankLLM,
		RerankChunkSize: plan.RerankChunkSize,
		HybridApplied:   plan.HybridApplied,
	})
	return RerankOutcomeDecision{
		UseRerankedHits: true,
		Mode:            plan.BaseMode + "+rerank-" + tag,
	}
}

// RerankOutcomeApplication captures the current and reranked hit sets after a
// rerank attempt has been planned. The caller still owns warning text.
type RerankOutcomeApplication[Hit any] struct {
	CurrentHits  []Hit
	RerankedHits []Hit
	CurrentMode  string
	Outcome      RerankOutcomeDecision
}

// RerankOutcomeApplicationResult is the effective dispatch state after the
// planned rerank outcome has been applied.
type RerankOutcomeApplicationResult[Hit any] struct {
	Hits         []Hit
	Mode         string
	WarnFallback bool
}

// ApplyRerankOutcome preserves docs-puller's post-rerank assignment behavior:
// failed rerank attempts keep the existing hits/mode and only warn, while
// successful attempts replace hits and mode with the reranked values.
func ApplyRerankOutcome[Hit any](plan RerankOutcomeApplication[Hit]) RerankOutcomeApplicationResult[Hit] {
	result := RerankOutcomeApplicationResult[Hit]{
		Hits:         plan.CurrentHits,
		Mode:         plan.CurrentMode,
		WarnFallback: plan.Outcome.WarnFallback,
	}
	if plan.Outcome.UseRerankedHits {
		result.Hits = plan.RerankedHits
		result.Mode = plan.Outcome.Mode
	}
	return result
}

var (
	errHybridFunctionRequired         = errors.New("searchdispatch: hybrid retrieval function is required")
	errEmbeddingRerankFunctionMissing = errors.New("searchdispatch: embedding rerank function is required")
	errLLMRerankFunctionMissing       = errors.New("searchdispatch: LLM rerank function is required")
)

// HitTransform is a pipeline callback that can replace the current hit set.
type HitTransform[Hit any] func([]Hit) ([]Hit, error)

// RerankProcessingPlan captures the flags needed to run hybrid retrieval and
// rerank after first-stage retrieval has produced a candidate set.
type RerankProcessingPlan struct {
	Rerank          bool
	RerankLLM       bool
	RerankHybrid    bool
	RerankChunkSize int
	BM25Confident   bool
}

// RerankProcessing captures the current hit state plus the concrete callbacks
// owned by the caller. The caller still owns query/options binding and warning
// text; this package owns the stage order and fallback assignments.
type RerankProcessing[Hit any] struct {
	Hits            []Hit
	Mode            string
	Plan            RerankProcessingPlan
	Hybrid          HitTransform[Hit]
	EmbeddingRerank HitTransform[Hit]
	LLMRerank       HitTransform[Hit]
}

// RerankProcessingResult is the effective hit state after hybrid/rerank
// processing, plus warning signals for the caller to render.
type RerankProcessingResult[Hit any] struct {
	Hits               []Hit
	Mode               string
	HybridApplied      bool
	HybridErr          error
	RerankErr          error
	WarnHybridFallback bool
	WarnRerankFallback bool
}

// RunRerankProcessing preserves docs-puller's hybrid/rerank stage order:
// plan hybrid from the original BM25 confidence and hit count, optionally
// expand candidates, then re-plan rerank from the post-hybrid hit count while
// keeping the same BM25 confidence gate.
func RunRerankProcessing[Hit any](input RerankProcessing[Hit]) RerankProcessingResult[Hit] {
	result := RerankProcessingResult[Hit]{
		Hits: input.Hits,
		Mode: input.Mode,
	}
	rerankPlan := PlanRerankStages(RerankStagePlan{
		Rerank:        input.Plan.Rerank,
		RerankLLM:     input.Plan.RerankLLM,
		RerankHybrid:  input.Plan.RerankHybrid,
		BM25Confident: input.Plan.BM25Confident,
		HitCount:      len(result.Hits),
	})
	if rerankPlan.ApplyHybrid {
		expanded, err := runHitTransform(input.Hybrid, result.Hits, errHybridFunctionRequired)
		hybridOutcome := PlanHybridOutcome(HybridOutcomePlan{HybridErrored: err != nil})
		if hybridOutcome.WarnFallback {
			result.HybridErr = err
			result.WarnHybridFallback = true
		}
		if hybridOutcome.UseExpandedHits {
			result.Hits = expanded
			result.HybridApplied = hybridOutcome.HybridApplied
		}
	}

	rerankPlan = PlanRerankStages(RerankStagePlan{
		Rerank:        input.Plan.Rerank,
		RerankLLM:     input.Plan.RerankLLM,
		RerankHybrid:  input.Plan.RerankHybrid,
		BM25Confident: input.Plan.BM25Confident,
		HitCount:      len(result.Hits),
	})
	if !rerankPlan.ApplyRerank {
		return result
	}

	var (
		reranked []Hit
		err      error
	)
	rerankCall := PlanRerankCall(RerankCallPlan{
		Rerank:    input.Plan.Rerank,
		RerankLLM: input.Plan.RerankLLM,
	})
	if rerankCall.UseLLM {
		reranked, err = runHitTransform(input.LLMRerank, result.Hits, errLLMRerankFunctionMissing)
	} else if rerankCall.UseEmbedding {
		reranked, err = runHitTransform(input.EmbeddingRerank, result.Hits, errEmbeddingRerankFunctionMissing)
	}
	outcome := PlanRerankOutcome(RerankOutcomePlan{
		RerankErrored:   err != nil,
		BaseMode:        result.Mode,
		RerankLLM:       input.Plan.RerankLLM,
		RerankChunkSize: input.Plan.RerankChunkSize,
		HybridApplied:   result.HybridApplied,
	})
	applied := ApplyRerankOutcome(RerankOutcomeApplication[Hit]{
		CurrentHits:  result.Hits,
		RerankedHits: reranked,
		CurrentMode:  result.Mode,
		Outcome:      outcome,
	})
	if applied.WarnFallback {
		result.RerankErr = err
		result.WarnRerankFallback = true
	}
	result.Hits = applied.Hits
	result.Mode = applied.Mode
	return result
}

func runHitTransform[Hit any](transform HitTransform[Hit], hits []Hit, missingErr error) ([]Hit, error) {
	if transform == nil {
		return nil, missingErr
	}
	return transform(hits)
}

// BM25ConfidenceFunc reports whether the current BM25-ordered hits are
// confident enough to skip hybrid/rerank.
type BM25ConfidenceFunc[Hit any] func([]Hit) bool

// PostRetrievalProcessingPlan captures the optional post-backend stages that
// run before final output shaping.
type PostRetrievalProcessingPlan struct {
	VersionPolicy   VersionPolicyProcessingPlan
	Profile         ProfileProcessingPlan
	Rerank          bool
	RerankLLM       bool
	RerankHybrid    bool
	RerankChunkSize int
}

// PostRetrievalProcessing wires concrete post-backend callbacks into the
// generic sequencing runner. The caller owns query/options binding, matching,
// scoring, concrete rerank calls, warning text, and source-specific hygiene
// penalties.
type PostRetrievalProcessing[Hit any] struct {
	Hits                    []Hit
	Mode                    string
	Plan                    PostRetrievalProcessingPlan
	ApplyVersionPolicy      HitProcessor[Hit]
	AnnotateProfile         HitProcessor[Hit]
	StrictProfileFilter     HitProcessor[Hit]
	BM25Confident           BM25ConfidenceFunc[Hit]
	Hybrid                  HitTransform[Hit]
	EmbeddingRerank         HitTransform[Hit]
	LLMRerank               HitTransform[Hit]
	PostRerankVersionFilter HitProcessor[Hit]
	SourceHygiene           HitProcessor[Hit]
}

// PostRetrievalProcessingResult is the effective hit state after optional
// post-backend processing, plus warning signals for the caller to render.
type PostRetrievalProcessingResult[Hit any] struct {
	Hits               []Hit
	Mode               string
	HybridErr          error
	RerankErr          error
	WarnHybridFallback bool
	WarnRerankFallback bool
}

// RunPostRetrievalProcessing preserves docs-puller's post-backend order:
// version policy first, profile annotation/filtering, BM25 confidence gate,
// optional hybrid/rerank, hard post-rerank version filter, then source hygiene.
func RunPostRetrievalProcessing[Hit any](input PostRetrievalProcessing[Hit]) PostRetrievalProcessingResult[Hit] {
	result := PostRetrievalProcessingResult[Hit]{
		Hits: input.Hits,
		Mode: input.Mode,
	}
	versionPlan := PlanVersionPolicyProcessing(input.Plan.VersionPolicy)
	if versionPlan.ApplyPreRerank && input.ApplyVersionPolicy != nil {
		result.Hits = input.ApplyVersionPolicy(result.Hits)
	}
	profilePlan := PlanProfileProcessing(input.Plan.Profile)
	result.Hits = ApplyProfileProcessing(result.Hits, profilePlan, input.AnnotateProfile, input.StrictProfileFilter)

	bm25Confident := false
	if input.BM25Confident != nil {
		bm25Confident = input.BM25Confident(result.Hits)
	}
	rerankResult := RunRerankProcessing(RerankProcessing[Hit]{
		Hits: result.Hits,
		Mode: result.Mode,
		Plan: RerankProcessingPlan{
			Rerank:          input.Plan.Rerank,
			RerankLLM:       input.Plan.RerankLLM,
			RerankHybrid:    input.Plan.RerankHybrid,
			RerankChunkSize: input.Plan.RerankChunkSize,
			BM25Confident:   bm25Confident,
		},
		Hybrid:          input.Hybrid,
		EmbeddingRerank: input.EmbeddingRerank,
		LLMRerank:       input.LLMRerank,
	})
	result.Hits = rerankResult.Hits
	result.Mode = rerankResult.Mode
	result.HybridErr = rerankResult.HybridErr
	result.RerankErr = rerankResult.RerankErr
	result.WarnHybridFallback = rerankResult.WarnHybridFallback
	result.WarnRerankFallback = rerankResult.WarnRerankFallback

	if versionPlan.ApplyPostRerankFilter && input.PostRerankVersionFilter != nil {
		result.Hits = input.PostRerankVersionFilter(result.Hits)
	}
	if input.SourceHygiene != nil {
		result.Hits = input.SourceHygiene(result.Hits)
	}
	return result
}

// BM25ConfidencePlan captures the score-only decision used to skip expensive
// hybrid/rerank stages when BM25 has a clear winner.
type BM25ConfidencePlan struct {
	Gate        float64
	HitCount    int
	TopScore    int
	SecondScore int
}

// PlanBM25Confidence reports whether the top BM25 score beats the second by
// at least Gate fraction relative to the top score.
func PlanBM25Confidence(plan BM25ConfidencePlan) bool {
	if plan.Gate <= 0 || plan.HitCount < 2 || plan.TopScore <= 0 {
		return false
	}
	gap := float64(plan.TopScore-plan.SecondScore) / float64(plan.TopScore)
	return gap >= plan.Gate
}

// CompactOutputPresetPlan captures the --compact preset values after flag
// parsing. The caller still owns option mutation and user-facing flag defaults.
type CompactOutputPresetPlan struct {
	Compact            bool
	SnippetLen         int
	MaxSnippets        int
	DefaultSnippetLen  int
	DefaultMaxSnippets int
}

// CompactOutputPresetDecision returns the effective snippet knobs after the
// compact preset is applied. Explicit user values win over compact defaults.
type CompactOutputPresetDecision struct {
	SnippetLen  int
	MaxSnippets int
}

// PlanCompactOutputPreset preserves docs-puller's --compact behavior:
// compact fills unset snippet knobs, but does not override explicit values.
func PlanCompactOutputPreset(plan CompactOutputPresetPlan) CompactOutputPresetDecision {
	decision := CompactOutputPresetDecision{
		SnippetLen:  plan.SnippetLen,
		MaxSnippets: plan.MaxSnippets,
	}
	if !plan.Compact {
		return decision
	}
	if decision.SnippetLen == 0 {
		decision.SnippetLen = plan.DefaultSnippetLen
	}
	if decision.MaxSnippets == 0 {
		decision.MaxSnippets = plan.DefaultMaxSnippets
	}
	return decision
}

// OutputOverridePlan captures the user-facing output shaping flags. The caller
// still owns hit mutation, file reads, and snippet extraction.
type OutputOverridePlan struct {
	Compact     bool
	NoSnippets  bool
	MaxSnippets int
	SnippetLen  int
}

// OutputOverrideDecision tells the caller which post-processing actions are
// needed before emitting search hits.
type OutputOverrideDecision struct {
	Apply          bool
	DropSnippets   bool
	RetuneSnippets bool
	DropURL        bool
}

// PlanOutputOverrides preserves the existing precedence: --no-snippets wins
// over snippet retuning, while --compact independently drops URLs.
func PlanOutputOverrides(plan OutputOverridePlan) OutputOverrideDecision {
	needRetune := plan.MaxSnippets > 0 || plan.SnippetLen > 0
	decision := OutputOverrideDecision{
		DropSnippets:   plan.NoSnippets,
		RetuneSnippets: !plan.NoSnippets && needRetune,
		DropURL:        plan.Compact,
	}
	decision.Apply = decision.DropSnippets || decision.RetuneSnippets || decision.DropURL
	return decision
}
