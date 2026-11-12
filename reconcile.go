package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type reconciliation struct {
	SchemaVersion int              `json:"schema_version"`
	ReadOnly      bool             `json:"read_only"`
	Indexed       int              `json:"indexed"`
	Indexable     int              `json:"indexable"`
	Issues        []reconcileIssue `json:"issues"`
}
type reconcileIssue struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// reconcileCorpus compares membership, not just totals. It deliberately does
// not migrate manifests, delete orphans, rebuild indexes, or call providers.
func reconcileCorpus(out string) (reconciliation, error) {
	report := reconciliation{SchemaVersion: 1, ReadOnly: true, Issues: []reconcileIssue{}}
	root, err := os.OpenRoot(out)
	if err != nil {
		return report, err
	}
	defer root.Close()
	indexed := map[string]bool{}
	indexURL := url.URL{Scheme: "file", Path: filepath.Join(out, ".cache", "search.db"), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", indexURL.String())
	if err != nil {
		return report, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT path FROM docs")
	if err != nil {
		return report, fmt.Errorf("read FTS membership: %w", err)
	}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return report, err
		}
		indexed[path] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return report, err
	}
	report.Indexed = len(indexed)
	seen := map[string]bool{}
	sources, err := os.ReadDir(out)
	if err != nil {
		return report, err
	}
	for _, source := range sources {
		if !source.IsDir() || strings.HasPrefix(source.Name(), ".") {
			continue
		}
		name := source.Name()
		manifestPaths := map[string]bool{}
		b, readErr := root.ReadFile(filepath.Join(name, manifestFile))
		hasManifest := readErr == nil
		meta := map[string]ftsDocMeta{}
		if hasManifest {
			var m manifest
			if err := json.Unmarshal(b, &m); err != nil {
				return report, fmt.Errorf("%s manifest: %w", name, err)
			}
			if m.Version > manifestVersion {
				return report, fmt.Errorf("%s: unsupported manifest version %d", name, m.Version)
			}
			for _, entry := range m.Entries {
				if entry.Path == "" {
					continue
				}
				rel := filepath.Clean(entry.Path)
				if !filepath.IsLocal(rel) {
					report.Issues = append(report.Issues, reconcileIssue{name + "/" + entry.Path, "invalid_manifest_path"})
					continue
				}
				path := canonicalLogicalPath(filepath.ToSlash(rel))
				if !strings.HasPrefix(path, name+"/") {
					report.Issues = append(report.Issues, reconcileIssue{path, "invalid_manifest_source"})
					continue
				}
				meta[strings.TrimPrefix(path, name+"/")] = ftsDocMeta{URL: entry.URL, Warning: entry.Warning}
				if manifestPaths[path] {
					report.Issues = append(report.Issues, reconcileIssue{path, "duplicate_manifest_path"})
				}
				manifestPaths[path] = true
				if _, err := root.Stat(path); err != nil {
					report.Issues = append(report.Issues, reconcileIssue{path, "manifest_file_unavailable"})
				}
			}
		} else if !os.IsNotExist(readErr) {
			return report, readErr
		} else {
			report.Issues = append(report.Issues, reconcileIssue{name, "missing_manifest"})
		}
		err := filepath.WalkDir(filepath.Join(out, name), func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			rel, err := filepath.Rel(out, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if !strings.HasSuffix(rel, ".md") || d.Name() == "_INDEX.md" {
				return nil
			}
			seen[rel] = true
			local, _ := filepath.Rel(filepath.Join(out, name), path)
			eligible := shouldIndexFTSDocPath(name, filepath.ToSlash(local), meta[filepath.ToSlash(local)]) && statusFileHasIndexableContent(path)
			if eligible {
				report.Indexable++
			}
			if hasManifest && !manifestPaths[rel] {
				report.Issues = append(report.Issues, reconcileIssue{rel, "orphan_file"})
			}
			if eligible && !indexed[rel] {
				report.Issues = append(report.Issues, reconcileIssue{rel, "missing_index_entry"})
			}
			if !eligible && indexed[rel] {
				report.Issues = append(report.Issues, reconcileIssue{rel, "excluded_but_indexed"})
			}
			return nil
		})
		if err != nil {
			return report, err
		}
	}
	for path := range indexed {
		if !seen[path] {
			report.Issues = append(report.Issues, reconcileIssue{path, "indexed_file_unavailable"})
		}
	}
	sort.Slice(report.Issues, func(i, j int) bool {
		a, b := report.Issues[i], report.Issues[j]
		if a.Path == b.Path {
			return a.Kind < b.Kind
		}
		return a.Path < b.Path
	})
	return report, nil
}

func cmdReconcile(args []string) {
	plan := len(args) > 0 && args[0] == "plan"
	verify := len(args) > 0 && args[0] == "verify"
	if plan || verify {
		args = args[1:]
	}
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	out := fs.String("out", defaultOpts().out, "corpus directory")
	planPath := fs.String("plan", "", "saved repair preview to verify (verify subcommand only)")
	fs.Bool("json", true, "emit read-only membership report")
	fs.Parse(args)
	if fs.NArg() != 0 || (verify && *planPath == "") || (!verify && *planPath != "") {
		die(fmt.Errorf("usage: reconcile [plan | verify --plan FILE] [--out DIR] [--json]"))
	}
	absolute, err := filepath.Abs(*out)
	if err != nil {
		die(err)
	}
	if verify {
		f, err := os.Open(*planPath)
		if err != nil {
			die(err)
		}
		defer f.Close()
		decoder := json.NewDecoder(f)
		decoder.DisallowUnknownFields()
		var saved repairPlan
		if err := decoder.Decode(&saved); err != nil {
			die(fmt.Errorf("read repair plan: %w", err))
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			die(fmt.Errorf("repair plan must contain exactly one JSON object"))
		}
		current, err := verifyRepairPlan(absolute, saved)
		if err != nil {
			die(err)
		}
		result := struct {
			SchemaVersion string `json:"schema_version"`
			ReadOnly      bool   `json:"read_only"`
			Valid         bool   `json:"valid"`
			PlanDigest    string `json:"plan_digest"`
			StateDigest   string `json:"state_digest"`
		}{"docs-puller.repair-verification.v1", true, true, current.PlanDigest, current.StateDigest}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			die(err)
		}
		return
	}
	if plan {
		report, err := buildRepairPlan(absolute)
		if err != nil {
			die(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			die(err)
		}
		return
	}
	report, err := reconcileCorpus(absolute)
	if err != nil {
		die(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		die(err)
	}
}
