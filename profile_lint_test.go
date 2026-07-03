package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLintProfile_Clean(t *testing.T) {
	out := t.TempDir()
	makeSource(t, out, "supabase", []string{"guides/auth.md", "guides/storage.md"})
	makeSource(t, out, "kb", []string{"a.md", "b.md"})

	p := mustLoadProfileFromYAML(t, out, "t", `name: t
sources:
  - id: supabase
  - id: kb
`)

	r := lintProfile(p, []string{"supabase", "kb"}, out)
	if !r.clean() {
		t.Fatalf("expected clean: %+v", r)
	}
	if len(r.UntrackedSources) != 0 {
		t.Errorf("expected no untracked: %+v", r.UntrackedSources)
	}
}

func TestLintProfile_StaleEntries(t *testing.T) {
	out := t.TempDir()
	makeSource(t, out, "supabase", []string{"a.md"})

	p := mustLoadProfileFromYAML(t, out, "t", `name: t
sources:
  - id: supabase
  - id: clickhouse
  - id: postgresql
`)

	r := lintProfile(p, []string{"supabase"}, out)
	if r.clean() {
		t.Fatalf("expected drift: %+v", r)
	}
	want := map[string]bool{"clickhouse": true, "postgresql": true}
	got := map[string]bool{}
	for _, s := range r.StaleEntries {
		got[s] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing stale: %s", k)
		}
	}
}

func TestLintProfile_EmptyGlobs(t *testing.T) {
	out := t.TempDir()
	makeSource(t, out, "microsoft-learn", []string{
		"azure/cli/login.md",
		"azure/cosmos/intro.md",
	})

	p := mustLoadProfileFromYAML(t, out, "t", `name: t
sources:
  - id: microsoft-learn
    include:
      - "azure/cli/**"
      - "azure/storage/**"
      - "azure/synapse/**"
`)

	r := lintProfile(p, []string{"microsoft-learn"}, out)
	if r.clean() {
		t.Fatalf("expected empty-glob drift: %+v", r)
	}
	emptyGlobs := map[string]bool{}
	for _, e := range r.EmptyGlobs {
		emptyGlobs[e.Glob] = true
	}
	if !emptyGlobs["azure/storage/**"] {
		t.Errorf("expected azure/storage/** as empty: %+v", r.EmptyGlobs)
	}
	if !emptyGlobs["azure/synapse/**"] {
		t.Errorf("expected azure/synapse/** as empty: %+v", r.EmptyGlobs)
	}
	if emptyGlobs["azure/cli/**"] {
		t.Errorf("azure/cli/** should not be empty (login.md exists)")
	}
}

func TestLintProfile_UntrackedSources(t *testing.T) {
	out := t.TempDir()
	makeSource(t, out, "supabase", []string{"a.md"})
	makeSource(t, out, "redis", []string{"b.md"})
	makeSource(t, out, "expo", []string{"c.md"})

	p := mustLoadProfileFromYAML(t, out, "t", `name: t
sources:
  - id: supabase
`)

	r := lintProfile(p, []string{"supabase", "redis", "expo"}, out)
	want := map[string]bool{"redis": true, "expo": true}
	got := map[string]bool{}
	for _, s := range r.UntrackedSources {
		got[s] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing untracked: %s", k)
		}
	}
}

// Helpers

func makeSource(t *testing.T, out, src string, relPaths []string) {
	t.Helper()
	srcDir := filepath.Join(out, src)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range relPaths {
		full := filepath.Join(srcDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("# "+rel+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mustLoadProfileFromYAML(t *testing.T, out, name, yaml string) *Profile {
	t.Helper()
	dir := filepath.Join(out, "profiles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadProfile(name, out)
	if err != nil {
		t.Fatalf("LoadProfile %q: %v", name, err)
	}
	return p
}
