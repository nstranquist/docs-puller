// Command demo-smoke verifies the public docs-puller demo from outside both
// providers. It uses one fixed synthetic query and never emits document text.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	serviceName      = "docs-puller-demo"
	fixedQuery       = "How do external content FTS5 tables work?"
	maxResponseBytes = 128 << 10
)

var allowedSources = map[string]bool{
	"go":         true,
	"postgresql": true,
	"sqlite":     true,
}

var allowedSourceHosts = map[string]map[string]bool{
	"go":         {"go.dev": true},
	"postgresql": {"postgresql.org": true, "www.postgresql.org": true},
	"sqlite":     {"sqlite.org": true, "www.sqlite.org": true},
}

var allowedSourceLicenses = map[string]string{
	"go":         "CC BY 4.0 website content, unless noted otherwise",
	"postgresql": "PostgreSQL documentation license",
	"sqlite":     "Public domain documentation",
}

type config struct {
	BaseURL         string
	OriginURL       string
	ExpectedCommit  string
	ExpectedCorpus  string
	ExpectedVersion string
	MaxLatency      time.Duration
	AllowHTTP       bool
}

type report struct {
	SchemaVersion int           `json:"schema_version"`
	OK            bool          `json:"ok"`
	CheckedAt     string        `json:"checked_at"`
	BaseURL       string        `json:"base_url"`
	BuildID       string        `json:"build_id,omitempty"`
	Commit        string        `json:"commit,omitempty"`
	CorpusDigest  string        `json:"corpus_digest,omitempty"`
	Checks        []checkResult `json:"checks"`
}

type checkResult struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
	RequestID string `json:"request_id,omitempty"`
	Failure   string `json:"failure,omitempty"`
}

type healthResponse struct {
	OK      bool   `json:"ok"`
	Service string `json:"service"`
	BuildID string `json:"build_id"`
}

type corpusIdentity struct {
	ID            string `json:"id"`
	Digest        string `json:"digest"`
	IndexDigest   string `json:"index_digest"`
	DocumentCount int    `json:"document_count"`
	SourceCount   int    `json:"source_count"`
	RetrievedAt   string `json:"retrieved_at"`
}

type readinessResponse struct {
	OK        bool           `json:"ok"`
	Service   string         `json:"service"`
	Origin    string         `json:"origin"`
	Corpus    corpusIdentity `json:"corpus"`
	CheckedAt string         `json:"checked_at"`
}

type metadataResponse struct {
	OK            bool           `json:"ok"`
	SchemaVersion int            `json:"schema_version"`
	Service       string         `json:"service"`
	Engine        engineIdentity `json:"engine"`
	BuildID       string         `json:"build_id"`
	Commit        string         `json:"commit"`
	DeployedAt    string         `json:"deployed_at"`
	Corpus        corpusIdentity `json:"corpus"`
	Limits        limits         `json:"limits"`
}

type engineIdentity struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Mode    string `json:"mode"`
}

type limits struct {
	QueryCharacters int `json:"query_characters"`
	Results         int `json:"results"`
	TimeoutMS       int `json:"timeout_ms"`
	ResponseBytes   int `json:"response_bytes"`
}

type sourcesResponse struct {
	OK      bool           `json:"ok"`
	Corpus  corpusIdentity `json:"corpus"`
	Sources []source       `json:"sources"`
}

type source struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	DocumentCount int    `json:"document_count"`
	Homepage      string `json:"homepage"`
	License       string `json:"license"`
}

type searchResponse struct {
	OK          bool           `json:"ok"`
	Query       string         `json:"query"`
	Engine      string         `json:"engine"`
	Mode        string         `json:"mode"`
	ElapsedMS   int            `json:"elapsed_ms"`
	ResultCount int            `json:"result_count"`
	Corpus      corpusIdentity `json:"corpus"`
	Results     []searchResult `json:"results"`
}

type searchResult struct {
	Title    string    `json:"title"`
	Source   string    `json:"source"`
	Path     string    `json:"path"`
	URL      string    `json:"url"`
	Score    int       `json:"score"`
	Snippets []snippet `json:"snippets"`
}

type snippet struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

