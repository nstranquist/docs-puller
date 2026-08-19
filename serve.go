package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	serveAPIVersion      = "v1"
	maxDocAPIPreviewSize = 1 << 20
)

// Web UI: `docs-puller serve [--port 7799]` runs an HTTP server that exposes
// search via /api/search and the static UI at /. Same Go functions as the
// CLI — no shelling out, no extra latency.

//go:embed web
var webFS embed.FS

func cmdServe(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 7799, "TCP port to listen on")
	addr := fs.String("addr", "127.0.0.1", "bind address (use 0.0.0.0 for LAN)")
	authToken := fs.String("auth-token", "", "require `Authorization: Bearer <token>` on every request (or --auth-token-file / $DOCS_SERVE_TOKEN)")
	authTokenFile := fs.String("auth-token-file", "", "read the bearer token from the first line of this file")
	bindOpts(fs, &o)
	if err := fs.Parse(args); err != nil {
		die(err)
	}

	token, err := resolveServeAuthToken(*authToken, *authTokenFile)
	if err != nil {
		die(err)
	}
	// Local-only by default: refuse to expose the corpus on a non-loopback
	// address without a token so a phone over LAN/tailnet can't hit an open
	// endpoint.
	if token == "" && !loopbackOnly(*addr) {
		die(fmt.Errorf("refusing to bind non-loopback addr %q without an auth token — set --auth-token, --auth-token-file, or $DOCS_SERVE_TOKEN", *addr))
	}

	// Open the FTS5 index once at startup and reuse it across requests via
	// liveFTSIndex, which Stat()s search.db on each search and reopens when
	// mtime advances — so an out-of-process `reindex`/`pull` is picked up
	// automatically without a server restart. Per-request Stat is ~µs; the
	// ~800ms cold-open tax only happens at startup and after each reindex.
	live, err := newLiveFTSIndex(o.out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fts5: open failed at startup: %v — first query will retry\n", err)
	}
	if live != nil {
		defer live.close()
		live.withSearch(func(idx *ftsIndex) {
			if idx == nil {
				return
			}
			n, _ := idx.totalDocs()
			fmt.Fprintf(os.Stderr, "fts5: persistent index opened (%d docs)\n", n)
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", searchAPIHandler(o, live))
	mux.HandleFunc("/api/sources", sourcesAPIHandler(o))
	mux.HandleFunc("/api/status", statusAPIHandler(o, live))
	mux.HandleFunc("/api/doc", docAPIHandler(o))

	// Serve embedded web/ at /. embed strips the "web" prefix.
	sub, err := stripFS(webFS, "web")
	if err != nil {
		die(err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	listen := fmt.Sprintf("%s:%d", *addr, *port)
	url := fmt.Sprintf("http://%s/", listen)
	authMode := "anonymous (loopback only)"
	if token != "" {
		authMode = "bearer token required"
	}
	fmt.Fprintf(os.Stderr, "docs-puller serve → %s  (auth: %s; Ctrl-C to stop)\n", url, authMode)
	// CORS answers OPTIONS preflight before auth runs (browsers/RN send no
	// Authorization on preflight); withAuth gates every real request.
	if err := http.ListenAndServe(listen, withCORS(withAuth(token, mux))); err != nil {
		die(err)
	}
}

// stripFS returns a sub-FS rooted at prefix so that embedded "web/index.html"
// is served as "/index.html" without the "web/" prefix in the URL.
func stripFS(efs embed.FS, prefix string) (fs.FS, error) {
	return fs.Sub(efs, prefix)
}

// withCORS allows fetches from anywhere (the UI is served same-origin; this is
// for the mobile app + ad-hoc curl/fetch). It answers OPTIONS preflight itself
// and advertises the Authorization header so the bearer-gated API is reachable
// from a cross-origin client.
func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

type statusAPIResponse struct {
	OK         bool   `json:"ok"`
	APIVersion string `json:"apiVersion"`
	Root       string `json:"root"`
	TotalDocs  int    `json:"totalDocs"`
	Sources    int    `json:"sources"`
}

// statusAPIHandler is the health/probe endpoint the mobile app's dev.mjs hits
// to discover a reachable daemon and pre-fill Settings.
func statusAPIHandler(defaults pullOpts, live *liveFTSIndex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		total := 0
		if live != nil {
			live.withSearch(func(idx *ftsIndex) {
				if idx == nil {
					return
				}
				if n, err := idx.totalDocs(); err == nil {
					total = n
				}
			})
		}
		srcCount := 0
		if srcs, err := listSources(defaults.out); err == nil {
			srcCount = len(srcs)
		}
		writeJSON(w, http.StatusOK, statusAPIResponse{
			OK:         true,
			APIVersion: serveAPIVersion,
			Root:       defaults.out,
			TotalDocs:  total,
			Sources:    srcCount,
		})
	}
}

type docAPIResponse struct {
	Source     string `json:"source"`
	Path       string `json:"path"`
	Title      string `json:"title,omitempty"`
	URL        string `json:"url,omitempty"`
	Content    string `json:"content"`
	Bytes      int    `json:"bytes"`
	TotalBytes int    `json:"total_bytes"`
	Truncated  bool   `json:"truncated"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	TotalLines int    `json:"total_lines"`
}

// docAPIHandler returns the full markdown body of one doc so the app can render
// a reader view (search only returns snippets). `path` accepts either the
// "<source>/<rel>" form returned in /api/search Hit.Path or a bare "<rel>".
// Path traversal is rejected: the resolved file must stay within <out>/<source>.
func docAPIHandler(defaults pullOpts) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source := r.URL.Query().Get("source")
		rawPath := r.URL.Query().Get("path")
		if source == "" || rawPath == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing source or path"})
			return
		}
		maxBytes, err := parseBoundedDocParameter(r, "max_bytes", maxDocAPIPreviewSize)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		focusLine, err := parseBoundedDocParameter(r, "line", 10_000_000)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if focusLine != 0 && maxBytes == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "line requires max_bytes"})
			return
		}
		if source == "." || source == ".." || hasWindowsDrivePrefix(source) || strings.ContainsAny(source, `/\`) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
			return
		}
		rel := strings.TrimPrefix(strings.ReplaceAll(rawPath, `\`, "/"), source+"/")
		clean, ok := cleanLogicalRelativePath(rel)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
			return
		}
		srcDir := filepath.Join(defaults.out, source)
		full := filepath.Join(srcDir, filepath.FromSlash(clean))
		if relCheck, err := filepath.Rel(srcDir, full); err != nil || strings.HasPrefix(relCheck, "..") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
			return
		}
		data, err := os.ReadFile(full)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		window, err := makeDocumentWindow(data, maxBytes, focusLine)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		urlByPath, _ := loadManifestMaps(srcDir, source)
		writeJSON(w, http.StatusOK, docAPIResponse{
			Source:     source,
			Path:       clean,
			Title:      firstMarkdownHeading(data),
			URL:        urlByPath[clean],
			Content:    window.Content,
			Bytes:      len([]byte(window.Content)),
			TotalBytes: window.TotalBytes,
			Truncated:  window.Truncated,
			StartLine:  window.StartLine,
			EndLine:    window.EndLine,
			TotalLines: window.TotalLines,
		})
	}
}

type documentWindow struct {
	Content    string
	TotalBytes int
	Truncated  bool
	StartLine  int
	EndLine    int
	TotalLines int
}

func parseBoundedDocParameter(r *http.Request, name string, maximum int) (int, error) {
	values, present := r.URL.Query()[name]
	if !present {
		return 0, nil
	}
	if len(values) != 1 || values[0] == "" {
		return 0, fmt.Errorf("%s must appear once", name)
	}
	value, err := strconv.Atoi(values[0])
	if err != nil || value < 1 || value > maximum {
		return 0, fmt.Errorf("%s must be an integer from 1 to %d", name, maximum)
	}
	return value, nil
}

func makeDocumentWindow(data []byte, maxBytes, focusLine int) (documentWindow, error) {
	content := []byte(strings.ToValidUTF8(string(data), "\uFFFD"))
	lineStarts := []int{0}
	for index, value := range content {
		if value == '\n' && index+1 < len(content) {
			lineStarts = append(lineStarts, index+1)
		}
	}
	totalLines := len(lineStarts)
	if focusLine == 0 {
		focusLine = 1
	}
	if focusLine > totalLines {
		return documentWindow{}, fmt.Errorf("line exceeds document line count")
	}
	if maxBytes == 0 || len(content) <= maxBytes {
		return documentWindow{
			Content:    string(content),
			TotalBytes: len(content),
			StartLine:  1,
			EndLine:    totalLines,
			TotalLines: totalLines,
		}, nil
	}

	focusOffset := lineStarts[focusLine-1]
	wantedStart := max(0, focusOffset-maxBytes/2)
	startLineIndex := 0
	for startLineIndex+1 < len(lineStarts) && lineStarts[startLineIndex] < wantedStart {
		startLineIndex++
	}
	if lineStarts[startLineIndex] > focusOffset {
		startLineIndex--
	}
	start := lineStarts[startLineIndex]
	end := min(len(content), start+maxBytes)
	if end < len(content) {
		if lastNewline := bytes.LastIndexByte(content[start:end], '\n'); lastNewline >= 0 {
			end = start + lastNewline + 1
		}
		for end > start && !utf8.Valid(content[start:end]) {
			end--
		}
	}
	if end <= focusOffset {
		startLineIndex = focusLine - 1
		start = focusOffset
		end = min(len(content), start+maxBytes)
		for end > start && !utf8.Valid(content[start:end]) {
			end--
		}
	}
	window := content[start:end]
	endLine := startLineIndex + 1 + bytes.Count(window, []byte{'\n'})
	if len(window) > 0 && window[len(window)-1] == '\n' && endLine > startLineIndex+1 {
		endLine--
	}
	return documentWindow{
		Content:    string(window),
		TotalBytes: len(content),
		Truncated:  start > 0 || end < len(content),
		StartLine:  startLineIndex + 1,
		EndLine:    endLine,
		TotalLines: totalLines,
	}, nil
}

func firstMarkdownHeading(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# ") {
			return strings.TrimSpace(t[2:])
		}
	}
	return ""
}

func searchAPIHandler(defaults pullOpts, live *liveFTSIndex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("q")
		if query == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing q"})
			return
		}
		o := searchOpts{
			out:    defaults.out,
			source: r.URL.Query().Get("source"),
			limit:  10,
		}
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
				o.limit = n
			}
		}
		if r.URL.Query().Get("mode") == "scan" {
			o.useScan = true
		}

		start := time.Now()
		var (
			hits    []searchHit
			scanned int
			mode    string
		)
		if live != nil {
			live.withSearch(func(idx *ftsIndex) {
				hits, scanned, mode = dispatchSearch(query, o, idx)
			})
		} else {
			hits, scanned, mode = dispatchSearch(query, o, nil)
		}
		elapsed := time.Since(start)

		writeJSON(w, http.StatusOK, searchAPIResponse{
			Query:     query,
			Mode:      mode,
			Scanned:   scanned,
			ElapsedMS: elapsed.Milliseconds(),
			Results:   hits,
		})
	}
}

type searchAPIResponse struct {
	Query     string      `json:"query"`
	Mode      string      `json:"mode"`
	Scanned   int         `json:"scanned"`
	ElapsedMS int64       `json:"elapsed_ms"`
	Results   []searchHit `json:"results"`
}

type sourceInfo struct {
	Name string `json:"name"`
	Docs int    `json:"docs"`
}

type sourcesAPIResponse struct {
	Root    string       `json:"root"`
	Sources []sourceInfo `json:"sources"`
}

func sourcesAPIHandler(defaults pullOpts) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sources, err := listSources(defaults.out)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out := make([]sourceInfo, 0, len(sources))
		for _, s := range sources {
			n, err := countMarkdownFiles(filepath.Join(defaults.out, s))
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not count source documents"})
				return
			}
			out = append(out, sourceInfo{Name: s, Docs: n})
		}
		writeJSON(w, http.StatusOK, sourcesAPIResponse{
			Root:    defaults.out,
			Sources: out,
		})
	}
}

func countMarkdownFiles(srcDir string) (int, error) {
	var n int
	err := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if name == "_INDEX.md" || name == manifestFile || name == legacyManifestFile {
			return nil
		}
		if filepath.Ext(name) == ".md" {
			n++
		}
		return nil
	})
	return n, err
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		log.Printf("docs-puller serve: write JSON response: %v", err)
	}
}
