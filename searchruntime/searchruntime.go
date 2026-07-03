// Package searchruntime exposes docs-puller's importable in-process search
// adapter. During M7+2 it owns the searchcore.Query-to-dispatch caller
// boundary while the concrete dispatch engine continues to move out of
// cmd/docs-puller's package main.
package searchruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nstranquist/docs-puller/internal/apppaths"
	"github.com/nstranquist/docs-puller/internal/searchdispatch"
	"github.com/nstranquist/docs-puller/internal/sourcehygiene"
	"github.com/nstranquist/docs-puller/searchcore"
)

// Searcher is the canonical public search contract re-exported for callers
// that select docs-puller's importable runtime package.
type Searcher = searchcore.Searcher

// Options is the stable subset of docs-puller search options that a
// searchcore.Query can override at the importable caller boundary.
type Options struct {
	Limit     int
	Source    string
	RerankLLM bool
}

// Request is the dispatch-facing request shape emitted by this package.
type Request struct {
	Query   string
	Options Options
}

// DispatchRequest is the runtime-facing structured dispatch input used while
// package-main options and index handles continue to move behind this package.
type DispatchRequest[Opts any, Shared any] = searchdispatch.Request[Opts, Shared]

// DispatchResult is the runtime-facing structured dispatch output used while
// concrete dispatch behavior continues to move behind this package.
type DispatchResult[Hit any] = searchdispatch.Result[Hit]

// Hit is the docs-puller runtime search result shape. It intentionally keeps
// fields that are useful to the CLI pipeline even when they are not part of
// the public searchcore contract.
type Hit struct {
	Path                string    `json:"path"`
	Source              string    `json:"source"`
	SourceFamily        string    `json:"source_family,omitempty"`
	SourceID            string    `json:"source_id,omitempty"`
	DocsVersion         string    `json:"docs_version,omitempty"`
	VersionLane         string    `json:"version_lane,omitempty"`
	PinScope            string    `json:"pin_scope,omitempty"`
	Title               string    `json:"title,omitempty"`
	URL                 string    `json:"url,omitempty"`
	Score               int       `json:"score"`
	InProfile           bool      `json:"in_profile,omitempty"`
	InProfileSub        bool      `json:"in_profile_sub,omitempty"`
	Snippets            []Snippet `json:"snippets,omitempty"`
	VersionScoreApplied bool      `json:"-"`
}

// Snippet is a line-level docs-puller runtime excerpt.
type Snippet struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// DispatchFunc is supplied by the package that owns the concrete docs-puller
// search engine.
type DispatchFunc func(context.Context, Request) ([]Hit, error)

// DispatchEngineFunc is supplied by the package that owns a concrete
// docs-puller dispatch engine. It accepts the structured dispatch request used
// while the engine is still being extracted from package main.
type DispatchEngineFunc[Opts any, Shared any] func(context.Context, DispatchRequest[Opts, Shared]) DispatchResult[Hit]

// DispatchEngineConfig wires an importable searchcore.Searcher constructor to
// a concrete dispatch engine's option and shared-index types.
type DispatchEngineConfig[Opts any, Shared any] struct {
	BaseOptions   Options
	EngineOptions Opts
	SharedIndex   Shared
	ApplyOptions  func(Opts, Options) Opts
	Dispatch      DispatchEngineFunc[Opts, Shared]
}

// NewSearcher returns a searchcore.Searcher that maps Query overrides onto the
// provided base options before calling dispatch.
func NewSearcher(base Options, dispatch DispatchFunc) searchcore.Searcher {
	if dispatch == nil {
		return searchcore.NewInProcessSearcher(nil)
	}
	return searchcore.NewInProcessSearcher(func(ctx context.Context, q searchcore.Query) ([]searchcore.Hit, error) {
		opts := base
		if q.Limit > 0 {
			opts.Limit = q.Limit
		}
		if q.Source != "" {
			opts.Source = q.Source
		}
		if q.Rerank {
			opts.RerankLLM = true
		}
		hits, err := dispatch(ctx, Request{
			Query:   q.Text,
			Options: opts,
		})
		if err != nil {
			return nil, err
		}
		return HitsToCore(hits), nil
	})
}

// NewDispatchEngineSearcher returns an importable searchcore.Searcher for a
// concrete docs-puller dispatch engine. Callers provide the engine-owned option
// type plus an adapter that applies the public Query overrides.
func NewDispatchEngineSearcher[Opts any, Shared any](cfg DispatchEngineConfig[Opts, Shared]) searchcore.Searcher {
	if cfg.Dispatch == nil {
		return searchcore.NewInProcessSearcher(nil)
	}
	return NewSearcher(cfg.BaseOptions, func(ctx context.Context, req Request) ([]Hit, error) {
		opts := cfg.EngineOptions
		if cfg.ApplyOptions != nil {
			opts = cfg.ApplyOptions(opts, req.Options)
		}
		result := cfg.Dispatch(ctx, DispatchRequest[Opts, Shared]{
			Query:     req.Query,
			Opts:      opts,
			SharedIdx: cfg.SharedIndex,
		})
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result.Hits, nil
	})
}

// BM25IsConfident reports whether BM25's top-1 score exceeds top-2 by at
// least gate fraction relative to top-1. When true, the BM25 ranking is
// considered authoritative enough to skip rerank.
func BM25IsConfident(hits []Hit, gate float64) bool {
	var topScore, secondScore int
	if len(hits) > 0 {
		topScore = hits[0].Score
	}
	if len(hits) > 1 {
		secondScore = hits[1].Score
	}
	return searchdispatch.PlanBM25Confidence(searchdispatch.BM25ConfidencePlan{
		Gate:        gate,
		HitCount:    len(hits),
		TopScore:    topScore,
		SecondScore: secondScore,
	})
}

// SourceHygienePenalty returns a non-negative downranking penalty for a hit.
type SourceHygienePenalty func(Hit) int

// ExpandedSourceHygieneLimit returns the candidate window needed for source
// hygiene to downrank lower-value generated/reference pages.
func ExpandedSourceHygieneLimit(limit int) int {
	return sourcehygiene.ExpandedLimit(limit)
}

// DefaultSourceHygienePenalty returns docs-puller's standard source hygiene
// penalty for a runtime hit.
func DefaultSourceHygienePenalty(hit Hit) int {
	return sourcehygiene.Penalty(hit.Path, hit.URL)
}

// ProfileMatcher is the profile membership contract needed by the runtime
// annotation step.
type ProfileMatcher interface {
	Match(source, relPath string) (in bool, sub bool)
}

// ProfileResolveOptions is the runtime-neutral profile-precedence request
// shape passed to the caller-owned resolver.
type ProfileResolveOptions struct {
	FlagProfile   string
	FlagNoProfile bool
	OutDir        string
	Cwd           string
}

// ProfileSelectionInput contains the callbacks needed to resolve and load an
// active profile without importing the command package's concrete profile type.
type ProfileSelectionInput[Profile any] struct {
	Options                  ProfileResolveOptions
	Strict                   bool
	Resolve                  func(ProfileResolveOptions) (name, reason string)
	Load                     func(name, outDir string) (Profile, error)
	WarnLoadFailed           func(name string, err error)
	WarnStrictWithoutProfile func()
}

// ProfileSelectionResult is the resolved profile state applied to search
// options before dispatch.
type ProfileSelectionResult[Profile any] struct {
	Name    string
	Reason  string
	Profile Profile
	Loaded  bool
}

// VersionPolicyOptions captures the version-policy state needed by the
// runtime post-retrieval pipeline.
type VersionPolicyOptions struct {
	Active     bool
	SourceID   string
	LatestOnly bool
	Version    string
}

// VersionPolicyState captures the runtime-visible version policy fields used
// by pre-retrieval planning and output headers.
type VersionPolicyState struct {
	SourceFamily string
	SourceID     string
	Version      string
	LatestOnly   bool
	CwdScope     string
	PreferLatest bool
}

// VersionPolicySourceInfo is the runtime-visible version metadata attached to
// a hit after caller-owned source registry enrichment.
type VersionPolicySourceInfo struct {
	SourceFamily string
	SourceID     string
	DocsVersion  string
	VersionLane  string
	PinScope     string
}

// VersionPolicySourceInfoResolveInput contains caller-owned source metadata
// lookups for resolving the runtime-visible source info attached to hits.
type VersionPolicySourceInfoResolveInput struct {
	Source              string
	LatestLane          string
	OtherPinnedLane     string
	PinnedSourceInfo    func(source string) (VersionPolicySourceInfo, bool)
	ParsePinnedSourceID func(source string) (family, version string, ok bool)
	LatestVersion       func(source string) (version string, ok bool)
}

// VersionPolicyFamilySource is the runtime-neutral identity of one indexed
// source for source-family fan-out.
type VersionPolicyFamilySource struct {
	Source       string
	SourceFamily string
}

// VersionPolicyHardFilterOptions adapts runtime-visible policy state into the
// hard-filter option shape used after rerank. Source-family-only and
// prefer-latest policies are intentionally soft and do not create hard filter
// constraints.
func VersionPolicyHardFilterOptions(state VersionPolicyState) VersionPolicyOptions {
	return VersionPolicyOptions{
		Active:     true,
		SourceID:   state.SourceID,
		LatestOnly: state.LatestOnly,
		Version:    state.Version,
	}
}

// ApplyVersionPolicySourceInfo copies caller-enriched source metadata onto a
// runtime hit before version matching, scoring, or filtering.
func ApplyVersionPolicySourceInfo(hit Hit, info VersionPolicySourceInfo) Hit {
	hit.SourceFamily = info.SourceFamily
	hit.SourceID = info.SourceID
	hit.DocsVersion = info.DocsVersion
	hit.VersionLane = info.VersionLane
	hit.PinScope = info.PinScope
	return hit
}

// ApplyAndMatchVersionPolicySourceInfo applies caller-enriched source metadata
// and reports whether the resulting hit satisfies the runtime policy state.
func ApplyAndMatchVersionPolicySourceInfo(hit Hit, info VersionPolicySourceInfo, state VersionPolicyState) (Hit, bool) {
	hit = ApplyVersionPolicySourceInfo(hit, info)
	if !VersionPolicyHitMatches(hit, state) {
		return hit, false
	}
	return hit, true
}

// ResolveVersionPolicySourceInfo applies docs-puller's source-info precedence
// rules over caller-owned pins and registry callbacks.
func ResolveVersionPolicySourceInfo(input VersionPolicySourceInfoResolveInput) VersionPolicySourceInfo {
	info := VersionPolicySourceInfo{
		SourceFamily: input.Source,
		SourceID:     input.Source,
		VersionLane:  input.LatestLane,
	}
	if input.PinnedSourceInfo != nil {
		if pinned, ok := input.PinnedSourceInfo(input.Source); ok {
			return pinned
		}
	}
	if input.ParsePinnedSourceID != nil {
		if family, version, ok := input.ParsePinnedSourceID(input.Source); ok {
			return VersionPolicySourceInfo{
				SourceFamily: family,
				SourceID:     input.Source,
				DocsVersion:  version,
				VersionLane:  input.OtherPinnedLane,
			}
		}
	}
	if input.LatestVersion != nil {
		if version, ok := input.LatestVersion(input.Source); ok {
			info.DocsVersion = version
		}
	}
	return info
}

// VersionPolicyFamilySourceIDs returns sorted source IDs matching a source
// family. Concrete source discovery and registry enrichment stay in the caller.
func VersionPolicyFamilySourceIDs(sourceFamily string, sources []VersionPolicyFamilySource) []string {
	if sourceFamily == "" {
		return nil
	}
	var out []string
	for _, source := range sources {
		if source.SourceFamily == sourceFamily {
			out = append(out, source.Source)
		}
	}
	sort.Strings(out)
	return out
}

// VersionPolicyFamilySourceIDsFromSources maps source IDs through a
// caller-owned metadata lookup, then returns the sorted IDs matching a source
// family.
func VersionPolicyFamilySourceIDsFromSources(sourceFamily string, sources []string, sourceInfo func(source string) VersionPolicySourceInfo) []string {
	if sourceFamily == "" {
		return nil
	}
	familySources := make([]VersionPolicyFamilySource, 0, len(sources))
	for _, source := range sources {
		info := VersionPolicySourceInfo{SourceFamily: source}
		if sourceInfo != nil {
			info = sourceInfo(source)
		}
		familySources = append(familySources, VersionPolicyFamilySource{
			Source:       source,
			SourceFamily: info.SourceFamily,
		})
	}
	return VersionPolicyFamilySourceIDs(sourceFamily, familySources)
}

// ParseVersionPolicyPinnedSourceID parses a version-pinned source ID of the
// form "<family>__v<version>". When familyExists is supplied, the family must
// be recognized by the caller-owned registry.
func ParseVersionPolicyPinnedSourceID(source string, familyExists func(family string) bool) (family, version string, ok bool) {
	i := strings.Index(source, "__v")
	if i < 0 {
		return "", "", false
	}
	family = source[:i]
	if familyExists != nil && !familyExists(family) {
		return "", "", false
	}
	version = source[i+3:]
	return family, version, true
}

// VersionPolicyEmbeddedParseError formats an embedded version-policy registry
// parse failure.
func VersionPolicyEmbeddedParseError(err error) error {
	return fmt.Errorf("parse embedded version policy: %w", err)
}

// VersionPolicyFileParseError formats a version-policy sidecar parse failure.
func VersionPolicyFileParseError(path string, err error) error {
	return fmt.Errorf("parse %s: %w", path, err)
}

// VersionPolicyPinnedPageFetchError formats a pinned source page-fetch failure.
func VersionPolicyPinnedPageFetchError(sourceID, pageURL string, err error) error {
	return fmt.Errorf("%s %s: %w", sourceID, pageURL, err)
}

// VersionPolicyPinnedPagePathError formats a pinned source page-path validation
// failure.
func VersionPolicyPinnedPagePathError(sourceID, pagePath string, err error) error {
	return fmt.Errorf("%s %s: %w", sourceID, pagePath, err)
}

// VersionPolicyMatcher enriches a hit with version metadata and reports
// whether it satisfies the active policy.
type VersionPolicyMatcher func(Hit) (Hit, bool)

// VersionPolicyScorer returns a score boost for a version-policy-matched hit.
type VersionPolicyScorer func(Hit) int

// VersionPolicyScoreOptions captures docs-puller's version-policy score
// preferences after the caller has resolved the active policy.
type VersionPolicyScoreOptions struct {
	SourceID           string
	Version            string
	LatestOnly         bool
	PreferLatest       bool
	CwdPinnedSourceIDs map[string]bool
}

// PostRetrievalOptions captures runtime-hit post-retrieval flags.
type PostRetrievalOptions struct {
	VersionPolicy   VersionPolicyOptions
	ProfileStrict   bool
	Rerank          bool
	RerankLLM       bool
	RerankHybrid    bool
	RerankChunkSize int
	RerankGate      float64
	UserLimit       int
}

// PostRetrievalProcessing wires concrete callbacks into the importable
// runtime-hit post-retrieval pipeline.
type PostRetrievalProcessing struct {
	Hits                    []Hit
	Mode                    string
	Options                 PostRetrievalOptions
	Profile                 ProfileMatcher
	ApplyVersionPolicy      func([]Hit) []Hit
	Hybrid                  func([]Hit) ([]Hit, error)
	EmbeddingRerank         func([]Hit) ([]Hit, error)
	LLMRerank               func([]Hit) ([]Hit, error)
	PostRerankVersionFilter func([]Hit) []Hit
	SourceHygienePenalty    SourceHygienePenalty
}

// PostRetrievalProcessingResult is the effective runtime-hit state after
// optional post-backend processing.
type PostRetrievalProcessingResult struct {
	Hits               []Hit
	Mode               string
	HybridErr          error
	RerankErr          error
	WarnHybridFallback bool
	WarnRerankFallback bool
}

// HybridCandidate is an embedding-side candidate path selected before rerank.
type HybridCandidate struct {
	Path string
}

// HybridScoredPath is a provider/index-neutral embedding candidate scored
// against the query vector.
type HybridScoredPath struct {
	Path  string
	Score float32
}

// DefaultHybridDepthPenaltyPerSegment slightly prefers shallower paths when
// embedding cosines are otherwise near-tied.
const DefaultHybridDepthPenaltyPerSegment = 0.005

// DefaultHybridRRFK is the Reciprocal Rank Fusion smoothing constant used by
// docs-puller's weighted BM25/cosine rerank policy.
const DefaultHybridRRFK = 60

// DefaultHybridBM25RankWeight is the weighted-RRF contribution of the
// identifier-aware BM25 rank.
const DefaultHybridBM25RankWeight = 0.7

// DefaultHybridCosineRankWeight is the weighted-RRF contribution of the
// embedding cosine rank.
const DefaultHybridCosineRankWeight = 0.3

// HybridRankFusionScore computes docs-puller's default weighted Reciprocal
// Rank Fusion score from 1-indexed BM25 and cosine ranks. A cosine rank of 0
// means no vector was available, so only BM25 contributes.
func HybridRankFusionScore(bm25Rank, cosineRank int) float64 {
	return reciprocalRankContribution(bm25Rank, DefaultHybridRRFK, DefaultHybridBM25RankWeight) +
		reciprocalRankContribution(cosineRank, DefaultHybridRRFK, DefaultHybridCosineRankWeight)
}

func reciprocalRankContribution(rank, k int, weight float64) float64 {
	if rank <= 0 || weight == 0 || k+rank <= 0 {
		return 0
	}
	return weight / float64(k+rank)
}

// HybridFlatFallbackWarning returns the stable warning emitted when the flat
// vector sidecar is unavailable and hybrid retrieval falls back to sqlite.
func HybridFlatFallbackWarning(err error) PipelineWarning {
	return PipelineWarning{
		Message: fmt.Sprintf("rerank-hybrid: flat embedding index unavailable: %v — using sqlite scan", err),
	}
}

// NewHybridFlatFallbackWarningCall builds the WarnFlatFallback callback used by
// hybrid retrieval while keeping the concrete writer caller-owned.
func NewHybridFlatFallbackWarningCall(w io.Writer) func(error) {
	if w == nil {
		return nil
	}
	return func(err error) {
		fmt.Fprintln(w, HybridFlatFallbackWarning(err).Message)
	}
}

// NewHybridScoredPath builds a provider/index-neutral hybrid candidate from
// caller-owned flat-index fields.
func NewHybridScoredPath(path string, score float32) HybridScoredPath {
	return HybridScoredPath{
		Path:  path,
		Score: score,
	}
}

// HybridFlatTopKCandidate exposes the path and score fields runtime needs from
// a caller-owned flat-vector candidate.
type HybridFlatTopKCandidate interface {
	HybridPath() string
	HybridScore() float32
}

// HybridFlatTopKSource calls a caller-owned flat vector index and returns its
// concrete scored candidate shape.
type HybridFlatTopKSource[Candidate HybridFlatTopKCandidate] func(outDir, model string, queryVec []float32, k int, depthPenalty float32) ([]Candidate, bool, error)

// NewHybridFlatTopKCall adapts caller-owned flat-index candidates to the
// provider/index-neutral HybridScoredPath shape consumed by RunHybridRetrieval.
func NewHybridFlatTopKCall[Candidate HybridFlatTopKCandidate](topK HybridFlatTopKSource[Candidate]) func(outDir, model string, queryVec []float32, k int, depthPenalty float32) ([]HybridScoredPath, bool, error) {
	if topK == nil {
		return nil
	}
	return func(outDir, model string, queryVec []float32, k int, depthPenalty float32) ([]HybridScoredPath, bool, error) {
		candidates, ok, err := topK(outDir, model, queryVec, k, depthPenalty)
		if err != nil || !ok {
			return nil, ok, err
		}
		scored := make([]HybridScoredPath, 0, len(candidates))
		for _, candidate := range candidates {
			scored = append(scored, NewHybridScoredPath(candidate.HybridPath(), candidate.HybridScore()))
		}
		return scored, ok, nil
	}
}

// CosineSimilarity returns the cosine of the angle between two embedding
// vectors. Empty, zero-norm, or dimension-mismatched vectors score 0.
func CosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		na += fa * fa
		nb += fb * fb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// SplitEmbeddingTextChunks splits text into approximately chunkSize-character
// chunks, preferring paragraph boundaries in the last half of the chunk window.
// It intentionally uses no overlap; chunked retrieval max-pools cosine across
// chunks while keeping storage cost bounded.
func SplitEmbeddingTextChunks(text string, chunkSize int) []string {
	if chunkSize <= 0 || len(text) <= chunkSize {
		return []string{text}
	}
	var chunks []string
	for start := 0; start < len(text); {
		end := start + chunkSize
		if end >= len(text) {
			chunks = append(chunks, text[start:])
			break
		}
		floor := start + chunkSize/2 - 1
		if floor < start {
			floor = start
		}
		bestBreak := -1
		for i := end; i >= floor; i-- {
			if i+1 < len(text) && text[i] == '\n' && text[i+1] == '\n' {
				bestBreak = i
				break
			}
		}
		if bestBreak >= 0 {
			end = bestBreak
		}
		chunks = append(chunks, text[start:end])
		start = end
		for start < len(text) && (text[start] == '\n' || text[start] == ' ') {
			start++
		}
	}
	return chunks
}

// HybridTitleLoader returns a candidate title for a corpus-relative path.
type HybridTitleLoader func(path string) string

// NewHybridTitleLoader adapts a caller-owned title extractor that expects a
// filesystem path into the runtime's corpus-relative title loader shape.
func NewHybridTitleLoader(outDir string, extractTitle func(path string) string) HybridTitleLoader {
	if extractTitle == nil {
		return nil
	}
	return func(path string) string {
		return extractTitle(filepath.Join(outDir, path))
	}
}

// DefaultLLMRerankSnippetChars is the body excerpt size used when building
// provider-neutral rerank candidates.
const DefaultLLMRerankSnippetChars = 400

// LLMRerankCandidate is the provider-neutral candidate shape shared by
// OpenAI-compatible LLM-as-judge prompts and trained reranker adapters.
type LLMRerankCandidate struct {
	Index   int
	Path    string
	Title   string
	Snippet string
}

// LLMRankUsage captures token usage returned by OpenAI-compatible chat APIs
// after a rerank response is parsed.
type LLMRankUsage struct {
	PromptTokens     int
	CompletionTokens int
	Present          bool
}

// ChatCompletionRankResponse is the provider-neutral result of parsing an
// OpenAI-compatible chat-completions rerank response.
type ChatCompletionRankResponse struct {
	Order         []int
	Usage         LLMRankUsage
	ProviderError string
}

// HyDEChatResponse is the provider-neutral result of parsing an
// OpenAI-compatible chat-completions response for HyDE query rewriting.
type HyDEChatResponse struct {
	Document      string
	Usage         LLMRankUsage
	ProviderError string
}

// EmbeddingResponseUsage captures token usage returned by an embedding provider
// after a response is parsed.
type EmbeddingResponseUsage struct {
	PromptTokens int
	TotalTokens  int
	Present      bool
}

// OpenAIEmbeddingResponse is the provider-neutral result of parsing an OpenAI
// embeddings response.
type OpenAIEmbeddingResponse struct {
	Vectors       []EmbeddingVectorResult
	Usage         EmbeddingResponseUsage
	ProviderError string
}

// OpenAIEmbeddingSuccess is the command-facing success projection for one
// OpenAI embeddings response. The command still owns the concrete usage sink.
type OpenAIEmbeddingSuccess struct {
	Vectors           [][]float32
	UsageEvent        EmbeddingUsageEvent
	UsageEventPresent bool
}

type openAIEmbeddingRequest struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

// RerankProviderPolicyProvider is the provider metadata needed to resolve a
// rerank request without importing the command's concrete provider registry.
type RerankProviderPolicyProvider struct {
	Name          string
	OpenAICompat  bool
	PricingModels []string
}

// NewRerankProviderPolicyProvider builds deterministic provider-policy
// metadata from caller-owned registry fields.
func NewRerankProviderPolicyProvider(name string, openAICompat bool, pricingModels []string) RerankProviderPolicyProvider {
	models := append([]string(nil), pricingModels...)
	sort.Strings(models)
	return RerankProviderPolicyProvider{
		Name:          name,
		OpenAICompat:  openAICompat,
		PricingModels: models,
	}
}

// RerankProviderPolicyInput contains provider/model selection state for an
// LLM/provider rerank call.
type RerankProviderPolicyInput struct {
	RequestedProvider string
	DefaultProvider   string
	RequestedModel    string
	DefaultModel      string
	Providers         []RerankProviderPolicyProvider
}

// RerankProviderPolicyResult is the resolved provider/model pair for a
// rerank call.
type RerankProviderPolicyResult struct {
	ProviderName string
	Model        string
}

// ProviderAPIKeyInput captures caller-owned environment lookup for providers
// that may or may not require an API key.
type ProviderAPIKeyInput struct {
	KeyEnv string
	Getenv func(string) string
}

// DefaultEmbeddingAPIKeyEnv is the API-key env var used by docs-puller's
// OpenAI-backed embedding rerank and hybrid retrieval paths.
const DefaultEmbeddingAPIKeyEnv = "OPENAI_API_KEY"

// DefaultOpenAIEmbeddingEndpoint is docs-puller's default OpenAI embeddings
// HTTP endpoint.
const DefaultOpenAIEmbeddingEndpoint = "https://api.openai.com/v1/embeddings"

// DefaultEmbeddingModel is docs-puller's default OpenAI embedding model.
const DefaultEmbeddingModel = "text-embedding-3-small"

// DefaultRerankK is docs-puller's bake-off-tuned BM25 candidate pool size
// before reranking.
const DefaultRerankK = 10

var embeddingUsagePricingUSDPer1M = map[string]float64{
	"text-embedding-3-small": 0.02,
	"text-embedding-3-large": 0.13,
	"text-embedding-ada-002": 0.10,
}

// DefaultEmbeddingInputMaxChars is docs-puller's conservative per-input
// embedding character cap. OpenAI's hard cap is 8192 tokens per single input:
// English prose tokenizes at roughly 4 chars/token, code/JSON at roughly 3,
// and dense CJK or tight punctuation can approach 2. 16000 chars / 2 leaves
// headroom under 8192 tokens.
//
// Tradeoff: long docs lose the tail of their body. For embedding-based
// reranking, the first roughly 4000 tokens usually carry enough signal because
// docs-puller prepends the extracted title before embedding.
const DefaultEmbeddingInputMaxChars = 16000

// DefaultEmbeddingPreflightCharsPerToken is the conservative cost-estimate
// ratio used before making embedding API calls.
const DefaultEmbeddingPreflightCharsPerToken = 4

// DefaultEmbeddingBatchTokenCap is docs-puller's conservative per-request
// /embeddings batch token budget. OpenAI caps total tokens per embeddings
// request at 300k; 180k leaves headroom because rough char/token estimates can
// underbudget dense docs.
const DefaultEmbeddingBatchTokenCap = 180000

// DefaultEmbeddingBatchMaxInputs is OpenAI's per-request embedding input array
// cap.
const DefaultEmbeddingBatchMaxInputs = 2048

// DefaultEmbeddingBatchCharsPerToken is the conservative batch-packing estimate
// for code/JSON-heavy docs.
const DefaultEmbeddingBatchCharsPerToken = 3

// DefaultEmbeddingRerankCallSite is the audit call-site label used for
// docs-puller embedding rerank requests.
const DefaultEmbeddingRerankCallSite = "docs-puller.search.embed_rerank"

// DefaultEmbeddingUsageCallSite is the audit call-site label used for
// docs-puller direct embedding requests.
const DefaultEmbeddingUsageCallSite = "docs-puller.embed"

// DefaultHybridRetrievalCallSite is the audit call-site label used for
// docs-puller hybrid retrieval embedding requests.
const DefaultHybridRetrievalCallSite = "docs-puller.search.hybrid_retrieval"

