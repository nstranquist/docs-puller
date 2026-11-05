package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLocalReplacementRejectsBeforeWriting(t *testing.T) {
	source, out := t.TempDir(), t.TempDir()
	for i := 0; i < 60; i++ {
		if err := os.WriteFile(filepath.Join(source, fmt.Sprintf("%d.md", i)), []byte("# Original\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sources := []localBatchSource{{name: "safe", walkRoot: source}}
	o := pullOpts{out: out, replaceSource: true}
	if _, err := ingestLocalBatch(sources, o, nil, nil); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 60; i++ {
		os.Remove(filepath.Join(source, fmt.Sprintf("%d.md", i)))
	}
	os.WriteFile(filepath.Join(source, "0.md"), []byte("# Changed\n"), 0600)
	_, err := ingestLocalBatch(sources, o, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "early membership check") {
		t.Fatalf("%v", err)
	}
	body, err := os.ReadFile(filepath.Join(out, "safe/0.md"))
	if err != nil || string(body) != "# Original\n" {
		t.Fatalf("guard wrote file: %s %v", body, err)
	}
	// A restored input invalidates the rejection immediately; no stale negative cache.
	for i := 1; i < 60; i++ {
		os.WriteFile(filepath.Join(source, fmt.Sprintf("%d.md", i)), []byte("# Restored\n"), 0600)
	}
	if _, err := ingestLocalBatch(sources, o, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := preflightLocalReplacement([]localBatchSource{{name: "safe", walkRoot: filepath.Join(source, "missing")}}, out, nil); err == nil {
		t.Fatal("missing root accepted")
	}
}
func TestLocalBatchSerializesCollectors(t *testing.T) {
	source, out := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(source, "a.md"), []byte("# Shared\n"), 0600)
	var wg sync.WaitGroup
	results := make(chan localBatchSummary, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := ingestLocalBatch([]localBatchSource{{name: "shared", walkRoot: source}}, pullOpts{out: out, replaceSource: true}, nil, nil)
			results <- s
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	writes := 0
	for s := range results {
		writes += s.WriteCount
	}
	if writes != 1 {
		t.Fatalf("duplicate collector writes: %d", writes)
	}
}

func TestLocalBatchStableOriginNoopAndIndexRepair(t *testing.T) {
	source, out := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(source, "a.md"), []byte("# Stable\n"), 0600)
	sources := []localBatchSource{{name: "stable", walkRoot: source, originBase: "nicos-crawl://stable"}}
	o := pullOpts{out: out, replaceSource: true}
	if _, err := ingestLocalBatch(sources, o, nil, nil); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(out, "stable", manifestFile), filepath.Join(out, "stable", "_INDEX.md"), filepath.Join(out, "_INDEX.md")}
	before := make(map[string]string)
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		before[p] = string(body)
	}
	s, err := ingestLocalBatch(sources, o, nil, nil)
	if err != nil || s.WriteCount != 0 || s.ChangedPathCount != 0 {
		t.Fatalf("no-op: %+v %v", s, err)
	}
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil || string(body) != before[p] {
			t.Fatalf("no-op changed %s: %v", p, err)
		}
	}
	os.Remove(filepath.Join(out, "stable", "_INDEX.md"))
	if _, err := ingestLocalBatch(sources, o, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "stable", "_INDEX.md")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(source, "a.md"), []byte("# Changed\n"), 0600)
	s, err = ingestLocalBatch(sources, o, nil, nil)
	if err != nil || s.WriteCount != 1 || s.ChangedPathCount != 1 {
		t.Fatalf("incremental: %+v %v", s, err)
	}
	body, _ := os.ReadFile(filepath.Join(out, "stable", "_INDEX.md"))
	if !strings.Contains(string(body), "Changed") {
		t.Fatalf("stale index: %s", body)
	}
}

func TestLocalReplacementRejectsInvalidManifestBeforeCopy(t *testing.T) {
	for _, name := range []string{manifestFile, legacyManifestFile} {
		t.Run(name, func(t *testing.T) {
			source, out := t.TempDir(), t.TempDir()
			os.WriteFile(filepath.Join(source, "a.md"), []byte("# New\n"), 0600)
			dest := filepath.Join(out, "safe")
			os.MkdirAll(dest, 0700)
			os.WriteFile(filepath.Join(dest, "a.md"), []byte("# Old\n"), 0600)
			os.WriteFile(filepath.Join(dest, name), []byte("{broken"), 0600)
			_, err := ingestLocalBatch([]localBatchSource{{name: "safe", walkRoot: source}}, pullOpts{out: out, replaceSource: true}, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "replacement manifest") {
				t.Fatalf("%v", err)
			}
			body, err := os.ReadFile(filepath.Join(dest, "a.md"))
			if err != nil || string(body) != "# Old\n" {
				t.Fatalf("copy before validation: %q %v", body, err)
			}
		})
	}
}

func TestLocalBatchRepairsSameSizeArtifactCorruption(t *testing.T) {
	source, out := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(source, "a.md"), []byte("# Original\n"), 0600)
	sources := []localBatchSource{{name: "safe", walkRoot: source}}
	opts := pullOpts{out: out, replaceSource: true}
	if _, err := ingestLocalBatch(sources, opts, nil, nil); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(out, "safe", "a.md"), []byte("# Modified\n"), 0600)
	summary, err := ingestLocalBatch(sources, opts, nil, nil)
	if err != nil || summary.WriteCount != 1 {
		t.Fatalf("%+v %v", summary, err)
	}
	body, _ := os.ReadFile(filepath.Join(out, "safe", "a.md"))
	if string(body) != "# Original\n" {
		t.Fatalf("unrepaired: %q", body)
	}
}
