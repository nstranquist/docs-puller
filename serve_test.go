package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchAPIHandlerFallsBackWhenLiveIndexMissing(t *testing.T) {
	out := t.TempDir()
	srcDir := filepath.Join(out, "local")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "guide.md"), []byte("# Guide\n\nalpha beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	handler := searchAPIHandler(pullOpts{out: out}, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/search?q=alpha", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got searchAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Mode != "scan" {
		t.Fatalf("mode = %q, want scan", got.Mode)
	}
	if got.Scanned != 1 {
		t.Fatalf("scanned = %d, want 1", got.Scanned)
	}
	if len(got.Results) != 1 || got.Results[0].Path != "local/guide.md" {
		t.Fatalf("results = %+v, want local/guide.md", got.Results)
	}
}

func TestWithAuth(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	cases := []struct {
		name   string
		token  string
		header string
		want   int
	}{
		{"anonymous passthrough", "", "", http.StatusOK},
		{"missing bearer", "secret", "", http.StatusUnauthorized},
		{"wrong bearer", "secret", "Bearer nope", http.StatusUnauthorized},
		{"correct bearer", "secret", "Bearer secret", http.StatusOK},
		{"non-bearer header", "secret", "Basic secret", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/search?q=x", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			withAuth(tc.token, ok).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestWithCORSPreflight(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	req := httptest.NewRequest(http.MethodOptions, "/api/search", nil)
	rec := httptest.NewRecorder()
	withCORS(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if called {
		t.Fatal("preflight must not reach the wrapped handler")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
		t.Errorf("Allow-Headers missing Authorization: %q", got)
	}
}

func TestLoopbackOnly(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true, "127.0.0.2": true, "localhost": true, "LOCALHOST": true,
		"::1": true, "[::1]": true,
		"127.0.0.1:7799": true, "localhost:7799": true, "[::1]:7799": true,
		"0.0.0.0": false, "192.168.1.5": false, "10.0.0.2": false,
		// An empty addr composes to ":<port>", which binds every interface.
		// It must never be treated as loopback or `serve` would run anonymous
		// while reachable from the LAN/tailnet.
		"": false, ":7799": false,
		// Unspecified/any-interface forms must also fail closed.
		"::": false, "[::]": false, "[::]:7799": false,
		"0.0.0.0:7799": false, "192.168.1.5:7799": false,
		// Ambiguous, malformed, and non-IP names fail closed.
		"localhost.localdomain": false, "127.1": false, "[::1%lo0]": false,
	}
	for addr, want := range cases {
		if got := loopbackOnly(addr); got != want {
			t.Errorf("loopbackOnly(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestResolveServeAuthToken(t *testing.T) {
	if tok, _ := resolveServeAuthToken("inline-wins", ""); tok != "inline-wins" {
		t.Errorf("inline = %q", tok)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "token.txt")
	if err := os.WriteFile(f, []byte("file-token\nignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, _ := resolveServeAuthToken("", f); tok != "file-token" {
		t.Errorf("file = %q (want first line trimmed)", tok)
	}
	if _, err := resolveServeAuthToken("", filepath.Join(dir, "missing")); err == nil {
		t.Error("expected error for unreadable token file")
	}
	t.Setenv("DOCS_SERVE_TOKEN", "env-token")
	if tok, _ := resolveServeAuthToken("", ""); tok != "env-token" {
		t.Errorf("env = %q", tok)
	}
}

func TestDocAPIHandler(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "react")
	if err := os.MkdirAll(filepath.Join(srcDir, "guides"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Hooks\n\nUse hooks wisely.\n"
	if err := os.WriteFile(filepath.Join(srcDir, "guides", "hooks.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	h := docAPIHandler(pullOpts{out: dir})

	t.Run("reads full body, accepts source-prefixed path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/doc?source=react&path=react/guides/hooks.md", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var got docAPIResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Content != body {
			t.Errorf("content = %q", got.Content)
		}
		if got.Title != "Hooks" {
			t.Errorf("title = %q, want Hooks", got.Title)
		}
		if got.Bytes != len(body) {
			t.Errorf("bytes = %d, want %d", got.Bytes, len(body))
		}
		if got.TotalBytes != len(body) || got.Truncated || got.StartLine != 1 || got.EndLine != 3 || got.TotalLines != 3 {
			t.Errorf("full document window = %#v", got)
		}
	})

	t.Run("returns a bounded window around the requested line", func(t *testing.T) {
		lines := make([]string, 240)
		for index := range lines {
			lines[index] = fmt.Sprintf("line-%03d documented behavior with enough text to form a useful preview", index+1)
		}
		largeBody := strings.Join(lines, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(srcDir, "guides", "hooks.md"), []byte(largeBody), 0o644); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/doc?source=react&path=guides/hooks.md&max_bytes=800&line=120", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
		}
		var got docAPIResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Truncated || got.Bytes > 800 || got.TotalBytes != len(largeBody) {
			t.Fatalf("bounded document window = %#v", got)
		}
		if got.StartLine > 120 || got.EndLine < 120 || !strings.Contains(got.Content, "line-120") {
			t.Fatalf("window does not contain focus line: %#v", got)
		}
	})

	t.Run("rejects invalid preview bounds", func(t *testing.T) {
		for _, query := range []string{
			"max_bytes=0",
			"max_bytes=100&line=9999",
			"line=2",
			"max_bytes=100&max_bytes=200",
		} {
			req := httptest.NewRequest(http.MethodGet, "/api/doc?source=react&path=guides/hooks.md&"+query, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("query %q: status = %d, want 400", query, rec.Code)
			}
		}
	})

	t.Run("rejects path traversal", func(t *testing.T) {
		for _, bad := range []string{"../../../etc/passwd", "react/../../etc/passwd", "/etc/passwd", `C:%5CWindows%5Cwin.ini`} {
			req := httptest.NewRequest(http.MethodGet, "/api/doc?source=react&path="+bad, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("path %q: status = %d, want 400", bad, rec.Code)
			}
		}
		req := httptest.NewRequest(http.MethodGet, "/api/doc?source=C%3A&path=guide.md", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("drive source status = %d, want 400", rec.Code)
		}
	})

	t.Run("404 missing file, 400 missing params", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/doc?source=react&path=guides/nope.md", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("missing file status = %d, want 404", rec.Code)
		}
		req = httptest.NewRequest(http.MethodGet, "/api/doc?source=react", nil)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("missing path status = %d, want 400", rec.Code)
		}
	})
}

func TestStatusAPIHandler(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "react"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "react", "hooks.md"), []byte("# Hooks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	statusAPIHandler(pullOpts{out: dir}, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got statusAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.APIVersion != serveAPIVersion {
		t.Errorf("got %+v", got)
	}
	if got.Sources < 1 {
		t.Errorf("sources = %d, want >=1", got.Sources)
	}
	if got.Root != dir {
		t.Errorf("root = %q, want %q", got.Root, dir)
	}
}
