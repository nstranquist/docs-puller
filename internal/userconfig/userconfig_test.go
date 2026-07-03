package userconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatchCwdProfileFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	home := filepath.Join(dir, "home")
	stack := filepath.Join(home, "code", "acme-tools", "pkg")
	if err := os.MkdirAll(stack, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(`
cwd_profiles:
  - profile: acme
    roots:
      - ~/code/acme-tools
`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DOCS_PULLER_CONFIG", cfgPath)
	t.Setenv("HOME", home)
	Reset()

	got, ok := MatchCwdProfile(stack)
	if !ok || got != "acme" {
		t.Fatalf("MatchCwdProfile = %q, %v; want acme, true", got, ok)
	}
}

func TestClassifyToolsMonorepoFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
tools_pin_scopes:
  - path_contains: /code/acme-tools
    scope: acme-tools
  - basename: factory
    scope: factory
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCS_PULLER_CONFIG", cfgPath)
	Reset()

	kind, scope, ok := ClassifyToolsMonorepo("/tmp/code/acme-tools/apps")
	if !ok || kind != "tools" || scope != "acme-tools" {
		t.Fatalf("classify = %q,%q,%v", kind, scope, ok)
	}
	kind, scope, ok = ClassifyToolsMonorepo("/tmp/factory")
	if !ok || scope != "factory" {
		t.Fatalf("basename classify = %q,%q,%v", kind, scope, ok)
	}
}

func TestProfileSearchDirs(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	profiles := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profiles, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("profiles_dir: profiles\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCS_PULLER_CONFIG", cfgPath)
	t.Setenv("DOCS_PULLER_HOME", filepath.Join(dir, "state"))
	Reset()

	dirs, err := ProfileSearchDirs("/tmp/docs")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "profiles")
	found := false
	for _, d := range dirs {
		if d == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("ProfileSearchDirs = %v, want %q included", dirs, want)
	}
}
