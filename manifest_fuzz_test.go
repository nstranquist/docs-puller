package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// FuzzManifestParse drives the per-source manifest decoder. Every pull reads
// <out>/<source>/manifest.json to learn which URLs are already fetched; that
// file outlives any single run and can be stale, truncated, or hand-edited, so
// its decode is a trust boundary. This fuzzes the json.Unmarshal at the heart of
// loadOrMigrateManifest.
//
// Invariants: no panic; and the manifest schema is canonical under JSON —
// encoding/json sorts map keys, so a decoded manifest re-marshals to a byte form
// that is a fixed point. A value that failed to round-trip would mean the
// on-disk state cannot be faithfully rewritten by writeManifestAtomic.
func FuzzManifestParse(f *testing.F) {
	seeds := []string{
		`{"version":1,"entries":{}}`,
		`{"version":1,"entries":{"https://x/a":{"url":"https://x/a","source":"x","path":"a.md","mode":"http","sha256":"ab","fetched_at":"2026-01-01T00:00:00Z"}}}`,
		`{"entries":{"u":{"url":"u","fetched_at":"t","unchanged":true,"warning":"low-content"}}}`, // version omitted
		`{}`,
		`null`,
		`[]`, // wrong root type: decode error
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var m manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return // a corrupt manifest is surfaced as ManifestParseError, not a crash.
		}
		b1, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("a manifest that decoded cleanly failed to re-marshal: %v", err)
		}
		var m2 manifest
		if err := json.Unmarshal(b1, &m2); err != nil {
			t.Fatalf("re-decode of a re-marshalled manifest failed: %v (bytes %s)", err, b1)
		}
		b2, err := json.Marshal(m2)
		if err != nil {
			t.Fatalf("re-marshal failed: %v", err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("manifest JSON is not canonical:\n first  %s\n second %s", b1, b2)
		}
	})
}
