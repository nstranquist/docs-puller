package main

import (
	"net/http"

	"github.com/nstranquist/docs-puller/evidence"
)

func evidenceAPIHandler(defaults pullOpts) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET required"})
			return
		}
		path := r.URL.Query().Get("path")
		if path == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "corpus-relative path required"})
			return
		}
		budget, err := parseBoundedDocParameter(r, "max_bytes", 65536)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if budget == 0 {
			budget = 16000
		}
		line, err := parseBoundedDocParameter(r, "line", 10000000)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		lines, err := parseBoundedDocParameter(r, "lines", 10000)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if lines == 0 {
			lines = 60
		}
		pack, err := evidence.Build(r.Context(), defaults.out, []evidence.Request{{Path: path, Line: line, Lines: lines}}, budget)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "evidence read unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, pack)
	}
}
