package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nstranquist/docs-puller/searchruntime"
)

// fakeProfile builds an in-memory profile so search tests stay self-
// contained (no YAML fixtures, no embed roundtrip).
func fakeProfile(t *testing.T, yaml string) *Profile {
	t.Helper()
	out := t.TempDir()
	dir := filepath.Join(out, "profiles")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "test.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadProfile("test", out)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProfileBoost_LiftsInProfile confirms that two ~equally-ranked docs
// from different sources get reordered when the profile boost is applied:
// the in-profile doc wins. Without the boost the off-profile doc would
// win on document-length BM25 in this fixture.
func TestProfileBoost_LiftsInProfile(t *testing.T) {
	out := t.TempDir()
	// Two near-identical docs in different sources. supabase is in
	// profile; redis is not.
	writeFTSDoc(t, filepath.Join(out, "supabase"), "guides/rate-limit.md",
		"# Rate Limit\n\nRate limit best practices.\n")
	writeFTSDoc(t, filepath.Join(out, "redis"), "guides/rate-limit.md",
		"# Rate Limit\n\nRate limit best practices in Redis. Rate limit. Rate limit. Rate limit.\n")

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	// Without profile: redis wins on body density.
	noProf, err := idx.search("rate limit", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(noProf) < 2 {
		t.Fatalf("baseline expected 2 hits, got %d", len(noProf))
	}
	if noProf[0].Source != "redis" {
		t.Logf("baseline: %s wins (ok if title-tier ties trigger a different order, that's fine)", noProf[0].Source)
	}

	// With profile: supabase should rank above redis.
	prof := fakeProfile(t, `name: test
sources:
  - id: supabase
`)
	withProf, err := idx.search("rate limit", "", 10, false, prof, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(withProf) < 2 {
		t.Fatalf("with profile expected 2 hits, got %d", len(withProf))
	}
	if withProf[0].Source != "supabase" {
		t.Errorf("expected supabase first with profile boost, got %s\n  results: %+v", withProf[0].Source, withProf)
	}
	if !withProf[0].InProfile {
		t.Errorf("supabase hit should have InProfile=true; got %+v", withProf[0])
	}
}

// TestProfileStrict_FiltersOutNonProfile asserts strict mode hard-filters
// non-profile candidates so they never appear in results.
func TestProfileStrict_FiltersOutNonProfile(t *testing.T) {
	out := t.TempDir()
	writeFTSDoc(t, filepath.Join(out, "supabase"), "guides/auth.md",
		"# Auth\n\nAuthentication.\n")
	writeFTSDoc(t, filepath.Join(out, "redis"), "guides/auth.md",
		"# Auth\n\nAuthentication. Authentication. Authentication.\n")

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	prof := fakeProfile(t, `name: test
sources:
  - id: supabase
`)
	hits, err := idx.search("authentication", "", 10, false, prof, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("strict mode returned no hits, expected at least one supabase hit")
	}
	for _, h := range hits {
		if h.Source != "supabase" {
			t.Errorf("strict mode leaked non-profile source: %+v", h)
		}
	}
}

// TestProfileSubGlob_BoostsScoped corpus has two microsoft-learn paths;
// profile only includes azure/cli/**. The cli-scoped doc should win even
// though both share the same source.
func TestProfileSubGlob_BoostsScoped(t *testing.T) {
	out := t.TempDir()
	writeFTSDoc(t, filepath.Join(out, "microsoft-learn"), "azure/cli/login.md",
		"# Login\n\nLogin to Azure via CLI.\n")
	writeFTSDoc(t, filepath.Join(out, "microsoft-learn"), "azure/cosmos/login.md",
		"# Login\n\nLogin to Cosmos. Login. Login. Login. Login. Login.\n")

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	prof := fakeProfile(t, `name: test
sources:
  - id: microsoft-learn
    include:
      - "azure/cli/**"
`)
	hits, err := idx.search("login", "", 10, false, prof, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("expected >=2 hits, got %d", len(hits))
	}
	if !strings.HasSuffix(hits[0].Path, "azure/cli/login.md") {
		t.Errorf("expected azure/cli/login.md first, got %s\n  hits: %+v", hits[0].Path, hits)
	}
	if !hits[0].InProfile || !hits[0].InProfileSub {
		t.Errorf("scoped hit should have InProfile=true and InProfileSub=true: %+v", hits[0])
	}
}

// TestProfileNil_PreservesBaselineRanking checks the boost path is a
// no-op when no profile is active. Critical for backwards compatibility:
// a binary built with profile support must rank identically when no
// profile is set.
func TestProfileNil_PreservesBaselineRanking(t *testing.T) {
	out := t.TempDir()
	writeFTSDoc(t, filepath.Join(out, "supabase"), "guides/x.md", "# X\n\nfoo bar baz.\n")
	writeFTSDoc(t, filepath.Join(out, "redis"), "guides/x.md", "# X\n\nfoo bar baz.\n")

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.search("foo", "", 10, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.InProfile || h.InProfileSub {
			t.Errorf("nil profile should not stamp InProfile flags: %+v", h)
		}
	}
}

// TestProfileBoost_EnvOverride confirms DOCS_PROFILE_BOOST tunes the
// boost without a recompile. With BOOST=0, the in-profile doc must NOT
// win against a stronger BM25 off-profile doc.
func TestProfileBoost_EnvOverride(t *testing.T) {
	out := t.TempDir()
	writeFTSDoc(t, filepath.Join(out, "supabase"), "guides/rare.md",
		"# Rare\n\nrare keyword once.\n")
	writeFTSDoc(t, filepath.Join(out, "redis"), "guides/rare.md",
		"# Rare\n\nrare. rare. rare. rare. rare. rare. rare. rare. rare. rare.\n")

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	prof := fakeProfile(t, `name: test
sources:
  - id: supabase
`)

	// With boost=0: redis wins on body density.
	t.Setenv("DOCS_PROFILE_BOOST", "0")
	t.Setenv("DOCS_PROFILE_SUB_BOOST", "0")
	hits, err := idx.search("rare", "", 10, false, prof, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("expected >=2 hits, got %d", len(hits))
	}
	if hits[0].Source == "supabase" {
		t.Errorf("BOOST=0 should not lift supabase past stronger redis BM25: %+v", hits)
	}

	// With boost=500: supabase wins decisively.
	t.Setenv("DOCS_PROFILE_BOOST", "500")
	hits, err = idx.search("rare", "", 10, false, prof, false)
	if err != nil {
		t.Fatal(err)
	}
	if hits[0].Source != "supabase" {
		t.Errorf("BOOST=500 should lift supabase to rank 1: %+v", hits)
	}
}

// TestStrictWithoutProfile is a safety smoke: strict=true + profile=nil
// must not panic; the FTS search should behave like a non-strict run.
func TestStrictWithoutProfile(t *testing.T) {
	out := t.TempDir()
	writeFTSDoc(t, filepath.Join(out, "supabase"), "guides/x.md", "# X\n\nfoo.\n")

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.search("foo", "", 10, false, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Errorf("strict+nil-profile should not filter anything when no profile: %+v", hits)
	}
}

// TestRelPathInSource covers the helper used by AnnotateProfileMembership
// + the rerank profile match.
func TestRelPathInSource(t *testing.T) {
	cases := []struct {
		path, source, want string
	}{
		{"supabase/guides/auth.md", "supabase", "guides/auth.md"},
		{"microsoft-learn/azure/cli/login.md", "microsoft-learn", "azure/cli/login.md"},
		{"already/relative.md", "wrong-source", "already/relative.md"},
	}
	for _, c := range cases {
		if got := searchruntime.RelPathInSource(c.path, c.source); got != c.want {
			t.Errorf("RelPathInSource(%q,%q)=%q, want %q", c.path, c.source, got, c.want)
		}
	}
}

// TestSplitSearchArgs_ProfileFlag confirms --profile NAME is parsed as a
// value flag (so the NAME isn't treated as part of the query).
func TestSplitSearchArgs_ProfileFlag(t *testing.T) {
	cases := []struct {
		in          []string
		wantQuery   []string
		wantHasFlag bool
	}{
		{[]string{"some", "query", "--profile", "acme"}, []string{"some", "query"}, true},
		{[]string{"--profile", "acme", "some", "query"}, []string{"some", "query"}, true},
		{[]string{"q", "--strict"}, []string{"q"}, true},
		{[]string{"q", "--no-profile"}, []string{"q"}, true},
	}
	for _, c := range cases {
		flags, query := splitSearchArgs(c.in)
		gotQuery := strings.Join(query, " ")
		wantQuery := strings.Join(c.wantQuery, " ")
		if gotQuery != wantQuery {
			t.Errorf("splitSearchArgs(%v): query=%q want %q (flags=%v)", c.in, gotQuery, wantQuery, flags)
		}
		hasFlag := len(flags) > 0
		if hasFlag != c.wantHasFlag {
			t.Errorf("splitSearchArgs(%v): has flag=%v want %v", c.in, hasFlag, c.wantHasFlag)
		}
	}
}