// DefaultHyDEAPIKeyEnv is the API-key env var used by docs-puller's
// OpenAI-backed HyDE query rewriting path.
const DefaultHyDEAPIKeyEnv = DefaultEmbeddingAPIKeyEnv

// DefaultHyDEUsageCallSite is the audit call-site label used for
// docs-puller HyDE query rewriting usage events.
const DefaultHyDEUsageCallSite = "docs-puller.rerank_hyde"

// DefaultRerankUsageCallSite is the audit call-site label used for
// docs-puller rerank usage events.
const DefaultRerankUsageCallSite = "docs-puller.rerank_llm"

// DefaultLLMRerankModel is docs-puller's default chat model for LLM rerank and
// HyDE query rewriting.
const DefaultLLMRerankModel = "gpt-4.1-mini"

// DefaultRerankProvider is docs-puller's default provider for LLM rerank.
const DefaultRerankProvider = "openai"

// RerankUsagePricing is provider-neutral per-1M-token pricing metadata.
type RerankUsagePricing struct {
	InputPer1M  float64
	OutputPer1M float64
}

// NewRerankUsagePricing builds provider-neutral usage-pricing metadata from
// caller-owned registry fields.
func NewRerankUsagePricing(inputPer1M, outputPer1M float64) RerankUsagePricing {
	return RerankUsagePricing{
		InputPer1M:  inputPer1M,
		OutputPer1M: outputPer1M,
	}
}

// HyDEUsagePricing returns the chat-model pricing used by HyDE query
// rewriting. Unknown models fall back to the gpt-4.1-mini default.
func HyDEUsagePricing(model string) RerankUsagePricing {
	switch model {
	case "gpt-4o":
		return NewRerankUsagePricing(2.50, 10.00)
	case "gpt-4.1-nano":
		return NewRerankUsagePricing(0.10, 0.40)
	default:
		return NewRerankUsagePricing(0.15, 0.60)
	}
}

// EstimateHyDEUsageCents estimates the HyDE chat-call cost in cents.
func EstimateHyDEUsageCents(model string, inputTokens, outputTokens int) float64 {
	return BuildRerankUsageEvent(RerankUsageEventInput{
		Model:           model,
		CallSite:        DefaultHyDEUsageCallSite,
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		ProviderPricing: HyDEUsagePricing(model),
	}).Cents
}

// BuildHyDEUsageEvent computes the stable HyDE query-rewriting usage audit
// payload. HyDE currently rides OpenAI chat completions, so the provider name
// is fixed while the caller still owns the concrete recorder.
func BuildHyDEUsageEvent(model string, inputTokens, outputTokens int) RerankUsageEvent {
	return BuildRerankUsageEvent(RerankUsageEventInput{
		ProviderName:    "openai",
		Model:           model,
		CallSite:        DefaultHyDEUsageCallSite,
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		ProviderPricing: HyDEUsagePricing(model),
	})
}

// EmbeddingUsagePricing returns docs-puller's OpenAI embedding price table in
// dollars per 1M tokens. Unknown models return zero so usage recording remains
// best-effort if a caller adds a model before updating the table.
func EmbeddingUsagePricing(model string) float64 {
	return embeddingUsagePricingUSDPer1M[model]
}

// IsKnownEmbeddingModel reports whether docs-puller's embedding preflight and
// usage accounting know the model's pricing.
func IsKnownEmbeddingModel(model string) bool {
	_, ok := embeddingUsagePricingUSDPer1M[model]
	return ok
}

// KnownEmbeddingModels returns the embedding models with configured pricing in
// stable display order.
func KnownEmbeddingModels() []string {
	models := make([]string, 0, len(embeddingUsagePricingUSDPer1M))
	for model := range embeddingUsagePricingUSDPer1M {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

// ResolveEmbeddingUsageTokens returns the token count to record for embedding
// usage. OpenAI embedding responses normally populate prompt_tokens, but older
// or provider-compatible responses may only populate total_tokens.
func ResolveEmbeddingUsageTokens(promptTokens, totalTokens int) int {
	if promptTokens != 0 {
		return promptTokens
	}
	return totalTokens
}

// BuildEmbeddingInput returns the text sent to the embedding API. When a title
// is present, it is prepended with a blank line so whole-doc and chunked inputs
// share the same standalone context format.
func BuildEmbeddingInput(title, body string) string {
	if title == "" {
		return body
	}
	return title + "\n\n" + body
}

// EstimateEmbeddingUsageUSD estimates embedding-call cost in USD.
func EstimateEmbeddingUsageUSD(model string, tokens int) float64 {
	dollars := float64(tokens) / 1_000_000 * EmbeddingUsagePricing(model)
	if dollars < 0 {
		return 0
	}
	return dollars
}

// ClampEmbeddingInputChars applies docs-puller's per-input embedding character
// cap to a measured input length.
func ClampEmbeddingInputChars(chars int) int {
	if chars <= 0 {
		return 0
	}
	if chars > DefaultEmbeddingInputMaxChars {
		return DefaultEmbeddingInputMaxChars
	}
	return chars
}

// TruncateEmbeddingInput applies docs-puller's per-input embedding character
// cap to a concrete input string.
func TruncateEmbeddingInput(text string) string {
	if len(text) <= DefaultEmbeddingInputMaxChars {
		return text
	}
	return text[:DefaultEmbeddingInputMaxChars]
}

// BuildTruncatedEmbeddingBatchInputs maps a command-owned batch into provider
// input strings and applies the shared per-input character cap.
func BuildTruncatedEmbeddingBatchInputs[T any](batch []T, text func(T) string) []string {
	out := make([]string, len(batch))
	for i, item := range batch {
		out[i] = TruncateEmbeddingInput(text(item))
	}
	return out
}

// PlanEmbeddingBatchAppend estimates one embedding input's batch tokens and
// reports whether adding it should flush the current batch first.
func PlanEmbeddingBatchAppend(batchLen, batchTokens, inputChars int) (int, bool) {
	inputTokens := EstimateEmbeddingBatchTokensFromChars(ClampEmbeddingInputChars(inputChars))
	flush := batchLen > 0 && (batchTokens+inputTokens > DefaultEmbeddingBatchTokenCap || batchLen >= DefaultEmbeddingBatchMaxInputs)
	return inputTokens, flush
}

// ChunkEmbeddingBatchItems emits embedding request batches using docs-puller's
// shared batch-token and input-count limits.
func ChunkEmbeddingBatchItems[T any](items []T, inputChars func(T) int) <-chan []T {
	out := make(chan []T)
	go func() {
		defer close(out)
		var batch []T
		batchTokens := 0
		flush := func() {
			if len(batch) == 0 {
				return
			}
			out <- batch
			batch = nil
			batchTokens = 0
		}
		for _, item := range items {
			itemTokens, shouldFlush := PlanEmbeddingBatchAppend(len(batch), batchTokens, inputChars(item))
			if shouldFlush {
				flush()
			}
			batch = append(batch, item)
			batchTokens += itemTokens
		}
		flush()
	}()
	return out
}

// MergeEmbeddingSplitResults combines the successful halves of a recursively
// split embedding batch retry.
func MergeEmbeddingSplitResults[T any](baseDropped int, leftItems []T, leftVecs [][]float32, leftDropped int, rightItems []T, rightVecs [][]float32, rightDropped int) ([]T, [][]float32, int) {
	outItems := make([]T, 0, len(leftItems)+len(rightItems))
	outItems = append(outItems, leftItems...)
	outItems = append(outItems, rightItems...)
	outVecs := make([][]float32, 0, len(leftVecs)+len(rightVecs))
	outVecs = append(outVecs, leftVecs...)
	outVecs = append(outVecs, rightVecs...)
	return outItems, outVecs, baseDropped + leftDropped + rightDropped
}

// EmbeddingBatchWithFallbackInput captures the caller-owned pieces needed to
// embed one batch while runtime owns split/drop retry orchestration.
type EmbeddingBatchWithFallbackInput[T any] struct {
	Batch  []T
	Text   func(T) string
	Embed  func([]string) ([][]float32, error)
	OnDrop func(T)
}

// RunEmbeddingBatchWithFallback embeds one batch, splitting provider-wide token
// limit failures and dropping provider-reported per-input oversize items before
// retrying the remaining batch.
func RunEmbeddingBatchWithFallback[T any](input EmbeddingBatchWithFallbackInput[T]) ([]T, [][]float32, int, error) {
	if input.Text == nil {
		return nil, nil, 0, EmbeddingInputTextCallMissingError()
	}
	if input.Embed == nil {
		return nil, nil, 0, EmbeddingBatchCallMissingError()
	}
	return runEmbeddingBatchWithFallback(input, input.Batch)
}

func runEmbeddingBatchWithFallback[T any](input EmbeddingBatchWithFallbackInput[T], batch []T) ([]T, [][]float32, int, error) {
	dropped := 0
	for {
		texts := BuildTruncatedEmbeddingBatchInputs(batch, input.Text)
		vecs, err := input.Embed(texts)
		if err == nil {
			return batch, vecs, dropped, nil
		}
		if IsEmbeddingBatchTokenLimitError(err) {
			mid, ok := EmbeddingBatchSplitPoint(len(batch))
			if !ok {
				return nil, nil, 0, err
			}
			leftItems, leftVecs, leftDropped, leftErr := runEmbeddingBatchWithFallback(input, batch[:mid])
			if leftErr != nil {
				return nil, nil, 0, leftErr
			}
			rightItems, rightVecs, rightDropped, rightErr := runEmbeddingBatchWithFallback(input, batch[mid:])
			if rightErr != nil {
				return nil, nil, 0, rightErr
			}
			outItems, outVecs, outDropped := MergeEmbeddingSplitResults(dropped, leftItems, leftVecs, leftDropped, rightItems, rightVecs, rightDropped)
			return outItems, outVecs, outDropped, nil
		}
		droppedItem, nextBatch, ok := DropEmbeddingInputFromError(batch, err)
		if !ok {
			return nil, nil, 0, err
		}
		if input.OnDrop != nil {
			input.OnDrop(droppedItem)
		}
		batch = nextBatch
		dropped++
		if len(batch) == 0 {
			return nil, nil, dropped, nil
		}
	}
}

// EstimateEmbeddingTokensFromChars estimates embedding input tokens from a
// character count for dry-run/max-cost preflight.
func EstimateEmbeddingTokensFromChars(chars int) int {
	if chars <= 0 {
		return 0
	}
	return chars / DefaultEmbeddingPreflightCharsPerToken
}

// EstimateEmbeddingBatchTokensFromChars estimates embedding request batch
// tokens from a character count.
func EstimateEmbeddingBatchTokensFromChars(chars int) int {
	if chars <= 0 {
		return 0
	}
	return chars / DefaultEmbeddingBatchCharsPerToken
}

// EstimateEmbeddingUsageCents estimates embedding-call cost in cents.
func EstimateEmbeddingUsageCents(model string, tokens int) float64 {
	return EstimateEmbeddingUsageUSD(model, tokens) * 100
}

// BuildEmbeddingUsageEvent computes the stable embedding usage audit payload.
func BuildEmbeddingUsageEvent(input EmbeddingUsageEventInput) EmbeddingUsageEvent {
	callSite := input.CallSite
	if callSite == "" {
		callSite = DefaultEmbeddingUsageCallSite
	}
	tokens := ResolveEmbeddingUsageTokens(input.PromptTokens, input.TotalTokens)
	return EmbeddingUsageEvent{
		ProviderName: input.ProviderName,
		Model:        input.Model,
		CallSite:     callSite,
		Tokens:       tokens,
		Cents:        EstimateEmbeddingUsageCents(input.Model, tokens),
	}
}

// BuildHyDEPrompt builds the prompt that asks an LLM to imagine the canonical
// documentation passage for a search query.
func BuildHyDEPrompt(query string) string {
	return fmt.Sprintf(`You are helping retrieve technical documentation. Given a user's search query, generate a SHORT (about 150 words) hypothetical documentation passage that would be the ideal canonical answer.

Style requirements:
- Match the tone of authoritative technical reference docs (Supabase, PostgreSQL, React docs, etc.)
- Use the proper technical terminology, including likely keywords, identifiers, and section headers
- Write as if YOU were the docs author, not as if responding to a question
- Output ONLY the passage. No preamble, no quotation marks, no "Here is..."
- Plain text, no markdown formatting

Query: %s

Hypothetical passage:`, query)
}

// ResolveHyDEModel returns the requested HyDE chat model, falling back to the
// caller-owned default when no model was requested.
func ResolveHyDEModel(requested, defaultModel string) string {
	if requested == "" {
		return defaultModel
	}
	return requested
}

// HyDEMissingAPIKeyWarning returns the stable warning used when HyDE cannot
// run because its provider API key is absent.
func HyDEMissingAPIKeyWarning(apiKeyEnv string) string {
	return fmt.Sprintf("rerank-hyde: %s not set — using raw query for embedding", apiKeyEnv)
}

// HyDEGenerationFallbackWarning returns the stable warning used when HyDE
// generation fails and search falls back to embedding the raw query.
func HyDEGenerationFallbackWarning(err error) string {
	return fmt.Sprintf("rerank-hyde: %v — using raw query for embedding", err)
}

// HyDEProviderNotRegisteredError formats the registry bootstrap failure for
// the OpenAI-backed HyDE provider.
func HyDEProviderNotRegisteredError() error {
	return errors.New("openai provider not registered (registry mis-init)")
}

// HyDEOpenAIStatusError formats OpenAI-compatible HyDE status failures and
// applies the caller-selected body preview limit.
func HyDEOpenAIStatusError(status int, body string, limit int) error {
	return fmt.Errorf("openai status %d: %s", status, truncateForError(body, limit))
}

// HyDEProviderError formats a provider-supplied HyDE error message.
func HyDEProviderError(message string) error {
	return fmt.Errorf("openai error: %s", message)
}

// HyDEEmptyResponseError formats the structural HyDE response error used when
// the provider returns no usable hypothetical document.
func HyDEEmptyResponseError() error {
	return errors.New("empty hyde response")
}

// NormalizeHyDEDocument trims provider output and reports whether it contains
// usable hypothetical-document text.
func NormalizeHyDEDocument(content string) (string, bool) {
	doc := strings.TrimSpace(content)
	if doc == "" {
		return "", false
	}
	return doc, true
}

// HyDERetryBackoff returns the retry delay used before a non-initial HyDE
// provider retry attempt.
func HyDERetryBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	return time.Duration(1<<attempt) * time.Second
}

// HyDERetryableStatus reports whether a HyDE provider response should be
// retried by the caller-owned HTTP loop.
func HyDERetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// EmbeddingRetryBackoff returns the retry delay used before a non-initial
// embedding provider retry attempt.
func EmbeddingRetryBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	return time.Duration(1<<attempt) * time.Second
}

// DefaultEmbeddingMaxAttempts is the number of OpenAI embedding HTTP attempts,
// including the first try.
const DefaultEmbeddingMaxAttempts = 3

// EmbeddingRetryableStatus reports whether an embedding provider response
// should be retried by the caller-owned HTTP loop.
func EmbeddingRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// EmbeddingSuccessStatus reports whether an embedding provider response is a
// successful terminal response.
func EmbeddingSuccessStatus(status int) bool {
	return status == http.StatusOK
}

// IsEmbeddingBatchTokenLimitError reports whether a provider error says the
// whole embedding request exceeded its token budget. Callers can use this to
// split a batch while keeping per-input oversize parsing at the provider edge.
func IsEmbeddingBatchTokenLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "max tokens") ||
		strings.Contains(msg, "maximum context") ||
		strings.Contains(msg, "too many tokens") ||
		strings.Contains(msg, "requested") && strings.Contains(msg, "tokens")
}

// EmbeddingBatchSplitPoint returns the midpoint used when retrying an embedding
// batch that exceeded the provider's total request-token budget.
func EmbeddingBatchSplitPoint(inputCount int) (int, bool) {
	if inputCount <= 1 {
		return 0, false
	}
	return inputCount / 2, true
}

var embeddingInputIndexErrorRE = regexp.MustCompile(`Invalid 'input\[(\d+)\]'`)

// ParseEmbeddingInputIndexError returns the input index from a provider error
// that identifies one oversize embedding input, such as "Invalid 'input[3]'".
func ParseEmbeddingInputIndexError(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	m := embeddingInputIndexErrorRE.FindStringSubmatch(err.Error())
	if m == nil {
		return 0, false
	}
	idx, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return 0, false
	}
	return idx, true
}

// EmbeddingInputIndexInRange reports whether a provider-reported input index
// can be applied to the caller's current embedding batch.
func EmbeddingInputIndexInRange(index, inputCount int) bool {
	return index >= 0 && index < inputCount
}

// DropEmbeddingInputAt returns the provider-reported oversize input and the
// remaining embedding batch to retry.
func DropEmbeddingInputAt[T any](batch []T, index int) (T, []T, bool) {
	var zero T
	if !EmbeddingInputIndexInRange(index, len(batch)) {
		return zero, batch, false
	}
	dropped := batch[index]
	copy(batch[index:], batch[index+1:])
	batch[len(batch)-1] = zero
	return dropped, batch[:len(batch)-1], true
}

// DropEmbeddingInputFromError parses a provider-reported input index and drops
// that item from the batch.
func DropEmbeddingInputFromError[T any](batch []T, err error) (T, []T, bool) {
	var zero T
	index, ok := ParseEmbeddingInputIndexError(err)
	if !ok {
		return zero, batch, false
	}
	return DropEmbeddingInputAt(batch, index)
}

// EmbeddingOversizeInputSkipMessage formats the stderr notice emitted when a
// provider reports one oversized embedding input.
func EmbeddingOversizeInputSkipMessage(path string, chars int) string {
	return fmt.Sprintf("  oversize: %s — skipping (%d chars exceeds per-input token cap)\n", path, chars)
}

// EmbeddingOversizeChunkSkipMessage formats the stderr notice emitted when a
// provider reports one oversized chunked embedding input.
func EmbeddingOversizeChunkSkipMessage(path string, chunkIdx int) string {
	return fmt.Sprintf("  oversize chunk: %s#%d — skipping\n", path, chunkIdx)
}

// EmbeddingOversizeDocsWarning formats the summary warning emitted after
// dropping oversized whole-document embedding inputs.
func EmbeddingOversizeDocsWarning(skipped int) string {
	if skipped <= 0 {
		return ""
	}
	return fmt.Sprintf("warning: skipped %d docs that exceeded the 8192-token per-input cap even after truncation\n", skipped)
}

// EmbeddingOversizeChunksWarning formats the summary warning emitted after
// dropping oversized chunked embedding inputs.
func EmbeddingOversizeChunksWarning(skipped int) string {
	if skipped <= 0 {
		return ""
	}
	return fmt.Sprintf("warning: skipped %d chunks that exceeded the 8192-token per-input cap\n", skipped)
}

// EmbeddingNoDocsFoundMessage formats the stderr notice emitted when no docs
// are available for whole-doc or chunked embedding.
func EmbeddingNoDocsFoundMessage() string {
	return "no docs found\n"
}

// EmbeddingUpToDateMessage formats the stdout summary emitted when all
// whole-document embeddings are current.
func EmbeddingUpToDateMessage(skipped int, model string) string {
	return fmt.Sprintf("up to date: %d docs already embedded for model=%s\n", skipped, model)
}

// EmbeddingChunkUpToDateMessage formats the stdout summary emitted when all
// chunked embeddings are current.
func EmbeddingChunkUpToDateMessage(skipped int, model string, chunkSize int) string {
	return fmt.Sprintf("up to date: %d docs already chunked-embedded for model=%s chunk_size=%d\n", skipped, model, chunkSize)
}

// EmbeddingFlatIndexWrittenMessage formats the stdout summary emitted after
// writing the whole-document flat embedding sidecar.
func EmbeddingFlatIndexWrittenMessage(model string, docs, dim int) string {
	return fmt.Sprintf("wrote flat embedding index: model=%s docs=%d dim=%d\n", model, docs, dim)
}

// EmbeddingFlatIndexMetadataInvalidError formats the validation error returned
// when a flat embedding sidecar's metadata no longer matches runtime
// expectations.
func EmbeddingFlatIndexMetadataInvalidError() error {
	return errors.New("flat embedding metadata is stale or invalid")
}

// EmbeddingFlatIndexVectorBytesError formats the validation error returned when
// the flat embedding vector sidecar size does not match metadata.
func EmbeddingFlatIndexVectorBytesError(got, want int) error {
	return fmt.Errorf("flat embedding vector bytes=%d, want %d", got, want)
}

// EmbeddingFlatIndexVectorFileEmptyError formats the validation error returned
// when the flat embedding vector sidecar exists but has no bytes to map.
func EmbeddingFlatIndexVectorFileEmptyError(path string) error {
	return fmt.Errorf("%s is empty", path)
}

// EmbeddingFlatIndexVectorInvalidError formats the validation error returned
// when a stored embedding vector cannot be written into the flat sidecar.
func EmbeddingFlatIndexVectorInvalidError(path string, dim, blobBytes int) error {
	return fmt.Errorf("embedding %s has dim=%d blob_bytes=%d", path, dim, blobBytes)
}

// EmbeddingFlatIndexMixedDimensionsError formats the validation error returned
// when stored embeddings for one model do not share a dimension.
func EmbeddingFlatIndexMixedDimensionsError(model string, existingDim, newDim int) error {
	return fmt.Errorf("mixed embedding dimensions for model %s: %d and %d", model, existingDim, newDim)
}

// EmbeddingLegacyMigrationSummaryMessage formats the stdout summary emitted
// after copying legacy embeddings into embeddings.db.
func EmbeddingLegacyMigrationSummaryMessage(docs, chunks, models int, targetPath string) string {
	return fmt.Sprintf("migrated legacy embeddings: docs=%d chunks=%d models=%d -> %s\n", docs, chunks, models, targetPath)
}

// EmbeddingLegacyNotFoundError formats the error returned when no legacy
// embedding tables exist in the old search database.
func EmbeddingLegacyNotFoundError(path string) error {
	return fmt.Errorf("legacy embeddings not found in %s", path)
}

// EmbeddingWriteFlatOnlySourceError formats the validation error returned when
// --write-flat-only is combined with a source filter.
func EmbeddingWriteFlatOnlySourceError() error {
	return errors.New("--write-flat-only writes a model-wide sidecar; omit --source")
}

// EmbeddingWriteFlatOnlyChunkSizeError formats the validation error returned
// when --write-flat-only is combined with chunked embedding.
func EmbeddingWriteFlatOnlyChunkSizeError() error {
	return errors.New("--write-flat-only only supports whole-doc embeddings; omit --chunk-size")
}

// EmbeddingWriteFlatOnlyMissingCacheError formats the operator hint returned
// when no cached whole-document embeddings exist for the requested model.
func EmbeddingWriteFlatOnlyMissingCacheError(model string) error {
	return fmt.Errorf("no cached embeddings found for model=%s; run `docs-puller embed --model %s` first", model, model)
}

// EmbeddingWriteFlatOnlyInspectCacheError formats an error returned while
// inspecting cached whole-doc embeddings for flat-index-only writes.
func EmbeddingWriteFlatOnlyInspectCacheError(err error) error {
	return fmt.Errorf("inspect cached embeddings: %w", err)
}

// EmbeddingFlatIndexWriteError formats an error returned while refreshing the
// model-wide flat embedding sidecar.
func EmbeddingFlatIndexWriteError(err error) error {
	return fmt.Errorf("write flat index: %w", err)
}

// EmbeddingFlatIndexWriteModelError formats an error returned while refreshing
// a model-specific flat embedding sidecar during legacy migration.
func EmbeddingFlatIndexWriteModelError(model string, err error) error {
	return fmt.Errorf("write flat index for %s: %w", model, err)
}

// EmbeddingUnknownModelError formats the validation error returned when the
// requested embedding model is not configured.
func EmbeddingUnknownModelError(model string) error {
	return fmt.Errorf("unknown model %q (known: %v)", model, KnownEmbeddingModels())
}

// EmbeddingSourceNotFoundError formats the validation error returned when an
// embedding source filter does not match any indexed source directory.
func EmbeddingSourceNotFoundError(source, outDir string) error {
	return fmt.Errorf("source %q not found under %s", source, outDir)
}

// EmbeddingPlanMessage formats the stdout dry-run/plan summary emitted before
// whole-document embedding starts.
func EmbeddingPlanMessage(model string, docs, skipped, estimatedTokens int, estimatedCostUSD float64) string {
	return fmt.Sprintf("embed plan: model=%s docs=%d skipped=%d est_tokens=%d est_cost=$%.4f\n", model, docs, skipped, estimatedTokens, estimatedCostUSD)
}

// EmbeddingChunkPlanMessage formats the stdout dry-run/plan summary emitted
// before chunked embedding starts.
func EmbeddingChunkPlanMessage(model string, chunkSize, chunks, skippedDocs, estimatedTokens int, estimatedCostUSD float64) string {
	return fmt.Sprintf("chunked embed plan: model=%s chunk_size=%d chunks=%d docs_skipped=%d est_tokens=%d est_cost=$%.4f\n", model, chunkSize, chunks, skippedDocs, estimatedTokens, estimatedCostUSD)
}

// EmbeddingMaxCostExceededError formats the error returned when the estimated
// embedding cost exceeds the operator-provided maximum.
func EmbeddingMaxCostExceededError(estimatedCostUSD, maxCostUSD float64) error {
	return fmt.Errorf("estimated cost $%.4f > --max-cost $%.4f", estimatedCostUSD, maxCostUSD)
}

// EmbeddingProgressMessage formats the stderr progress line emitted after one
// whole-document embedding batch is stored.
func EmbeddingProgressMessage(stored, total int) string {
	return fmt.Sprintf("embedded %d/%d (%.1f%%)\n", stored, total, embeddingProgressPercent(stored, total))
}

// EmbeddingChunkProgressMessage formats the stderr progress line emitted after
// one chunked embedding batch is stored.
func EmbeddingChunkProgressMessage(stored, total int) string {
	return fmt.Sprintf("embedded %d/%d chunks (%.1f%%)\n", stored, total, embeddingProgressPercent(stored, total))
}

func embeddingProgressPercent(stored, total int) float64 {
	if total <= 0 {
		return 0
	}
	return 100 * float64(stored) / float64(total)
}

// EmbeddingSummaryMessage formats the stdout summary emitted after whole-doc
// embeddings are stored.
func EmbeddingSummaryMessage(stored int, elapsed time.Duration, model string) string {
	return fmt.Sprintf("embedded %d docs in %s (model=%s)\n", stored, elapsed, model)
}

// EmbeddingChunkSummaryMessage formats the stdout summary emitted after chunked
// embeddings are stored.
func EmbeddingChunkSummaryMessage(stored int, elapsed time.Duration, model string, chunkSize int) string {
	return fmt.Sprintf("embedded %d chunks in %s (model=%s chunk_size=%d)\n", stored, elapsed, model, chunkSize)
}

// EmbeddingFlatIndexWriteWarning formats the warning emitted when refreshing
// the whole-document flat embedding sidecar fails after embeddings are current.
func EmbeddingFlatIndexWriteWarning(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("embedding-flat-index: write failed: %v\n", err)
}

// NewEmbeddingOversizeInputDropCall builds an OnDrop callback for whole-document
// embedding inputs that were rejected as oversized by the provider.
func NewEmbeddingOversizeInputDropCall[T any](w io.Writer, path func(T) string, text func(T) string) func(T) {
	if w == nil || path == nil || text == nil {
		return nil
	}
	return func(item T) {
		fmt.Fprint(w, EmbeddingOversizeInputSkipMessage(path(item), len(text(item))))
	}
}

// NewEmbeddingOversizeChunkDropCall builds an OnDrop callback for chunked
// embedding inputs that were rejected as oversized by the provider.
func NewEmbeddingOversizeChunkDropCall[T any](w io.Writer, path func(T) string, chunkIdx func(T) int) func(T) {
	if w == nil || path == nil || chunkIdx == nil {
		return nil
	}
	return func(item T) {
		fmt.Fprint(w, EmbeddingOversizeChunkSkipMessage(path(item), chunkIdx(item)))
	}
}

