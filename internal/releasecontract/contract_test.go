package releasecontract

import (
	"strings"
	"testing"
)

func validManifestJSON() string {
	return `{
  "schema_version": 1,
  "product_id": "product.docs-puller",
  "name": "docs-puller",
  "module": "github.com/nstranquist/docs-puller",
  "binary": "docs-puller",
  "version": "v0.6.0",
  "tag": "v0.6.0",
  "release_date": "2026-08-18",
  "go_version": "1.26",
  "contract_version": 1,
  "archive_template": "docs-puller_{semver}_{goos}_{goarch}.{ext}",
  "checksums_name": "checksums.txt",
  "sbom_template": "docs-puller_{semver}_sbom.cdx.json",
  "provenance_template": "docs-puller_{semver}_provenance.intoto.jsonl",
  "release_manifest_name": "release-manifest.json",
  "commands": ["demo", "version"],
  "capabilities": ["contract.version-json.v1"],
  "targets": [{"goos":"linux","goarch":"amd64","archive":"tar.gz"}],
  "consumers": {
    "nship_launch": "nship-launch.yaml",
    "ndev_dependency": "nicos-dev/config/external-tools/docs-puller.json",
    "catalog_product": "nicos-dev/docs/feature-catalog/products/product.docs-puller.md",
    "portfolio_policy": "nicos-dev/config/portfolio-policy.yaml",
    "external_projects": "nicos-dev/config/external-projects.yaml"
  }
}`
}

func TestParseAndRenderReleaseNames(t *testing.T) {
	manifest, err := Parse([]byte(validManifestJSON()))
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.ArchiveName(manifest.Targets[0]); got != "docs-puller_0.6.0_linux_amd64.tar.gz" {
		t.Fatalf("archive name = %q", got)
	}
	if manifest.SBOMName() != "docs-puller_0.6.0_sbom.cdx.json" {
		t.Fatalf("SBOM name = %q", manifest.SBOMName())
	}
	if manifest.UserAgent() != "docs-puller/0.6.0 (+https://github.com/nstranquist/docs-puller)" {
		t.Fatalf("user agent = %q", manifest.UserAgent())
	}
}

func TestParseAcceptsPatchLevelGoVersion(t *testing.T) {
	body := strings.Replace(validManifestJSON(), `"go_version": "1.26"`, `"go_version": "1.26.6"`, 1)
	manifest, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.GoVersion != "1.26.6" {
		t.Fatalf("go_version = %q, want exact patch", manifest.GoVersion)
	}
}

func TestBuildIdentityRoundTrip(t *testing.T) {
	commit := strings.Repeat("a", 40)
	identity := BuildIdentity("v0.6.0", commit)
	version, gotCommit, ok := ParseBuildIdentity(identity)
	if !ok || version != "v0.6.0" || gotCommit != commit {
		t.Fatalf("ParseBuildIdentity(%q) = %q, %q, %v", identity, version, gotCommit, ok)
	}
	for _, invalid := range []string{
		"", "v0.6.0@" + commit, BuildIdentity("0.6.0", commit),
		BuildIdentity("v0.6.0", "ABC"), BuildIdentity("v0.6.0", strings.Repeat("a", 39)),
	} {
		if _, _, ok := ParseBuildIdentity(invalid); ok {
			t.Fatalf("invalid build identity %q was accepted", invalid)
		}
	}
}

func TestParseRejectsDriftProneManifestShapes(t *testing.T) {
	tests := map[string]string{
		"unknown field":   strings.Replace(validManifestJSON(), `"schema_version": 1`, `"schema_version": 1, "extra": true`, 1),
		"tag mismatch":    strings.Replace(validManifestJSON(), `"tag": "v0.6.0"`, `"tag": "v0.6.1"`, 1),
		"bad semver":      strings.Replace(validManifestJSON(), `"version": "v0.6.0"`, `"version": "0.6"`, 1),
		"unsorted":        strings.Replace(validManifestJSON(), `["demo", "version"]`, `["version", "demo"]`, 1),
		"duplicate":       strings.Replace(validManifestJSON(), `["demo", "version"]`, `["demo", "demo"]`, 1),
		"trailing JSON":   validManifestJSON() + `{}`,
		"unsafe output":   strings.Replace(validManifestJSON(), `"checksums.txt"`, `"../checksums.txt"`, 1),
		"unsafe consumer": strings.Replace(validManifestJSON(), `"nship-launch.yaml"`, `"../nship-launch.yaml"`, 1),
		"bad date":        strings.Replace(validManifestJSON(), `"2026-08-18"`, `"2026-02-30"`, 1),
		"bad go version":  strings.Replace(validManifestJSON(), `"go_version": "1.26"`, `"go_version": "1.26.x"`, 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(body)); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}
}
