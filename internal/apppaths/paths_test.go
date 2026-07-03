package apppaths

import (
	"path/filepath"
	"testing"
)

func TestStateDirDefaultsAndOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCS_PULLER_HOME", "")
	t.Setenv("DOCS_PULLER_LEGACY_NDEV_PATHS", "")
	t.Setenv("XDG_STATE_HOME", "")

	got, err := StateDir()
	if err != nil {
		t.Fatalf("StateDir default returned error: %v", err)
	}
	if want := filepath.Join(home, ".docs-puller"); got != want {
		t.Fatalf("StateDir default = %s, want %s", got, want)
	}

	xdg := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", xdg)
	got, err = StateDir()
	if err != nil {
		t.Fatalf("StateDir XDG returned error: %v", err)
	}
	if want := filepath.Join(xdg, "docs-puller"); got != want {
		t.Fatalf("StateDir XDG = %s, want %s", got, want)
	}

	t.Setenv("DOCS_PULLER_LEGACY_NDEV_PATHS", "1")
	got, err = StateDir()
	if err != nil {
		t.Fatalf("StateDir legacy returned error: %v", err)
	}
	if want := filepath.Join(home, ".nicos-dev"); got != want {
		t.Fatalf("StateDir legacy = %s, want %s", got, want)
	}

	override := filepath.Join(t.TempDir(), "custom")
	t.Setenv("DOCS_PULLER_HOME", override)
	got, err = StateDir()
	if err != nil {
		t.Fatalf("StateDir override returned error: %v", err)
	}
	if got != override {
		t.Fatalf("StateDir override = %s, want %s", got, override)
	}
}

func TestDerivedDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "docs-puller")
	t.Setenv("DOCS_PULLER_HOME", root)
	t.Setenv("DOCS_PULLER_LEGACY_NDEV_PATHS", "")

	evalRoot, err := EvalRunRoot()
	if err != nil {
		t.Fatalf("EvalRunRoot returned error: %v", err)
	}
	if want := filepath.Join(root, "evals"); evalRoot != want {
		t.Fatalf("EvalRunRoot = %s, want %s", evalRoot, want)
	}

	usageDir, err := UsageLogDir()
	if err != nil {
		t.Fatalf("UsageLogDir returned error: %v", err)
	}
	if want := filepath.Join(root, "state"); usageDir != want {
		t.Fatalf("UsageLogDir = %s, want %s", usageDir, want)
	}
}