// EmbeddingVectorResult is one provider-returned embedding vector and the
// provider's input index for that vector.
type EmbeddingVectorResult struct {
	Index  int
	Vector []float32
}

// OrderEmbeddingVectorsByIndex returns provider vectors in input-index order.
func OrderEmbeddingVectorsByIndex(results []EmbeddingVectorResult) [][]float32 {
	ordered := append([]EmbeddingVectorResult(nil), results...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Index < ordered[j].Index
	})
	vectors := make([][]float32, len(ordered))
	for i, result := range ordered {
		vectors[i] = result.Vector
	}
	return vectors
}

// ValidateEmbeddingVectorCount verifies that a provider returned one embedding
// vector per requested input.
func ValidateEmbeddingVectorCount(vectorCount, inputCount int) error {
	if vectorCount != inputCount {
		return OpenAIEmbeddingVectorCountError(vectorCount, inputCount)
	}
	return nil
}

// OpenAIEmbeddingVectorCountError formats the validation error returned when
// OpenAI embeddings return a vector count that does not match the input count.
func OpenAIEmbeddingVectorCountError(vectorCount, inputCount int) error {
	return fmt.Errorf("openai returned %d vectors for %d inputs", vectorCount, inputCount)
}

// EmbeddingStatusError formats the provider status error used by embedding
// callers. Retryable responses keep a shorter body preview because they may be
// retained only as the last retry error.
func EmbeddingStatusError(status int, body string, retryable bool) error {
	limit := 500
	if retryable {
		limit = 200
	}
	return fmt.Errorf("openai status %d: %s", status, truncateForError(body, limit))
}

// OpenAIEmbeddingStatusDecision applies the OpenAI embeddings status policy for
// a caller-owned retry loop. It returns retry=true only when the caller should
// retain the error and continue to the next attempt.
func OpenAIEmbeddingStatusDecision(status int, body []byte) (retry bool, err error) {
	if EmbeddingRetryableStatus(status) {
		return true, EmbeddingStatusError(status, string(body), true)
	}
	if !EmbeddingSuccessStatus(status) {
		return false, EmbeddingStatusError(status, string(body), false)
	}
	return false, nil
}

// EmbeddingProviderError formats a provider-reported embedding error payload.
func EmbeddingProviderError(message string) error {
	return fmt.Errorf("openai error: %s", message)
}

// BuildOpenAIEmbeddingRequestBody builds the OpenAI embeddings JSON payload.
func BuildOpenAIEmbeddingRequestBody(model string, input []string) ([]byte, error) {
	return json.Marshal(openAIEmbeddingRequest{Input: input, Model: model})
}

// NewOpenAIEmbeddingRequest builds a caller-owned OpenAI embeddings HTTP
// request. Callers still own endpoint selection, headers, retry, and response
// handling.
func NewOpenAIEmbeddingRequest(endpoint string, body []byte) (*http.Request, error) {
	return http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
}

// ApplyOpenAIEmbeddingRequestHeaders applies the stable OpenAI embeddings
// request headers to a caller-owned HTTP request.
func ApplyOpenAIEmbeddingRequestHeaders(req *http.Request, apiKey, userAgent string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", userAgent)
}

// PrepareOpenAIEmbeddingRequest builds the default OpenAI embeddings request
// and returns the reusable body bytes needed by retry attempts.
func PrepareOpenAIEmbeddingRequest(model string, input []string, apiKey, userAgent string) (*http.Request, []byte, error) {
	body, err := BuildOpenAIEmbeddingRequestBody(model, input)
	if err != nil {
		return nil, nil, err
	}
	req, err := NewOpenAIEmbeddingRequest(DefaultOpenAIEmbeddingEndpoint, body)
	if err != nil {
		return nil, nil, err
	}
	ApplyOpenAIEmbeddingRequestHeaders(req, apiKey, userAgent)
	return req, body, nil
}

// ResetOpenAIEmbeddingRequestBody resets a caller-owned OpenAI embeddings
// request body before an HTTP retry.
func ResetOpenAIEmbeddingRequestBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
}

// ReadAndCloseOpenAIEmbeddingResponseBody reads and closes an OpenAI embeddings
// HTTP response body while preserving the command path's historical
// best-effort read/close behavior.
func ReadAndCloseOpenAIEmbeddingResponseBody(resp *http.Response) []byte {
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		return body
	}
	return body
}

// ParseOpenAIEmbeddingResponse parses an OpenAI embeddings response into
// provider-neutral vectors and usage fields.
func ParseOpenAIEmbeddingResponse(raw []byte) (OpenAIEmbeddingResponse, error) {
	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Usage *struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage,omitempty"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return OpenAIEmbeddingResponse{}, OpenAIEmbeddingResponseParseError(err)
	}
	if parsed.Error != nil {
		return OpenAIEmbeddingResponse{ProviderError: parsed.Error.Message}, nil
	}
	out := OpenAIEmbeddingResponse{
		Vectors: make([]EmbeddingVectorResult, len(parsed.Data)),
	}
	for i, result := range parsed.Data {
		out.Vectors[i] = EmbeddingVectorResult{Index: result.Index, Vector: result.Embedding}
	}
	if parsed.Usage != nil {
		out.Usage = EmbeddingResponseUsage{
			PromptTokens: parsed.Usage.PromptTokens,
			TotalTokens:  parsed.Usage.TotalTokens,
			Present:      true,
		}
	}
	return out, nil
}

// OpenAIEmbeddingResponseParseError formats an OpenAI embeddings JSON parse
// failure while preserving the underlying parser error for callers that unwrap.
func OpenAIEmbeddingResponseParseError(err error) error {
	return fmt.Errorf("parse response: %w", err)
}

// BuildOpenAIEmbeddingSuccess projects a parsed OpenAI embeddings response into
// ordered vectors and an optional provider-neutral usage event.
func BuildOpenAIEmbeddingSuccess(parsed OpenAIEmbeddingResponse, model, callSite string) (OpenAIEmbeddingSuccess, error) {
	if parsed.ProviderError != "" {
		return OpenAIEmbeddingSuccess{}, EmbeddingProviderError(parsed.ProviderError)
	}
	out := OpenAIEmbeddingSuccess{
		Vectors: OrderEmbeddingVectorsByIndex(parsed.Vectors),
	}
	if parsed.Usage.Present {
		out.UsageEvent = BuildEmbeddingUsageEvent(EmbeddingUsageEventInput{
			ProviderName: "openai",
			Model:        model,
			CallSite:     callSite,
			PromptTokens: parsed.Usage.PromptTokens,
			TotalTokens:  parsed.Usage.TotalTokens,
		})
		out.UsageEventPresent = true
	}
	return out, nil
}

// DecodeOpenAIEmbeddingSuccessResponse parses and projects an OpenAI embeddings
// success response into command-facing vectors and optional usage metadata.
func DecodeOpenAIEmbeddingSuccessResponse(raw []byte, model, callSite string) (OpenAIEmbeddingSuccess, error) {
	parsed, err := ParseOpenAIEmbeddingResponse(raw)
	if err != nil {
		return OpenAIEmbeddingSuccess{}, err
	}
	return BuildOpenAIEmbeddingSuccess(parsed, model, callSite)
}

// ProcessOpenAIEmbeddingHTTPResponse reads a provider response, applies the
// status policy, and decodes successful OpenAI embeddings responses. It returns
// retry=true only when the caller-owned retry loop should keep err as lastErr.
func ProcessOpenAIEmbeddingHTTPResponse(resp *http.Response, model, callSite string) (OpenAIEmbeddingSuccess, bool, error) {
	body := ReadAndCloseOpenAIEmbeddingResponseBody(resp)
	retryStatus, statusErr := OpenAIEmbeddingStatusDecision(resp.StatusCode, body)
	if retryStatus || statusErr != nil {
		return OpenAIEmbeddingSuccess{}, retryStatus, statusErr
	}
	success, err := DecodeOpenAIEmbeddingSuccessResponse(body, model, callSite)
	if err != nil {
		return OpenAIEmbeddingSuccess{}, false, err
	}
	return success, false, nil
}

// RunOpenAIEmbeddingHTTPAttempt resets the reusable request body, executes one
// OpenAI embeddings HTTP attempt, and applies response status/decoding policy.
func RunOpenAIEmbeddingHTTPAttempt(req *http.Request, body []byte, do func(*http.Request) (*http.Response, error), model, callSite string) (OpenAIEmbeddingSuccess, bool, error) {
	ResetOpenAIEmbeddingRequestBody(req, body)
	if do == nil {
		do = http.DefaultClient.Do
	}
	resp, err := do(req)
	if err != nil {
		return OpenAIEmbeddingSuccess{}, true, err
	}
	return ProcessOpenAIEmbeddingHTTPResponse(resp, model, callSite)
}

// RunOpenAIEmbeddingHTTPAttempts owns the OpenAI embeddings retry loop while
// callers keep request construction, HTTP transport, and usage sinks.
func RunOpenAIEmbeddingHTTPAttempts(req *http.Request, body []byte, do func(*http.Request) (*http.Response, error), model, callSite string, sleep func(time.Duration)) (OpenAIEmbeddingSuccess, error) {
	var lastErr error
	for attempt := 0; attempt < DefaultEmbeddingMaxAttempts; attempt++ {
		if attempt > 0 {
			delay := EmbeddingRetryBackoff(attempt)
			if sleep != nil {
				sleep(delay)
			} else {
				time.Sleep(delay)
			}
		}
		success, retryStatus, statusErr := RunOpenAIEmbeddingHTTPAttempt(req, body, do, model, callSite)
		if retryStatus {
			lastErr = statusErr
			continue
		}
		if statusErr != nil {
			return OpenAIEmbeddingSuccess{}, statusErr
		}
		return success, nil
	}
	return OpenAIEmbeddingSuccess{}, lastErr
}

// RunOpenAIEmbeddingHTTP prepares and executes an OpenAI embeddings HTTP call.
// The caller still owns the concrete HTTP transport, sleep function, and usage
// sink.
func RunOpenAIEmbeddingHTTP(model string, input []string, apiKey, userAgent string, do func(*http.Request) (*http.Response, error), callSite string, sleep func(time.Duration)) (OpenAIEmbeddingSuccess, error) {
	req, body, err := PrepareOpenAIEmbeddingRequest(model, input, apiKey, userAgent)
	if err != nil {
		return OpenAIEmbeddingSuccess{}, err
	}
	return RunOpenAIEmbeddingHTTPAttempts(req, body, do, model, callSite, sleep)
}

// RunOpenAIEmbeddingVectors executes an OpenAI embeddings call, records the
// optional usage event through the caller-owned sink, and returns vectors.
func RunOpenAIEmbeddingVectors(model string, input []string, apiKey, userAgent string, do func(*http.Request) (*http.Response, error), callSite string, sleep func(time.Duration), record EmbeddingUsageRecorder) ([][]float32, error) {
	success, err := RunOpenAIEmbeddingHTTP(model, input, apiKey, userAgent, do, callSite, sleep)
	if err != nil {
		return nil, err
	}
	EmitOpenAIEmbeddingUsageEvent(success, record)
	return success.Vectors, nil
}

// RunOpenAIEmbeddingValidatedVectors executes an OpenAI embeddings call and
// verifies the provider returned one vector per input.
func RunOpenAIEmbeddingValidatedVectors(model string, input []string, apiKey, userAgent string, do func(*http.Request) (*http.Response, error), callSite string, sleep func(time.Duration), record EmbeddingUsageRecorder) ([][]float32, error) {
	vectors, err := RunOpenAIEmbeddingVectors(model, input, apiKey, userAgent, do, callSite, sleep, record)
	if err != nil {
		return nil, err
	}
	if err := ValidateEmbeddingVectorCount(len(vectors), len(input)); err != nil {
		return nil, err
	}
	return vectors, nil
}

// RunOpenAIEmbeddingValidatedVectorsWithUsageLog executes a validated OpenAI
// embeddings vector call and adapts usage events into the command usage log
// writer shape.
func RunOpenAIEmbeddingValidatedVectorsWithUsageLog(model string, input []string, apiKey, userAgent string, do func(*http.Request) (*http.Response, error), callSite string, sleep func(time.Duration), record UsageLogRecorder) ([][]float32, error) {
	return RunOpenAIEmbeddingValidatedVectors(model, input, apiKey, userAgent, do, callSite, sleep, AdaptEmbeddingUsageLogRecorder(record))
}

// OpenAIEmbeddingBatchCallInput captures the concrete OpenAI embedding provider
// callback dependencies that command code injects into batch fallback runtime.
type OpenAIEmbeddingBatchCallInput struct {
	Model     string
	APIKey    string
	UserAgent string
	Do        func(*http.Request) (*http.Response, error)
	CallSite  string
	Sleep     func(time.Duration)
	Record    UsageLogRecorder
}

// NewOpenAIEmbeddingBatchCall builds the provider callback shape consumed by
// RunEmbeddingBatchWithFallback.
func NewOpenAIEmbeddingBatchCall(input OpenAIEmbeddingBatchCallInput) func([]string) ([][]float32, error) {
	return func(texts []string) ([][]float32, error) {
		return RunOpenAIEmbeddingValidatedVectorsWithUsageLog(input.Model, texts, input.APIKey, input.UserAgent, input.Do, input.CallSite, input.Sleep, input.Record)
	}
}

// OpenAIEmbeddingBatchWithFallbackInput contains a caller-owned batch plus the
// concrete OpenAI provider dependencies needed to run fallback embedding.
type OpenAIEmbeddingBatchWithFallbackInput[T any] struct {
	OpenAI OpenAIEmbeddingBatchCallInput
	Batch  []T
	Text   func(T) string
	OnDrop func(T)
}

// RunOpenAIEmbeddingBatchWithFallback builds the OpenAI batch provider callback
// and runs the runtime fallback loop for oversized inputs.
func RunOpenAIEmbeddingBatchWithFallback[T any](input OpenAIEmbeddingBatchWithFallbackInput[T]) ([]T, [][]float32, int, error) {
	embed := NewOpenAIEmbeddingBatchCall(input.OpenAI)
	return RunEmbeddingBatchWithFallback(EmbeddingBatchWithFallbackInput[T]{
		Batch:  input.Batch,
		Text:   input.Text,
		Embed:  embed,
		OnDrop: input.OnDrop,
	})
}

// OpenAIEmbeddingQueryCallInput captures the concrete OpenAI embedding provider
// callback dependencies for query-embedding runtime paths.
type OpenAIEmbeddingQueryCallInput struct {
	UserAgent string
	Do        func(*http.Request) (*http.Response, error)
	CallSite  string
	Record    UsageLogRecorder
}

// NewOpenAIEmbeddingQueryCall builds the EmbedQueryCall shape consumed by
// RunEmbeddingRerank and RunHybridRetrieval.
func NewOpenAIEmbeddingQueryCall(input OpenAIEmbeddingQueryCallInput) EmbedQueryCall {
	return func(model, apiKey, query string) ([][]float32, error) {
		return RunOpenAIEmbeddingTextWithUsageLog(model, query, apiKey, input.UserAgent, input.Do, input.CallSite, input.Record)
	}
}

// RunOpenAIEmbeddingTextWithUsageLog executes a validated OpenAI embeddings
// call for one text input. Search query-embedding paths use this wrapper so
// they do not route through the command-owned batch embedding shim.
func RunOpenAIEmbeddingTextWithUsageLog(model, text, apiKey, userAgent string, do func(*http.Request) (*http.Response, error), callSite string, record UsageLogRecorder) ([][]float32, error) {
	return RunOpenAIEmbeddingValidatedVectorsWithUsageLog(model, []string{text}, apiKey, userAgent, do, callSite, nil, record)
}

// EmitOpenAIEmbeddingUsageEvent forwards the optional usage event produced from
// an OpenAI embeddings response. It returns true when an event was recorded.
func EmitOpenAIEmbeddingUsageEvent(success OpenAIEmbeddingSuccess, record EmbeddingUsageRecorder) bool {
	if !success.UsageEventPresent || record == nil {
		return false
	}
	record(success.UsageEvent)
	return true
}

// AdaptEmbeddingUsageLogRecorder adapts the provider-neutral embedding usage
// event into the command-owned usage audit writer signature.
func AdaptEmbeddingUsageLogRecorder(record UsageLogRecorder) EmbeddingUsageRecorder {
	if record == nil {
		return nil
	}
	return func(event EmbeddingUsageEvent) {
		record(event.ProviderName, event.Model, event.CallSite, event.Tokens, 0, event.Cents)
	}
}

// RerankUsageEventInput contains the data needed to shape one rerank usage
// audit event.
type RerankUsageEventInput struct {
	ProviderName    string
	Model           string
	CallSite        string
	InputTokens     int
	OutputTokens    int
	ProviderPricing RerankUsagePricing
}

// RerankUsageEvent is the provider-neutral usage payload that command code can
// forward to its concrete audit log writer.
type RerankUsageEvent struct {
	ProviderName string
	Model        string
	CallSite     string
	InputTokens  int
	OutputTokens int
	Cents        float64
}

// EmbeddingUsageEventInput contains provider-neutral embedding usage fields.
type EmbeddingUsageEventInput struct {
	ProviderName string
	Model        string
	CallSite     string
	PromptTokens int
	TotalTokens  int
}

// EmbeddingUsageEvent is the provider-neutral usage payload for an embedding
// request.
type EmbeddingUsageEvent struct {
	ProviderName string
	Model        string
	CallSite     string
	Tokens       int
	Cents        float64
}

// EmbeddingUsageRecorder receives one provider-neutral embedding usage event.
type EmbeddingUsageRecorder func(EmbeddingUsageEvent)

// UsageLogRecorder is the concrete append-only usage audit writer shape used
// by the command package.
type UsageLogRecorder func(providerName, model, callSite string, inputTokens, outputTokens int, cents float64)

// RerankUsageRecorder receives one provider-neutral rerank usage event.
type RerankUsageRecorder func(RerankUsageEvent)

// LLMRerankCall calls one provider family after RunLLMRerank has built the
// provider-neutral candidate set.
type LLMRerankCall func([]LLMRerankCandidate) ([]int, error)

// ChatLLMRerankCall calls an OpenAI-compatible chat reranker with the prompt
// built by RunLLMRerank.
type ChatLLMRerankCall func(prompt string) ([]int, error)

// LLMRerankRunInput contains the provider-neutral inputs and injected
// provider-family calls needed to rerank runtime hits.
type LLMRerankRunInput struct {
	Query        string
	Hits         []Hit
	OutDir       string
	SnippetChars int
	ProviderName string
	Chat         ChatLLMRerankCall
	Cohere       LLMRerankCall
	TEI          LLMRerankCall
}

// HybridRetrievalRunInput contains the provider/index callbacks needed to
// expand BM25 hits with embedding-nearest candidates before rerank.
type HybridRetrievalRunInput[Index EmbeddingRerankIndex] struct {
	Query                  string
	BM25Hits               []Hit
	OutDir                 string
	Model                  string
	DefaultModel           string
	APIKey                 string
	APIKeyEnv              string
	K                      int
	DefaultK               int
	DepthPenaltyPerSegment float32
	UseHyDE                bool
	GenerateHyDE           func(query string) (string, bool)
	EmbedQuery             EmbedQueryCall
	FlatTopK               func(outDir, model string, queryVec []float32, k int, depthPenalty float32) ([]HybridScoredPath, bool, error)
	WarnFlatFallback       func(error)
	OpenIndex              func(outDir string) (Index, error)
	LoadVectors            func(Index, string) (map[string][]float32, error)
	ScoreVector            func(queryVec, docVec []float32) float32
	Title                  HybridTitleLoader
}

// EmbeddingRerankIndex is the concrete vector index handle surface needed by
// RunEmbeddingRerank.
type EmbeddingRerankIndex interface {
	Close() error
}

// EmbeddingIndexOpenCall opens a caller-owned embedding index store.
type EmbeddingIndexOpenCall[Index EmbeddingRerankIndex] func(outDir string, readOnly bool) (Index, error)

// NewReadOnlyEmbeddingIndexOpenCall adapts a read/write-capable index opener to
// the read-only OpenIndex callback shape used by search runtime paths.
func NewReadOnlyEmbeddingIndexOpenCall[Index EmbeddingRerankIndex](open EmbeddingIndexOpenCall[Index]) func(outDir string) (Index, error) {
	return func(outDir string) (Index, error) {
		return open(outDir, true)
	}
}

// HybridVectorLoadCall loads caller-owned embedding vectors for hybrid
// retrieval's SQLite fallback path.
type HybridVectorLoadCall[Index EmbeddingRerankIndex] func(Index, string) (map[string][]float32, error)

// NewHybridVectorLoadCall adapts a caller-owned vector loader to the
// LoadVectors callback shape used by hybrid retrieval. A nil loader remains nil
// so RunHybridRetrieval's existing dependency guard still reports the error.
func NewHybridVectorLoadCall[Index EmbeddingRerankIndex](load HybridVectorLoadCall[Index]) func(Index, string) (map[string][]float32, error) {
	if load == nil {
		return nil
	}
	return func(index Index, model string) (map[string][]float32, error) {
		return load(index, model)
	}
}

// EmbeddingRerankWholeCall reorders hits using caller-owned whole-document
// embedding vectors.
type EmbeddingRerankWholeCall[Index EmbeddingRerankIndex] func(Index, []Hit, []float32) ([]Hit, error)

// NewEmbeddingRerankWholeCall adapts a caller-owned whole-document reranker to
// the RerankWhole callback shape used by RunEmbeddingRerank.
func NewEmbeddingRerankWholeCall[Index EmbeddingRerankIndex](rerank EmbeddingRerankWholeCall[Index]) func(Index, []Hit, []float32) ([]Hit, error) {
	if rerank == nil {
		return nil
	}
	return func(index Index, hits []Hit, queryVec []float32) ([]Hit, error) {
		return rerank(index, hits, queryVec)
	}
}

// EmbeddingRerankChunkedCall reorders hits using caller-owned chunk embedding
// vectors.
type EmbeddingRerankChunkedCall[Index EmbeddingRerankIndex] func(Index, []Hit, []float32, string, int) ([]Hit, error)

// NewEmbeddingRerankChunkedCall adapts a caller-owned chunked reranker to the
// RerankChunked callback shape used by RunEmbeddingRerank.
func NewEmbeddingRerankChunkedCall[Index EmbeddingRerankIndex](rerank EmbeddingRerankChunkedCall[Index]) func(Index, []Hit, []float32, string, int) ([]Hit, error) {
	if rerank == nil {
		return nil
	}
	return func(index Index, hits []Hit, queryVec []float32, model string, chunkSize int) ([]Hit, error) {
		return rerank(index, hits, queryVec, model, chunkSize)
	}
}

// EmbedQueryCall embeds a single search query using the caller-owned provider.
type EmbedQueryCall func(model, apiKey, query string) ([][]float32, error)

// EmbeddingRerankRunInput contains the provider/index callbacks needed to
// rerank runtime hits by query embedding similarity.
type EmbeddingRerankRunInput[Index EmbeddingRerankIndex] struct {
	Query         string
	Hits          []Hit
	OutDir        string
	Model         string
	DefaultModel  string
	APIKey        string
	APIKeyEnv     string
	ChunkSize     int
	EmbedQuery    EmbedQueryCall
	OpenIndex     func(outDir string) (Index, error)
	RerankWhole   func(Index, []Hit, []float32) ([]Hit, error)
	RerankChunked func(Index, []Hit, []float32, string, int) ([]Hit, error)
}

// HTTPDoer is the small HTTP client surface required by provider-backed
// rerank calls.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// ChatCompletionRankerInput contains the injected dependencies required for
// an OpenAI-compatible chat-completions rerank call.
type ChatCompletionRankerInput struct {
	ProviderName string
	BaseURL      string
	Model        string
	APIKey       string
	Prompt       string
	UserAgent    string
	HTTPClient   HTTPDoer
	Sleep        func(time.Duration)
	RecordUsage  func(LLMRankUsage)
}

// CohereRankerInput contains the injected dependencies required for a Cohere
// v2 /rerank call.
type CohereRankerInput struct {
	BaseURL       string
	Model         string
	APIKey        string
	Query         string
	Candidates    []LLMRerankCandidate
	UserAgent     string
	HTTPClient    HTTPDoer
	Sleep         func(time.Duration)
	BeforeAttempt func()
	RetryAfter    func(http.Header) time.Duration
	RecordUsage   func()
}

// TEIRankerInput contains the injected dependencies required for a local TEI
// /rerank call.
type TEIRankerInput struct {
	BaseURL      string
	DefaultModel string
	Query        string
	Candidates   []LLMRerankCandidate
	UserAgent    string
	HTTPClient   HTTPDoer
	Sleep        func(time.Duration)
	RecordUsage  func(documentCount int)
}

// RerankOrderCandidate is a hit plus a caller-computed rerank score. The
// runtime package owns the final ordering shape while callers keep the
// concrete vector/scoring implementation.
type RerankOrderCandidate struct {
	Hit      Hit
	Score    float64
	BaseRank int
}

// BackendRetrievalOptions captures first-stage backend routing knobs.
type BackendRetrievalOptions struct {
	UseScan             bool
	FTSOnly             bool
	SharedIndexProvided bool
	CachedTotalDocs     int
}

// BackendRetrieval wires concrete backend callbacks into the runtime-hit
// first-stage retrieval runner.
type BackendRetrieval[Index any] struct {
	Options     BackendRetrievalOptions
	SharedIndex Index
	OpenLocal   func() (Index, bool)
	Query       func(Index) ([]Hit, error)
	Count       func(Index) int
	Close       func(Index)
	Scan        func() ([]Hit, int)
}

// BackendAdapter builds a backend retrieval value from package-specific
// options and concrete index/search callbacks.
type BackendAdapter[Opts any, Index any] struct {
	Options         BackendRetrievalOptions
	BaseOptions     Opts
	SharedIndex     Index
	ApplyPlan       func(Opts, PreRetrievalPlan) Opts
	CachedTotalDocs func(Opts) int
	OpenLocal       func(Opts) (Index, bool)
	Query           func(Index, Opts) ([]Hit, error)
	QueryText       string
	IndexQuery      func(Index, string, Opts) ([]Hit, error)
	Count           func(Index) int
	IndexCount      func(Index) (int, error)
	Close           func(Index)
	IndexClose      func(Index) error
	Scan            func(Opts) ([]Hit, int)
}

// VersionPolicyFTSQueryInput contains the callbacks needed to run a
// version-policy-aware FTS query without importing the concrete FTS index.
type VersionPolicyFTSQueryInput struct {
	UseFamilyFanout bool
	SourceIDs       []string
	Direct          func() ([]Hit, error)
	SearchSource    func(sourceID string) ([]Hit, error)
}

// IndexExistsFunc reports whether a local backend index is available under a
// root path.
type IndexExistsFunc func(root string) bool

// IndexOpenFunc opens a local backend index under a root path.
type IndexOpenFunc[Index any] func(root string) (Index, error)

// BackendRetrievalResult is the effective first-stage backend state plus
// warning signals for the caller to render.
type BackendRetrievalResult struct {
	Hits             []Hit
	Scanned          int
	Mode             string
	FTSQueryErr      error
	WarnMissingIndex bool
	WarnQueryError   bool
	Degraded         bool
}

// PreRetrievalOptions captures runtime planning knobs before first-stage
// backend retrieval.
type PreRetrievalOptions struct {
	UserLimit              int
	CurrentLimit           int
	Rerank                 bool
	RerankLLM              bool
	RerankK                int
	HygieneLimit           int
	NeedsWideCandidatePool bool
	CurrentSource          string
	PolicySourceID         string
}

