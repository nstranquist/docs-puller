package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderLLMsTxt(t *testing.T) {
	entries := []indexEntry{
		{path: "guides/auth.md", title: "Auth", url: "https://x.dev/guides/auth"},
		{path: "guides/storage.md", title: "Storage", url: "https://x.dev/guides/storage"},
		{path: "intro.md", title: "Introduction", url: ""}, // no URL → falls back to path
		{path: "api/messages.md", title: "Messages API", url: "https://x.dev/api/messages"},
	}
	got := renderLLMsTxt("supabase", entries, "2026-05-28T00:00:00Z")

	wants := []string{
		"# supabase\n",
		"> Vendor docs for supabase, mirrored locally by docs-puller (4 pages). Last upstream fetch: 2026-05-28T00:00:00Z.",
		"## api\n",
		"## guides\n",
		"## Docs\n", // top-level "intro.md" (no slash) → the flat "Docs" section
		"- [Auth](https://x.dev/guides/auth)",
		"- [Messages API](https://x.dev/api/messages)",
		"- [Introduction](intro.md)", // url fallback to path
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("rendered llms.txt missing %q\n---\n%s", w, got)
		}
	}

	// Sections must be alphabetically ordered: api < guides < intro/Docs.
	iAPI := strings.Index(got, "## api")
	iGuides := strings.Index(got, "## guides")
	if iAPI < 0 || iGuides < 0 || iAPI > iGuides {
		t.Errorf("section order wrong: api at %d, guides at %d", iAPI, iGuides)
	}
}

func TestRenderLLMsTxt_NoFetchDate(t *testing.T) {
	got := renderLLMsTxt("go", []indexEntry{{path: "ref/spec.md", title: "Spec", url: "https://go.dev/ref/spec"}}, "")
	if strings.Contains(got, "Last upstream fetch") {
		t.Errorf("empty fetch date should be omitted, got:\n%s", got)
	}
	if !strings.Contains(got, "(1 pages)") {
		t.Errorf("expected page count in summary, got:\n%s", got)
	}
}

// A top-level .md (no slash) lands in the flat "Docs" section.
func TestRenderLLMsTxt_FlatGroupKey(t *testing.T) {
	got := renderLLMsTxt("vscode", []indexEntry{{path: "api.md", title: "API", url: "https://code.visualstudio.com/api"}}, "")
	if !strings.Contains(got, "## Docs\n") {
		t.Errorf("single top-level doc should land under the flat 'Docs' section, got:\n%s", got)
	}
}

// End-to-end against a temp corpus dir confirms the file is written.
func TestEmitLLMsTxt_WritesFile(t *testing.T) {
	out := t.TempDir()
	srcDir := filepath.Join(out, "demo")
	if err := os.MkdirAll(filepath.Join(srcDir, "guides"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(srcDir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("guides/intro.md", "---\ntitle: Intro\n---\nhello\n")
	write("overview.md", "---\ntitle: Architecture\n---\nbody\n")

	entries, latest, err := collectEntries(srcDir, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	content := renderLLMsTxt("demo", entries, latest)
	dest := filepath.Join(srcDir, "llms.txt")
	if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "# demo\n") {
		t.Errorf("llms.txt should start with H1 title, got:\n%s", got)
	}
	if !strings.Contains(string(got), "Intro") || !strings.Contains(string(got), "Architecture") {
		t.Errorf("llms.txt missing titles, got:\n%s", got)
	}
}
