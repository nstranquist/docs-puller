package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nstranquist/docs-puller/internal/releasecontract"
)

func TestRewriteScopedYAMLBlockChangesOnlyDocsPuller(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	body := `products:
    product.docs-puller:
        proof_url: https://github.com/nstranquist/docs-puller/releases/tag/v0.5.0
    product.other:
        proof_url: https://github.com/example/other/releases/tag/v1.2.3
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := releasecontract.Manifest{Version: "v0.6.0", ReleaseDate: "2026-08-18"}
	changed, err := rewriteScopedYAMLBlock(path, `^    product\.docs-puller:\s*$`, `^    [A-Za-z0-9_.-]+:\s*$`, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected scoped rewrite")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.Contains(text, "docs-puller/releases/tag/v0.6.0") {
		t.Fatalf("docs-puller version was not updated:\n%s", text)
	}
	if !strings.Contains(text, "other/releases/tag/v1.2.3") {
		t.Fatalf("other product version was changed:\n%s", text)
	}
}

func TestDependencySnapshotRoundTripUsesManifestContract(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docs-puller.json")
	body := `{
  "schema_version": 1,
  "name": "docs-puller",
  "ownership": "standalone",
  "repository": "https://github.com/nstranquist/docs-puller",
  "module": "github.com/nstranquist/docs-puller",
  "local_source": "~/tools/docs-puller",
  "managed_binary": ".ndev/bin/docs-puller",
  "released_fallback": {"version": "v0.5.0", "contract_version": 1},
  "development_contract": {"minimum_version": 1, "required_capabilities": ["search.fts5"]},
  "commands": ["version"],
  "next_release_action": "verify"
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := releasecontract.Manifest{
		Module: "github.com/nstranquist/docs-puller", Version: "v0.6.0",
		ContractVersion: 1, Commands: []string{"demo", "version"},
	}
	changed, err := rewriteDependencySnapshot(path, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected dependency snapshot rewrite")
	}
	var report syncReport
	checkDependencySnapshot(&report, path, releasecontract.Manifest{
		Module: "github.com/nstranquist/docs-puller", Version: "v0.6.0",
		ContractVersion: 1, Commands: []string{"demo", "version"},
		Capabilities: []string{"search.fts5"},
	})
	if len(report.Drift) != 0 {
		t.Fatalf("rewritten snapshot still drifted: %v", report.Drift)
	}
}

func TestNShipRewriteUpdatesModuleAndExpectedVersion(t *testing.T) {
	body := []byte(`argv: [go, run, github.com/nstranquist/docs-puller@v0.4.0, version, --expect, v0.4.0, --json]`)
	manifest := releasecontract.Manifest{Module: "github.com/nstranquist/docs-puller", Version: "v0.6.0"}
	body = rewriteNShipLaunch(body, manifest)
	got := string(body)
	if strings.Count(got, "v0.6.0") != 2 || strings.Contains(got, "v0.4.0") {
		t.Fatalf("nship contract still drifted: %s", got)
	}
}
