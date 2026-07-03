package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestShouldIndexFTSDocSkipsNonEnglishLocaleRoots(t *testing.T) {
	cases := map[string]bool{
		"de/guides/auth.md":            false,
		"zh-cn/reference/cli.md":       false,
		"pt-br/getting-started.md":     false,
		"fr/api/messages.md":           false,
		"en/guides/auth.md":            true,
		"en-us/guides/auth.md":         true,
		"go/ref/spec.md":               true,
		"api/encryption.md":            true,
		"cloud/features/ai-ml/ask.md":  true,
		"docs/extensions/reference.md": true,
	}
	for rel, want := range cases {
		if got := shouldIndexFTSDoc("src", rel, []byte("# Doc\n"), ftsDocMeta{}); got != want {
			t.Errorf("shouldIndexFTSDoc(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestLoadFTSDocMeta(t *testing.T) {
	out := t.TempDir()
	srcDir := filepath.Join(out, "supabase")
	if err := writeManifestAtomic(srcDir, manifest{
		Version: manifestVersion,
		Entries: map[string]result{
			"https://example.com/a": {
				URL:       "https://example.com/a",
				Source:    "supabase",
				Path:      "supabase/guides/a.md",
				FetchedAt: "2026-05-01T00:00:00Z",
				Warning:   "low-content",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	got := loadFTSDocMeta(srcDir, "supabase")
	meta, ok := got["guides/a.md"]
	if !ok {
		t.Fatalf("missing guides/a.md in %+v", got)
	}
	if meta.URL != "https://example.com/a" || meta.Warning != "low-content" {
		t.Fatalf("unexpected meta: %+v", meta)
	}
}

func TestFTSRebuildSkipsNonEnglishLocaleDocs(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "vendor")
	writeFTSDoc(t, src, "index.md", "# Canonical\n\nenglish-token\n")
	writeFTSDoc(t, src, "de/index.md", "# Deutsch\n\ngerman-token\n")

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	n, err := idx.totalDocs()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("indexed docs = %d, want 1", n)
	}
	hits, err := idx.search("german-token", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		paths := make([]string, 0, len(hits))
		for _, h := range hits {
			paths = append(paths, h.Path)
		}
		t.Fatalf("locale doc should not be searchable, got %s", strings.Join(paths, ", "))
	}
}
