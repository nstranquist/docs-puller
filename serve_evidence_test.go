package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/nstranquist/docs-puller/evidence"
)

func TestEvidenceAPI(t *testing.T) {
	out := t.TempDir()
	os.Mkdir(filepath.Join(out, "demo"), 0700)
	os.WriteFile(filepath.Join(out, "demo", "a.md"), []byte("# API\ntext"), 0600)
	handler := evidenceAPIHandler(pullOpts{out: out})
	for _, tc := range []struct {
		method, url string
		status      int
	}{{"GET", "/api/evidence?path=demo/a.md", 200}, {"POST", "/api/evidence?path=demo/a.md", 405}, {"GET", "/api/evidence", 400}, {"GET", "/api/evidence?path=demo/a.md&max_bytes=999999", 400}} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(tc.method, tc.url, nil))
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.url, w.Code, w.Body.String())
		}
		if tc.status == http.StatusOK {
			var p evidence.Pack
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || len(p.Passages) != 1 {
				t.Fatalf("%+v %v", p, err)
			}
		}
	}
}