// PreRetrievalPlan is the effective first-stage limit/source plan.
type PreRetrievalPlan struct {
	UserLimit int
	BM25Limit int
	Source    string
}

// OutputOverrideOptions captures user-facing output shaping flags over
// runtime hits.
type OutputOverrideOptions struct {
	Compact     bool
	NoSnippets  bool
	MaxSnippets int
	SnippetLen  int
}

// SearchOutputOptions captures profile/version metadata used by both text and
// JSON search output.
type SearchOutputOptions struct {
	ProfileName   string
	ProfileReason string
	ProfileStrict bool
	SourceFilter  string
	Version       VersionPolicyState
	Compact       bool
	Explain       bool
}

// SearchOutputMetadata is the derived profile/version metadata for output.
type SearchOutputMetadata struct {
	Header      string
	ProfileMode string
	Version     string
	PinContext  string
}

// SearchJSONEnvelope is docs-puller's stable JSON output shape.
type SearchJSONEnvelope struct {
	Query         string `json:"query"`
	Mode          string `json:"mode,omitempty"`
	Scanned       int    `json:"scanned"`
	Profile       string `json:"profile,omitempty"`
	ProfileReason string `json:"profile_reason,omitempty"`
	ProfileMode   string `json:"profile_mode,omitempty"`
	Version       string `json:"version,omitempty"`
	PinContext    string `json:"pin_context,omitempty"`
	Results       []Hit  `json:"results"`
}

// SearchTelemetryInput contains the runtime search metadata needed to build
// one append-only query-log entry. Storage and operator commands remain owned
// by the caller.
type SearchTelemetryInput struct {
	Timestamp    time.Time
	Query        string
	Intent       string
	SourceFilter string
	Mode         string
	Scanned      int
	Limit        int
	Output       SearchOutputOptions
	Hits         []Hit
	Rerank       bool
	RerankLLM    bool
	RerankHybrid bool
}

// SearchTelemetryEntry is docs-puller's stable query-log JSONL shape.
type SearchTelemetryEntry struct {
	Timestamp       string `json:"timestamp"`
	Query           string `json:"query"`
	Intent          string `json:"intent,omitempty"`
	SourceFilter    string `json:"source_filter,omitempty"`
	Profile         string `json:"profile,omitempty"`
	Mode            string `json:"mode"`
	Scanned         int    `json:"scanned"`
	Limit           int    `json:"limit"`
	ResultCount     int    `json:"result_count"`
	TopPath         string `json:"top_path,omitempty"`
	TopSource       string `json:"top_source,omitempty"`
	TopSourceFamily string `json:"top_source_family,omitempty"`
	Version         string `json:"version,omitempty"`
	PinContext      string `json:"pin_context,omitempty"`
	Rerank          bool   `json:"rerank,omitempty"`
	RerankLLM       bool   `json:"rerank_llm,omitempty"`
	RerankHybrid    bool   `json:"rerank_hybrid,omitempty"`
}

// SearchTelemetryFixtureInput contains query-log entries plus caller-owned
// fixture filters. File I/O and YAML encoding remain owned by the caller.
type SearchTelemetryFixtureInput struct {
	Entries []SearchTelemetryEntry
	Intent  string
	Since   time.Time
	Exclude map[string]bool
}

// SearchTelemetryFixtureQuery is the runtime-neutral candidate shape produced
// from search telemetry before the caller adapts it to its concrete eval type.
type SearchTelemetryFixtureQuery struct {
	Query  string
	Source string
	Expect []string
	Note   string
}

// ShouldLogSearchQuery applies docs-puller's query-log enablement policy. The
// caller owns environment lookup; the runtime owns the stable precedence.
func ShouldLogSearchQuery(flagLogQuery bool, envValue string) bool {
	switch strings.ToLower(strings.TrimSpace(envValue)) {
	case "0", "false", "no":
		return false
	default:
		return flagLogQuery
	}
}

// SearchTelemetryQueryKey normalizes query text for telemetry fixture dedupe
// and caller-owned exclude-fixture lookups.
func SearchTelemetryQueryKey(query string) string {
	return strings.ToLower(strings.TrimSpace(query))
}

// SearchTelemetryQueryLogPath returns docs-puller's stable query-log location
// under the caller-owned output root. Filesystem creation, locking, reads, and
// writes remain caller-owned.
func SearchTelemetryQueryLogPath(outDir string) string {
	return filepath.Join(outDir, ".cache", "query-log.jsonl")
}

// SearchTelemetryEmptyLogMessage formats the human message emitted when the
// caller-owned query-log storage has no entries.
func SearchTelemetryEmptyLogMessage(queryLogPath string) string {
	return fmt.Sprintf("no query telemetry at %s\n", queryLogPath)
}

const searchTelemetryUsage = `docs-puller telemetry — inspect search query logs

Usage:
  docs-puller telemetry log                 [--out DIR] [--limit N] [--json]
  docs-puller telemetry fixture --out-file PATH
                                             [--out DIR] [--limit N] [--intent LABEL] [--since RFC3339]
                                             [--exclude-fixture PATH[,PATH...]]

Search telemetry is ON by default since 2026-05-04 (needed to grow the
production-telemetry fixture). Disable per-call with ` + "`--log-query=false`" + `
or globally with ` + "`DOCS_PULLER_QUERY_LOG=0`" + `.
`

// SearchTelemetryUsage returns the stable help text for `docs-puller
// telemetry`. The caller still owns stdout/stderr and exit status.
func SearchTelemetryUsage() string {
	return searchTelemetryUsage
}

// SearchTelemetryFixtureOutFileRequiredMessage formats the stable stderr
// message emitted when telemetry fixture export is missing --out-file. Flag
// parsing, stderr ownership, and process exit remain caller-owned.
func SearchTelemetryFixtureOutFileRequiredMessage() string {
	return "telemetry fixture: --out-file is required\n"
}

// SearchTelemetryUnknownSubcommandMessage formats the stable stderr message
// emitted for an unknown telemetry subcommand. Dispatch, stderr writes, and
// process exit remain caller-owned.
func SearchTelemetryUnknownSubcommandMessage(subcommand string) string {
	return fmt.Sprintf("telemetry: unknown subcommand %q\n%s", subcommand, SearchTelemetryUsage())
}

// SearchTelemetryFixtureWrittenMessage formats the stable stdout summary
// emitted after telemetry fixture export succeeds. File creation, YAML
// encoding, stdout writes, and process exit remain caller-owned.
func SearchTelemetryFixtureWrittenMessage(count int, outFile string) string {
	return fmt.Sprintf("wrote %d telemetry-derived fixture candidates to %s\n", count, outFile)
}

// SearchTelemetryFixtureNoMatchesMessage formats the stable error text emitted
// when telemetry fixture export filters out every query-log entry. Matching
// logic, error construction, and process exit remain caller-owned.
func SearchTelemetryFixtureNoMatchesMessage() string {
	return "no telemetry entries matched"
}

// SearchTelemetryFixtureNoMatchesError returns the stable error emitted when
// telemetry fixture export filters out every query-log entry. Matching logic
// and process exit remain caller-owned.
func SearchTelemetryFixtureNoMatchesError() error {
	return errors.New(SearchTelemetryFixtureNoMatchesMessage())
}

// SearchTelemetryFixtureCreateDirError wraps telemetry fixture output-directory
// creation failures. Directory selection and creation remain caller-owned.
func SearchTelemetryFixtureCreateDirError(path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("telemetry fixture: create output dir %s: %w", path, err)
}

// SearchTelemetryFixtureCreateFileError wraps telemetry fixture output-file
// creation failures. File creation and lifecycle remain caller-owned.
func SearchTelemetryFixtureCreateFileError(path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("telemetry fixture: create output file %s: %w", path, err)
}

// SearchTelemetryFixtureEncodeError wraps a telemetry fixture YAML encode
// failure with stable command context. When closing the output file also fails,
// the returned error wraps both failures. File lifecycle and process exit
// remain caller-owned.
func SearchTelemetryFixtureEncodeError(encodeErr, closeErr error) error {
	if encodeErr == nil {
		return nil
	}
	if closeErr != nil {
		return fmt.Errorf("telemetry fixture: encode yaml: %w; close output file: %w", encodeErr, closeErr)
	}
	return fmt.Errorf("telemetry fixture: encode yaml: %w", encodeErr)
}

// SearchTelemetryFixtureCloseError wraps a telemetry fixture output-file close
// failure after a successful YAML encode. File lifecycle and process exit
// remain caller-owned.
func SearchTelemetryFixtureCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("telemetry fixture: close output file: %w", err)
}

// SearchTelemetryQueryLogCreateDirError wraps telemetry query-log cache
// directory creation failures. Directory selection and creation remain
// caller-owned.
func SearchTelemetryQueryLogCreateDirError(path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("create query-log dir %s: %w", path, err)
}

// SearchTelemetryQueryLogAppendOpenError wraps telemetry query-log append open
// failures. File flags, permissions, and lifecycle remain caller-owned.
func SearchTelemetryQueryLogAppendOpenError(path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("open query-log for append %s: %w", path, err)
}

// SearchTelemetryQueryLogEncodeError wraps a telemetry query-log JSON encode
// failure with stable command context. When closing the output file also fails,
// the returned error wraps both failures. File lifecycle and process exit
// remain caller-owned.
func SearchTelemetryQueryLogEncodeError(encodeErr, closeErr error) error {
	if encodeErr == nil {
		return nil
	}
	if closeErr != nil {
		return fmt.Errorf("encode query-log json: %w; close query-log file: %w", encodeErr, closeErr)
	}
	return fmt.Errorf("encode query-log json: %w", encodeErr)
}

// SearchTelemetryQueryLogCloseError wraps a telemetry query-log close failure
// after a successful JSON encode. File lifecycle remains caller-owned.
func SearchTelemetryQueryLogCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close query-log file: %w", err)
}

// SearchTelemetryQueryLogOpenError wraps telemetry query-log open failures with
// stable command context. Missing-file handling remains caller-owned.
func SearchTelemetryQueryLogOpenError(path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("open query-log %s: %w", path, err)
}

// SearchTelemetryQueryLogReadError wraps telemetry query-log scanner and close
// failures with stable command context. When both reading and closing fail, the
// returned error wraps both failures. File lifecycle remains caller-owned.
func SearchTelemetryQueryLogReadError(readErr, closeErr error) error {
	switch {
	case readErr != nil && closeErr != nil:
		return fmt.Errorf("read query-log: %w; close query-log file: %w", readErr, closeErr)
	case readErr != nil:
		return fmt.Errorf("read query-log: %w", readErr)
	case closeErr != nil:
		return fmt.Errorf("close query-log file: %w", closeErr)
	default:
		return nil
	}
}

// SearchTelemetryJSONEncodeError wraps a telemetry JSON output encode failure
// with stable command context. JSON encoder construction, stdout ownership, and
// process exit remain caller-owned.
func SearchTelemetryJSONEncodeError(err error) error {
	return fmt.Errorf("telemetry log: encode json: %w", err)
}

// SearchTelemetrySinceParseError wraps an RFC3339 parse failure with the stable
// telemetry flag context. The caller owns time parsing and process exit.
func SearchTelemetrySinceParseError(err error) error {
	return fmt.Errorf("parse --since: %w", err)
}

// SearchTelemetryExcludeFixtureLoadError wraps an exclude-fixture load failure
// with the stable telemetry flag context. The caller owns path normalization,
// fixture loading, and process exit.
func SearchTelemetryExcludeFixtureLoadError(path string, err error) error {
	return fmt.Errorf("load --exclude-fixture %s: %w", path, err)
}

// EvalDiagnoseBaselineLoadError formats a baseline eval-diagnose file load
// failure.
func EvalDiagnoseBaselineLoadError(path string, err error) error {
	return fmt.Errorf("load baseline %s: %w", path, err)
}

// EvalDiagnoseCurrentLoadError formats a current eval-diagnose file load
// failure.
func EvalDiagnoseCurrentLoadError(path string, err error) error {
	return fmt.Errorf("load current %s: %w", path, err)
}

// ArticleRedditJSONFetchError wraps a Reddit JSON endpoint fetch failure. The
// command owns URL rewriting, HTTP execution, and article rendering.
func ArticleRedditJSONFetchError(err error) error {
	return fmt.Errorf("reddit json fetch: %w", err)
}

// ArticleRedditJSONParseError wraps a Reddit JSON listing parse failure. The
// command owns the concrete Reddit response shape and article rendering.
func ArticleRedditJSONParseError(err error) error {
	return fmt.Errorf("reddit json parse: %w", err)
}

// ArticleRedditEmptyListingsError formats a Reddit JSON structural failure.
// The command owns endpoint rewriting, listing shape, and Markdown rendering.
func ArticleRedditEmptyListingsError() error {
	return errors.New("reddit json: empty listings")
}

// ArticleRedditPostParseError wraps a Reddit post payload parse failure. The
// command owns the concrete Reddit post shape and article rendering.
func ArticleRedditPostParseError(err error) error {
	return fmt.Errorf("reddit post parse: %w", err)
}

// EvalSuiteDiffError wraps a suite-level baseline diff failure. The command
// owns baseline path selection, fixture execution, and process exit.
func EvalSuiteDiffError(path string, err error) error {
	return fmt.Errorf("diff %s: %w", path, err)
}

// EvalSuiteParseError wraps a suite-level baseline JSON parse failure. The
// command owns file reads, report-shape fallback, and process exit.
func EvalSuiteParseError(err error) error {
	return fmt.Errorf("parse: %w", err)
}

// EvalSuiteFixtureIncludeExcludeConflictError formats include/exclude flag
// conflicts. The command owns flag parsing and fixture selection.
func EvalSuiteFixtureIncludeExcludeConflictError(name string) error {
	return fmt.Errorf("fixture %s appears in both --include-fixture and --exclude-fixture", name)
}

// EvalSuiteIncludeFixtureNotFoundError formats a selected fixture miss. The
// command owns fixture directory scanning and YAML fixture validation.
func EvalSuiteIncludeFixtureNotFoundError(name, dir string) error {
	return fmt.Errorf("--include-fixture %s did not match a fixture in %s", name, dir)
}

// EvalFixtureLoadError wraps a single eval fixture file load failure. The
// caller owns fixture path selection and file IO.
func EvalFixtureLoadError(path string, err error) error {
	return fmt.Errorf("load fixture %s: %w", path, err)
}

// EvalFixtureNoQueriesError formats validation for an eval fixture with no
// query rows.
func EvalFixtureNoQueriesError(path string) error {
	return fmt.Errorf("fixture %s has no queries", path)
}

// EvalFixtureParseError wraps YAML parse failures for a single eval fixture.
func EvalFixtureParseError(err error) error {
	return fmt.Errorf("parse: %w", err)
}

// EvalFixtureEmptyQueryError formats validation for an eval query row with an
// empty query string.
func EvalFixtureEmptyQueryError(index int) error {
	return fmt.Errorf("query[%d] has empty q", index)
}

// EvalFixtureNoExpectEntriesError formats validation for an eval query row
// without expected document entries.
func EvalFixtureNoExpectEntriesError(index int, query string) error {
	return fmt.Errorf("query[%d] %q has no expect entries", index, query)
}

// EvalProfileLoadError wraps an eval profile-load failure. The caller owns
// concrete profile lookup and option application.
func EvalProfileLoadError(name string, err error) error {
	return fmt.Errorf("load profile %q: %w", name, err)
}

// EvalDiffError wraps a baseline diff failure for a single eval run. The
// caller owns baseline path selection and diff emission.
func EvalDiffError(path string, err error) error {
	return fmt.Errorf("diff %s: %w", path, err)
}

// EvalDiffParseError wraps baseline eval JSON parse failures.
func EvalDiffParseError(err error) error {
	return fmt.Errorf("parse: %w", err)
}

// EvalJSONEncodeError wraps failures while writing eval JSON to an output
// stream. The command owns the concrete JSON schema.
func EvalJSONEncodeError(err error) error {
	return fmt.Errorf("encode eval json: %w", err)
}

// EvalSourceDrift is one source-level Hit@5 drift row for an eval run report.
type EvalSourceDrift struct {
	Source         string  `json:"source"`
	BaselineHitAt5 float64 `json:"baseline_hit_at_5"`
	CurrentHitAt5  float64 `json:"current_hit_at_5"`
	DeltaPP        float64 `json:"delta_pp"`
}

// BuildEvalSourceDrift builds deterministic source-level Hit@5 drift rows from
// baseline/current per-source metric maps.
func BuildEvalSourceDrift(prev, current map[string]float64) []EvalSourceDrift {
	srcKeys := map[string]bool{}
	for key := range prev {
		srcKeys[key] = true
	}
	for key := range current {
		srcKeys[key] = true
	}
	keys := make([]string, 0, len(srcKeys))
	for key := range srcKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]EvalSourceDrift, 0, len(keys))
	for _, key := range keys {
		baseline, after := prev[key], current[key]
		if baseline == after {
			continue
		}
		out = append(out, EvalSourceDrift{
			Source:         key,
			BaselineHitAt5: baseline,
			CurrentHitAt5:  after,
			DeltaPP:        (after - baseline) * 100,
		})
	}
	return out
}

// WriteEvalJSONArtifact writes one eval JSON artifact to path. The caller owns
// the concrete JSON schema; runtime owns create/write/close error control.
func WriteEvalJSONArtifact(path string, write func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	writeErr := write(f)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// AppendEvalJSONLArtifact appends one eval JSONL row to path. The caller owns
// the concrete row schema; runtime owns append/write/close error control.
func AppendEvalJSONLArtifact(path string, write func(io.Writer) error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	writeErr := write(f)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// EvalRunArtifactPaths contains the stable filesystem paths for a recorded
// eval run. The command owns report construction and file writes.
type EvalRunArtifactPaths struct {
	ReportPath string
	LatestPath string
}

// EvalRunArtifactPlan contains the path-planning inputs for a recorded eval
// run. The command owns report construction and file writes.
type EvalRunArtifactPlan struct {
	Root        string
	Timestamp   string
	Now         func() time.Time
	UserHomeDir func() (string, error)
}

// PlanEvalRunArtifacts resolves the report and latest paths for a recorded
// eval run without creating directories or writing files.
func PlanEvalRunArtifacts(plan EvalRunArtifactPlan) (EvalRunArtifactPaths, error) {
	root := strings.TrimSpace(plan.Root)
	if root == "" {
		var err error
		root, err = DefaultEvalRunRoot(plan.UserHomeDir)
		if err != nil {
			return EvalRunArtifactPaths{}, err
		}
	}
	stamp := plan.Timestamp
	if parsed, err := time.Parse(time.RFC3339, plan.Timestamp); err == nil {
		stamp = parsed.UTC().Format("20060102T150405Z")
	} else {
		now := plan.Now
		if now == nil {
			now = time.Now
		}
		stamp = now().UTC().Format("20060102T150405Z")
	}
	return EvalRunArtifactPaths{
		ReportPath: filepath.Join(root, "runs", stamp, "report.json"),
		LatestPath: filepath.Join(root, "latest.json"),
	}, nil
}

// WriteEvalRunArtifacts creates the timestamped run directory and writes the
// report and latest artifacts through the caller-owned writer.
func WriteEvalRunArtifacts(paths EvalRunArtifactPaths, write func(path string) error) error {
	if err := os.MkdirAll(filepath.Dir(paths.ReportPath), 0o755); err != nil {
		return err
	}
	if err := write(paths.ReportPath); err != nil {
		return err
	}
	if err := write(paths.LatestPath); err != nil {
		return err
	}
	return nil
}

// WriteEvalSweepBaseline creates the baseline directory and writes the fresh
// eval-sweep baseline through the caller-owned writer.
func WriteEvalSweepBaseline(path string, write func(path string) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return EvalSweepCreateBaselineDirError(err)
	}
	if err := write(path); err != nil {
		return EvalSweepWriteBaselineError(path, err)
	}
	return nil
}

// EvalSweepDiffArgsConfig contains the eval-sweep child diff argument inputs.
// The command owns executable resolution and process execution.
type EvalSweepDiffArgsConfig struct {
	FixturePath        string
	Out                string
	UseScan            bool
	Limit              int
	BaselinePath       string
	ProfileName        string
	StrictProfile      bool
	Rerank             bool
	RerankK            int
	RerankModel        string
	RerankGate         float64
	RerankChunkSize    int
	TokenBudget        int
	TokenBudgetSet     bool
	DefaultTokenBudget int
	AnswerContext      bool
}

// EvalSweepDiffArgs plans the docs-puller eval child arguments used to diff a
// post-command eval run against the freshly captured baseline.
func EvalSweepDiffArgs(cfg EvalSweepDiffArgsConfig) []string {
	args := []string{
		"eval",
		"--fixture", cfg.FixturePath,
		"--out", cfg.Out,
		"--limit", strconv.Itoa(cfg.Limit),
		"--diff", cfg.BaselinePath,
	}
	if cfg.UseScan {
		args = append(args, "--scan")
	}
	if cfg.TokenBudgetSet && cfg.TokenBudget != cfg.DefaultTokenBudget {
		args = append(args, "--token-budget", strconv.Itoa(cfg.TokenBudget))
	}
	if cfg.AnswerContext {
		args = append(args, "--answer-context")
	}
	if cfg.ProfileName != "" {
		args = append(args, "--profile", cfg.ProfileName)
		if cfg.StrictProfile {
			args = append(args, "--strict")
		}
	}
	if cfg.Rerank {
		args = append(args,
			"--rerank",
			"--rerank-k", strconv.Itoa(cfg.RerankK),
			"--rerank-model", cfg.RerankModel,
		)
		if cfg.RerankGate > 0 {
			args = append(args, "--rerank-gate", strconv.FormatFloat(cfg.RerankGate, 'f', -1, 64))
		}
		if cfg.RerankChunkSize > 0 {
			args = append(args, "--rerank-chunk-size", strconv.Itoa(cfg.RerankChunkSize))
		}
	}
	return args
}

// DefaultEvalSweepBaselinePath resolves the default fresh-baseline path for an
// eval-sweep run.
func DefaultEvalSweepBaselinePath(out string, now func() time.Time) string {
	if now == nil {
		now = time.Now
	}
	return filepath.Join(out, ".cache", "eval-baseline-"+now().UTC().Format("20060102T150405Z")+".json")
}

// DefaultEvalFixturePath resolves eval's default fixture path. It prefers the
// repository fixture, then falls back to working-directory-relative paths so a
// feature workspace can override the fixture without passing --fixture.
func DefaultEvalFixturePath(repoRoot string, exists func(string) bool) string {
	if repoRoot == "" {
		repoRoot = "."
	}
	if exists == nil {
		exists = func(string) bool { return false }
	}
	candidates := []string{
		filepath.Join(repoRoot, "cmd", "docs-puller", "eval", "fixture.yaml"),
		filepath.Join("cmd", "docs-puller", "eval", "fixture.yaml"),
		filepath.Join("eval", "fixture.yaml"),
	}
	for _, candidate := range candidates {
		if exists(candidate) {
			return candidate
		}
	}
	return candidates[0]
}

// EvalCaseResult is one per-query docs-puller eval result.
type EvalCaseResult struct {
	Query                         string   `json:"query"`
	QueryType                     string   `json:"query_type,omitempty"`
	Source                        string   `json:"source,omitempty"`
	ExpectedSource                string   `json:"expected_source,omitempty"`
	ExpectedDoc                   string   `json:"expected_doc,omitempty"`
	Expect                        []string `json:"expect"`
	GotPaths                      []string `json:"got_paths"`
	HitAt1                        bool     `json:"hit_at_1"`
	HitAt3                        bool     `json:"hit_at_3"`
	HitAt5                        bool     `json:"hit_at_5"`
	HitAt10                       bool     `json:"hit_at_10"`
	RecallAt5                     float64  `json:"recall_at_5"`
	FirstHitRank                  int      `json:"first_hit_rank"`
	Rank                          int      `json:"rank"`
	TokensReturned                int      `json:"tokens_returned"`
	TokensToFirstHit              int      `json:"tokens_to_first_hit,omitempty"`
	TokenBudgetHit                bool     `json:"token_budget_hit"`
	AnswerContextTokensReturned   int      `json:"answer_context_tokens_returned,omitempty"`
	AnswerContextTokensToFirstHit int      `json:"answer_context_tokens_to_first_hit,omitempty"`
	AnswerContextBudgetHit        bool     `json:"answer_context_budget_hit,omitempty"`
	LatencyMS                     float64  `json:"latency_ms"`
	Note                          string   `json:"note,omitempty"`
	BackendMode                   string   `json:"backend_mode,omitempty"`
}

// EvalSummary is docs-puller's aggregate eval summary schema.
type EvalSummary struct {
	Mode                       string             `json:"mode"`
	N                          int                `json:"n"`
	HitAt1                     float64            `json:"hit_at_1"`
	HitAt3                     float64            `json:"hit_at_3"`
	HitAt5                     float64            `json:"hit_at_5"`
	HitAt10                    float64            `json:"hit_at_10"`
	RecallAt5                  float64            `json:"recall_at_5"`
	MRR                        float64            `json:"mrr"`
	P50MS                      float64            `json:"p50_ms"`
	P99MS                      float64            `json:"p99_ms"`
	BySource                   map[string]float64 `json:"by_source_hit_at_5"`
	TokenBudgetTokens          int                `json:"token_budget_tokens,omitempty"`
	TokenBudgetHitRate         float64            `json:"token_budget_hit_rate,omitempty"`
	P50TokensReturned          float64            `json:"p50_tokens_returned,omitempty"`
	P99TokensReturned          float64            `json:"p99_tokens_returned,omitempty"`
	AnswerContextEnabled       bool               `json:"answer_context_enabled,omitempty"`
	AnswerContextBudgetHitRate float64            `json:"answer_context_budget_hit_rate,omitempty"`
	P50AnswerContextTokens     float64            `json:"p50_answer_context_tokens,omitempty"`
	P99AnswerContextTokens     float64            `json:"p99_answer_context_tokens,omitempty"`
	DegradedQueries            []string           `json:"degraded_queries,omitempty"`
}

// EvalJSONArtifact is the stable JSON shape emitted by `docs-puller eval
// --json` and written by `--write-baseline`.
type EvalJSONArtifact struct {
	Summary EvalSummary      `json:"summary"`
	Results []EvalCaseResult `json:"results"`
}

// EvalSummaryJSONLRow is one append-only eval summary row.
type EvalSummaryJSONLRow struct {
	Timestamp string      `json:"timestamp"`
	Summary   EvalSummary `json:"summary"`
}

// EvalRunReport is the stable record-run report JSON schema.
type EvalRunReport struct {
	SchemaVersion     string            `json:"schema_version"`
	Timestamp         string            `json:"timestamp"`
	Fixture           string            `json:"fixture"`
	DocsRoot          string            `json:"docs_root"`
	Limit             int               `json:"limit"`
	TokenBudgetTokens int               `json:"token_budget_tokens"`
	BaselinePath      string            `json:"baseline_path,omitempty"`
	Summary           EvalSummary       `json:"summary"`
	SourceDrift       []EvalSourceDrift `json:"source_drift,omitempty"`
	Results           []EvalCaseResult  `json:"results"`
}

// EvalRunReportInput is the command-owned data needed to build an eval
// record-run report.
type EvalRunReportInput struct {
	FixturePath       string
	DocsRoot          string
	Limit             int
	TokenBudgetTokens int
	BaselinePath      string
	Results           []EvalCaseResult
	Summary           EvalSummary
	SourceDrift       []EvalSourceDrift
	Now               time.Time
}

// BuildEvalRunReport builds the stable record-run report value.
func BuildEvalRunReport(in EvalRunReportInput) EvalRunReport {
	return EvalRunReport{
		SchemaVersion:     "docs-puller-eval-report/v1",
		Timestamp:         in.Now.UTC().Format(time.RFC3339),
		Fixture:           in.FixturePath,
		DocsRoot:          in.DocsRoot,
		Limit:             in.Limit,
		TokenBudgetTokens: in.TokenBudgetTokens,
		BaselinePath:      in.BaselinePath,
		Summary:           in.Summary,
		SourceDrift:       in.SourceDrift,
		Results:           in.Results,
	}
}

// EncodeEvalJSONArtifact writes the stable indented eval JSON artifact.
func EncodeEvalJSONArtifact(w io.Writer, rs []EvalCaseResult, s EvalSummary) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(EvalJSONArtifact{Summary: s, Results: rs})
}

