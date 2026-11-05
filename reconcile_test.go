package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReconciliationMembershipWithoutMutation(t *testing.T) {
	out := t.TempDir()
	dir := filepath.Join(out, "demo")
	os.MkdirAll(dir, 0700)
	for _, name := range []string{"present.md", "orphan.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("# Document\nUseful API reference\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m := newManifest()
	m.Entries["https://example.org/a"] = result{Path: "demo/present.md"}
	m.Entries["https://example.org/b"] = result{Path: "demo/missing.md"}
	b, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(dir, manifestFile), b, 0600)
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	idx.close()
	report, err := reconcileCorpus(out)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, issue := range report.Issues {
		counts[issue.Kind]++
	}
	if counts["orphan_file"] != 1 || counts["missing_index_entry"] != 2 || counts["manifest_file_unavailable"] != 1 {
		t.Fatalf("%+v", report)
	}
	after, _ := os.ReadFile(filepath.Join(dir, manifestFile))
	if string(after) != string(b) {
		t.Fatal("manifest changed")
	}
}
