package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestVerifyRepairPlanRejectsStaleAndAlteredInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, out string, plan *repairPlan)
		want   string
	}{
		{"unchanged", func(t *testing.T, out string, plan *repairPlan) {}, ""},
		{"content", func(t *testing.T, out string, plan *repairPlan) {
			if err := os.WriteFile(filepath.Join(out, "demo", "old.md"), []byte("# New\nreference\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}, "stale repair plan"},
		{"manifest", func(t *testing.T, out string, plan *repairPlan) {
			p := filepath.Join(out, "demo", manifestFile)
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, append(b, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
		}, "stale repair plan"},
		{"membership", func(t *testing.T, out string, plan *repairPlan) {
			if err := os.WriteFile(filepath.Join(out, "demo", "other.md"), []byte("# Other\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}, "stale repair plan"},
		{"root", func(t *testing.T, out string, plan *repairPlan) { plan.Corpus = filepath.Join(out, "other") }, "corpus does not match"},
		{"schema", func(t *testing.T, out string, plan *repairPlan) { plan.SchemaVersion = "future" }, "unsupported repair plan"},
		{"mutation", func(t *testing.T, out string, plan *repairPlan) { plan.ReadOnly = false }, "unsupported repair plan"},
		{"tamper", func(t *testing.T, out string, plan *repairPlan) { plan.Candidates[0].Disposition = "approved" }, "digest mismatch"},
		{"missing-digest", func(t *testing.T, out string, plan *repairPlan) { plan.PlanDigest = "" }, "digest mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			dir := filepath.Join(out, "demo")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "old.md")
			if err := os.WriteFile(file, []byte("# Old\nreference\n"), 0600); err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(newManifest())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, manifestFile), b, 0600); err != nil {
				t.Fatal(err)
			}
			idx, err := openFTSIndex(out)
			if err != nil {
				t.Fatal(err)
			}
			idx.close()
			plan, err := buildRepairPlan(out)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(t, out, &plan)
			got, err := verifyRepairPlan(out, plan)
			if tc.want == "" {
				if err != nil || got.PlanDigest != plan.PlanDigest {
					t.Fatalf("valid preview rejected: %+v %v", got, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("verification lost original: %v", err)
			}
			if _, err := os.Stat(filepath.Join(out, ".archive")); !os.IsNotExist(err) {
				t.Fatalf("verification created archive: %v", err)
			}
		})
	}
}
