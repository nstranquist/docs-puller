package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nstranquist/docs-puller/internal/userconfig"
	"github.com/nstranquist/docs-puller/searchruntime"
)

func TestLoadProfile_Embedded(t *testing.T) {
	p, err := LoadProfile("example", t.TempDir())
	if err != nil {
		t.Fatalf("LoadProfile example: %v", err)
	}
	if p.Name != "example" {
		t.Errorf("name: got %q, want example", p.Name)
	}
	if len(p.Sources) < 3 {
		t.Errorf("sources: got %d, want at least 3", len(p.Sources))
	}
	if !p.wholeSources["supabase"] {
		t.Errorf("expected supabase as whole-source member")
	}
	if !p.wholeSources["go"] {
		t.Errorf("expected go as whole-source member")
	}
}

func TestLoadProfile_OverrideWins(t *testing.T) {
	out := t.TempDir()
	overrideDir := filepath.Join(out, "profiles")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	overrideYAML := []byte("name: example\nsources:\n  - id: only-this-source\n")
	if err := os.WriteFile(filepath.Join(overrideDir, "example.yaml"), overrideYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadProfile("example", out)
	if err != nil {
		t.Fatalf("LoadProfile override: %v", err)
	}
	if len(p.Sources) != 1 || p.Sources[0].ID != "only-this-source" {
		t.Errorf("override didn't win: %+v", p.Sources)
	}
}

func TestLoadProfile_RejectsMismatchedName(t *testing.T) {
	out := t.TempDir()
	overrideDir := filepath.Join(out, "profiles")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := []byte("name: somethingelse\nsources:\n  - id: x\n")
	if err := os.WriteFile(filepath.Join(overrideDir, "example.yaml"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	profile, err := LoadProfile("example", out)
	if profile != nil {
		t.Fatalf("LoadProfile mismatched name returned profile: %+v", profile)
	}
	if err == nil || err.Error() != searchruntime.ProfileNameMismatchError("example", "somethingelse").Error() {
		t.Fatalf("LoadProfile mismatched name err = %v", err)
	}
}

func TestLoadProfile_RejectsEmptySources(t *testing.T) {
	out := t.TempDir()
	overrideDir := filepath.Join(out, "profiles")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := []byte("name: example\nsources: []\n")
	if err := os.WriteFile(filepath.Join(overrideDir, "example.yaml"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	profile, err := LoadProfile("example", out)
	if profile != nil {
		t.Fatalf("LoadProfile empty sources returned profile: %+v", profile)
	}
	if err == nil || err.Error() != searchruntime.ProfileNoSourcesError("example").Error() {
		t.Fatalf("LoadProfile empty sources err = %v", err)
	}
}

func TestLoadProfile_RejectsMissingSourceID(t *testing.T) {
	out := t.TempDir()
	overrideDir := filepath.Join(out, "profiles")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := []byte("name: example\nsources:\n  - include: ['**/*.md']\n")
	if err := os.WriteFile(filepath.Join(overrideDir, "example.yaml"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	profile, err := LoadProfile("example", out)
	if profile != nil {
		t.Fatalf("LoadProfile missing source id returned profile: %+v", profile)
	}
	if err == nil || err.Error() != searchruntime.ProfileSourceMissingIDError("example").Error() {
		t.Fatalf("LoadProfile missing source id err = %v", err)
	}
}

func TestLoadProfile_RejectsEmptyName(t *testing.T) {
	profile, err := LoadProfile("", t.TempDir())
	if profile != nil {
		t.Fatalf("LoadProfile empty name returned profile: %+v", profile)
	}
	if err == nil || err.Error() != searchruntime.ProfileNameRequiredError().Error() {
		t.Fatalf("LoadProfile empty name err = %v", err)
	}
}

func TestLoadProfile_NotFound(t *testing.T) {
	profile, err := LoadProfile("does-not-exist", t.TempDir())
	if profile != nil {
		t.Fatalf("LoadProfile not found returned profile: %+v", profile)
	}
	if err == nil || err.Error() != searchruntime.ProfileNotFoundError("does-not-exist").Error() {
		t.Fatalf("LoadProfile not found err = %v", err)
	}
}

func TestProfileMatch_WholeSource(t *testing.T) {
	out := t.TempDir()
	overrideDir := filepath.Join(out, "profiles")
	_ = os.MkdirAll(overrideDir, 0o755)
	yaml := []byte("name: t\nsources:\n  - id: supabase\n  - id: kb\n")
	_ = os.WriteFile(filepath.Join(overrideDir, "t.yaml"), yaml, 0o644)
	p, err := LoadProfile("t", out)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		source, rel string
		wantIn      bool
		wantSub     bool
	}{
		{"supabase", "guides/auth.md", true, false},
		{"kb", "any/depth/note.md", true, false},
		{"redis", "anything.md", false, false},
	}
	for _, c := range cases {
		in, sub := p.Match(c.source, c.rel)
		if in != c.wantIn || sub != c.wantSub {
			t.Errorf("%s/%s: got (%v,%v), want (%v,%v)", c.source, c.rel, in, sub, c.wantIn, c.wantSub)
		}
	}
}

func TestProfileMatch_SubSourceGlob(t *testing.T) {
	out := t.TempDir()
	overrideDir := filepath.Join(out, "profiles")
	_ = os.MkdirAll(overrideDir, 0o755)
	yaml := []byte(`name: t
sources:
  - id: microsoft-learn
    include:
      - "azure/cli/**"
      - "azure/storage/**"
  - id: chrome
    include:
      - "**/manifest.md"
`)
	_ = os.WriteFile(filepath.Join(overrideDir, "t.yaml"), yaml, 0o644)
	p, err := LoadProfile("t", out)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		source, rel string
		wantIn      bool
		wantSub     bool
	}{
		{"microsoft-learn", "azure/cli/login.md", true, true},
		{"microsoft-learn", "azure/cli/storage/blob.md", true, true},
		{"microsoft-learn", "azure/storage/blobs/intro.md", true, true},
		{"microsoft-learn", "azure/cosmos/intro.md", false, false},
		{"microsoft-learn", "azure/devops/pipelines.md", false, false},
		{"chrome", "docs/extensions/reference/manifest.md", true, true},
		{"chrome", "docs/extensions/reference/manifest/permissions.md", false, false},
		{"redis", "anything.md", false, false},
	}
	for _, c := range cases {
		in, sub := p.Match(c.source, c.rel)
		if in != c.wantIn || sub != c.wantSub {
			t.Errorf("%s/%s: got (%v,%v), want (%v,%v)", c.source, c.rel, in, sub, c.wantIn, c.wantSub)
		}
	}
}

func TestCompileGlob_Patterns(t *testing.T) {
	cases := []struct {
		glob, path string
		match      bool
	}{
		{"*.md", "foo.md", true},
		{"*.md", "sub/foo.md", false},
		{"**", "any/depth/of/path.md", true},
		{"**/foo.md", "foo.md", true},
		{"**/foo.md", "sub/foo.md", true},
		{"**/foo.md", "deep/sub/foo.md", true},
		{"a/b/c.md", "a/b/c.md", true},
		{"a/b/c.md", "a/b/d.md", false},
		{"prefix-*", "prefix-x", true},
		{"prefix-*", "prefix-x/y", false},
	}
	for _, c := range cases {
		rx, err := compileGlob(c.glob)
		if err != nil {
			t.Errorf("compileGlob %q: %v", c.glob, err)
			continue
		}
		if got := rx.MatchString(c.path); got != c.match {
			t.Errorf("compileGlob(%q).Match(%q) = %v, want %v", c.glob, c.path, got, c.match)
		}
	}
}

func TestCompileGlob_RejectsEmptyGlob(t *testing.T) {
	rx, err := compileGlob("")
	if rx != nil {
		t.Fatalf("compileGlob empty returned regexp: %v", rx)
	}
	if err == nil || err.Error() != searchruntime.ProfileEmptyGlobError().Error() {
		t.Fatalf("compileGlob empty err = %v", err)
	}
}

func TestResolveActiveProfile_Precedence(t *testing.T) {
	out := t.TempDir()

	// Synthesize a fake home so the cwd-stack heuristic is deterministic
	// regardless of where the test binary runs.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.yaml")
	stackRoot := filepath.Join(fakeHome, "code", "acme-tools")
	stackCwd := filepath.Join(stackRoot, "deep", "subdir")
	if err := os.WriteFile(cfgPath, []byte(`
cwd_profiles:
  - profile: acme
    roots:
      - ~/code/acme-tools
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCS_PULLER_CONFIG", cfgPath)
	userconfig.Reset()
	if err := os.MkdirAll(stackCwd, 0o755); err != nil {
		t.Fatal(err)
	}
	nonStackCwd := filepath.Join(fakeHome, "elsewhere")
	if err := os.MkdirAll(nonStackCwd, 0o755); err != nil {
		t.Fatal(err)
	}

	// Helpers
	envEmpty := func(string) string { return "" }
	envWith := func(k, v string) func(string) string {
		return func(key string) string {
			if key == k {
				return v
			}
			return ""
		}
	}

	cases := []struct {
		name       string
		opts       ResolveOpts
		writeFile  string // optional <out>/.profile content
		wantName   string
		wantReason string
	}{
		{
			name:       "no-profile-flag-wins",
			opts:       ResolveOpts{FlagNoProfile: true, FlagProfile: "x", Out: out, Env: envWith("DOCS_PROFILE", "y")},
			wantReason: "flag-no-profile",
		},
		{
			name:       "explicit-flag-wins-over-env",
			opts:       ResolveOpts{FlagProfile: "explicit", Env: envWith("DOCS_PROFILE", "env"), Out: out},
			wantName:   "explicit",
			wantReason: "flag-explicit",
		},
		{
			name:       "env-wins-over-out-pin",
			opts:       ResolveOpts{Env: envWith("DOCS_PROFILE", "envname"), Out: out, Cwd: "/tmp"},
			writeFile:  "outpin\n",
			wantName:   "envname",
			wantReason: "env-DOCS_PROFILE",
		},
		{
			name:       "out-pin-wins-over-cwd",
			opts:       ResolveOpts{Out: out, Cwd: stackCwd, Env: envEmpty},
			writeFile:  "outpin\n",
			wantName:   "outpin",
			wantReason: "out-pin",
		},
		{
			name:       "cwd-config-fallback",
			opts:       ResolveOpts{Out: out, Cwd: stackCwd, Env: envEmpty},
			wantName:   "acme",
			wantReason: "cwd-acme",
		},
		{
			name:       "none-when-cwd-not-stack",
			opts:       ResolveOpts{Out: out, Cwd: nonStackCwd, Env: envEmpty},
			wantReason: "none",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			userconfig.Reset()
			t.Setenv("DOCS_PULLER_CONFIG", cfgPath)
			pinPath := filepath.Join(out, ".profile")
			_ = os.Remove(pinPath)
			if c.writeFile != "" {
				if err := os.WriteFile(pinPath, []byte(c.writeFile), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			gotName, gotReason := ResolveActiveProfile(c.opts)
			if gotName != c.wantName || gotReason != c.wantReason {
				t.Errorf("got (%q,%q), want (%q,%q)", gotName, gotReason, c.wantName, c.wantReason)
			}
		})
	}
}

func TestListProfiles_OverlayDedup(t *testing.T) {
	out := t.TempDir()
	dir := filepath.Join(out, "profiles")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "vscope.yaml"), []byte("name: vscope\nsources:\n  - id: y\n"), 0o644)
	names := ListProfiles(out)
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	if !have["example"] || !have["vscope"] {
		t.Errorf("expected example + vscope; got %v", names)
	}
}

func TestProfile_JSONShape(t *testing.T) {
	p, err := LoadProfile("example", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: name + sources visible, no panic on marshal.
	if !strings.Contains(string(data), `"Name":"example"`) {
		t.Errorf("missing Name in JSON: %s", data)
	}
}