// EncodeEvalSummaryJSONLRow writes one eval summary JSONL row.
func EncodeEvalSummaryJSONLRow(w io.Writer, s EvalSummary, now time.Time) error {
	row := EvalSummaryJSONLRow{
		Timestamp: now.UTC().Format(time.RFC3339),
		Summary:   s,
	}
	enc := json.NewEncoder(w)
	return enc.Encode(row)
}

// EncodeEvalRunReportJSON writes the stable indented record-run report JSON.
func EncodeEvalRunReportJSON(w io.Writer, report EvalRunReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// DefaultEvalQueryType resolves eval's default query type from the fixture
// basename. Fixture YAML can override this with a top-level query_type.
func DefaultEvalQueryType(fixturePath string) string {
	base := strings.ToLower(filepath.Base(fixturePath))
	switch {
	case strings.Contains(base, "telemetry"):
		return "production_telemetry"
	case strings.Contains(base, "natural"), strings.HasPrefix(base, "nl-"):
		return "tech_stack_search"
	default:
		return "library_search"
	}
}

// EvalQueryTextPresent reports whether an eval fixture row has a non-empty
// query after trimming human-authored whitespace.
func EvalQueryTextPresent(query string) bool {
	return strings.TrimSpace(query) != ""
}

// ResolveEvalQueryType preserves an explicit per-row query type, or returns
// the fixture default when the row leaves query_type blank.
func ResolveEvalQueryType(fixturePath, queryType string) string {
	if strings.TrimSpace(queryType) == "" {
		return DefaultEvalQueryType(fixturePath)
	}
	return queryType
}

// EvalRepoRootFromExecutable walks from an executable path toward the checkout
// root, using a go.work sentinel when present. It is best-effort and returns "."
// when no sentinel is found.
func EvalRepoRootFromExecutable(executable string, exists func(string) bool) string {
	if executable == "" || exists == nil {
		return "."
	}
	dir := filepath.Dir(executable)
	for i := 0; i < 6; i++ {
		if exists(filepath.Join(dir, "go.work")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}

// NormalizeEvalPath canonicalizes a fixture path or search-result path for
// equality matching. Fixture entries and search results both include the source
// as the first path segment, so no source-prefix stripping is needed.
func NormalizeEvalPath(path string) string {
	return strings.TrimPrefix(strings.TrimSpace(path), "/")
}

// EvalSourceFromPath returns the leading source segment from an eval fixture or
// result path after NormalizeEvalPath.
func EvalSourceFromPath(path string) string {
	path = NormalizeEvalPath(path)
	if path == "" {
		return ""
	}
	return strings.SplitN(path, "/", 2)[0]
}

// EvalAnswerContextFilePath safely maps a returned hit path to the
// corresponding answer-context document path under outDir.
func EvalAnswerContextFilePath(outDir, hitPath string) (string, bool) {
	rel := filepath.Clean(NormalizeEvalPath(hitPath))
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return filepath.Join(outDir, rel), true
}

// ApproxEvalTokens returns eval's stable, cheap token approximation: trimmed
// byte length divided by four, rounded up.
func ApproxEvalTokens(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

// ApproxEvalHitTokens returns eval's fallback token approximation for a
// returned search hit when the full answer-context document is unavailable.
func ApproxEvalHitTokens(hit Hit) int {
	parts := []string{hit.Path, hit.Title, hit.URL}
	for _, snippet := range hit.Snippets {
		parts = append(parts, snippet.Text)
	}
	return ApproxEvalTokens(strings.Join(parts, "\n"))
}

// EvalPercentile returns eval's stable percentile approximation. It copies and
// sorts the values, then truncates the percentile index.
func EvalPercentile(vals []float64, percentile float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	idx := int(float64(len(sorted)-1) * percentile)
	return sorted[idx]
}

// TruncateEvalStrings returns at most n strings from values, preserving the
// caller-owned backing array behavior used by eval result paths.
func TruncateEvalStrings(values []string, n int) []string {
	if len(values) > n {
		return values[:n]
	}
	return values
}

// DefaultEvalRunRoot resolves the default local root for recorded eval runs.
func DefaultEvalRunRoot(userHomeDir func() (string, error)) (string, error) {
	if os.Getenv("DOCS_PULLER_HOME") != "" {
		return apppaths.EvalRunRoot()
	}
	if userHomeDir == nil {
		return apppaths.EvalRunRoot()
	}
	homeDir := userHomeDir
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", EvalRunHomeDirEmptyError()
	}
	if os.Getenv("DOCS_PULLER_LEGACY_NDEV_PATHS") == "1" {
		return filepath.Join(home, ".nicos-dev", "evals", "docs-puller"), nil
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "docs-puller", "evals"), nil
	}
	return filepath.Join(home, ".docs-puller", "evals"), nil
}

// EvalRecordRunError wraps failure to persist a stable eval run artifact. The
// command owns report construction and filesystem writes.
func EvalRecordRunError(err error) error {
	return fmt.Errorf("record-run: %w", err)
}

// EvalRunHomeDirEmptyError formats the default eval run root failure when the
// OS returns an empty home directory.
func EvalRunHomeDirEmptyError() error {
	return errors.New("home directory is empty")
}

// EvalWriteBaselineWarning formats the best-effort write-baseline failure
// notice emitted by eval.
func EvalWriteBaselineWarning(err error) string {
	return fmt.Sprintf("eval: write-baseline: %v\n", err)
}

// EvalWriteSummaryWarning formats the best-effort summary JSONL write failure
// notice emitted by eval.
func EvalWriteSummaryWarning(err error) string {
	return fmt.Sprintf("eval: write: %v\n", err)
}

// EvalRecordRunReportWrittenMessage formats the record-run report artifact
// notice emitted after eval persists a timestamped report.
func EvalRecordRunReportWrittenMessage(path string) string {
	return fmt.Sprintf("eval: wrote report %s\n", path)
}

// EvalRecordRunLatestUpdatedMessage formats the record-run latest artifact
// notice emitted after eval updates the stable latest pointer.
func EvalRecordRunLatestUpdatedMessage(path string) string {
	return fmt.Sprintf("eval: updated latest %s\n", path)
}

// EvalSweepBaselineCapturedMessage formats the eval-sweep baseline artifact
// notice emitted before the post-baseline command runs.
func EvalSweepBaselineCapturedMessage(path string) string {
	return fmt.Sprintf("eval-sweep: captured fresh baseline at %s\n", path)
}

// EvalSweepCommandRequiredMessage formats the eval-sweep usage text emitted
// when no post-baseline command is provided.
func EvalSweepCommandRequiredMessage() string {
	return "eval-sweep: command required after --\n" +
		"usage: docs-puller eval-sweep [--fixture PATH] [--baseline PATH] -- <command...>\n"
}

// EvalCheckFixturePath maps one fixture expected path to the corpus filesystem
// path checked by eval --check-fixture.
func EvalCheckFixturePath(outDir, expectedPath string) string {
	return filepath.Join(outDir, NormalizeEvalPath(expectedPath))
}

// EvalCheckFixtureMissingLine formats one missing expected-path diagnostic
// emitted by eval --check-fixture.
func EvalCheckFixtureMissingLine(expect, query, path string) string {
	return fmt.Sprintf("  ✗ %s\n      query: %q\n      path: %s\n", expect, query, path)
}

// EvalCheckFixtureOKMessage formats the successful eval --check-fixture
// summary.
func EvalCheckFixtureOKMessage(queryCount int) string {
	return fmt.Sprintf("OK — all %d fixture queries' expects exist in the corpus.\n", queryCount)
}

// EvalCheckFixtureMissingSummary formats the failing eval --check-fixture
// summary.
func EvalCheckFixtureMissingSummary(missing int) string {
	return fmt.Sprintf("\n%d fixture entries point at non-existent paths. Update fixture or re-pull the affected source.\n", missing)
}

// EvalCheckFixtureMissingPath is one missing expected-path diagnostic.
type EvalCheckFixtureMissingPath struct {
	Expect string
	Query  string
	Path   string
}

// EvalCheckFixtureTextOutput formats eval --check-fixture stdout.
func EvalCheckFixtureTextOutput(queryCount int, missing []EvalCheckFixtureMissingPath) string {
	if len(missing) == 0 {
		return EvalCheckFixtureOKMessage(queryCount)
	}
	var b strings.Builder
	for _, m := range missing {
		b.WriteString(EvalCheckFixtureMissingLine(m.Expect, m.Query, m.Path))
	}
	return b.String()
}

// EvalBySourceProfileIgnoredWarning formats the warning emitted when eval
// --by-source receives a profile, because by-source measures source-internal
// ranking.
func EvalBySourceProfileIgnoredWarning() string {
	return "eval: --profile is ignored under --by-source (within-source ranking is what's measured)\n"
}

// EvalBySourceFTSNotReadyWarning formats the by-source FTS readiness warning.
func EvalBySourceFTSNotReadyWarning() string {
	return "eval: FTS5 index not ready after 30s — by-source proceeding anyway.\n"
}

// EvalFTSNotReadyWarning formats the standalone eval FTS readiness warning.
func EvalFTSNotReadyWarning() string {
	return "eval: FTS5 index not ready after 30s — proceeding anyway. Per-query degrade is recorded as backend_mode.\n"
}

// EvalBySourceHeader formats the by-source eval section heading.
func EvalBySourceHeader() string {
	return "=== Per-source eval (source-filtered queries only) ===\n"
}

// EvalBySourceTableHeader formats the by-source eval table header.
func EvalBySourceTableHeader() string {
	return fmt.Sprintf("%-25s %4s %7s %7s %7s %7s %8s %8s\n",
		"source", "n", "hit@1", "hit@3", "hit@5", "hit@10", "p50ms", "p99ms")
}

// EvalBySourceMetricRow is one source-filtered eval summary row.
type EvalBySourceMetricRow struct {
	Source  string
	N       int
	HitAt1  float64
	HitAt3  float64
	HitAt5  float64
	HitAt10 float64
	P50MS   float64
	P99MS   float64
}

// SortedEvalBySourceMetricRows returns rows ordered by worst Hit@5 first, then
// source name for deterministic by-source eval output.
func SortedEvalBySourceMetricRows(rows []EvalBySourceMetricRow) []EvalBySourceMetricRow {
	sorted := append([]EvalBySourceMetricRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].HitAt5 != sorted[j].HitAt5 {
			return sorted[i].HitAt5 < sorted[j].HitAt5
		}
		return sorted[i].Source < sorted[j].Source
	})
	return sorted
}

// EvalBySourceRow formats one by-source eval result row.
func EvalBySourceRow(source string, n int, hitAt1, hitAt3, hitAt5, hitAt10, p50MS, p99MS float64) string {
	return fmt.Sprintf("%-25s %4d %7.1f%% %7.1f%% %7.1f%% %7.1f%% %8.0f %8.0f\n",
		source, n,
		100*hitAt1, 100*hitAt3, 100*hitAt5, 100*hitAt10,
		p50MS, p99MS)
}

// EvalBySourceTextOutput formats the complete source-filtered eval report.
func EvalBySourceTextOutput(rows []EvalBySourceMetricRow) string {
	var b strings.Builder
	b.WriteString(EvalBySourceHeader())
	b.WriteString(EvalBySourceTableHeader())
	for _, r := range SortedEvalBySourceMetricRows(rows) {
		b.WriteString(EvalBySourceRow(
			r.Source, r.N,
			r.HitAt1, r.HitAt3, r.HitAt5, r.HitAt10,
			r.P50MS, r.P99MS,
		))
	}
	return b.String()
}

// EvalRankText formats a first-hit rank for eval's human-readable tables.
func EvalRankText(rank int) string {
	if rank == 0 {
		return "—"
	}
	return strconv.Itoa(rank)
}

// EvalMissRankText formats the miss-list rank detail for eval text output.
func EvalMissRankText(rank int) string {
	if rank > 0 {
		return fmt.Sprintf("rank %d", rank)
	}
	return "miss"
}

// EvalMissLine formats one miss row for eval text output.
func EvalMissLine(query string, rank int) string {
	return fmt.Sprintf("  ✗ %-50s  → %s\n", evalTruncateText(query, 50), EvalMissRankText(rank))
}

// EvalMissesHeader formats the non-verbose eval miss section heading.
func EvalMissesHeader(misses, total int) string {
	return fmt.Sprintf("Misses (%d / %d):\n", misses, total)
}

// EvalMissExpectLine formats one expected-path detail under a miss row.
func EvalMissExpectLine(path string) string {
	return fmt.Sprintf("      expect: %s\n", path)
}

// EvalMissGotPathLine formats the first returned path detail under a miss row.
func EvalMissGotPathLine(path string) string {
	return fmt.Sprintf("      got[1]: %s\n", path)
}

// EvalMissesFooter formats the blank line after the miss section.
func EvalMissesFooter() string {
	return "\n"
}

// EvalVerboseTableHeader formats eval's verbose per-query table header.
func EvalVerboseTableHeader() string {
	return "query                                                       hit@1 @3 @5 @10  rank  ms\n" +
		"------------------------------------------------------------ ----- -- -- ---  ----  ---\n"
}

// EvalVerboseResultLine formats one eval verbose per-query table row.
func EvalVerboseResultLine(query string, hitAt1, hitAt3, hitAt5, hitAt10 bool, rank int, latencyMS float64) string {
	return fmt.Sprintf("%-60s  %-3s %-2s %-2s %-3s  %-4s  %.0f\n",
		evalTruncateText(query, 60),
		evalTick(hitAt1), evalTick(hitAt3), evalTick(hitAt5), evalTick(hitAt10),
		EvalRankText(rank), latencyMS)
}

// EvalDiffGainedLine formats a per-query diff where a query gained a hit.
func EvalDiffGainedLine(query string, rank int) string {
	return fmt.Sprintf("  ✓ %-50s — → %d", evalTruncateText(query, 50), rank)
}

// EvalDiffLostLine formats a per-query diff where a query lost its hit.
func EvalDiffLostLine(query string, previousRank int) string {
	return fmt.Sprintf("  ✗ %-50s %d → —", evalTruncateText(query, 50), previousRank)
}

// EvalDiffMovedLine formats a per-query diff where both runs hit but the rank
// changed.
func EvalDiffMovedLine(query string, previousRank, currentRank int) string {
	arrow := "↑"
	if currentRank > previousRank {
		arrow = "↓"
	}
	return fmt.Sprintf("  %s %-50s %d → %d", arrow, evalTruncateText(query, 50), previousRank, currentRank)
}

// EvalDiffAddedLine formats a query added to the current fixture.
func EvalDiffAddedLine(query string) string {
	return "  + " + evalTruncateText(query, 60)
}

// EvalDiffRemovedLine formats a query removed from the current fixture.
func EvalDiffRemovedLine(query string) string {
	return "  - " + evalTruncateText(query, 60)
}

// EvalDiffPerQueryChangesHeader formats the per-query diff section heading.
func EvalDiffPerQueryChangesHeader() string {
	return "\nPer-query changes:\n"
}

// EvalDiffFixtureAdditionsHeader formats the fixture-additions diff section
// heading.
func EvalDiffFixtureAdditionsHeader() string {
	return "\nFixture additions:\n"
}

// EvalDiffFixtureRemovalsHeader formats the fixture-removals diff section
// heading.
func EvalDiffFixtureRemovalsHeader() string {
	return "\nFixture removals:\n"
}

// SortedEvalDiffSectionLines returns a sorted copy of preformatted diff lines.
func SortedEvalDiffSectionLines(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	out := append([]string(nil), lines...)
	sort.Strings(out)
	return out
}

// EvalDiffSectionLines formats a stable block of preformatted diff lines.
func EvalDiffSectionLines(lines []string) string {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// EvalDiffTextInput contains the already-classified eval diff data used to
// render human-readable diff output.
type EvalDiffTextInput struct {
	BaselinePath    string
	BaselineSummary EvalSummary
	CurrentSummary  EvalSummary
	SourceDrift     []EvalSourceDrift
	Gained          []string
	Lost            []string
	Moved           []string
	Added           []string
	Removed         []string
}

// EvalDiffHeader formats the human-readable eval diff heading.
func EvalDiffHeader(baselinePath string, baselineN, currentN int) string {
	return fmt.Sprintf("=== Diff %s → current (n: %d → %d) ===\n", baselinePath, baselineN, currentN)
}

// EvalDiffMetricHeader formats the aggregate metric diff table header.
func EvalDiffMetricHeader() string {
	return fmt.Sprintf("%-15s %12s    %12s    %s\n", "metric", "baseline", "current", "delta")
}

// EvalDiffMetricSeparator formats the aggregate metric diff table separator.
func EvalDiffMetricSeparator() string {
	return strings.Repeat("-", 64) + "\n"
}

// EvalDiffPercentPointLine formats one percentage-point metric diff row.
func EvalDiffPercentPointLine(label string, before, after float64) string {
	delta := (after - before) * 100
	sign := "+"
	if delta < 0 {
		sign = ""
	}
	return fmt.Sprintf("%-15s %11.0f%%    %11.0f%%    %s%.1f pp\n", label, before*100, after*100, sign, delta)
}

// EvalDiffFloatLine formats one floating-point metric diff row.
func EvalDiffFloatLine(label string, before, after float64, scale int) string {
	delta := after - before
	sign := "+"
	if delta < 0 {
		sign = ""
	}
	return fmt.Sprintf("%-15s %12.*f    %12.*f    %s%.*f\n",
		label, scale, before, scale, after, sign, scale, delta)
}

// EvalSummaryHeader formats the human-readable eval summary header.
func EvalSummaryHeader(mode string, n int) string {
	return fmt.Sprintf("=== Summary (%s, n=%d) ===\n", mode, n)
}

// EvalSummaryMetricsLine formats the hit-rate and ranking metric row in eval
// text output.
func EvalSummaryMetricsLine(hitAt1, hitAt3, hitAt5, hitAt10, mrr, recallAt5 float64) string {
	return fmt.Sprintf("Hit@1=%.0f%%  Hit@3=%.0f%%  Hit@5=%.0f%%  Hit@10=%.0f%%  MRR=%.3f  Recall@5=%.0f%%\n",
		hitAt1*100, hitAt3*100, hitAt5*100, hitAt10*100, mrr, recallAt5*100)
}

// EvalSummaryLatencyLine formats the eval latency percentile row.
func EvalSummaryLatencyLine(p50MS, p99MS float64) string {
	return fmt.Sprintf("Latency p50=%.0fms  p99=%.0fms\n", p50MS, p99MS)
}

// EvalSummaryAnswerContextLine formats answer-context token metrics when that
// eval mode is enabled.
func EvalSummaryAnswerContextLine(p50Tokens, p99Tokens, budgetHitRate float64) string {
	return fmt.Sprintf("Answer context tokens p50=%.0f  p99=%.0f  budget-hit=%.0f%%\n",
		p50Tokens, p99Tokens, budgetHitRate*100)
}

// SortedEvalSummarySources returns source keys in the deterministic order used
// by eval's per-source summary section.
func SortedEvalSummarySources(bySource map[string]float64) []string {
	keys := make([]string, 0, len(bySource))
	for k := range bySource {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// EvalSummaryBySourceHeader formats the per-source summary section header.
func EvalSummaryBySourceHeader() string {
	return "By source (Hit@5):\n"
}

// EvalSummaryBySourceLine formats one per-source Hit@5 summary row.
func EvalSummaryBySourceLine(source string, hitAt5 float64) string {
	return fmt.Sprintf("  %-20s %.0f%%\n", source, hitAt5*100)
}

// EvalTextOutput formats the human-readable eval output.
func EvalTextOutput(rs []EvalCaseResult, s EvalSummary, verbose bool) string {
	var b strings.Builder
	misses := 0
	for _, r := range rs {
		if !r.HitAt5 {
			misses++
		}
	}
	if verbose {
		b.WriteString(EvalVerboseTableHeader())
		for _, r := range rs {
			b.WriteString(EvalVerboseResultLine(r.Query, r.HitAt1, r.HitAt3, r.HitAt5, r.HitAt10, r.FirstHitRank, r.LatencyMS))
		}
	} else if misses > 0 {
		b.WriteString(EvalMissesHeader(misses, len(rs)))
		for _, r := range rs {
			if r.HitAt5 {
				continue
			}
			b.WriteString(EvalMissLine(r.Query, r.FirstHitRank))
			for _, e := range r.Expect {
				b.WriteString(EvalMissExpectLine(e))
			}
			if len(r.GotPaths) > 0 {
				b.WriteString(EvalMissGotPathLine(r.GotPaths[0]))
			}
		}
		b.WriteString(EvalMissesFooter())
	}
	b.WriteString(EvalSummaryHeader(s.Mode, s.N))
	b.WriteString(EvalSummaryMetricsLine(s.HitAt1, s.HitAt3, s.HitAt5, s.HitAt10, s.MRR, s.RecallAt5))
	b.WriteString(EvalSummaryLatencyLine(s.P50MS, s.P99MS))
	if s.AnswerContextEnabled {
		b.WriteString(EvalSummaryAnswerContextLine(s.P50AnswerContextTokens, s.P99AnswerContextTokens, s.AnswerContextBudgetHitRate))
	}
	if len(s.BySource) > 0 {
		b.WriteString(EvalSummaryBySourceHeader())
		for _, source := range SortedEvalSummarySources(s.BySource) {
			b.WriteString(EvalSummaryBySourceLine(source, s.BySource[source]))
		}
	}
	return b.String()
}

// EvalDiffTextOutput formats the complete human-readable eval diff output.
func EvalDiffTextOutput(in EvalDiffTextInput) string {
	var b strings.Builder
	b.WriteString(EvalDiffHeader(in.BaselinePath, in.BaselineSummary.N, in.CurrentSummary.N))
	b.WriteString(EvalDiffMetricHeader())
	b.WriteString(EvalDiffMetricSeparator())
	b.WriteString(EvalDiffPercentPointLine("Hit@1", in.BaselineSummary.HitAt1, in.CurrentSummary.HitAt1))
	b.WriteString(EvalDiffPercentPointLine("Hit@3", in.BaselineSummary.HitAt3, in.CurrentSummary.HitAt3))
	b.WriteString(EvalDiffPercentPointLine("Hit@5", in.BaselineSummary.HitAt5, in.CurrentSummary.HitAt5))
	b.WriteString(EvalDiffPercentPointLine("Hit@10", in.BaselineSummary.HitAt10, in.CurrentSummary.HitAt10))
	b.WriteString(EvalDiffPercentPointLine("Recall@5", in.BaselineSummary.RecallAt5, in.CurrentSummary.RecallAt5))
	b.WriteString(EvalDiffFloatLine("MRR", in.BaselineSummary.MRR, in.CurrentSummary.MRR, 3))
	b.WriteString(EvalDiffFloatLine("p50 ms", in.BaselineSummary.P50MS, in.CurrentSummary.P50MS, 0))
	b.WriteString(EvalDiffFloatLine("p99 ms", in.BaselineSummary.P99MS, in.CurrentSummary.P99MS, 0))

	if len(in.SourceDrift) > 0 {
		b.WriteString(EvalDiffPerSourceHeader())
		for _, row := range in.SourceDrift {
			b.WriteString(EvalDiffPerSourceLine(row.Source, row.BaselineHitAt5, row.CurrentHitAt5))
		}
	}

	gained := SortedEvalDiffSectionLines(in.Gained)
	lost := SortedEvalDiffSectionLines(in.Lost)
	moved := SortedEvalDiffSectionLines(in.Moved)
	added := SortedEvalDiffSectionLines(in.Added)
	removed := SortedEvalDiffSectionLines(in.Removed)
	if len(gained)+len(lost)+len(moved)+len(added)+len(removed) > 0 {
		b.WriteString(EvalDiffPerQueryChangesHeader())
	}
	b.WriteString(EvalDiffSectionLines(gained))
	b.WriteString(EvalDiffSectionLines(lost))
	b.WriteString(EvalDiffSectionLines(moved))
	if len(added) > 0 {
		b.WriteString(EvalDiffFixtureAdditionsHeader())
		b.WriteString(EvalDiffSectionLines(added))
	}
	if len(removed) > 0 {
		b.WriteString(EvalDiffFixtureRemovalsHeader())
		b.WriteString(EvalDiffSectionLines(removed))
	}
	return b.String()
}

// EvalDiffPerSourceHeader formats the per-source diff section header.
func EvalDiffPerSourceHeader() string {
	return "\nPer-source Hit@5 changes:\n"
}

// EvalDiffPerSourceLine formats one per-source Hit@5 diff row.
func EvalDiffPerSourceLine(source string, baselineHitAt5, currentHitAt5 float64) string {
	delta := (currentHitAt5 - baselineHitAt5) * 100
	sign := "+"
	if delta < 0 {
		sign = ""
	}
	return fmt.Sprintf("  %-20s %.0f%% → %.0f%%  (%s%.0f pp)\n", source, baselineHitAt5*100, currentHitAt5*100, sign, delta)
}

func evalTruncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func evalTick(ok bool) string {
	if ok {
		return "✓"
	}
	return " "
}

// EvalSweepCreateBaselineDirError wraps an eval-sweep baseline directory
// creation failure. The caller owns the selected baseline path.
func EvalSweepCreateBaselineDirError(err error) error {
	return fmt.Errorf("create baseline dir: %w", err)
}

// EvalSweepWriteBaselineError wraps an eval-sweep baseline write failure.
func EvalSweepWriteBaselineError(path string, err error) error {
	return fmt.Errorf("write baseline %s: %w", path, err)
}

// EvalSweepCommandError wraps the command failure caught after eval-sweep
// captures its fresh baseline.
func EvalSweepCommandError(err error) error {
	return fmt.Errorf("command failed: %w", err)
}

// EvalSweepResolveExecutableError wraps failure to resolve the current
// executable for the eval-sweep child diff command.
func EvalSweepResolveExecutableError(err error) error {
	return fmt.Errorf("resolve executable: %w", err)
}

// GatsbyFetchError wraps a Gatsby page-data fetch failure. The command owns
// URL selection, HTTP execution, and static-query traversal.
func GatsbyFetchError(rawURL string, err error) error {
	return fmt.Errorf("fetch %s: %w", rawURL, err)
}

// GatsbyParseError wraps a Gatsby page-data parse failure. The command owns
// the concrete page-data shape and static-query traversal.
func GatsbyParseError(rawURL string, err error) error {
	return fmt.Errorf("parse %s: %w", rawURL, err)
}

// GatsbyBaseURLParseError wraps base URL parsing for Gatsby static-query
// discovery. The command owns URL construction for query JSONs.
func GatsbyBaseURLParseError(err error) error {
	return fmt.Errorf("parse base url: %w", err)
}

// GatsbyNoStaticQueryHashesError formats a Gatsby page-data structural
// precondition failure. The command owns page-data retrieval and traversal.
func GatsbyNoStaticQueryHashesError(rawURL string) error {
	return fmt.Errorf("no staticQueryHashes in %s", rawURL)
}

// GatsbyNoAllMdxSlugsError formats the failure to find public MDX slugs across
// Gatsby static queries. The command owns query traversal and slug filtering.
func GatsbyNoAllMdxSlugsError(queryCount int) error {
	return fmt.Errorf("no allMdx.nodes[].slug found across %d static queries", queryCount)
}

// SitemapFetchError wraps a sitemap fetch failure. The command owns sitemap
// URL selection, HTTP execution, and recursive sitemap traversal.
func SitemapFetchError(rawURL string, err error) error {
	return fmt.Errorf("fetch %s: %w", rawURL, err)
}

// SitemapParseError wraps a sitemap XML root parse failure. The command owns
// XML decoding, sitemap type dispatch, and nested sitemap traversal.
func SitemapParseError(rawURL string, err error) error {
	return fmt.Errorf("parse %s: %w", rawURL, err)
}

// SitemapUnexpectedRootError formats a sitemap XML root classification failure.
// The command owns XML decoding, recursion, and URL resolution.
func SitemapUnexpectedRootError(root, rawURL string) error {
	return fmt.Errorf("unexpected sitemap root <%s> at %s", root, rawURL)
}

// SitemapGunzipError wraps gzip decode setup for .gz sitemap bodies. The
// command owns body fetching, reader lifecycle, and content reads.
func SitemapGunzipError(err error) error {
	return fmt.Errorf("gunzip: %w", err)
}

// WriteLockOpenError wraps opening the docs-puller write lock file. The
// command owns lock path construction and release lifecycle.
func WriteLockOpenError(err error) error {
	return fmt.Errorf("open write-lock: %w", err)
}

// WriteLockFlockError wraps acquiring the exclusive docs-puller write lock.
// The command owns file descriptor lifecycle and unlock behavior.
func WriteLockFlockError(err error) error {
	return fmt.Errorf("flock: %w", err)
}

// DocCRootURLError wraps a DocC root URL conversion failure.
func DocCRootURLError(err error) error {
	return fmt.Errorf("root url: %w", err)
}

// DocCNotAbsoluteURLError formats the DocC page URL conversion precondition
// failure. The command owns URL parsing and source mapping.
func DocCNotAbsoluteURLError(rawURL string) error {
	return fmt.Errorf("not an absolute URL: %s", rawURL)
}

// DocCRootRejectedError formats the initial DocC frontier rejection. The
// command owns filter evaluation, visited tracking, and crawl scheduling.
func DocCRootRejectedError(rootPage string) error {
	return fmt.Errorf("root URL %s rejected by filter or already visited", rootPage)
}

// DocCParseError wraps a DocC node JSON parse failure. The command owns the
// concrete DocC node shape and crawl result plumbing.
func DocCParseError(err error) error {
	return fmt.Errorf("parse: %w", err)
}

// IndexCollectSourceError wraps a per-source index collection failure. The
// command owns filesystem walking, title extraction, and manifest lookup.
func IndexCollectSourceError(source string, err error) error {
	return fmt.Errorf("collect %s: %w", source, err)
}

// IndexWriteSourceError wraps writing one source-level _INDEX.md file. The
// command owns index rendering and atomicity policy.
func IndexWriteSourceError(source string, err error) error {
	return fmt.Errorf("write %s/_INDEX.md: %w", source, err)
}

// ManifestParseError wraps parsing one docs-puller manifest JSON file.
func ManifestParseError(path string, err error) error {
	return fmt.Errorf("parse %s: %w", path, err)
}

// ManifestMigrationError wraps migrating a legacy JSONL manifest to the
// bounded JSON manifest. The command owns manifest file IO and legacy cleanup.
func ManifestMigrationError(legacyPath, path string, err error) error {
	return fmt.Errorf("migrate %s -> %s: %w", legacyPath, path, err)
}

// ManifestLoadSourceError wraps loading one source-level docs-puller manifest.
func ManifestLoadSourceError(source string, err error) error {
	return fmt.Errorf("load %s manifest: %w", source, err)
}

// ManifestWriteSourceError wraps writing one source-level docs-puller manifest.
func ManifestWriteSourceError(source string, err error) error {
	return fmt.Errorf("write %s manifest: %w", source, err)
}

// SearchTelemetryLogRow formats one human-readable query-log row. Storage,
// JSON output, and stdout writes remain owned by the caller.
func SearchTelemetryLogRow(entry SearchTelemetryEntry) string {
	top := entry.TopPath
	if top == "" {
		top = "—"
	}
	intent := entry.Intent
	if intent == "" {
		intent = "unlabeled"
	}
	return fmt.Sprintf("%s  %-12s  %-8s  %s  -> %s\n", entry.Timestamp, intent, entry.Mode, entry.Query, top)
}

// CompactOutputPresetOptions captures the --compact preset values after flag
// parsing.
type CompactOutputPresetOptions struct {
	Compact            bool
	SnippetLen         int
	MaxSnippets        int
	DefaultSnippetLen  int
	DefaultMaxSnippets int
}

// CompactOutputPreset is the effective snippet knob pair after applying the
// compact preset.
type CompactOutputPreset struct {
	SnippetLen  int
	MaxSnippets int
}

// SnippetRetuner reloads or otherwise recomputes snippets for a hit when
// output shaping needs a different snippet count or length.
type SnippetRetuner func(Hit) ([]Snippet, error)

// SnippetExtractor derives snippets from one document body and normalized
// lowercase query.
type SnippetExtractor func(body string, queryLower string, maxSnippets int, snippetLen int) []Snippet

// SourceLister returns known source directory names under the docs root.
type SourceLister func(root string) ([]string, error)

// SourceURLLoader returns source-relative markdown paths to canonical URLs.
type SourceURLLoader func(sourceDir string, sourceName string) map[string]string

// TitleExtractor derives a display title from a markdown path.
type TitleExtractor func(path string) string

// ScanOptions captures the fallback filesystem scan knobs.
type ScanOptions struct {
	Root       string
	Source     string
	Limit      int
	Exact      bool
	TitleBoost int
}

// ScanInput wires package-specific source/title/manifest/snippet helpers into
// the importable fallback scanner.
type ScanInput struct {
	Query           string
	Options         ScanOptions
	ListSources     SourceLister
	LoadURLByPath   SourceURLLoader
	ExtractTitle    TitleExtractor
	ExtractSnippets SnippetExtractor
}

// NewFileSnippetRetuner builds a retuner that reloads hit bodies from root and
// delegates snippet extraction to the caller's concrete extractor.
func NewFileSnippetRetuner(root string, query string, maxSnippets int, snippetLen int, extract SnippetExtractor) SnippetRetuner {
	if extract == nil {
		return nil
	}
	queryLower := strings.ToLower(query)
	return func(hit Hit) ([]Snippet, error) {
		path := filepath.Join(root, hit.Path)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return extract(string(data), queryLower, maxSnippets, snippetLen), nil
	}
}

// RunScan performs docs-puller's fallback filesystem scan over markdown files.
func RunScan(input ScanInput) ([]Hit, int) {
	if input.ListSources == nil {
		return nil, 0
	}
	sources, err := input.ListSources(input.Options.Root)
	if err != nil || len(sources) == 0 {
		return nil, 0
	}
	if input.Options.Source != "" {
		sources = []string{input.Options.Source}
	}
	queryLower := strings.ToLower(input.Query)
	var (
		all     []Hit
		scanned int
	)
	for _, source := range sources {
		hits, count := scanSource(scanSourceInput{
			Dir:             filepath.Join(input.Options.Root, source),
			Source:          source,
			QueryLower:      queryLower,
			Exact:           input.Options.Exact,
			TitleBoost:      input.Options.TitleBoost,
			LoadURLByPath:   input.LoadURLByPath,
			ExtractTitle:    input.ExtractTitle,
			ExtractSnippets: input.ExtractSnippets,
		})
		all = append(all, hits...)
		scanned += count
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		return all[i].Path < all[j].Path
	})
	if input.Options.Limit > 0 && len(all) > input.Options.Limit {
		all = all[:input.Options.Limit]
	}
	return all, scanned
}

type scanSourceInput struct {
	Dir             string
	Source          string
	QueryLower      string
	Exact           bool
	TitleBoost      int
	LoadURLByPath   SourceURLLoader
	ExtractTitle    TitleExtractor
	ExtractSnippets SnippetExtractor
}

func scanSource(input scanSourceInput) ([]Hit, int) {
	urlByPath := map[string]string{}
	if input.LoadURLByPath != nil {
		urlByPath = input.LoadURLByPath(input.Dir, input.Source)
	}
	var (
		hits    []Hit
		scanned int
	)
	filepath.WalkDir(input.Dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".md") || name == "_INDEX.md" {
			return nil
		}
		scanned++
		hit := scanFile(scanFileInput{
			Path:            path,
			Source:          input.Source,
			QueryLower:      input.QueryLower,
			Exact:           input.Exact,
			TitleBoost:      input.TitleBoost,
			ExtractTitle:    input.ExtractTitle,
			ExtractSnippets: input.ExtractSnippets,
		})
		if hit.Score == 0 {
			return nil
		}
		rel, err := filepath.Rel(input.Dir, path)
		if err != nil {
			return nil
		}
		hit.Path = filepath.Join(input.Source, rel)
		hit.URL = urlByPath[rel]
		hits = append(hits, hit)
		return nil
	})
	return hits, scanned
}

