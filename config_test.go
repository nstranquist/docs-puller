package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nstranquist/docs-puller/internal/userconfig"
)

func TestConfigInit_WritesStateDirFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("DOCS_PULLER_CONFIG", "")
	t.Setenv("DOCS_PULLER_LEGACY_NDEV_PATHS", "")
	userconfig.Reset()

	cmdConfigInit([]string{"--profile", "acme", "--force"})

	configPath := filepath.Join(home, ".docs-puller", "config.yaml")
	profilePath := filepath.Join(home, ".docs-puller", "profiles", "acme.yaml")
	configBody, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configBody), "profile: acme") {
		t.Fatalf("config missing acme profile ref: %s", configBody)
	}
	profileBody, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(profileBody), "name: acme") {
		t.Fatalf("profile file wrong: %s", profileBody)
	}
}

func TestWriteConfigInitFile_RefusesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := writeConfigInitFile(path, []byte("x"), false); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigInitFile(path, []byte("y"), false); err == nil {
		t.Fatal("expected error when file exists without force")
	}
	if err := writeConfigInitFile(path, []byte("y"), true); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "y" {
		t.Fatalf("force write = %q", body)
	}
}

func TestConfigFilePath_RespectsEnv(t *testing.T) {
	t.Setenv("DOCS_PULLER_CONFIG", "~/custom/config.yaml")
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := configFilePath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "custom", "config.yaml")
	if got != want {
		t.Fatalf("configFilePath = %q, want %q", got, want)
	}
}