type documentResponse struct {
	OK          bool           `json:"ok"`
	Source      string         `json:"source"`
	Path        string         `json:"path"`
	Title       string         `json:"title"`
	URL         string         `json:"url"`
	ContentType string         `json:"content_type"`
	Content     string         `json:"content"`
	Bytes       int            `json:"bytes"`
	TotalBytes  int            `json:"total_bytes"`
	Truncated   bool           `json:"truncated"`
	StartLine   int            `json:"start_line"`
	EndLine     int            `json:"end_line"`
	TotalLines  int            `json:"total_lines"`
	Corpus      corpusIdentity `json:"corpus"`
}

type verifier struct {
	config  config
	baseURL *url.URL
	client  *http.Client
	report  report
}

func main() {
	var cfg config
	flag.StringVar(&cfg.BaseURL, "base-url", "https://docs-puller-demo.darthbitcoin.workers.dev", "public demo base URL")
	flag.StringVar(&cfg.OriginURL, "origin-url", "", "optional Fly origin URL to verify rejects anonymous requests")
	flag.StringVar(&cfg.ExpectedCommit, "expect-commit", "", "required deployment commit when set")
	flag.StringVar(&cfg.ExpectedCorpus, "expect-corpus", "", "required corpus SHA-256 when set")
	flag.StringVar(&cfg.ExpectedVersion, "expect-version", "", "required docs-puller version when set")
	flag.DurationVar(&cfg.MaxLatency, "max-latency", 5*time.Second, "maximum end-to-end latency for one request")
	flag.BoolVar(&cfg.AllowHTTP, "allow-http", false, "allow HTTP for an isolated local test")
	flag.Parse()
	if flag.NArg() != 0 {
		fatalf("unexpected arguments")
	}

	result, err := run(context.Background(), cfg, nil)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if encodeErr := encoder.Encode(result); encodeErr != nil {
		fatalf("write report: %v", encodeErr)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo-smoke: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config, client *http.Client) (report, error) {
	baseURL, err := validateBaseURL(cfg.BaseURL, cfg.AllowHTTP)
	if err != nil {
		return report{}, err
	}
	if cfg.MaxLatency <= 0 || cfg.MaxLatency > 30*time.Second {
		return report{}, fmt.Errorf("max latency must be greater than zero and no more than 30s")
	}
	if client == nil {
		client = &http.Client{
			Timeout: cfg.MaxLatency,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	v := &verifier{
		config:  cfg,
		baseURL: baseURL,
		client:  client,
		report: report{
			SchemaVersion: 1,
			CheckedAt:     time.Now().UTC().Format(time.RFC3339Nano),
			BaseURL:       strings.TrimRight(baseURL.String(), "/"),
			Checks:        make([]checkResult, 0, 12),
		},
	}
	if err := v.verify(ctx); err != nil {
		return v.report, err
	}
	v.report.OK = true
	return v.report, nil
}

func (v *verifier) verify(ctx context.Context) error {
	var health healthResponse
	if err := v.getJSON(ctx, "health", "/healthz", &health); err != nil {
		return err
	}
	if !health.OK || health.Service != serviceName || strings.TrimSpace(health.BuildID) == "" {
		return v.failLast("health response has an invalid service identity")
	}

	var readiness readinessResponse
	if err := v.getJSON(ctx, "readiness", "/readyz", &readiness); err != nil {
		return err
	}
	if !readiness.OK || readiness.Service != serviceName || readiness.Origin != "ready" {
		return v.failLast("readiness response is not ready")
	}
	if err := validateCorpus(readiness.Corpus); err != nil {
		return v.failLast(err.Error())
	}

	var metadata metadataResponse
	if err := v.getJSON(ctx, "metadata", "/api/v1/demo/meta", &metadata); err != nil {
		return err
	}
	if err := v.validateMetadata(metadata); err != nil {
		return v.failLast(err.Error())
	}
	if health.BuildID != metadata.BuildID {
		return v.failLast("health and metadata build IDs differ")
	}
	if readiness.Corpus != metadata.Corpus {
		return v.failLast("readiness and metadata corpus identities differ")
	}
	v.report.BuildID = metadata.BuildID
	v.report.Commit = metadata.Commit
	v.report.CorpusDigest = metadata.Corpus.Digest

	var sources sourcesResponse
	if err := v.getJSON(ctx, "sources", "/api/v1/demo/sources", &sources); err != nil {
		return err
	}
	if err := validateSources(sources, metadata.Corpus.Digest); err != nil {
		return v.failLast(err.Error())
	}

	searchPath := "/api/v1/demo/search?q=" + url.QueryEscape(fixedQuery) + "&source=sqlite&limit=3&mode=fts5"
	var search searchResponse
	if err := v.getJSON(ctx, "search", searchPath, &search); err != nil {
		return err
	}
	if err := validateSearch(search, metadata.Corpus.Digest); err != nil {
		return v.failLast(err.Error())
	}

	first := search.Results[0]
	documentPath := "/api/v1/demo/doc?source=" + url.QueryEscape(first.Source) + "&path=" + url.QueryEscape(first.Path) + "&line=" + strconv.Itoa(first.Snippets[0].Line)
	var document documentResponse
	if err := v.getJSON(ctx, "document", documentPath, &document); err != nil {
		return err
	}
	if err := validateDocument(document, first, metadata.Corpus.Digest); err != nil {
		return v.failLast(err.Error())
	}

	for _, path := range []string{"/", "/demo/", "/method/", "/robots.txt", "/sitemap.xml"} {
		if err := v.getAsset(ctx, path); err != nil {
			return err
		}
	}
	if err := v.verifyCORS(ctx); err != nil {
		return err
	}
	if v.config.OriginURL != "" {
		if err := v.verifyOriginRejectsAnonymous(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (v *verifier) validateMetadata(metadata metadataResponse) error {
	if !metadata.OK || metadata.SchemaVersion != 1 || metadata.Service != serviceName {
		return fmt.Errorf("metadata has an invalid service identity")
	}
	if metadata.Engine.Name != "docs-puller" || metadata.Engine.Mode != "fts5" || metadata.Engine.Version == "" {
		return fmt.Errorf("metadata has an invalid engine identity")
	}
	if metadata.BuildID == "" || len(metadata.Commit) < 7 {
		return fmt.Errorf("metadata has an invalid build identity")
	}
	if _, err := time.Parse(time.RFC3339Nano, metadata.DeployedAt); err != nil {
		return fmt.Errorf("metadata has an invalid deployment time")
	}
	if err := validateCorpus(metadata.Corpus); err != nil {
		return err
	}
	if metadata.Limits != (limits{QueryCharacters: 160, Results: 10, TimeoutMS: 4000, ResponseBytes: 65536}) {
		return fmt.Errorf("metadata limits do not match the public contract")
	}
	if v.config.ExpectedCommit != "" && metadata.Commit != v.config.ExpectedCommit {
		return fmt.Errorf("deployment commit %s does not match the expected commit", metadata.Commit)
	}
	if v.config.ExpectedCorpus != "" && metadata.Corpus.Digest != v.config.ExpectedCorpus {
		return fmt.Errorf("corpus digest does not match the expected lock")
	}
	if v.config.ExpectedVersion != "" && metadata.Engine.Version != v.config.ExpectedVersion {
		return fmt.Errorf("engine version %s does not match the expected version", metadata.Engine.Version)
	}
	return nil
}

func validateCorpus(corpus corpusIdentity) error {
	if corpus.ID != "public-sample-v1" || corpus.DocumentCount != 24 || corpus.SourceCount != 3 {
		return fmt.Errorf("corpus identity is outside the reviewed boundary")
	}
	if !validDigest(corpus.Digest) || !validDigest(corpus.IndexDigest) {
		return fmt.Errorf("corpus digest is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, corpus.RetrievedAt); err != nil {
		return fmt.Errorf("corpus retrieval time is invalid")
	}
	return nil
}

func validateSources(response sourcesResponse, corpusDigest string) error {
	if !response.OK || len(response.Sources) != len(allowedSources) {
		return fmt.Errorf("source catalog is not the closed three-source set")
	}
	seen := make(map[string]bool, len(response.Sources))
	for _, source := range response.Sources {
		if !allowedSources[source.ID] || seen[source.ID] || source.DocumentCount != 8 || source.Label == "" || source.License != allowedSourceLicenses[source.ID] {
			return fmt.Errorf("source catalog contains an invalid source")
		}
		homepage, err := url.Parse(source.Homepage)
		if err != nil || homepage.Scheme != "https" || !allowedSourceHosts[source.ID][homepage.Hostname()] {
			return fmt.Errorf("source catalog contains an invalid homepage")
		}
		seen[source.ID] = true
	}
	if err := validateCorpus(response.Corpus); err != nil {
		return err
	}
	if response.Corpus.Digest != corpusDigest {
		return fmt.Errorf("source catalog used an unexpected corpus")
	}
	return nil
}

func validateSearch(response searchResponse, corpusDigest string) error {
	if !response.OK || response.Query != fixedQuery || response.Engine != "docs-puller" || response.Mode != "fts5" {
		return fmt.Errorf("search response has an invalid engine contract")
	}
	if response.ResultCount != len(response.Results) || len(response.Results) == 0 || len(response.Results) > 3 {
		return fmt.Errorf("search response has an invalid result count")
	}
	if response.Corpus.Digest != corpusDigest {
		return fmt.Errorf("search used an unexpected corpus")
	}
	for _, result := range response.Results {
		if !allowedSources[result.Source] || result.Title == "" || result.Path == "" || len(result.Snippets) == 0 || len(result.Snippets) > 3 {
			return fmt.Errorf("search returned an invalid result")
		}
		sourceURL, err := url.Parse(result.URL)
		if err != nil || sourceURL.Scheme != "https" || !allowedSourceHosts[result.Source][sourceURL.Hostname()] {
			return fmt.Errorf("search returned an invalid source URL")
		}
	}
	return nil
}

func validateDocument(document documentResponse, result searchResult, corpusDigest string) error {
	if !document.OK || document.Source != result.Source || document.Path != result.Path || document.Title == "" || document.ContentType != "text/markdown" {
		return fmt.Errorf("document response does not match its search result")
	}
	if document.Bytes < 1 || document.Bytes > 32000 || len(document.Content) != document.Bytes || document.TotalBytes < document.Bytes || document.TotalBytes > 2<<20 {
		return fmt.Errorf("document response exceeds the public content boundary")
	}
	if !document.Truncated || document.Bytes >= document.TotalBytes || document.StartLine < 1 || document.StartLine > result.Snippets[0].Line || document.EndLine < result.Snippets[0].Line || document.EndLine > document.TotalLines {
		return fmt.Errorf("document response does not contain the matched excerpt")
	}
	if document.Corpus.Digest != corpusDigest {
		return fmt.Errorf("document used an unexpected corpus")
	}
	if document.URL != result.URL {
		return fmt.Errorf("document source URL does not match its search result")
	}
	return nil
}

func (v *verifier) getJSON(ctx context.Context, name, path string, target any) error {
	request, err := v.newRequest(ctx, path)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, result, err := v.do(request, name, routeOnly(path))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		if closeErr := closeResponse(response.Body); closeErr != nil {
			return v.fail(result, fmt.Sprintf("unexpected HTTP status %d and unreadable body", response.StatusCode))
		}
		return v.fail(result, fmt.Sprintf("unexpected HTTP status %d", response.StatusCode))
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		if closeErr := closeResponse(response.Body); closeErr != nil {
			return v.fail(result, "response is not JSON and its body could not be closed")
		}
		return v.fail(result, "response is not JSON")
	}
	data, err := readAndClose(response.Body)
	if err != nil {
		return v.fail(result, "response body could not be read")
	}
	if err := decodeStrictJSON(bytes.NewReader(data), target); err != nil {
		return v.fail(result, "response does not match the closed JSON contract")
	}
	if err := validateSecurityHeaders(response.Header); err != nil {
		return v.fail(result, err.Error())
	}
	v.pass(result)
	return nil
}

func (v *verifier) getAsset(ctx context.Context, path string) error {
	request, err := v.newRequest(ctx, path)
	if err != nil {
		return err
	}
	response, result, err := v.do(request, "asset", path)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		if closeErr := closeResponse(response.Body); closeErr != nil {
			return v.fail(result, fmt.Sprintf("asset returned HTTP %d with an unreadable body", response.StatusCode))
		}
		return v.fail(result, fmt.Sprintf("asset returned HTTP %d", response.StatusCode))
	}
	data, err := readAndClose(response.Body)
	if err != nil {
		return v.fail(result, "asset body could not be read")
	}
	if len(data) == 0 {
		return v.fail(result, "asset body is empty")
	}
	if err := validateSecurityHeaders(response.Header); err != nil {
		return v.fail(result, err.Error())
	}
	v.pass(result)
	return nil
}

func (v *verifier) verifyCORS(ctx context.Context) error {
	request, err := v.newRequest(ctx, "/api/v1/demo/meta")
	if err != nil {
		return err
	}
	request.Header.Set("Origin", "https://untrusted.example")
	response, result, err := v.do(request, "cors", "/api/v1/demo/meta")
	if err != nil {
		return err
	}
	if err := closeResponse(response.Body); err != nil {
		return v.fail(result, "CORS response body could not be closed")
	}
	if response.StatusCode != http.StatusForbidden || response.Header.Get("Access-Control-Allow-Origin") != "" {
		return v.fail(result, "untrusted cross-origin request was not rejected")
	}
	v.pass(result)
	return nil
}

func (v *verifier) verifyOriginRejectsAnonymous(ctx context.Context) error {
	origin, err := validateBaseURL(v.config.OriginURL, v.config.AllowHTTP)
	if err != nil {
		return fmt.Errorf("origin URL: %w", err)
	}
	requestURL := *origin
	requestURL.Path = "/api/status"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return err
	}
	response, result, err := v.do(request, "origin-auth", "/api/status")
	if err != nil {
		return err
	}
	if err := closeResponse(response.Body); err != nil {
		return v.fail(result, "origin response body could not be closed")
	}
	if response.StatusCode != http.StatusUnauthorized {
		return v.fail(result, fmt.Sprintf("anonymous origin returned HTTP %d", response.StatusCode))
	}
	v.pass(result)
	return nil
}

func (v *verifier) newRequest(ctx context.Context, path string) (*http.Request, error) {
	reference, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	requestURL := v.baseURL.ResolveReference(reference)
	if requestURL.Host != v.baseURL.Host {
		return nil, fmt.Errorf("request path escaped the public origin")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "docs-puller-demo-smoke/1")
	request.Header.Set("X-Docs-Puller-Probe", "synthetic")
	return request, nil
}

func (v *verifier) do(request *http.Request, name, path string) (*http.Response, checkResult, error) {
	started := time.Now()
	response, err := v.client.Do(request)
	elapsed := time.Since(started)
	result := checkResult{Name: name, Path: path, ElapsedMS: elapsed.Milliseconds()}
	if err != nil {
		result.Failure = "request failed"
		v.report.Checks = append(v.report.Checks, result)
		return nil, result, fmt.Errorf("%s: request failed: %w", name, err)
	}
	result.Status = response.StatusCode
	result.RequestID = response.Header.Get("X-Request-ID")
	if elapsed > v.config.MaxLatency {
		if closeErr := closeResponse(response.Body); closeErr != nil {
			return nil, result, v.fail(result, "request exceeded the latency limit and its body did not close")
		}
		return nil, result, v.fail(result, "request exceeded the latency limit")
	}
	return response, result, nil
}

func (v *verifier) pass(result checkResult) {
	result.OK = true
	v.report.Checks = append(v.report.Checks, result)
}

func (v *verifier) fail(result checkResult, reason string) error {
	result.Failure = reason
	v.report.Checks = append(v.report.Checks, result)
	return fmt.Errorf("%s: %s", result.Name, reason)
}

func (v *verifier) failLast(reason string) error {
	if len(v.report.Checks) == 0 {
		return errors.New(reason)
	}
	last := &v.report.Checks[len(v.report.Checks)-1]
	last.OK = false
	last.Failure = reason
	return fmt.Errorf("%s: %s", last.Name, reason)
}

func validateBaseURL(value string, allowHTTP bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("base URL is invalid")
	}
	if parsed.Scheme != "https" && !(allowHTTP && parsed.Scheme == "http") {
		return nil, fmt.Errorf("base URL must use HTTPS")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/"
	return parsed, nil
}

func validateSecurityHeaders(headers http.Header) error {
	required := map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
	}
	keys := make([]string, 0, len(required))
	for key := range required {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !strings.Contains(headers.Get(key), required[key]) {
			return fmt.Errorf("required security header %s is missing", key)
		}
	}
	return nil
}

func decodeStrictJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("response contains trailing JSON")
	}
	return nil
}

func readAndClose(body io.ReadCloser) ([]byte, error) {
	data, readErr := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	closeErr := body.Close()
	if len(data) > maxResponseBytes {
		return nil, errors.Join(fmt.Errorf("response exceeds %d bytes", maxResponseBytes), closeErr)
	}
	return data, errors.Join(readErr, closeErr)
}

func closeResponse(body io.ReadCloser) error {
	_, readErr := io.Copy(io.Discard, io.LimitReader(body, maxResponseBytes+1))
	return errors.Join(readErr, body.Close())
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range strings.TrimPrefix(value, "sha256:") {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func routeOnly(path string) string {
	parsed, err := url.Parse(path)
	if err != nil {
		return path
	}
	return parsed.Path
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "demo-smoke: "+format+"\n", args...)
	os.Exit(1)
}
