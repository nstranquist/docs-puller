package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nstranquist/docs-puller/internal/releasecontract"
)

func TestDeterministicArchives(t *testing.T) {
	inputs := []archiveInput{
		{Name: "LICENSE", Mode: 0o644, Body: []byte("license\n")},
		{Name: "docs-puller", Mode: 0o755, Body: []byte("binary\n")},
	}
	fixed := time.Unix(1_700_000_000, 0).UTC()
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			first := filepath.Join(t.TempDir(), "first."+format)
			second := filepath.Join(t.TempDir(), "second."+format)
			if err := writeArchive(first, format, inputs, fixed); err != nil {
				t.Fatal(err)
			}
			if err := writeArchive(second, format, inputs, fixed); err != nil {
				t.Fatal(err)
			}
			a, _ := os.ReadFile(first)
			b, _ := os.ReadFile(second)
			if !bytes.Equal(a, b) {
				t.Fatal("archives built from the same inputs differ")
			}
		})
	}
}

func TestCycloneDXAndProvenanceAreDeterministic(t *testing.T) {
	manifest := releasecontract.Manifest{
		Name: "docs-puller", Module: "github.com/nstranquist/docs-puller", Version: "v0.6.0", GoVersion: "1.26",
		Targets: []releasecontract.Target{{GOOS: "linux", GOARCH: "amd64", Archive: "tar.gz"}},
	}
	modules := []moduleInfo{{Path: manifest.Module, Main: true}, {Path: "example.com/dependency", Version: "v1.2.3", Sum: "h1:test"}}
	fixed := time.Unix(1_700_000_000, 0).UTC()
	first, err := buildCycloneDX(manifest, "abc", fixed, modules)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := buildCycloneDX(manifest, "abc", fixed, modules)
	if !bytes.Equal(first, second) {
		t.Fatal("SBOM is not deterministic")
	}
	artifacts := []artifactRecord{{Name: "archive.tar.gz", SHA256: "abcd", Type: "archive"}}
	p1, err := buildProvenance(manifest, "abc", fixed, modules, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := buildProvenance(manifest, "abc", fixed, modules, artifacts)
	if !bytes.Equal(p1, p2) {
		t.Fatal("provenance is not deterministic")
	}
}

func TestCompareDirectoriesFailsClosedOnDifferentBytes(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "a"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(right, "a"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compareDirectories(left, right); err == nil {
		t.Fatal("different output directories were accepted")
	}
}

func TestGoPURLPreservesModulePathSeparators(t *testing.T) {
	got := goPURL("github.com/example/some module", "v1.2.3+build")
	want := "pkg:golang/github.com/example/some%20module@v1.2.3+build"
	if got != want {
		t.Fatalf("Go package URL = %q, want %q", got, want)
	}
}

func TestArtifactContractRejectsMissingTarget(t *testing.T) {
	manifest := releasecontract.Manifest{
		ChecksumsName: "checksums.txt", ReleaseManifestName: "release-manifest.json",
		SBOMTemplate: "docs-puller_{semver}_sbom.cdx.json", ProvenanceTemplate: "docs-puller_{semver}_provenance.intoto.jsonl",
		Version: "v0.6.0", ArchiveTemplate: "docs-puller_{semver}_{goos}_{goarch}.{ext}",
		Targets: []releasecontract.Target{{GOOS: "linux", GOARCH: "amd64", Archive: "tar.gz"}},
	}
	if err := verifyArtifactContract(t.TempDir(), manifest, nil); err == nil {
		t.Fatal("missing release target was accepted")
	}
}
