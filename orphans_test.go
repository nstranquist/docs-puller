package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeOrphanFixture(t *testing.T) string {
	t.Helper()
	out := t.TempDir()
	files := map[string]string{
		"vendor/guide.md":         "# Guide\n\nkept\n",
		"vendor/zh/guide.md":      "# 指南\n\norphan\n",
		"vendor/zh/deep/x.md":     "# x\n\norphan\n",
		"vendor/_INDEX.md":        "# index\n",
		"unmanaged/notes.md":      "# no manifest here\n",
		"vendor/assets/image.png": "png",
	}
	for rel, body := range files {
		p := filepath.Join(out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := manifest{Version: manifestVersion, Entries: map[string]result{
		"https://v.dev/guide": {URL: "https://v.dev/guide", Source: "vendor", Path: "vendor/guide.md", FetchedAt: "2026-11-02T00:00:00Z"},
	}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "vendor", manifestFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Age every fixture file past orphanMinAge, as real orphans are.
	old := time.Now().Add(-2 * orphanMinAge)
	_ = filepath.Walk(out, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			_ = os.Chtimes(p, old, old)
		}
		return nil
	})
	return out
}

func TestSourceOrphanFiles(t *testing.T) {
	out := writeOrphanFixture(t)
	got, ok := sourceOrphanFiles(out, "vendor", orphanMinAge)
	if !ok || !reflect.DeepEqual(got, []string{"zh/deep/x.md", "zh/guide.md"}) {
		t.Fatalf("orphans = %v ok=%v", got, ok)
	}
	if _, ok := sourceOrphanFiles(out, "unmanaged", orphanMinAge); ok {
		t.Fatal("a source without manifest.json must not report orphans")
	}
}

func TestPruneOrphanFilesRemovesFilesAndEmptyDirs(t *testing.T) {
	out := writeOrphanFixture(t)
	pruned, err := pruneOrphanFiles(out, orphanMinAge)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pruned, map[string]int{"vendor": 2}) {
		t.Fatalf("pruned = %v", pruned)
	}
	if _, err := os.Stat(filepath.Join(out, "vendor", "zh")); !os.IsNotExist(err) {
		t.Fatalf("empty zh/ dir should be removed, stat err = %v", err)
	}
	for _, keep := range []string{"vendor/guide.md", "vendor/_INDEX.md", "vendor/assets/image.png", "unmanaged/notes.md"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(keep))); err != nil {
			t.Fatalf("%s must survive pruning: %v", keep, err)
		}
	}
}

func TestStatusWarnsOnOrphanFiles(t *testing.T) {
	out := writeOrphanFixture(t)
	status, err := collectDocsStatus(out, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.OrphanSources) != 1 || status.OrphanSources[0].OrphanFiles != 2 {
		t.Fatalf("OrphanSources = %+v", status.OrphanSources)
	}
	joined := strings.Join(buildStatusWarningsWithOptions(status, false, false), "\n")
	if !strings.Contains(joined, "source vendor has 2 files that no manifest entry covers") || !strings.Contains(joined, "reindex --prune-orphans") {
		t.Fatalf("missing orphan warning:\n%s", joined)
	}
}

func TestOrphanDetectionSparesFreshFiles(t *testing.T) {
	out := writeOrphanFixture(t)
	inFlight := filepath.Join(out, "vendor", "new-page.md") // written by a pull still fetching
	if err := os.WriteFile(inFlight, []byte("# new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := sourceOrphanFiles(out, "vendor", orphanMinAge)
	for _, rel := range got {
		if rel == "new-page.md" {
			t.Fatalf("a fresh unmanaged file must not count as an orphan: %v", got)
		}
	}
	if _, err := pruneOrphanFiles(out, orphanMinAge); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Fatalf("prune deleted an in-flight file: %v", err)
	}
}
