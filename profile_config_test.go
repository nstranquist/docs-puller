package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nstranquist/docs-puller/internal/userconfig"
)

func TestListProfiles_FromConfigDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("cwd_profiles: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "acme.yaml"), []byte("name: acme\nsources:\n  - id: supabase\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCS_PULLER_CONFIG", cfgPath)
	userconfig.Reset()

	names := ListProfiles(t.TempDir())
	found := false
	for _, n := range names {
		if n == "acme" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListProfiles missing config-dir profile: %v", names)
	}
}
