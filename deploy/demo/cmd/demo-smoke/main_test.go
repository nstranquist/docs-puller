package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testCommit = "abcdef0123456789abcdef0123456789abcdef01"
	testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testIndex  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func TestRunVerifiesFullPublicPath(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(testHandler(t))
	t.Cleanup(server.Close)
	result, err := run(context.Background(), config{
		BaseURL:         server.URL,
		OriginURL:       server.URL,
		ExpectedCommit:  testCommit,
		ExpectedCorpus:  testDigest,
		ExpectedVersion: "v0.6.0",
		MaxLatency:      2 * time.Second,
		AllowHTTP:       true,
	}, nil)
	if err != nil {
		t.Fatalf("run smoke verification: %v", err)
	}
	if !result.OK || result.BuildID != "test-build" || result.Commit != testCommit || result.CorpusDigest != testDigest {
		t.Fatalf("report = %#v", result)
	}
	if len(result.Checks) != 13 {
		t.Fatalf("checks = %d, want 13", len(result.Checks))
	}
	for _, check := range result.Checks {
		if !check.OK || check.Failure != "" {
			t.Fatalf("failed check = %#v", check)
		}
	}
}

func TestRunFailsOnDeploymentIdentityDrift(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(testHandler(t))
	t.Cleanup(server.Close)
	result, err := run(context.Background(), config{
		BaseURL:        server.URL,
		ExpectedCommit: "0000000000000000000000000000000000000000",
		MaxLatency:     2 * time.Second,
		AllowHTTP:      true,
	}, nil)
	if err == nil {
		t.Fatal("smoke verification accepted an unexpected commit")
	}
	if result.OK || len(result.Checks) != 3 || result.Checks[2].Failure == "" {
		t.Fatalf("failure report = %#v", result)
	}
}

func TestValidateBaseURL(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"http://example.com",
		"https://user@example.com",
		"https://example.com?secret=yes",
		"file:///tmp/demo",
	} {
		if _, err := validateBaseURL(value, false); err == nil {
			t.Fatalf("validateBaseURL(%q) succeeded", value)
		}
	}
	if _, err := validateBaseURL("http://127.0.0.1:8080", true); err != nil {
		t.Fatalf("allow isolated HTTP: %v", err)
	}
}

func TestDecodeStrictJSONRejectsUnknownData(t *testing.T) {
	t.Parallel()

	var health healthResponse
	if err := decodeStrictJSON(strings.NewReader(`{"ok":true,"service":"docs-puller-demo","build_id":"x","extra":true}`), &health); err == nil {
		t.Fatal("unknown JSON field was accepted")
	}
	if validDigest("sha256:GG11111111111111111111111111111111111111111111111111111111111111") {
		t.Fatal("uppercase non-hex digest was accepted")
	}
}

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	corpus := corpusIdentity{
		ID:            "public-sample-v1",
		Digest:        testDigest,
		IndexDigest:   testIndex,
		DocumentCount: 24,
		SourceCount:   3,
		RetrievedAt:   "2026-08-19T01:06:48.000Z",
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		setTestSecurityHeaders(response.Header())
		response.Header().Set("X-Request-ID", "test-request")
		if request.Header.Get("Origin") == "https://untrusted.example" {
			response.WriteHeader(http.StatusForbidden)
			return
		}
		if request.URL.Path != "/api/status" && request.Header.Get("X-Docs-Puller-Probe") != "synthetic" {
			http.Error(response, "missing synthetic probe identity", http.StatusBadRequest)
			return
		}
		switch request.URL.Path {
		case "/healthz":
			writeTestJSON(t, response, healthResponse{OK: true, Service: serviceName, BuildID: "test-build"})
		case "/readyz":
			writeTestJSON(t, response, readinessResponse{OK: true, Service: serviceName, Origin: "ready", Corpus: corpus, CheckedAt: "2026-08-19T02:00:00Z"})
		case "/api/v1/demo/meta":
			writeTestJSON(t, response, metadataResponse{
				OK: true, SchemaVersion: 1, Service: serviceName,
				Engine:  engineIdentity{Name: "docs-puller", Version: "v0.6.0", Mode: "fts5"},
				BuildID: "test-build", Commit: testCommit, DeployedAt: "2026-08-19T02:00:00Z", Corpus: corpus,
				Limits: limits{QueryCharacters: 160, Results: 10, TimeoutMS: 4000, ResponseBytes: 65536},
			})
		case "/api/v1/demo/sources":
			writeTestJSON(t, response, sourcesResponse{OK: true, Corpus: corpus, Sources: []source{
				{ID: "sqlite", Label: "SQLite", DocumentCount: 8, Homepage: "https://sqlite.org/docs.html", License: allowedSourceLicenses["sqlite"]},
				{ID: "go", Label: "Go", DocumentCount: 8, Homepage: "https://go.dev/doc/", License: allowedSourceLicenses["go"]},
				{ID: "postgresql", Label: "PostgreSQL", DocumentCount: 8, Homepage: "https://www.postgresql.org/docs/", License: allowedSourceLicenses["postgresql"]},
			}})
		case "/api/v1/demo/search":
			writeTestJSON(t, response, searchResponse{
				OK: true, Query: fixedQuery, Engine: "docs-puller", Mode: "fts5", ElapsedMS: 2, ResultCount: 1, Corpus: corpus,
				Results: []searchResult{{Title: "SQLite FTS5 Extension", Source: "sqlite", Path: "fts5.md", URL: "https://sqlite.org/fts5.html", Score: 944, Snippets: []snippet{{Line: 1, Text: "External content"}}}},
			})
		case "/api/v1/demo/doc":
			writeTestJSON(t, response, documentResponse{OK: true, Source: "sqlite", Path: "fts5.md", Title: "SQLite FTS5 Extension", URL: "https://sqlite.org/fts5.html", ContentType: "text/markdown", Content: "# SQLite FTS5", Bytes: 13, TotalBytes: 100, Truncated: true, StartLine: 1, EndLine: 5, TotalLines: 10, Corpus: corpus})
		case "/api/status":
			response.WriteHeader(http.StatusUnauthorized)
		case "/", "/demo/", "/method/":
			response.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = response.Write([]byte("<!doctype html><title>docs-puller</title>"))
		case "/robots.txt":
			response.Header().Set("Content-Type", "text/plain")
			_, _ = response.Write([]byte("User-agent: *"))
		case "/sitemap.xml":
			response.Header().Set("Content-Type", "application/xml")
			_, _ = response.Write([]byte("<urlset/>"))
		default:
			http.NotFound(response, request)
		}
	})
}

func setTestSecurityHeaders(headers http.Header) {
	headers.Set("Content-Security-Policy", "default-src 'self'")
	headers.Set("Referrer-Policy", "no-referrer")
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("X-Frame-Options", "DENY")
}

func writeTestJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
