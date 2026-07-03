package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nstranquist/docs-puller/internal/userconfig"
)

func TestClassifyPinRoot_FromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
tools_pin_scopes:
  - path_contains: /code/nicos-tools
    scope: nicos-tools
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCS_PULLER_CONFIG", cfgPath)
	userconfig.Reset()

	got := classifyPinRoot("/home/user/code/nicos-tools")
	if got.Kind != "tools" || got.Scope != "nicos-tools" {
		t.Fatalf("classifyPinRoot = %+v, want tools/nicos-tools", got)
	}
}

func TestGenerateDocsPins_ToolsMonorepoFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
tools_pin_scopes:
  - path_contains: /code/nicos-tools
    scope: nicos-tools
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCS_PULLER_CONFIG", cfgPath)
	userconfig.Reset()

	tmp := t.TempDir()
	out := filepath.Join(tmp, "docs")
	platform := filepath.Join(tmp, "dev", "platform")
	nicosApp := filepath.Join(tmp, "dev", "nicos-app")
	tools := filepath.Join(tmp, "code", "nicos-tools")
	mustWriteFile(t, filepath.Join(platform, "package.json"), `{"devDependencies":{"typescript":"^5.4.0"}}`)
	mustWriteFile(t, filepath.Join(platform, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  .:
    devDependencies:
      typescript:
        specifier: ^5.4.0
        version: 5.7.2
  apps/e2e:
    devDependencies:
      '@playwright/test':
        specifier: ^1.49.1
        version: 1.49.1
`)
	mustWriteFile(t, filepath.Join(nicosApp, "package-lock.json"), `{"packages":{"node_modules/react":{"version":"19.0.0"}}}`)
	mustWriteFile(t, filepath.Join(tools, "services", "kanban", "web", "package-lock.json"), `{"packages":{"node_modules/react":{"version":"18.3.1"}}}`)

	pins, err := generateDocsPins(pinGenerationOptions{
		Out:   out,
		Roots: []string{platform, nicosApp, tools},
		Now:   time.Date(2026, 5, 4, 7, 55, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]docsPin{}
	for _, p := range pins.Pins {
		byID[p.SourceID] = p
	}
	if p := byID["react__v18"]; p.VersionLane != versionLaneToolsPinned || p.PinScope != "nicos-tools" {
		t.Fatalf("react tools pin = %#v", p)
	}
}
