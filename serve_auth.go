package main

// Bearer-token auth + loopback guard for `docs-puller serve`: anonymous
// mode (no token) is local-only — the server refuses to bind a non-loopback
// address without a token, so a phone over LAN/tailnet always talks to an
// authenticated endpoint. Token resolution order: inline flag, file (first
// line), then $DOCS_SERVE_TOKEN.

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

func resolveServeAuthToken(inline, filePath string) (string, error) {
	if inline != "" {
		return strings.TrimSpace(inline), nil
	}
	if filePath != "" {
		body, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("auth token file %s: %w", filePath, err)
		}
		return firstLineTrimmed(body), nil
	}
	if env := strings.TrimSpace(os.Getenv("DOCS_SERVE_TOKEN")); env != "" {
		return env, nil
	}
	return "", nil
}

func firstLineTrimmed(body []byte) string {
	s := string(body)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// withAuth wraps a handler with a constant-time Bearer-token check. Empty
// token is a no-op (anonymous mode).
func withAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := extractBearer(r.Header.Get("Authorization"))
		if got == "" || subtle.ConstantTimeCompare([]byte(got), expected) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="docs-serve"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func extractBearer(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// loopbackOnly reports whether addr binds only the loopback interface.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	switch strings.ToLower(host) {
	case "", "127.0.0.1", "localhost", "::1":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
