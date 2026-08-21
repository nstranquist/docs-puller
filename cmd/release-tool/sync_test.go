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
        verified_at: "2026-08-17"
        notes: 'Release v0.5.0 is published and attested.'
    product.other:
        proof_url: https://github.com/example/other/releases/tag/v1.2.3
        verified_at: "2026-01-02"
        notes: 'Release v1.2.3 is published and attested.'
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
	if !strings.Contains(text, "Release v0.6.0 is published and attested") || !strings.Contains(text, `verified_at: "2026-08-18"`) {
		t.Fatalf("docs-puller release claims were not updated:\n%s", text)
	}
	if !strings.Contains(text, "other/releases/tag/v1.2.3") || !strings.Contains(text, `verified_at: "2026-01-02"`) {
		t.Fatalf("other product version was changed:\n%s", text)
	}
}

func TestRewriteCatalogProductUpdatesEveryCurrentReleaseClaim(t *testing.T) {
	body := []byte(`launch_blocker: "OSS v0.5.0 and its demo are public."
business:
  verified_at: "2026-08-17"
  notes: "Published through v0.5.0."
showcase:
  verified_at: "2026-08-17"
  notes: "Public and attested v0.5.0 release."
  proof_url: "https://github.com/nstranquist/docs-puller/releases/tag/v0.5.0"
summary: "Apache-2.0, v0.5.0, CI green"
`)
	manifest := releasecontract.Manifest{Version: "v0.6.0", ReleaseDate: "2026-08-18"}
	got := rewriteCatalogProduct(body, manifest)
	text := string(got)
	if strings.Contains(text, "v0.5.0") || strings.Contains(text, "2026-08-17") {
		t.Fatalf("catalog product retained stale release claims:\n%s", text)
	}
	if strings.Count(text, "v0.6.0") != 5 {
		t.Fatalf("catalog product did not update every version claim:\n%s", text)
	}
	if strings.Count(text, `verified_at: "2026-08-18"`) != 2 {
		t.Fatalf("catalog product did not update both verification dates:\n%s", text)
	}
}

func TestScopedConsumerCheckRejectsStaleNarrativeWithCurrentProofURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	body := `products:
    product.docs-puller:
        proof_url: https://github.com/nstranquist/docs-puller/releases/tag/v0.6.0
        verified_at: "2026-08-17"
        notes: 'Release v0.5.0 is published and attested.'
    product.other:
        notes: 'Release v9.9.9 is published and attested.'
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var report syncReport
	checkScopedContainsAll(&report, path, `^    product\.docs-puller:\s*$`, `^    [A-Za-z0-9_.-]+:\s*$`, []string{
		"/releases/tag/v0.6.0",
		"Release v0.6.0 is published and attested",
		`verified_at: "2026-08-18"`,
	})
	if len(report.Drift) != 2 {
		t.Fatalf("stale narrative and date were not both rejected: %v", report.Drift)
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

func TestNShipSourceRepositoryLaunchIsVersionIndependent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nship-launch.yaml")
	body := `target_channel: source-repository
deploy:
  argv: [curl, https://github.com/nstranquist/docs-puller]
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := releasecontract.Manifest{
		Module:  "github.com/nstranquist/docs-puller",
		Version: "v0.8.0",
	}
	var report syncReport
	checkNShipLaunch(&report, path, manifest)
	if len(report.Drift) != 0 {
		t.Fatalf("source-repository launch drifted: %v", report.Drift)
	}

	if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "nstranquist", "someone-else")), 0o644); err != nil {
		t.Fatal(err)
	}
	checkNShipLaunch(&report, path, manifest)
	if len(report.Drift) != 1 || !strings.Contains(report.Drift[0], "missing") {
		t.Fatalf("wrong source repository drift = %v", report.Drift)
	}
}

func TestVersionFileMustExactlyMatchManifestSemVer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "VERSION")
	if err := os.WriteFile(path, []byte("0.6.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var report syncReport
	checkVersionFile(&report, path, "0.6.0")
	if len(report.Drift) != 0 {
		t.Fatalf("matching VERSION drifted: %v", report.Drift)
	}
	if err := os.WriteFile(path, []byte("v0.6.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkVersionFile(&report, path, "0.6.0")
	if len(report.Drift) != 1 || !strings.Contains(report.Drift[0], `version "v0.6.0" != "0.6.0"`) {
		t.Fatalf("prefixed VERSION drift = %v", report.Drift)
	}
}

func TestDemoDeploymentIdentityMatchesReviewedLockAndOwnsRoutingInConfig(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "corpus.lock.json")
	configPath := filepath.Join(dir, "wrangler.toml")
	workflowPath := filepath.Join(dir, "demo-deploy.yml")
	lock := `{
  "corpus_digest": "sha256:corpus",
  "index_digest": "sha256:index",
  "retrieved_at": "2026-08-20T22:49:35Z"
}`
	config := `[vars]
PUBLIC_ORIGIN = "https://docs-puller-demo.example"
SIDECAR_URL = "https://docs-puller-origin.example"
CORPUS_DIGEST = "sha256:corpus"
CORPUS_INDEX_DIGEST = "sha256:index"
CORPUS_RETRIEVED_AT = "2026-08-20T22:49:35Z"
`
	workflow := `pnpm exec wrangler deploy --var "BUILD_ID:${build_id}"`
	for path, body := range map[string]string{
		lockPath: lock, configPath: config, workflowPath: workflow,
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var report syncReport
	checkDemoDeploymentIdentity(&report, lockPath, configPath, workflowPath)
	if len(report.Drift) != 0 {
		t.Fatalf("matching deployment identity drifted: %v", report.Drift)
	}

	if err := os.WriteFile(configPath, []byte(strings.Replace(config, "sha256:index", "sha256:stale", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte(workflow+` --var 'SIDECAR_URL:https://attacker.invalid'`), 0o644); err != nil {
		t.Fatal(err)
	}
	report = syncReport{}
	checkDemoDeploymentIdentity(&report, lockPath, configPath, workflowPath)
	if len(report.Drift) != 2 || !strings.Contains(strings.Join(report.Drift, "\n"), "CORPUS_INDEX_DIGEST") || !strings.Contains(strings.Join(report.Drift, "\n"), "SIDECAR_URL") {
		t.Fatalf("deployment drift was not detected: %v", report.Drift)
	}
}