type scanFileInput struct {
	Path            string
	Source          string
	QueryLower      string
	Exact           bool
	TitleBoost      int
	ExtractTitle    TitleExtractor
	ExtractSnippets SnippetExtractor
}

func scanFile(input scanFileInput) Hit {
	data, err := os.ReadFile(input.Path)
	if err != nil {
		return Hit{}
	}
	title := ""
	if input.ExtractTitle != nil {
		title = input.ExtractTitle(input.Path)
	}
	hit := Hit{Source: input.Source, Title: title}
	if input.Exact {
		query := strings.TrimSpace(input.QueryLower)
		if query == "" {
			return hit
		}
		bodyLower := strings.ToLower(string(data))
		if hit.Title != "" && strings.Contains(strings.ToLower(hit.Title), query) {
			hit.Score += input.TitleBoost
		}
		hit.Score += strings.Count(bodyLower, query)
		if hit.Score == 0 {
			return hit
		}
		if input.ExtractSnippets != nil {
			hit.Snippets = input.ExtractSnippets(string(data), query, 0, 0)
		}
		return hit
	}

	tokens := strings.Fields(input.QueryLower)
	if len(tokens) == 0 {
		return hit
	}
	if hit.Title != "" {
		titleLower := strings.ToLower(hit.Title)
		for _, token := range tokens {
			if strings.Contains(titleLower, token) {
				hit.Score += input.TitleBoost
				break
			}
		}
	}
	bodyLower := strings.ToLower(string(data))
	for _, token := range tokens {
		hit.Score += strings.Count(bodyLower, token)
	}
	if hit.Score == 0 {
		return hit
	}
	if input.ExtractSnippets != nil {
		hit.Snippets = input.ExtractSnippets(string(data), input.QueryLower, 0, 0)
	}
	return hit
}

// PipelineBackend builds the concrete first-stage backend callbacks from the
// effective pre-retrieval plan.
type PipelineBackend[Index any] func(PreRetrievalPlan) BackendRetrieval[Index]

// PipelinePostRetrieval builds the concrete post-retrieval callbacks from the
// effective pre-retrieval plan. RunPipeline supplies the current hits and mode.
type PipelinePostRetrieval func(PreRetrievalPlan) PostRetrievalProcessing

// Pipeline wires runtime planning, backend retrieval, post-retrieval
// processing, and output shaping into one importable dispatch sequence.
type Pipeline[Index any] struct {
	PreRetrieval  PreRetrievalOptions
	Backend       PipelineBackend[Index]
	PostRetrieval PipelinePostRetrieval
	Output        OutputOverrideOptions
	Retune        SnippetRetuner
}

// PipelineResult is the effective dispatch state plus warning signals for the
// caller to render.
type PipelineResult struct {
	Hits               []Hit
	Scanned            int
	Mode               string
	PreRetrieval       PreRetrievalPlan
	FTSQueryErr        error
	HybridErr          error
	RerankErr          error
	WarnMissingIndex   bool
	WarnQueryError     bool
	WarnHybridFallback bool
	WarnRerankFallback bool
	Degraded           bool
}

// PipelineWarning is a user-facing warning generated from pipeline fallback
// state. Callers still choose where to render it.
type PipelineWarning struct {
	Message string
}

// NewBackendAdapter returns a pipeline backend builder that applies the
// pre-retrieval limit/source plan before constructing the concrete backend
// retrieval callbacks.
func NewBackendAdapter[Opts any, Index any](input BackendAdapter[Opts, Index]) PipelineBackend[Index] {
	return func(pre PreRetrievalPlan) BackendRetrieval[Index] {
		opts := input.BaseOptions
		if input.ApplyPlan != nil {
			opts = input.ApplyPlan(opts, pre)
		}
		options := input.Options
		if input.CachedTotalDocs != nil {
			options.CachedTotalDocs = input.CachedTotalDocs(opts)
		}
		count := input.Count
		if count == nil && input.IndexCount != nil {
			count = func(index Index) int {
				// Preserve docs-puller's historical totalDocs behavior: use the
				// count when available and let the query path continue otherwise.
				total, _ := input.IndexCount(index)
				return total
			}
		}
		closeIndex := input.Close
		if closeIndex == nil && input.IndexClose != nil {
			closeIndex = func(index Index) {
				if err := input.IndexClose(index); err != nil {
					// FTS close has historically been best-effort in the search
					// path; keep cleanup failures non-fatal until warning plumbing
					// exists for backend close errors.
				}
			}
		}
		return BackendRetrieval[Index]{
			Options:     options,
			SharedIndex: input.SharedIndex,
			OpenLocal: func() (Index, bool) {
				if input.OpenLocal == nil {
					var zero Index
					return zero, false
				}
				return input.OpenLocal(opts)
			},
			Query: func(index Index) ([]Hit, error) {
				if input.Query != nil {
					return input.Query(index, opts)
				}
				if input.IndexQuery != nil {
					return input.IndexQuery(index, input.QueryText, opts)
				}
				return nil, nil
			},
			Count: count,
			Close: closeIndex,
			Scan: func() ([]Hit, int) {
				if input.Scan == nil {
					return nil, 0
				}
				return input.Scan(opts)
			},
		}
	}
}

// NewLocalIndexOpener returns a backend OpenLocal callback that treats a
// missing index or open error as local index unavailability.
func NewLocalIndexOpener[Index any](root string, exists IndexExistsFunc, open IndexOpenFunc[Index]) func() (Index, bool) {
	return func() (Index, bool) {
		var zero Index
		if exists == nil || open == nil || !exists(root) {
			return zero, false
		}
		index, err := open(root)
		if err != nil {
			return zero, false
		}
		return index, true
	}
}

// ApplySourceHygiene applies a runtime-hit source hygiene penalty, then
// returns the planned result window.
func ApplySourceHygiene(hits []Hit, limit int, penalty SourceHygienePenalty) []Hit {
	return searchdispatch.ApplySourceHygiene(hits, limit, penalty)
}

// AnnotateProfileMembership stamps InProfile / InProfileSub on every hit.
func AnnotateProfileMembership(hits []Hit, profile ProfileMatcher) []Hit {
	if profile == nil {
		return hits
	}
	for i := range hits {
		rel := RelPathInSource(hits[i].Path, hits[i].Source)
		in, sub := profile.Match(hits[i].Source, rel)
		hits[i].InProfile = in
		hits[i].InProfileSub = sub
	}
	return hits
}

// ResolveSearchProfile owns the runtime-neutral profile selection flow:
// resolve the active profile name, load it if present, degrade load failures
// to no active profile, and warn when strict mode has nothing to enforce.
func ResolveSearchProfile[Profile any](input ProfileSelectionInput[Profile]) ProfileSelectionResult[Profile] {
	var zero Profile
	if input.Resolve == nil {
		if input.Strict && input.WarnStrictWithoutProfile != nil {
			input.WarnStrictWithoutProfile()
		}
		return ProfileSelectionResult[Profile]{Reason: "none"}
	}
	name, reason := input.Resolve(input.Options)
	result := ProfileSelectionResult[Profile]{Name: name, Reason: reason}
	if name != "" {
		if input.Load == nil {
			result.Name = ""
			result.Reason = "load-failed"
			if input.WarnLoadFailed != nil {
				input.WarnLoadFailed(name, ProfileLoadCallMissingError())
			}
		} else {
			profile, err := input.Load(name, input.Options.OutDir)
			if err != nil {
				result.Name = ""
				result.Reason = "load-failed"
				if input.WarnLoadFailed != nil {
					input.WarnLoadFailed(name, err)
				}
			} else {
				result.Profile = profile
				result.Loaded = true
			}
		}
	}
	if input.Strict && !result.Loaded {
		if input.WarnStrictWithoutProfile != nil {
			input.WarnStrictWithoutProfile()
		}
		result.Profile = zero
	}
	return result
}

// ProfileLoadCallMissingError returns the validation error used when the
// profile selection flow resolves an active profile but no profile loader
// callback is available.
func ProfileLoadCallMissingError() error {
	return errors.New("profile load call is nil")
}

// ProfileNameRequiredError returns the validation error used when a profile
// lookup is requested without a profile name.
func ProfileNameRequiredError() error {
	return errors.New("profile name is required")
}

// ProfileNotFoundError formats the validation error for a missing named
// profile. The caller owns concrete profile lookup order and filesystems.
func ProfileNotFoundError(name string) error {
	return fmt.Errorf("profile %q: not found in <out>/profiles or embedded set", name)
}

// ProfileParseError formats the parse failure for a named search profile.
func ProfileParseError(expectedName string, err error) error {
	return fmt.Errorf("profile %q: parse: %w", expectedName, err)
}

// ProfileNameMismatchError formats the validation error for profile YAML whose
// name field does not match the requested profile name.
func ProfileNameMismatchError(expectedName, actualName string) error {
	return fmt.Errorf("profile %q: name field is %q, expected %q", expectedName, actualName, expectedName)
}

// ProfileNoSourcesError formats the validation error for an empty profile.
func ProfileNoSourcesError(expectedName string) error {
	return fmt.Errorf("profile %q: must declare at least one source", expectedName)
}

// ProfileSourceMissingIDError formats the validation error for a source entry
// without an id in a named profile.
func ProfileSourceMissingIDError(expectedName string) error {
	return fmt.Errorf("profile %q: source entry missing id", expectedName)
}

// ProfileEmptyGlobError returns the validation error used for an empty profile
// include glob.
func ProfileEmptyGlobError() error {
	return errors.New("empty glob")
}

// ProfileSourceGlobError formats a profile include-glob compile failure.
func ProfileSourceGlobError(expectedName, sourceID, glob string, err error) error {
	return fmt.Errorf("profile %q: source %q: glob %q: %w", expectedName, sourceID, glob, err)
}

// FilterStrictProfile drops hits that were not annotated as part of the active
// profile.
func FilterStrictProfile(hits []Hit) []Hit {
	out := hits[:0]
	for _, hit := range hits {
		if hit.InProfile {
			out = append(out, hit)
		}
	}
	return out
}

// ApplyVersionPolicy enriches, filters, scores, and score-sorts hits for the
// pre-rerank version-policy pass. Concrete policy resolution and matching stay
// in the caller while this package owns the runtime-hit mutation order.
func ApplyVersionPolicy(hits []Hit, match VersionPolicyMatcher, score VersionPolicyScorer) []Hit {
	if match == nil {
		return hits
	}
	filtered := hits[:0]
	for _, hit := range hits {
		enriched, ok := match(hit)
		if !ok {
			continue
		}
		if score != nil && !enriched.VersionScoreApplied {
			enriched.Score += score(enriched)
			enriched.VersionScoreApplied = true
		}
		filtered = append(filtered, enriched)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Score != filtered[j].Score {
			return filtered[i].Score > filtered[j].Score
		}
		return filtered[i].Path < filtered[j].Path
	})
	return filtered
}

// FilterByVersionPolicy removes hits that do not match an active hard version
// policy without changing the order of surviving hits. Soft policies are
// handled by the pre-rerank version pass and intentionally no-op here.
func FilterByVersionPolicy(hits []Hit, opts VersionPolicyOptions, match VersionPolicyMatcher) []Hit {
	if !opts.Active {
		return hits
	}
	if opts.SourceID == "" && !opts.LatestOnly && opts.Version == "" {
		return hits
	}
	if match == nil {
		return hits
	}
	filtered := hits[:0]
	for _, hit := range hits {
		matchedHit, ok := match(hit)
		if !ok {
			continue
		}
		filtered = append(filtered, matchedHit)
	}
	return filtered
}

// VersionPolicyNeedsWideCandidatePool reports whether first-stage retrieval
// needs the wide candidate pool used for hard or soft version policies.
func VersionPolicyNeedsWideCandidatePool(state VersionPolicyState) bool {
	return state.SourceFamily != "" || state.SourceID != "" || state.Version != "" || state.LatestOnly
}

// VersionPolicyHeader returns the compact human-facing version policy header.
func VersionPolicyHeader(state VersionPolicyState) string {
	var parts []string
	if state.SourceFamily != "" {
		parts = append(parts, "family:"+state.SourceFamily)
	}
	if state.SourceID != "" {
		parts = append(parts, "source:"+state.SourceID)
	}
	if state.Version != "" {
		parts = append(parts, "override:"+state.Version)
	}
	if state.CwdScope != "" {
		parts = append(parts, "cwd:"+state.CwdScope)
	}
	if state.PreferLatest {
		parts = append(parts, "prefer:latest")
	}
	return strings.Join(parts, ",")
}

// VersionPolicyHitMatches reports whether an enriched runtime hit satisfies a
// provider-neutral version policy. Concrete registry enrichment stays in the
// caller; this function only reads hit metadata.
func VersionPolicyHitMatches(hit Hit, state VersionPolicyState) bool {
	if state.SourceID != "" {
		if hit.SourceID != "" {
			return hit.SourceID == state.SourceID
		}
		return hit.Source == state.SourceID
	}
	if state.SourceFamily != "" && hit.SourceFamily != state.SourceFamily {
		return false
	}
	if state.LatestOnly {
		return hit.VersionLane == "latest"
	}
	if state.Version != "" {
		return hit.DocsVersion == state.Version || strings.TrimPrefix(hit.SourceID, hit.SourceFamily+"__v") == state.Version
	}
	return true
}

// BuildSearchOutputMetadata derives the profile/version header suffix and JSON
// metadata fields shared by text and JSON search output.
func BuildSearchOutputMetadata(opts SearchOutputOptions) SearchOutputMetadata {
	versionHeader := ""
	if header := VersionPolicyHeader(opts.Version); header != "" {
		versionHeader = " version=" + header
	}
	metadata := SearchOutputMetadata{
		Header:     versionHeader,
		Version:    opts.Version.Version,
		PinContext: opts.Version.CwdScope,
	}
	if opts.ProfileName == "" {
		return metadata
	}
	metadata.ProfileMode = "boost"
	if opts.ProfileStrict {
		metadata.ProfileMode = "strict"
	}
	metadata.Header = fmt.Sprintf(" profile=%s (%s; %s)%s", opts.ProfileName, opts.ProfileReason, metadata.ProfileMode, versionHeader)
	return metadata
}

// NewSearchJSONEnvelope builds docs-puller's stable JSON search output object.
func NewSearchJSONEnvelope(query, mode string, scanned int, opts SearchOutputOptions, hits []Hit) SearchJSONEnvelope {
	metadata := BuildSearchOutputMetadata(opts)
	jsonHits := hits
	if opts.Compact && !opts.Explain {
		mode = ""
		metadata.ProfileMode = ""
		metadata.PinContext = ""
		opts.ProfileReason = ""
		jsonHits = CompactSearchJSONHits(hits, opts)
	}
	return SearchJSONEnvelope{
		Query:         query,
		Mode:          mode,
		Scanned:       scanned,
		Profile:       opts.ProfileName,
		ProfileReason: opts.ProfileReason,
		ProfileMode:   metadata.ProfileMode,
		Version:       metadata.Version,
		PinContext:    metadata.PinContext,
		Results:       jsonHits,
	}
}

// CompactSearchJSONHits removes redundant per-hit metadata from compact JSON
// while preserving fields that distinguish path-level versioned pages.
func CompactSearchJSONHits(hits []Hit, opts SearchOutputOptions) []Hit {
	out := make([]Hit, len(hits))
	copy(out, hits)
	for i := range out {
		if out[i].SourceFamily == out[i].Source {
			out[i].SourceFamily = ""
		}
		if out[i].SourceID == out[i].Source {
			out[i].SourceID = ""
		}
		if opts.SourceFilter != "" {
			out[i].InProfile = false
			out[i].InProfileSub = false
		}
	}
	return out
}

