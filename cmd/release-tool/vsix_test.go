package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestNormalizeVSIXRemovesOrderAndTimestampDrift(t *testing.T) {
	extension := extensionPackage{Name: "docs-puller-search", Version: "0.2.0", Publisher: "nstranquist", License: "Apache-2.0"}
	inputs := syntheticVSIXInputs()
	firstPath := filepath.Join(t.TempDir(), "first.vsix")
	secondPath := filepath.Join(t.TempDir(), "second.vsix")
	if err := writeArchive(firstPath, "zip", inputs, time.Unix(1_700_000_000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(inputs)
	if err := writeArchive(secondPath, "zip", inputs, time.Unix(1_800_000_000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1_900_000_000, 0).UTC()
	first, firstFiles, err := normalizeVSIX(firstPath, fixed, extension)
	if err != nil {
		t.Fatal(err)
	}
	second, secondFiles, err := normalizeVSIX(secondPath, fixed, extension)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || !slices.Equal(firstFiles, secondFiles) {
		t.Fatal("normalized VSIX output retained input order or timestamp drift")
	}
}

func TestNormalizeVSIXRejectsEveryUnexpectedFile(t *testing.T) {
	for _, leaked := range []string{
		"extension/src/secret.ts",
		"extension/tsconfig.test.json",
		"extension/assets/secret.json",
		"extension/package-lock.json",
	} {
		t.Run(leaked, func(t *testing.T) {
			inputs := append(syntheticVSIXInputs(), archiveInput{Name: leaked, Mode: 0o644, Body: []byte("secret")})
			path := filepath.Join(t.TempDir(), "unsafe.vsix")
			if err := writeArchive(path, "zip", inputs, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			_, _, err := normalizeVSIX(path, time.Now().UTC(), extensionPackage{Name: "docs-puller-search", Version: "0.2.0", Publisher: "nstranquist", License: "Apache-2.0"})
			if err == nil {
				t.Fatal("VSIX source leakage was accepted")
			}
		})
	}
}

func TestWriteReproducibleOutputRefusesDifferentExistingBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extension.vsix")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeReproducibleOutput(path, []byte("new")); err == nil {
		t.Fatal("different existing VSIX was overwritten")
	}
}

func TestWriteReproducibleOutputReusesIdenticalBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extension.vsix")
	reused, err := writeReproducibleOutput(path, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("new VSIX was reported as reused")
	}
	reused, err = writeReproducibleOutput(path, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("identical VSIX was not reused")
	}
}

func syntheticVSIXInputs() []archiveInput {
	return []archiveInput{
		{Name: "[Content_Types].xml", Mode: 0o644, Body: []byte("<types/>")},
		{Name: "extension.vsixmanifest", Mode: 0o644, Body: []byte(`<PackageManifest><Metadata><Identity Id="docs-puller-search" Version="0.2.0" Publisher="nstranquist"/><License>extension/LICENSE.txt</License></Metadata></PackageManifest>`)},
		{Name: "extension/LICENSE.txt", Mode: 0o644, Body: []byte("license")},
		{Name: "extension/NOTICE", Mode: 0o644, Body: []byte("notice")},
		{Name: "extension/out/client.js", Mode: 0o644, Body: []byte("exports.fetchJSON = () => {}")},
		{Name: "extension/out/extension.js", Mode: 0o644, Body: []byte("exports.activate = () => {}")},
		{Name: "extension/package.json", Mode: 0o644, Body: []byte(`{"name":"docs-puller-search","version":"0.2.0","publisher":"nstranquist","license":"Apache-2.0"}`)},
		{Name: "extension/readme.md", Mode: 0o644, Body: []byte("# README")},
	}
}
