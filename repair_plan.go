package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Repair plans are evidence, not permission to remove files. Collector review
// must decide whether each orphan is still current before an apply operation.
type repairPlan struct {
	SchemaVersion string            `json:"schema_version"`
	ReadOnly      bool              `json:"read_only"`
	Corpus        string            `json:"corpus"`
	StateDigest   string            `json:"state_digest"`
	PlanDigest    string            `json:"plan_digest"`
	Candidates    []repairCandidate `json:"candidates"`
	Issues        []reconcileIssue  `json:"issues"`
}
type repairCandidate struct {
	Path          string `json:"path"`
	ContentSHA256 string `json:"content_sha256"`
	Bytes         int64  `json:"bytes"`
	Disposition   string `json:"disposition"`
}

func digestJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func repairFileHash(root *os.Root, path string) (string, int64, error) {
	// Refuse symlinks even when their targets remain inside the root: a plan
	// identifies one regular file, not a mutable link or an external target.
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := range parts {
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil {
			return "", 0, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", 0, fmt.Errorf("symlink in repair path %q", path)
		}
	}
	f, err := root.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !before.Mode().IsRegular() {
		return "", 0, fmt.Errorf("not a regular file: %s", path)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	after, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if n != before.Size() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", 0, fmt.Errorf("file changed while planning: %s", path)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func buildRepairPlan(out string) (repairPlan, error) {
	absolute, err := filepath.Abs(out)
	if err != nil {
		return repairPlan{}, err
	}
	report, err := reconcileCorpus(absolute)
	if err != nil {
		return repairPlan{}, err
	}
	p := repairPlan{SchemaVersion: "docs-puller.repair-plan.v1", ReadOnly: true, Corpus: absolute, Candidates: []repairCandidate{}, Issues: report.Issues}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return p, err
	}
	defer root.Close()
	// Bind manifest bytes as well as membership: changing ownership or provenance
	// invalidates a plan even when the set of reported issues stays identical.
	manifests := map[string]string{}
	sources, err := os.ReadDir(absolute)
	if err != nil {
		return p, err
	}
	for _, s := range sources {
		if !s.IsDir() || strings.HasPrefix(s.Name(), ".") {
			continue
		}
		name := filepath.Join(s.Name(), manifestFile)
		hash, _, err := repairFileHash(root, name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return p, err
		}
		manifests[name] = hash
	}
	for _, issue := range report.Issues {
		if issue.Kind != "orphan_file" {
			continue
		}
		hash, n, err := repairFileHash(root, issue.Path)
		if err != nil {
			return p, err
		}
		p.Candidates = append(p.Candidates, repairCandidate{issue.Path, hash, n, "review_required"})
	}
	sort.Slice(p.Candidates, func(i, j int) bool { return p.Candidates[i].Path < p.Candidates[j].Path })
	p.StateDigest, err = digestJSON(struct {
		Report    reconciliation
		Manifests map[string]string
		Files     []repairCandidate
	}{report, manifests, p.Candidates})
	if err != nil {
		return p, err
	}
	// Detect membership changes during the filesystem scan. This is not a
	// transactional snapshot; apply must independently revalidate all inputs.
	again, err := reconcileCorpus(absolute)
	if err != nil {
		return p, err
	}
	a, _ := digestJSON(report)
	b, _ := digestJSON(again)
	if a != b {
		return p, fmt.Errorf("corpus membership changed while planning; retry after refresh completes")
	}
	p.PlanDigest, err = digestJSON(p)
	return p, err
}