// SearchJSONOutput builds docs-puller's stable indented JSON search output.
// The caller still owns stdout emission.
func SearchJSONOutput(query, mode string, scanned int, opts SearchOutputOptions, hits []Hit) (string, error) {
	out, err := json.MarshalIndent(NewSearchJSONEnvelope(query, mode, scanned, opts, hits), "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

// SearchJSONEncodeFailedMessage formats the stderr warning emitted when JSON
// search output cannot be encoded.
func SearchJSONEncodeFailedMessage(err error) string {
	return fmt.Sprintf("search: encode json: %v\n", err)
}

// SearchTextOutput builds docs-puller's stable human-readable search output.
// The caller still owns stdout emission.
func SearchTextOutput(query string, scanned int, hits []Hit, mode string, opts SearchOutputOptions) string {
	header := BuildSearchOutputMetadata(opts).Header
	if len(hits) == 0 {
		return SearchTextNoMatchesMessage(query, scanned, mode, header)
	}
	hits = DedupeSnippetsAcross(hits)
	var b strings.Builder
	b.WriteString(SearchTextMatchesHeaderMessage(len(hits), query, scanned, mode, header))
	for _, hit := range hits {
		b.WriteString(SearchTextHitBlock(hit))
	}
	return b.String()
}

// SearchTextNoMatchesMessage formats the stable text output header emitted
// when a search returns no hits.
func SearchTextNoMatchesMessage(query string, scanned int, mode, header string) string {
	return fmt.Sprintf("no matches for %q across %d docs (%s)%s\n", query, scanned, mode, header)
}

// SearchTextMatchesHeaderMessage formats the stable text output header emitted
// before one or more human-readable search hits.
func SearchTextMatchesHeaderMessage(count int, query string, scanned int, mode, header string) string {
	return fmt.Sprintf("%d match%s for %q across %d docs (%s)%s:\n\n", count, plural(count, "es"), query, scanned, mode, header)
}

// SearchTextHitBlock formats one human-readable search hit, including profile
// badges, URL, snippets, and the blank line separating hits.
func SearchTextHitBlock(hit Hit) string {
	title := hit.Title
	if title == "" {
		title = "(untitled)"
	}
	badge := ""
	if hit.InProfileSub {
		badge = "[profile*] "
	} else if hit.InProfile {
		badge = "[profile] "
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s  [%d]  %s\n", badge, hit.Path, hit.Score, title)
	if hit.URL != "" {
		fmt.Fprintf(&b, "  <%s>\n", hit.URL)
	}
	for _, snippet := range hit.Snippets {
		fmt.Fprintf(&b, "  L%d: %s\n", snippet.Line, snippet.Text)
	}
	b.WriteString("\n")
	return b.String()
}

func plural(n int, suffix string) string {
	if n == 1 {
		return ""
	}
	return suffix
}

// NewSearchTelemetryEntry builds one query-log entry from runtime search
// metadata without taking ownership of JSONL storage.
func NewSearchTelemetryEntry(input SearchTelemetryInput) SearchTelemetryEntry {
	metadata := BuildSearchOutputMetadata(input.Output)
	entry := SearchTelemetryEntry{
		Timestamp:    input.Timestamp.UTC().Format(time.RFC3339),
		Query:        input.Query,
		Intent:       input.Intent,
		SourceFilter: input.SourceFilter,
		Profile:      input.Output.ProfileName,
		Version:      metadata.Version,
		PinContext:   metadata.PinContext,
		Mode:         input.Mode,
		Scanned:      input.Scanned,
		Limit:        input.Limit,
		ResultCount:  len(input.Hits),
		Rerank:       input.Rerank,
		RerankLLM:    input.RerankLLM,
		RerankHybrid: input.RerankHybrid,
	}
	if len(input.Hits) > 0 {
		entry.TopPath = input.Hits[0].Path
		entry.TopSource = input.Hits[0].Source
		entry.TopSourceFamily = input.Hits[0].SourceFamily
	}
	return entry
}

// SearchTelemetryFixtureQueries converts query-log entries into eval-fixture
// candidates. The caller owns query-log storage, exclude-fixture loading, final
// sorting, and YAML encoding.
func SearchTelemetryFixtureQueries(input SearchTelemetryFixtureInput) []SearchTelemetryFixtureQuery {
	seen := map[string]bool{}
	var queries []SearchTelemetryFixtureQuery
	for _, entry := range input.Entries {
		if entry.Query == "" || entry.TopPath == "" {
			continue
		}
		if input.Intent != "" && entry.Intent != input.Intent {
			continue
		}
		if !input.Since.IsZero() {
			ts, err := time.Parse(time.RFC3339, entry.Timestamp)
			if err != nil || ts.Before(input.Since) {
				continue
			}
		}
		key := SearchTelemetryQueryKey(entry.Query)
		if seen[key] {
			continue
		}
		if input.Exclude[key] {
			continue
		}
		seen[key] = true
		noteParts := []string{"telemetry-candidate", "verify expect before promoting"}
		if entry.Intent != "" {
			noteParts = append(noteParts, "intent="+entry.Intent)
		}
		if entry.Mode != "" {
			noteParts = append(noteParts, "mode="+entry.Mode)
		}
		query := SearchTelemetryFixtureQuery{
			Query:  entry.Query,
			Expect: []string{entry.TopPath},
			Note:   strings.Join(noteParts, "; "),
		}
		if entry.SourceFilter != "" {
			query.Source = entry.SourceFilter
		}
		queries = append(queries, query)
	}
	return queries
}

// QueryLogAppendFailedWarning formats the stderr warning emitted when
// caller-owned query-log storage cannot append a telemetry entry.
func QueryLogAppendFailedWarning(err error) string {
	return fmt.Sprintf("query-log: append failed: %v\n", err)
}

// SearchQueryRequiredMessage formats the stderr message emitted when the
// search command is invoked without query terms.
func SearchQueryRequiredMessage() string {
	return "search: query required\n"
}

// SearchProfileLoadFailedWarning formats the stderr warning emitted when
// active profile resolution finds a profile but caller-owned loading fails.
func SearchProfileLoadFailedWarning(name string, err error) string {
	return fmt.Sprintf("search: profile %q failed to load (continuing without profile): %v\n", name, err)
}

// SearchStrictWithoutProfileWarning formats the stderr warning emitted when
// strict profile filtering is requested without an active profile.
func SearchStrictWithoutProfileWarning() string {
	return "search: --strict has no effect without an active profile\n"
}

// VersionPolicyScore preserves docs-puller's version-policy boost weights.
// Hard version constraints only filter; soft policies can boost cwd-pinned,
// latest, workspace-pinned, and tools-pinned candidates before rerank.
func VersionPolicyScore(hit Hit, opts VersionPolicyScoreOptions) int {
	if opts.SourceID != "" || opts.Version != "" || opts.LatestOnly {
		return 0
	}
	if opts.PreferLatest {
		switch hit.VersionLane {
		case "latest":
			return 500
		case "workspace-pinned":
			return 250
		case "tools-pinned":
			return 100
		default:
			return 0
		}
	}
	if opts.CwdPinnedSourceIDs[hit.SourceID] {
		return 600
	}
	switch hit.VersionLane {
	case "workspace-pinned":
		return 450
	case "latest":
		return 300
	case "tools-pinned":
		return 150
	case "other-pinned":
		return 50
	default:
		return 0
	}
}

// VersionPolicyScoreFromState adapts runtime-visible policy state into the
// score options used by the pre-rerank version-policy pass.
func VersionPolicyScoreFromState(hit Hit, state VersionPolicyState, cwdPinnedSourceIDs map[string]bool) int {
	return VersionPolicyScore(hit, VersionPolicyScoreOptions{
		SourceID:           state.SourceID,
		Version:            state.Version,
		LatestOnly:         state.LatestOnly,
		PreferLatest:       state.PreferLatest,
		CwdPinnedSourceIDs: cwdPinnedSourceIDs,
	})
}

// MergeHybridCandidates preserves BM25 hit order and appends embedding-only
// candidate paths for the downstream reranker.
func MergeHybridCandidates(bm25Hits []Hit, candidates []HybridCandidate, title HybridTitleLoader) []Hit {
	seen := map[string]bool{}
	out := make([]Hit, 0, len(bm25Hits)+len(candidates))
	for _, hit := range bm25Hits {
		if seen[hit.Path] {
			continue
		}
		seen[hit.Path] = true
		out = append(out, hit)
	}
	for _, candidate := range candidates {
		if candidate.Path == "" || seen[candidate.Path] {
			continue
		}
		seen[candidate.Path] = true
		hit := Hit{
			Path:   candidate.Path,
			Source: sourceFromPath(candidate.Path),
		}
		if title != nil {
			hit.Title = title(candidate.Path)
		}
		out = append(out, hit)
	}
	return out
}

// RunHybridRetrieval owns the provider-neutral hybrid-retrieval orchestration:
// optionally rewrite with HyDE, embed the query text, prefer a flat vector
// index when present, fall back to the caller-owned vector cache, and merge
// embedding-nearest paths into the BM25 candidate set.
func RunHybridRetrieval[Index EmbeddingRerankIndex](input HybridRetrievalRunInput[Index]) (hits []Hit, err error) {
	apiKeyEnv := input.APIKeyEnv
	if apiKeyEnv == "" {
		apiKeyEnv = "API key"
	}
	if input.APIKey == "" {
		return input.BM25Hits, ProviderAPIKeyNotSetError(apiKeyEnv)
	}
	model := input.Model
	if model == "" {
		model = input.DefaultModel
	}
	if input.EmbedQuery == nil {
		return input.BM25Hits, EmbeddingQueryCallMissingError()
	}
	embedInput := input.Query
	if input.UseHyDE && input.GenerateHyDE != nil {
		if hyde, ok := input.GenerateHyDE(input.Query); ok {
			embedInput = hyde
		}
	}
	vecs, err := input.EmbedQuery(model, input.APIKey, embedInput)
	if err != nil {
		return input.BM25Hits, EmbeddingQueryError(err)
	}
	if len(vecs) != 1 {
		return input.BM25Hits, EmbeddingQueryVectorCountError(len(vecs))
	}
	queryVec := vecs[0]
	k := input.K
	if k <= 0 {
		k = input.DefaultK
	}
	if k <= 0 {
		return input.BM25Hits, nil
	}

	var (
		scored []HybridScoredPath
		ok     bool
	)
	if input.FlatTopK != nil {
		var flatErr error
		scored, ok, flatErr = input.FlatTopK(input.OutDir, model, queryVec, k, input.DepthPenaltyPerSegment)
		if flatErr != nil && input.WarnFlatFallback != nil {
			input.WarnFlatFallback(flatErr)
		}
	}
	if !ok {
		if input.OpenIndex == nil {
			return input.BM25Hits, EmbeddingIndexOpenCallMissingError()
		}
		if input.LoadVectors == nil {
			return input.BM25Hits, EmbeddingLoadVectorsCallMissingError()
		}
		if input.ScoreVector == nil {
			return input.BM25Hits, EmbeddingScoreVectorCallMissingError()
		}
		var index Index
		index, err = input.OpenIndex(input.OutDir)
		if err != nil {
			return input.BM25Hits, EmbeddingIndexOpenError(err)
		}
		defer func() {
			if closeErr := index.Close(); closeErr != nil && err == nil {
				err = EmbeddingIndexCloseError(closeErr)
			}
		}()
		var docVecs map[string][]float32
		docVecs, err = input.LoadVectors(index, model)
		if err != nil {
			return input.BM25Hits, EmbeddingLoadVectorsError(err)
		}
		scored = make([]HybridScoredPath, 0, len(docVecs))
		for path, docVec := range docVecs {
			score := input.ScoreVector(queryVec, docVec)
			depth := strings.Count(path, "/")
			score -= float32(depth) * input.DepthPenaltyPerSegment
			scored = append(scored, HybridScoredPath{Path: path, Score: score})
		}
		sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
		if len(scored) > k {
			scored = scored[:k]
		}
	}

	candidates := make([]HybridCandidate, 0, len(scored))
	for _, candidate := range scored {
		candidates = append(candidates, HybridCandidate{Path: candidate.Path})
	}
	return MergeHybridCandidates(input.BM25Hits, candidates, input.Title), nil
}

// LoadLLMRerankCandidates reads short document excerpts for LLM/provider
// reranking. Missing files keep the candidate with title/path only so rerank
// failures do not discard BM25 results.
func LoadLLMRerankCandidates(hits []Hit, outDir string, snippetChars int) []LLMRerankCandidate {
	out := make([]LLMRerankCandidate, len(hits))
	for i, hit := range hits {
		candidate := LLMRerankCandidate{
			Index: i,
			Path:  hit.Path,
			Title: hit.Title,
		}
		full := filepath.Join(outDir, hit.Path)
		if data, err := os.ReadFile(full); err == nil {
			body := stripFrontmatter(string(data))
			if snippetChars > 0 && len(body) > snippetChars {
				body = body[:snippetChars]
			}
			body = strings.ReplaceAll(strings.TrimSpace(body), "\n", " ")
			candidate.Snippet = body
		}
		out[i] = candidate
	}
	return out
}

func stripFrontmatter(body string) string {
	if !strings.HasPrefix(body, "---") {
		return body
	}
	end := strings.Index(body[3:], "\n---")
	if end < 0 {
		return body
	}
	rest := body[3+end+4:]
	return strings.TrimLeft(rest, "\n")
}

// BuildLLMRerankPrompt builds the provider-independent JSON-mode prompt used
// by OpenAI-compatible LLM-as-judge rerankers.
func BuildLLMRerankPrompt(query string, candidates []LLMRerankCandidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Query: %q\n\nCandidates:\n", query)
	for _, candidate := range candidates {
		fmt.Fprintf(&b, "[%d] title=%q path=%s\n    snippet: %s\n",
			candidate.Index, candidate.Title, candidate.Path, candidate.Snippet)
	}
	b.WriteString("\nReturn ONLY a JSON object with one key, \"order\", whose value is an array of candidate indices (numbers) sorted from MOST to LEAST relevant for the query. Use every candidate exactly once. Prefer canonical reference docs over thematically-related guides. When you see near-duplicate candidates (version-pinned mirrors like `docs/0.81/...` vs `docs/0.82/...`, or sequential tutorial pages like `tutorial-1.md` and `tutorial-2.md`), pick the most canonical version (shorter path, no version prefix) and rank the duplicates last. Example: {\"order\": [3, 0, 1, 2]}")
	return b.String()
}

// LLMRerankDocumentText builds the title+snippet body used by trained
// reranker providers so they receive the same candidate text as the prompt
// path.
func LLMRerankDocumentText(candidate LLMRerankCandidate) string {
	if candidate.Snippet != "" {
		return candidate.Title + "\n" + candidate.Snippet
	}
	return candidate.Title
}

// ResolveRerankProviderPolicy applies docs-puller's supported-reranker policy
// over provider-neutral registry data.
func ResolveRerankProviderPolicy(input RerankProviderPolicyInput) (RerankProviderPolicyResult, error) {
	providerName := input.RequestedProvider
	if providerName == "" {
		providerName = input.DefaultProvider
	}

	provider, ok := findRerankProviderPolicy(input.Providers, providerName)
	if !ok {
		known := make([]string, 0, len(input.Providers))
		for _, p := range input.Providers {
			known = append(known, p.Name)
		}
		return RerankProviderPolicyResult{}, RerankProviderUnknownError(providerName, known)
	}

	if !provider.OpenAICompat && provider.Name != "cohere" && provider.Name != "tei" {
		return RerankProviderPolicyResult{}, RerankProviderUnsupportedError(providerName)
	}

	model := input.RequestedModel
	if model == "" {
		model = input.DefaultModel
	}
	if !rerankProviderHasModel(provider, model) {
		return RerankProviderPolicyResult{}, RerankProviderModelNotRegisteredError(model, provider.Name, provider.PricingModels)
	}

	return RerankProviderPolicyResult{ProviderName: provider.Name, Model: model}, nil
}

func findRerankProviderPolicy(providers []RerankProviderPolicyProvider, name string) (RerankProviderPolicyProvider, bool) {
	for _, provider := range providers {
		if provider.Name == name {
			return provider, true
		}
	}
	return RerankProviderPolicyProvider{}, false
}

func rerankProviderHasModel(provider RerankProviderPolicyProvider, model string) bool {
	for _, candidate := range provider.PricingModels {
		if candidate == model {
			return true
		}
	}
	return false
}

// RerankProviderUnknownError returns the validation error used when a
// requested rerank provider is absent from the provider registry.
func RerankProviderUnknownError(providerName string, known []string) error {
	known = append([]string(nil), known...)
	sort.Strings(known)
	return fmt.Errorf("unknown rerank provider %q (registry has: %v)", providerName, known)
}

// RerankProviderUnsupportedError formats the validation error returned when a
// provider is registered but no rerank adapter is implemented for it.
func RerankProviderUnsupportedError(providerName string) error {
	return fmt.Errorf("provider %q is registered but no rerank adapter is implemented (supported: OpenAI-compatible chat APIs, cohere, tei)", providerName)
}

// RerankProviderModelNotRegisteredError formats the validation error returned
// when a requested rerank model is absent from a provider's registry entry.
func RerankProviderModelNotRegisteredError(model, providerName string, known []string) error {
	known = append([]string(nil), known...)
	sort.Strings(known)
	return fmt.Errorf("model %q not registered for provider %q (registry knows: %v)", model, providerName, known)
}

// RerankProviderRegistryDisappearedError formats the defensive error returned
// when command-owned concrete registry lookup no longer matches policy input.
func RerankProviderRegistryDisappearedError(providerName string) error {
	return fmt.Errorf("rerank provider %q disappeared from registry", providerName)
}

// SecretViaNdev returns a secret from an optional external secrets CLI on PATH
// (best-effort — "" if absent or unset). docs-puller is a standalone module, so
// it shells out rather than importing a host keychain package. Keys set via that
// CLI can be found without an explicit shell export.
func SecretViaNdev(name string) string {
	if name == "" {
		return ""
	}
	ndev, err := exec.LookPath("ndev")
	if err != nil {
		return ""
	}
	out, err := exec.Command(ndev, "secrets", "get", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ResolveEmbeddingAPIKey returns the embedding-provider key from the
// environment, falling back to SecretViaNdev when set.
func ResolveEmbeddingAPIKey() string {
	if v := strings.TrimSpace(os.Getenv(DefaultEmbeddingAPIKeyEnv)); v != "" {
		return v
	}
	return SecretViaNdev(DefaultEmbeddingAPIKeyEnv)
}

// ResolveProviderAPIKey applies docs-puller's provider key policy while the
// caller keeps ownership of concrete environment access. It stays hermetic
// (env-injection only); callers that want the optional secrets CLI fallback
// compose it with SecretViaNdev at their own boundary.
func ResolveProviderAPIKey(input ProviderAPIKeyInput) (string, error) {
	if input.KeyEnv == "" {
		return "", nil
	}
	if input.Getenv == nil {
		return "", ProviderAPIKeyEnvLookupCallMissingError(input.KeyEnv)
	}
	apiKey := input.Getenv(input.KeyEnv)
	if apiKey == "" {
		return "", ProviderAPIKeyNotSetError(input.KeyEnv)
	}
	return apiKey, nil
}

// ProviderAPIKeyEnvLookupCallMissingError formats the validation error returned
// when provider API-key policy needs an environment lookup callback but it is
// absent.
func ProviderAPIKeyEnvLookupCallMissingError(keyEnv string) error {
	return fmt.Errorf("%s env lookup is nil", keyEnv)
}

// ProviderAPIKeyNotSetError formats the validation error returned when a
// required provider API-key environment variable is empty.
func ProviderAPIKeyNotSetError(keyEnv string) error {
	return fmt.Errorf("%s not set", keyEnv)
}

// EmbeddingInputTextCallMissingError returns the validation error used when
// embedding batch fallback needs the input-text callback but it is absent.
func EmbeddingInputTextCallMissingError() error {
	return errors.New("embedding input text call is nil")
}

// EmbeddingBatchCallMissingError returns the validation error used when
// embedding batch fallback needs the embedding batch callback but it is absent.
func EmbeddingBatchCallMissingError() error {
	return errors.New("embedding batch call is nil")
}

// EmbeddingQueryCallMissingError returns the validation error used when the
// query-embedding callback is required but absent.
func EmbeddingQueryCallMissingError() error {
	return errors.New("embed query call is nil")
}

// EmbeddingQueryError formats an error returned by a caller-owned query
// embedding callback while preserving the original error for unwrap checks.
func EmbeddingQueryError(err error) error {
	return fmt.Errorf("embed query: %w", err)
}

// EmbeddingQueryVectorCountError formats the validation error returned when the
// query-embedding provider returns anything other than one vector.
func EmbeddingQueryVectorCountError(count int) error {
	return fmt.Errorf("embed query returned %d vectors", count)
}

// EmbeddingIndexOpenCallMissingError returns the validation error used when the
// embeddings-index opener callback is required but absent.
func EmbeddingIndexOpenCallMissingError() error {
	return errors.New("open embeddings index call is nil")
}

// EmbeddingIndexOpenError formats an error returned by a caller-owned
// embeddings-index opener while preserving the original error for unwrap checks.
func EmbeddingIndexOpenError(err error) error {
	return fmt.Errorf("open embeddings.db: %w", err)
}

// EmbeddingIndexCloseError formats an error returned when closing an
// embeddings index while preserving the original error for unwrap checks.
func EmbeddingIndexCloseError(err error) error {
	return fmt.Errorf("close embeddings index: %w", err)
}

// EmbeddingDBBusyTimeoutError formats an error returned while configuring the
// embeddings database busy timeout.
func EmbeddingDBBusyTimeoutError(err error) error {
	return fmt.Errorf("set busy_timeout: %w", err)
}

// EmbeddingDBJournalModeError formats an error returned while configuring WAL
// mode for a writable embeddings database.
func EmbeddingDBJournalModeError(err error) error {
	return fmt.Errorf("set journal_mode=WAL: %w", err)
}

// EmbeddingDBSchemaEnsureError formats an error returned while ensuring the
// embeddings database schema exists.
func EmbeddingDBSchemaEnsureError(err error) error {
	return fmt.Errorf("ensure embeddings schema: %w", err)
}

// EmbeddingDBPKMigrationError formats an error returned while running the
// embeddings primary-key migration.
func EmbeddingDBPKMigrationError(err error) error {
	return fmt.Errorf("migrate embeddings PK: %w", err)
}

// EmbeddingSchemaReadError formats an error returned while reading the
// embeddings table schema during migration detection.
func EmbeddingSchemaReadError(err error) error {
	return fmt.Errorf("read embeddings schema: %w", err)
}

// EmbeddingMigrationBeginError formats an error returned while starting the
// embeddings schema migration transaction.
func EmbeddingMigrationBeginError(err error) error {
	return fmt.Errorf("begin migration tx: %w", err)
}

// EmbeddingMigrationStepError formats an error returned by a schema migration
// SQL step.
func EmbeddingMigrationStepError(stepPrefix string, err error) error {
	return fmt.Errorf("migrate step %q: %w", stepPrefix, err)
}

// EmbeddingLegacyOpenError formats an error returned while opening the legacy
// search database for embedding migration.
func EmbeddingLegacyOpenError(err error) error {
	return fmt.Errorf("open legacy search.db: %w", err)
}

// EmbeddingLegacyBusyTimeoutError formats an error returned while configuring
// the legacy search database busy timeout during embedding migration.
func EmbeddingLegacyBusyTimeoutError(err error) error {
	return fmt.Errorf("legacy busy_timeout: %w", err)
}

// EmbeddingCacheLoadError formats an error returned by caller-owned embedding
// cache loading while preserving the original error for unwrap checks.
func EmbeddingCacheLoadError(err error) error {
	return fmt.Errorf("load cache: %w", err)
}

// EmbeddingBatchError formats an error returned while embedding a whole-doc or
// chunked batch, while preserving the original error for unwrap checks.
func EmbeddingBatchError(err error) error {
	return fmt.Errorf("embed batch: %w", err)
}

// EmbeddingStoreBatchError formats an error returned while storing a whole-doc
// or chunked embedding batch, while preserving the original error for unwrap checks.
func EmbeddingStoreBatchError(err error) error {
	return fmt.Errorf("store batch: %w", err)
}

// EmbeddingLoadVectorsCallMissingError returns the validation error used when
// hybrid retrieval needs the fallback vector-loader callback but it is absent.
func EmbeddingLoadVectorsCallMissingError() error {
	return errors.New("load vectors call is nil")
}

// EmbeddingLoadVectorsError formats an error returned by a caller-owned hybrid
// vector-cache loader while preserving the original error for unwrap checks.
func EmbeddingLoadVectorsError(err error) error {
	return fmt.Errorf("load hybrid cache: %w", err)
}

// EmbeddingScoreVectorCallMissingError returns the validation error used when
// hybrid retrieval needs the fallback vector-scoring callback but it is absent.
func EmbeddingScoreVectorCallMissingError() error {
	return errors.New("score vector call is nil")
}

// CohereRerankCallMissingError returns the validation error used when the
// Cohere rerank callback is required but absent.
func CohereRerankCallMissingError() error {
	return errors.New("cohere rerank call is nil")
}

// CohereRerankCallError formats an error returned by a caller-owned Cohere
// rerank callback while preserving the original error for unwrap checks.
func CohereRerankCallError(err error) error {
	return fmt.Errorf("cohere call: %w", err)
}

// TEIRerankCallMissingError returns the validation error used when the TEI
// rerank callback is required but absent.
func TEIRerankCallMissingError() error {
	return errors.New("tei rerank call is nil")
}

// TEIRerankCallError formats an error returned by a caller-owned TEI rerank
// callback while preserving the original error for unwrap checks.
func TEIRerankCallError(err error) error {
	return fmt.Errorf("tei call: %w", err)
}

// LLMRerankCallMissingError returns the validation error used when the
// generic LLM rerank callback is required but absent.
func LLMRerankCallMissingError() error {
	return errors.New("llm rerank call is nil")
}

// LLMRerankCallError formats an error returned by a caller-owned generic LLM
// rerank callback while preserving the original error for unwrap checks.
func LLMRerankCallError(err error) error {
	return fmt.Errorf("llm call: %w", err)
}

// RerankHTTPClientMissingError formats the validation error returned when a
// provider ranker needs an HTTP client but none is supplied.
func RerankHTTPClientMissingError(providerName string) error {
	return fmt.Errorf("%s HTTP client is nil", providerName)
}

// RerankReadResponseError formats an error returned while reading a rerank
// provider response body while preserving the original error for unwrap checks.
func RerankReadResponseError(providerName string, err error) error {
	return fmt.Errorf("%s read response: %w", providerName, err)
}

// RerankCloseResponseError formats an error returned while closing a rerank
// provider response body while preserving the original error for unwrap checks.
func RerankCloseResponseError(providerName string, err error) error {
	return fmt.Errorf("%s close response: %w", providerName, err)
}

// RerankStatusError formats a provider rerank HTTP status error. Retryable
// status paths keep the shorter body preview used in retry-loop diagnostics.
func RerankStatusError(providerName string, status int, body string, retryable bool) error {
	limit := 500
	if retryable {
		limit = 200
	}
	return fmt.Errorf("%s status %d: %s", providerName, status, truncateForError(body, limit))
}

// RerankResponseParseError formats a rerank JSON response parse failure while
// preserving the underlying parser error for callers that unwrap.
func RerankResponseParseError(err error) error {
	return fmt.Errorf("parse response: %w", err)
}

// RerankResponseParseBodyError formats a rerank JSON response parse failure
// with a capped body preview while preserving the underlying parser error.
func RerankResponseParseBodyError(err error, body string) error {
	return fmt.Errorf("parse response: %w (body: %s)", err, truncateForError(body, 200))
}

// ChatCompletionNoChoicesError formats the structural parse error returned
// when an OpenAI-compatible chat rerank response has no choices.
func ChatCompletionNoChoicesError() error {
	return fmt.Errorf("no choices in response")
}

// RerankNoResultsError formats the structural parse error returned when a
// provider rerank response contains no rankable results.
func RerankNoResultsError(providerName string) error {
	return fmt.Errorf("no results in %s response", providerName)
}

// ChatCompletionParseOrderError formats an OpenAI-compatible chat rerank
// order parse failure with a capped raw content preview.
func ChatCompletionParseOrderError(err error, raw string) error {
	return fmt.Errorf("parse order: %w (raw: %s)", err, truncateForError(raw, 200))
}

// RerankProviderMessageError formats a provider-reported rerank error payload.
func RerankProviderMessageError(providerName, message string) error {
	return fmt.Errorf("%s error: %s", providerName, message)
}

// CohereRateLimitRetryError formats the Cohere retry-loop diagnostic returned
// after applying retry-after/default wait policy to a 429 response.
func CohereRateLimitRetryError(wait time.Duration) error {
	return fmt.Errorf("cohere 429 rate-limited; waiting %s before retry", wait)
}

// TEIRankerNotReachableError formats the TEI retry-loop diagnostic returned
// when the local rerank endpoint cannot be reached.
func TEIRankerNotReachableError(baseURL, model string, err error) error {
	return fmt.Errorf("tei not reachable at %s (start it with: text-embeddings-router --model-id %s --port 8080): %w", baseURL, model, err)
}

// EmbeddingRerankChunkedCallMissingError returns the validation error used when
// chunked embedding rerank is requested but the callback is absent.
func EmbeddingRerankChunkedCallMissingError() error {
	return errors.New("chunked embedding rerank call is nil")
}

// EmbeddingRerankWholeCallMissingError returns the validation error used when
// whole-document embedding rerank is requested but the callback is absent.
func EmbeddingRerankWholeCallMissingError() error {
	return errors.New("embedding rerank call is nil")
}

// DirectFTSQueryCallMissingError returns the validation error used when the
// direct FTS query callback is required but absent.
func DirectFTSQueryCallMissingError() error {
	return errors.New("direct FTS query call is nil")
}

// SourceFTSQueryCallMissingError returns the validation error used when the
// source-scoped FTS query callback is required but absent.
func SourceFTSQueryCallMissingError() error {
	return errors.New("source FTS query call is nil")
}

// FTSIndexBusyTimeoutError formats an error returned while configuring the
// FTS5 index busy timeout.
func FTSIndexBusyTimeoutError(err error) error {
	return fmt.Errorf("set busy_timeout: %w", err)
}

// FTSIndexJournalModeError formats an error returned while configuring WAL
// mode for the writable FTS5 index.
func FTSIndexJournalModeError(err error) error {
	return fmt.Errorf("set journal_mode=WAL: %w", err)
}

// FTSIndexSynchronousModeError formats an error returned while configuring
// NORMAL synchronous mode for the writable FTS5 index.
func FTSIndexSynchronousModeError(err error) error {
	return fmt.Errorf("set synchronous=NORMAL: %w", err)
}

// FTSIndexMigrateSchemaError formats an error returned while migrating the
// FTS5 index schema.
func FTSIndexMigrateSchemaError(err error) error {
	return fmt.Errorf("migrate schema: %w", err)
}

// FTSIndexInitSchemaError formats an error returned while initializing the
// FTS5 index schema.
func FTSIndexInitSchemaError(err error) error {
	return fmt.Errorf("init schema: %w", err)
}

// UnsupportedFTSReadModeError formats the read-mode validation error shared by
// FTS readers that support only read-only and immutable modes.
func UnsupportedFTSReadModeError(readMode string) error {
	return fmt.Errorf("unsupported FTS read mode %q", readMode)
}

// ImmutableFTSReadRequiresCheckpointError formats the immutable-reader
// precondition failure used when a WAL file contains uncheckpointed writes.
func ImmutableFTSReadRequiresCheckpointError(walPath string) error {
	return fmt.Errorf("immutable FTS read requires checkpointed index: WAL file exists at %s", walPath)
}

// SearchBatchFTSIndexOpenError formats the shared FTS index open error used by
// search-batch.
func SearchBatchFTSIndexOpenError(err error) error {
	return fmt.Errorf("open FTS5 index: %w", err)
}

// SearchBatchFTSDocCountError formats the shared FTS total-doc count error used
// by search-batch.
func SearchBatchFTSDocCountError(err error) error {
	return fmt.Errorf("count FTS5 docs: %w", err)
}

// SearchBatchFTSNotReadyError formats the readiness timeout used before
// search-batch opens the shared FTS5 index.
func SearchBatchFTSNotReadyError(timeout time.Duration) error {
	return fmt.Errorf("FTS5 index not ready after %s", timeout)
}

// SearchBatchEmptyQueryError formats the per-query validation error for
// search-batch rows without a query value.
func SearchBatchEmptyQueryError() error {
	return errors.New("empty query")
}

// SearchBatchNonFTSModeError formats the search-batch invariant failure used
// when a query falls back to a non-FTS mode.
func SearchBatchNonFTSModeError(query string) error {
	return fmt.Errorf("query %q did not use FTS5", query)
}

// BuildRerankUsageEvent computes the cost estimate and stable audit payload
// for one rerank usage event.
func BuildRerankUsageEvent(input RerankUsageEventInput) RerankUsageEvent {
	callSite := input.CallSite
	if callSite == "" {
		callSite = DefaultRerankUsageCallSite
	}
	cents := input.ProviderPricing.EstimateCents(input.InputTokens, input.OutputTokens)
	return RerankUsageEvent{
		ProviderName: input.ProviderName,
		Model:        input.Model,
		CallSite:     callSite,
		InputTokens:  input.InputTokens,
		OutputTokens: input.OutputTokens,
		Cents:        cents,
	}
}

// EmitRerankUsageEvent builds the stable usage event and forwards it to the
// caller-owned recorder when one is supplied.
func EmitRerankUsageEvent(input RerankUsageEventInput, record RerankUsageRecorder) RerankUsageEvent {
	event := BuildRerankUsageEvent(input)
	if record != nil {
		record(event)
	}
	return event
}

// EstimateCents returns a fractional USD-cents estimate for the given token
// usage.
func (p RerankUsagePricing) EstimateCents(inputTokens, outputTokens int) float64 {
	usd := float64(inputTokens)/1_000_000*p.InputPer1M +
		float64(outputTokens)/1_000_000*p.OutputPer1M
	cents := usd * 100
	if cents < 0 {
		return 0
	}
	return cents
}

// RunLLMRerank owns provider-neutral rerank orchestration: load candidate
// snippets, route to the selected provider family, then apply the returned
// rank-order list to the runtime hits.
func RunLLMRerank(input LLMRerankRunInput) ([]Hit, error) {
	snippetChars := input.SnippetChars
	if snippetChars <= 0 {
		snippetChars = DefaultLLMRerankSnippetChars
	}
	candidates := LoadLLMRerankCandidates(input.Hits, input.OutDir, snippetChars)

	var (
		order []int
		err   error
	)
	switch input.ProviderName {
	case "cohere":
		if input.Cohere == nil {
			return nil, CohereRerankCallMissingError()
		}
		order, err = input.Cohere(candidates)
		if err != nil {
			return nil, CohereRerankCallError(err)
		}
	case "tei":
		if input.TEI == nil {
			return nil, TEIRerankCallMissingError()
		}
		order, err = input.TEI(candidates)
		if err != nil {
			return nil, TEIRerankCallError(err)
		}
	default:
		if input.Chat == nil {
			return nil, LLMRerankCallMissingError()
		}
		prompt := BuildLLMRerankPrompt(input.Query, candidates)
		order, err = input.Chat(prompt)
		if err != nil {
			return nil, LLMRerankCallError(err)
		}
	}
	return ApplyLLMRankOrder(input.Hits, order), nil
}

// RunEmbeddingRerank owns provider-neutral embedding rerank orchestration:
// resolve the model/API-key contract, embed the query, open the vector index,
// then route to whole-doc or chunked cosine reranking.
func RunEmbeddingRerank[Index EmbeddingRerankIndex](input EmbeddingRerankRunInput[Index]) (reranked []Hit, err error) {
	apiKeyEnv := input.APIKeyEnv
	if apiKeyEnv == "" {
		apiKeyEnv = "API key"
	}
	if input.APIKey == "" {
		return nil, ProviderAPIKeyNotSetError(apiKeyEnv)
	}
	model := input.Model
	if model == "" {
		model = input.DefaultModel
	}
	if input.EmbedQuery == nil {
		return nil, EmbeddingQueryCallMissingError()
	}
	vecs, err := input.EmbedQuery(model, input.APIKey, input.Query)
	if err != nil {
		return nil, EmbeddingQueryError(err)
	}
	if len(vecs) != 1 {
		return nil, EmbeddingQueryVectorCountError(len(vecs))
	}
	if input.OpenIndex == nil {
		return nil, EmbeddingIndexOpenCallMissingError()
	}
	index, err := input.OpenIndex(input.OutDir)
	if err != nil {
		return nil, EmbeddingIndexOpenError(err)
	}
	defer func() {
		if closeErr := index.Close(); closeErr != nil && err == nil {
			err = EmbeddingIndexCloseError(closeErr)
		}
	}()
	if input.ChunkSize > 0 {
		if input.RerankChunked == nil {
			return nil, EmbeddingRerankChunkedCallMissingError()
		}
		return input.RerankChunked(index, input.Hits, vecs[0], model, input.ChunkSize)
	}
	if input.RerankWhole == nil {
		return nil, EmbeddingRerankWholeCallMissingError()
	}
	return input.RerankWhole(index, input.Hits, vecs[0])
}

type chatCompletionRankerReq struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Temperature    float64 `json:"temperature"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format,omitempty"`
}

type cohereRankerReq struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

type teiRankerReq struct {
	Query     string   `json:"query"`
	Texts     []string `json:"texts"`
	RawScores bool     `json:"raw_scores,omitempty"`
	Truncate  bool     `json:"truncate,omitempty"`
}

// CallChatCompletionRanker calls an OpenAI-compatible chat-completions
// provider and returns the parsed rerank order. The caller supplies HTTP,
// timing, user-agent, and usage-recording dependencies so this package stays
// importable without command globals.
func CallChatCompletionRanker(input ChatCompletionRankerInput) ([]int, error) {
	providerName := input.ProviderName
	if providerName == "" {
		providerName = "provider"
	}
	if input.HTTPClient == nil {
		return nil, RerankHTTPClientMissingError(providerName)
	}
	sleep := input.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	body, err := json.Marshal(chatCompletionRankerReq{
		Model: input.Model,
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{
			{Role: "system", Content: "You rank search results. Return only the JSON object specified."},
			{Role: "user", Content: input.Prompt},
		},
		Temperature: 0,
		ResponseFormat: &struct {
			Type string `json:"type"`
		}{Type: "json_object"},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, input.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+input.APIKey)
	req.Header.Set("User-Agent", input.UserAgent)

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			sleep(time.Duration(1<<attempt) * time.Second)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		resp, err := input.HTTPClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			lastErr = RerankReadResponseError(providerName, readErr)
			continue
		}
		if closeErr != nil {
			lastErr = RerankCloseResponseError(providerName, closeErr)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = RerankStatusError(providerName, resp.StatusCode, string(respBody), true)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, RerankStatusError(providerName, resp.StatusCode, string(respBody), false)
		}
		parsed, err := ParseChatCompletionRankResponse(respBody)
		if err != nil {
			return nil, err
		}
		if parsed.ProviderError != "" {
			return nil, RerankProviderMessageError(providerName, parsed.ProviderError)
		}
		if parsed.Usage.Present && input.RecordUsage != nil {
			input.RecordUsage(parsed.Usage)
		}
		return parsed.Order, nil
	}
	return nil, lastErr
}

// CallCohereRanker calls Cohere v2 /rerank and returns the parsed rerank
// order. The caller owns proactive throttling and usage accounting through
// injected hooks.
func CallCohereRanker(input CohereRankerInput) ([]int, error) {
	if input.HTTPClient == nil {
		return nil, RerankHTTPClientMissingError("cohere")
	}
	sleep := input.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	docs := make([]string, len(input.Candidates))
	for i, candidate := range input.Candidates {
		docs[i] = LLMRerankDocumentText(candidate)
	}
	body, err := json.Marshal(cohereRankerReq{
		Model:     input.Model,
		Query:     input.Query,
		Documents: docs,
		TopN:      len(docs),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, input.BaseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+input.APIKey)
	req.Header.Set("User-Agent", input.UserAgent)

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			sleep(time.Duration(1<<attempt) * time.Second)
		}
		if input.BeforeAttempt != nil {
			input.BeforeAttempt()
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		resp, err := input.HTTPClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			lastErr = RerankReadResponseError("cohere", readErr)
			continue
		}
		if closeErr != nil {
			lastErr = RerankCloseResponseError("cohere", closeErr)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := time.Duration(0)
			if input.RetryAfter != nil {
				wait = input.RetryAfter(resp.Header)
			}
			if wait <= 0 {
				wait = 60 * time.Second
			}
			lastErr = CohereRateLimitRetryError(wait)
			sleep(wait)
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = RerankStatusError("cohere", resp.StatusCode, string(respBody), true)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, RerankStatusError("cohere", resp.StatusCode, string(respBody), false)
		}
		order, providerMessage, err := ParseCohereRankResponse(respBody)
		if err != nil {
			return nil, err
		}
		if providerMessage != "" {
			return nil, RerankProviderMessageError("cohere", providerMessage)
		}
		if input.RecordUsage != nil {
			input.RecordUsage()
		}
		return order, nil
	}
	return nil, lastErr
}

// CallTEIRanker calls a local TEI /rerank endpoint and returns the parsed
// rerank order. The caller owns usage accounting through the injected hook.
func CallTEIRanker(input TEIRankerInput) ([]int, error) {
	if input.HTTPClient == nil {
		return nil, RerankHTTPClientMissingError("tei")
	}
	sleep := input.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	docs := make([]string, len(input.Candidates))
	for i, candidate := range input.Candidates {
		docs[i] = LLMRerankDocumentText(candidate)
	}
	body, err := json.Marshal(teiRankerReq{
		Query:    input.Query,
		Texts:    docs,
		Truncate: true,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, input.BaseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", input.UserAgent)

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			sleep(time.Duration(1<<attempt) * time.Second)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		resp, err := input.HTTPClient.Do(req)
		if err != nil {
			lastErr = TEIRankerNotReachableError(input.BaseURL, input.DefaultModel, err)
			continue
		}
		respBody, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			lastErr = RerankReadResponseError("tei", readErr)
			continue
		}
		if closeErr != nil {
			lastErr = RerankCloseResponseError("tei", closeErr)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = RerankStatusError("tei", resp.StatusCode, string(respBody), true)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, RerankStatusError("tei", resp.StatusCode, string(respBody), false)
		}
		order, err := ParseTEIRankResponse(respBody)
		if err != nil {
			return nil, err
		}
		if input.RecordUsage != nil {
			input.RecordUsage(len(docs))
		}
		return order, nil
	}
	return nil, lastErr
}

// ParseChatCompletionRankResponse parses an OpenAI-compatible
// chat-completions response into the provider-neutral rerank order.
func ParseChatCompletionRankResponse(raw []byte) (ChatCompletionRankResponse, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage,omitempty"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ChatCompletionRankResponse{}, RerankResponseParseError(err)
	}
	if parsed.Error != nil {
		return ChatCompletionRankResponse{ProviderError: parsed.Error.Message}, nil
	}
	if len(parsed.Choices) == 0 {
		return ChatCompletionRankResponse{}, ChatCompletionNoChoicesError()
	}
	content := parsed.Choices[0].Message.Content
	order, err := ParseLLMRankOrder(content)
	if err != nil {
		return ChatCompletionRankResponse{}, ChatCompletionParseOrderError(err, content)
	}
	out := ChatCompletionRankResponse{Order: order}
	if parsed.Usage != nil {
		out.Usage = LLMRankUsage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			Present:          true,
		}
	}
	return out, nil
}

