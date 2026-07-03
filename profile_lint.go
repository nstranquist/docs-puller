package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// cmdProfile dispatches `docs-puller profile <sub>` — list / show / lint.
// Lives next to cmdSearch / cmdEval and reuses listSources for ingest
// truth.
func cmdProfile(args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, profileUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		cmdProfileList(args[1:])
	case "show":
		cmdProfileShow(args[1:])
	case "lint":
		cmdProfileLint(args[1:])
	case "-h", "--help", "help":
		fmt.Print(profileUsage)
	default:
		fmt.Fprintf(os.Stderr, "profile: unknown subcommand %q\n%s", args[0], profileUsage)
		os.Exit(2)
	}
}

const profileUsage = `docs-puller profile — manage active-stack profiles for search

Usage:
  docs-puller profile list                  [--out DIR] [--json]
  docs-puller profile show <name>           [--out DIR] [--json]
  docs-puller profile lint [--profile NAME] [--out DIR] [--json]

A profile is a checked-in YAML whitelist of source dirs (with optional
sub-source path globs) that signals the user's active tech stack. At
search time, profile-matched candidates get a rerank boost so canonical
stack docs win without losing long-tail discoverability.

Lookup: <out>/profiles/<name>.yaml overrides the embedded profile.
`

func cmdProfileList(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("profile list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON instead of human-readable")
	bindOpts(fs, &o)
	fs.Parse(args)

	names := ListProfiles(o.out)
	if *asJSON {
		out := struct {
			Profiles []string `json:"profiles"`
		}{names}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}
	if len(names) == 0 {
		fmt.Println("(no profiles)")
		return
	}
	for _, n := range names {
		fmt.Println(n)
	}
}

func cmdProfileShow(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("profile show", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON instead of human-readable")
	bindOpts(fs, &o)
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "profile show: name required")
		os.Exit(2)
	}
	p, err := LoadProfile(rest[0], o.out)
	if err != nil {
		die(err)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(p)
		return
	}
	fmt.Printf("Profile: %s\n", p.Name)
	if p.Description != "" {
		fmt.Printf("  %s\n", p.Description)
	}
	fmt.Println()
	for _, s := range p.Sources {
		if len(s.Include) == 0 {
			fmt.Printf("  - %s\n", s.ID)
		} else {
			fmt.Printf("  - %s\n", s.ID)
			for _, glob := range s.Include {
				fmt.Printf("      include: %s\n", glob)
			}
		}
	}
}

// lintReport is the structured output of `profile lint`. JSON-stable
// shape so CI can diff it.
type lintReport struct {
	Profile          string            `json:"profile"`
	StaleEntries     []string          `json:"stale_entries"`
	EmptyGlobs       []emptyGlobReport `json:"empty_globs"`
	UntrackedSources []string          `json:"untracked_sources"`
}

type emptyGlobReport struct {
	Source string `json:"source"`
	Glob   string `json:"glob"`
}

func (r lintReport) clean() bool {
	return len(r.StaleEntries) == 0 && len(r.EmptyGlobs) == 0
}

func cmdProfileLint(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("profile lint", flag.ExitOnError)
	profileName := fs.String("profile", "", "profile to lint (default: auto-resolved)")
	asJSON := fs.Bool("json", false, "emit JSON instead of human-readable")
	bindOpts(fs, &o)
	fs.Parse(args)

	name := *profileName
	if name == "" {
		cwd, _ := os.Getwd()
		name, _ = ResolveActiveProfile(ResolveOpts{Out: o.out, Cwd: cwd})
		if name == "" {
			fmt.Fprintln(os.Stderr, "profile lint: no profile resolved (use --profile NAME)")
			os.Exit(2)
		}
	}
	p, err := LoadProfile(name, o.out)
	if err != nil {
		die(err)
	}
	srcs, err := listSources(o.out)
	if err != nil {
		die(fmt.Errorf("listing ingested sources at %s: %w", o.out, err))
	}
	report := lintProfile(p, srcs, o.out)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		printLintHuman(report)
	}
	if !report.clean() {
		os.Exit(1)
	}
}

// lintProfile audits a profile against the ingested-source list. Walks
// sub-source globs against the actual filesystem under <out>/<source>/
// to detect empty globs (a glob that no longer matches any ingested doc).
func lintProfile(p *Profile, ingested []string, out string) lintReport {
	r := lintReport{
		Profile:          p.Name,
		StaleEntries:     []string{},
		EmptyGlobs:       []emptyGlobReport{},
		UntrackedSources: []string{},
	}
	ingestedSet := map[string]bool{}
	for _, s := range ingested {
		ingestedSet[s] = true
	}
	referenced := map[string]bool{}
	for _, s := range p.Sources {
		referenced[s.ID] = true
		if !ingestedSet[s.ID] {
			r.StaleEntries = append(r.StaleEntries, s.ID)
			continue
		}
		if len(s.Include) == 0 {
			continue
		}
		matchedByGlob := globMatchCounts(filepath.Join(out, s.ID), s.Include)
		for _, glob := range s.Include {
			if matchedByGlob[glob] == 0 {
				r.EmptyGlobs = append(r.EmptyGlobs, emptyGlobReport{Source: s.ID, Glob: glob})
			}
		}
	}
	for _, src := range ingested {
		if !referenced[src] {
			r.UntrackedSources = append(r.UntrackedSources, src)
		}
	}
	sort.Strings(r.StaleEntries)
	sort.Strings(r.UntrackedSources)
	sort.SliceStable(r.EmptyGlobs, func(i, j int) bool {
		if r.EmptyGlobs[i].Source != r.EmptyGlobs[j].Source {
			return r.EmptyGlobs[i].Source < r.EmptyGlobs[j].Source
		}
		return r.EmptyGlobs[i].Glob < r.EmptyGlobs[j].Glob
	})
	return r
}

// globMatchCounts walks srcDir and counts how many .md files match each
// glob. relPath is computed relative to srcDir so globs anchor to the
// in-source structure.
func globMatchCounts(srcDir string, globs []string) map[string]int {
	counts := map[string]int{}
	for _, g := range globs {
		counts[g] = 0
	}
	compiled := make(map[string]*regexp.Regexp, len(globs))
	for _, g := range globs {
		if rx, err := compileGlob(g); err == nil {
			compiled[g] = rx
		}
	}
	_ = filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".md") || name == "_INDEX.md" {
			return nil
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		for g, rx := range compiled {
			if rx.MatchString(rel) {
				counts[g]++
			}
		}
		return nil
	})
	return counts
}

func printLintHuman(r lintReport) {
	fmt.Printf("Profile: %s\n", r.Profile)
	if r.clean() && len(r.UntrackedSources) == 0 {
		fmt.Println("  ✓ clean — no drift")
		return
	}
	if len(r.StaleEntries) > 0 {
		fmt.Println("  Stale entries (in profile, not ingested):")
		for _, s := range r.StaleEntries {
			fmt.Printf("    - %s\n", s)
		}
	}
	if len(r.EmptyGlobs) > 0 {
		fmt.Println("  Empty globs (in profile, zero matching docs):")
		for _, e := range r.EmptyGlobs {
			fmt.Printf("    - %s :: %s\n", e.Source, e.Glob)
		}
	}
	if len(r.UntrackedSources) > 0 {
		fmt.Println("  Untracked sources (ingested, not in profile):")
		for _, s := range r.UntrackedSources {
			fmt.Printf("    - %s\n", s)
		}
	}
}
