package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRepairPlanBindsContentAndManifestWithoutMutation(t *testing.T) {
	out := t.TempDir()
	dir := filepath.Join(out, "demo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "old.md")
	if err := os.WriteFile(path, []byte("# Old\nreference\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := newManifest()
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, manifestFile), b, 0600); err != nil {
		t.Fatal(err)
	}
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	idx.close()
	a, err := buildRepairPlan(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Candidates) != 1 || a.Candidates[0].Disposition != "review_required" || a.Candidates[0].Bytes != 16 {
		t.Fatalf("%+v", a)
	}
	same, err := buildRepairPlan(out)
	if err != nil {
		t.Fatal(err)
	}
	if a.PlanDigest != same.PlanDigest {
		t.Fatal("unstable plan")
	}
	if err := os.WriteFile(path, []byte("# New\nreference\n"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := buildRepairPlan(out)
	if err != nil {
		t.Fatal(err)
	}
	if a.StateDigest == changed.StateDigest {
		t.Fatal("content change not bound")
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFile), append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	changedManifest, err := buildRepairPlan(out)
	if err != nil {
		t.Fatal(err)
	}
	if changed.StateDigest == changedManifest.StateDigest {
		t.Fatal("manifest bytes not bound")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("preview removed original", err)
	}
	if _, err := os.Stat(filepath.Join(out, ".archive")); !os.IsNotExist(err) {
		t.Fatal("preview created archive")
	}
}

func TestRepairFileHashRejectsSymlinks(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "a.md"), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.md", filepath.Join(out, "b.md")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(out)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, _, err := repairFileHash(root, "b.md"); err == nil {
		t.Fatal("accepted symlink")
	}
}