// ParseHyDEChatResponse parses an OpenAI-compatible chat-completions response
// into the provider-neutral HyDE document shape.
func ParseHyDEChatResponse(raw []byte) (HyDEChatResponse, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage,omitempty"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return HyDEChatResponse{}, RerankResponseParseError(err)
	}
	if parsed.Error != nil {
		return HyDEChatResponse{ProviderError: parsed.Error.Message}, nil
	}
	if len(parsed.Choices) == 0 {
		return HyDEChatResponse{}, ChatCompletionNoChoicesError()
	}
	out := HyDEChatResponse{Document: parsed.Choices[0].Message.Content}
	if parsed.Usage != nil {
		out.Usage = LLMRankUsage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			Present:          true,
		}
	}
	return out, nil
}

// ParseCohereRankResponse parses a Cohere v2 /rerank response into a
// provider-neutral rank order.
func ParseCohereRankResponse(raw []byte) ([]int, string, error) {
	var parsed struct {
		Results []struct {
			Index int `json:"index"`
		} `json:"results"`
		Message string `json:"message,omitempty"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, "", RerankResponseParseError(err)
	}
	if parsed.Message != "" {
		return nil, parsed.Message, nil
	}
	if len(parsed.Results) == 0 {
		return nil, "", RerankNoResultsError("cohere")
	}
	order := make([]int, len(parsed.Results))
	for i, result := range parsed.Results {
		order[i] = result.Index
	}
	return order, "", nil
}

// ParseTEIRankResponse parses a TEI /rerank top-level array response into a
// provider-neutral rank order.
func ParseTEIRankResponse(raw []byte) ([]int, error) {
	var results []struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, RerankResponseParseBodyError(err, string(raw))
	}
	if len(results) == 0 {
		return nil, RerankNoResultsError("tei")
	}
	order := make([]int, len(results))
	for i, result := range results {
		order[i] = result.Index
	}
	return order, nil
}

func truncateForError(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// OrderRerankCandidates sorts caller-scored rerank candidates by descending
// rerank score, using the original BM25 rank as the stable tie-breaker.
func OrderRerankCandidates(candidates []RerankOrderCandidate) []Hit {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].BaseRank < candidates[j].BaseRank
	})
	out := make([]Hit, len(candidates))
	for i, candidate := range candidates {
		out[i] = candidate.Hit
	}
	return out
}

// ParseLLMRankOrder parses the provider-independent JSON payload returned by
// LLM-as-judge rerank prompts.
func ParseLLMRankOrder(raw string) ([]int, error) {
	var parsed struct {
		Order []int `json:"order"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, err
	}
	return parsed.Order, nil
}

// ApplyLLMRankOrder applies an LLM/provider rank-order list. Invalid or
// duplicate indices are ignored; omitted hits keep BM25 order at the tail.
func ApplyLLMRankOrder(hits []Hit, order []int) []Hit {
	if len(order) == 0 {
		return hits
	}
	seen := map[int]bool{}
	out := make([]Hit, 0, len(hits))
	for _, idx := range order {
		if idx < 0 || idx >= len(hits) || seen[idx] {
			continue
		}
		seen[idx] = true
		out = append(out, hits[idx])
	}
	for i, hit := range hits {
		if !seen[i] {
			out = append(out, hit)
		}
	}
	return out
}

func sourceFromPath(path string) string {
	if i := strings.IndexByte(path, '/'); i > 0 {
		return path[:i]
	}
	return ""
}

// RelPathInSource strips the leading "<source>/" segment from a path.
func RelPathInSource(path, source string) string {
	prefix := source + "/"
	if strings.HasPrefix(path, prefix) {
		return path[len(prefix):]
	}
	return path
}

// PlanPreRetrieval preserves docs-puller's runtime first-stage candidate
// limit and source-routing decisions.
func PlanPreRetrieval(opts PreRetrievalOptions) PreRetrievalPlan {
	limits := searchdispatch.PlanCandidateLimits(searchdispatch.CandidateLimitPlan{
		UserLimit:              opts.UserLimit,
		CurrentLimit:           opts.CurrentLimit,
		Rerank:                 opts.Rerank,
		RerankLLM:              opts.RerankLLM,
		RerankK:                opts.RerankK,
		HygieneLimit:           opts.HygieneLimit,
		NeedsWideCandidatePool: opts.NeedsWideCandidatePool,
	})
	source := searchdispatch.PlanFirstStageSource(searchdispatch.FirstStageSourcePlan{
		CurrentSource:  opts.CurrentSource,
		PolicySourceID: opts.PolicySourceID,
	})
	return PreRetrievalPlan{
		UserLimit: limits.UserLimit,
		BM25Limit: limits.BM25Limit,
		Source:    source.Source,
	}
}

// PlanCompactOutputPreset applies docs-puller's compact output preset while
// preserving explicit snippet options.
func PlanCompactOutputPreset(opts CompactOutputPresetOptions) CompactOutputPreset {
	preset := searchdispatch.PlanCompactOutputPreset(searchdispatch.CompactOutputPresetPlan{
		Compact:            opts.Compact,
		SnippetLen:         opts.SnippetLen,
		MaxSnippets:        opts.MaxSnippets,
		DefaultSnippetLen:  opts.DefaultSnippetLen,
		DefaultMaxSnippets: opts.DefaultMaxSnippets,
	})
	return CompactOutputPreset{
		SnippetLen:  preset.SnippetLen,
		MaxSnippets: preset.MaxSnippets,
	}
}

// ApplyOutputOverrides post-processes runtime hits for compact/no-snippet and
// snippet-retuning output options.
func ApplyOutputOverrides(hits []Hit, opts OutputOverrideOptions, retune SnippetRetuner) []Hit {
	decision := searchdispatch.PlanOutputOverrides(searchdispatch.OutputOverridePlan{
		Compact:     opts.Compact,
		NoSnippets:  opts.NoSnippets,
		MaxSnippets: opts.MaxSnippets,
		SnippetLen:  opts.SnippetLen,
	})
	if !decision.Apply {
		return hits
	}
	for i := range hits {
		if decision.DropSnippets {
			hits[i].Snippets = nil
		} else if decision.RetuneSnippets && retune != nil {
			if snippets, err := retune(hits[i]); err == nil {
				hits[i].Snippets = snippets
			}
		}
		if decision.DropURL {
			hits[i].URL = ""
		}
	}
	return hits
}

// DedupeSnippetsAcross suppresses snippet text that already appeared in a
// higher-ranked result. It preserves hit order and keeps later hits even when
// all snippets dedupe away.
func DedupeSnippetsAcross(hits []Hit) []Hit {
	seen := make(map[string]struct{})
	for i := range hits {
		if len(hits[i].Snippets) == 0 {
			continue
		}
		kept := hits[i].Snippets[:0]
		for _, snippet := range hits[i].Snippets {
			key := strings.TrimSpace(snippet.Text)
			if key == "" {
				continue
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			kept = append(kept, snippet)
		}
		hits[i].Snippets = kept
	}
	return hits
}

// RunPipeline preserves docs-puller's runtime dispatch order: plan
// pre-retrieval limits/source, run the selected backend, run post-retrieval
// stages, then apply output shaping.
func RunPipeline[Index any](input Pipeline[Index]) PipelineResult {
	pre := PlanPreRetrieval(input.PreRetrieval)
	result := PipelineResult{PreRetrieval: pre}

	retrieval := RunBackendRetrieval(input.Backend(pre))
	result.Hits = retrieval.Hits
	result.Scanned = retrieval.Scanned
	result.Mode = retrieval.Mode
	result.FTSQueryErr = retrieval.FTSQueryErr
	result.WarnMissingIndex = retrieval.WarnMissingIndex
	result.WarnQueryError = retrieval.WarnQueryError
	result.Degraded = retrieval.Degraded
	if result.Degraded {
		return result
	}

	if input.PostRetrieval != nil {
		post := input.PostRetrieval(pre)
		post.Hits = result.Hits
		post.Mode = result.Mode
		processed := RunPostRetrievalProcessing(post)
		result.Hits = processed.Hits
		result.Mode = processed.Mode
		result.HybridErr = processed.HybridErr
		result.RerankErr = processed.RerankErr
		result.WarnHybridFallback = processed.WarnHybridFallback
		result.WarnRerankFallback = processed.WarnRerankFallback
	}
	result.Hits = ApplyOutputOverrides(result.Hits, input.Output, input.Retune)
	return result
}

// PipelineWarnings converts pipeline fallback state into stable warning text.
func PipelineWarnings(result PipelineResult) []PipelineWarning {
	var warnings []PipelineWarning
	if result.WarnQueryError {
		warnings = append(warnings, PipelineWarning{
			Message: fmt.Sprintf("fts5: %v — falling back to scan", result.FTSQueryErr),
		})
	}
	if result.WarnMissingIndex {
		warnings = append(warnings, PipelineWarning{
			Message: "search: FTS5 index missing — falling back to scan. Run `docs-puller reindex` to build it.",
		})
	}
	if result.WarnHybridFallback {
		warnings = append(warnings, PipelineWarning{
			Message: fmt.Sprintf("rerank-hybrid: %v — using BM25 candidates only", result.HybridErr),
		})
	}
	if result.WarnRerankFallback {
		warnings = append(warnings, PipelineWarning{
			Message: fmt.Sprintf("rerank: %v — falling back to BM25 order", result.RerankErr),
		})
	}
	return warnings
}

// PipelineWarningsText formats pipeline fallback warnings for stderr output.
func PipelineWarningsText(result PipelineResult) string {
	var b strings.Builder
	for _, warning := range PipelineWarnings(result) {
		b.WriteString(warning.Message)
		b.WriteByte('\n')
	}
	return b.String()
}

// RunBackendRetrieval preserves docs-puller's runtime-hit first-stage
// retrieval order while keeping concrete index operations supplied by the
// caller.
func RunBackendRetrieval[Index any](input BackendRetrieval[Index]) BackendRetrievalResult {
	retrieval := searchdispatch.RunBackendRetrieval(searchdispatch.BackendRetrieval[Hit, Index]{
		Plan: searchdispatch.BackendRetrievalPlan{
			UseScan:             input.Options.UseScan,
			FTSOnly:             input.Options.FTSOnly,
			SharedIndexProvided: input.Options.SharedIndexProvided,
			CachedTotalDocs:     input.Options.CachedTotalDocs,
		},
		SharedIndex: input.SharedIndex,
		OpenLocal:   input.OpenLocal,
		Query:       input.Query,
		Count:       input.Count,
		Close:       input.Close,
		Scan:        input.Scan,
	})
	return BackendRetrievalResult{
		Hits:             retrieval.Hits,
		Scanned:          retrieval.Scanned,
		Mode:             retrieval.Mode,
		FTSQueryErr:      retrieval.FTSQueryErr,
		WarnMissingIndex: retrieval.WarnMissingIndex,
		WarnQueryError:   retrieval.WarnQueryError,
		Degraded:         retrieval.Degraded,
	}
}

// RunVersionPolicyFTSQuery owns the source-family fan-out merge used when a
// version policy asks FTS to search every pinned source ID in one family.
func RunVersionPolicyFTSQuery(input VersionPolicyFTSQueryInput) ([]Hit, error) {
	if !input.UseFamilyFanout || len(input.SourceIDs) == 0 {
		if input.Direct == nil {
			return nil, DirectFTSQueryCallMissingError()
		}
		return input.Direct()
	}
	if input.SearchSource == nil {
		return nil, SourceFTSQueryCallMissingError()
	}
	byPath := map[string]Hit{}
	for _, sourceID := range input.SourceIDs {
		hits, err := input.SearchSource(sourceID)
		if err != nil {
			return nil, err
		}
		for _, hit := range hits {
			existing, ok := byPath[hit.Path]
			if !ok || hit.Score > existing.Score {
				byPath[hit.Path] = hit
			}
		}
	}
	out := make([]Hit, 0, len(byPath))
	for _, hit := range byPath {
		out = append(out, hit)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// RunPostRetrievalProcessing preserves docs-puller's runtime-hit
// post-retrieval order while keeping concrete version/rerank implementations
// supplied by the caller.
func RunPostRetrievalProcessing(input PostRetrievalProcessing) PostRetrievalProcessingResult {
	var sourceHygiene func([]Hit) []Hit
	if input.SourceHygienePenalty != nil {
		sourceHygiene = func(hits []Hit) []Hit {
			return ApplySourceHygiene(hits, input.Options.UserLimit, input.SourceHygienePenalty)
		}
	}
	processed := searchdispatch.RunPostRetrievalProcessing(searchdispatch.PostRetrievalProcessing[Hit]{
		Hits: input.Hits,
		Mode: input.Mode,
		Plan: searchdispatch.PostRetrievalProcessingPlan{
			VersionPolicy: searchdispatch.VersionPolicyProcessingPlan{
				PolicyActive: input.Options.VersionPolicy.Active,
				SourceID:     input.Options.VersionPolicy.SourceID,
				LatestOnly:   input.Options.VersionPolicy.LatestOnly,
				Version:      input.Options.VersionPolicy.Version,
			},
			Profile: searchdispatch.ProfileProcessingPlan{
				Strict:        input.Options.ProfileStrict,
				ProfileActive: input.Profile != nil,
			},
			Rerank:          input.Options.Rerank,
			RerankLLM:       input.Options.RerankLLM,
			RerankHybrid:    input.Options.RerankHybrid,
			RerankChunkSize: input.Options.RerankChunkSize,
		},
		ApplyVersionPolicy: input.ApplyVersionPolicy,
		AnnotateProfile: func(hits []Hit) []Hit {
			return AnnotateProfileMembership(hits, input.Profile)
		},
		StrictProfileFilter: FilterStrictProfile,
		// Gate both hybrid expansion and rerank when BM25 has a clear winner;
		// otherwise hybrid can flood a good keyword match with broader semantic
		// neighbors before the reranker runs.
		BM25Confident: func(hits []Hit) bool {
			return BM25IsConfident(hits, input.Options.RerankGate)
		},
		Hybrid:                  input.Hybrid,
		EmbeddingRerank:         input.EmbeddingRerank,
		LLMRerank:               input.LLMRerank,
		PostRerankVersionFilter: input.PostRerankVersionFilter,
		SourceHygiene:           sourceHygiene,
	})
	return PostRetrievalProcessingResult{
		Hits:               processed.Hits,
		Mode:               processed.Mode,
		HybridErr:          processed.HybridErr,
		RerankErr:          processed.RerankErr,
		WarnHybridFallback: processed.WarnHybridFallback,
		WarnRerankFallback: processed.WarnRerankFallback,
	}
}

// HitsToCore converts runtime hits to the public searchcore result shape.
func HitsToCore(hits []Hit) []searchcore.Hit {
	out := make([]searchcore.Hit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, searchcore.Hit{
			Path:         hit.Path,
			Source:       hit.Source,
			SourceFamily: hit.SourceFamily,
			SourceID:     hit.SourceID,
			DocsVersion:  hit.DocsVersion,
			Title:        hit.Title,
			URL:          hit.URL,
			Score:        hit.Score,
			Snippets:     SnippetsToCore(hit.Snippets),
		})
	}
	return out
}

// SnippetsToCore converts runtime snippets to the public searchcore shape.
func SnippetsToCore(snippets []Snippet) []searchcore.Snippet {
	out := make([]searchcore.Snippet, 0, len(snippets))
	for _, snippet := range snippets {
		out = append(out, searchcore.Snippet{
			Line: snippet.Line,
			Text: snippet.Text,
		})
	}
	return out
}
