package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

type curationReport struct {
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

func (r curationReport) clean() bool {
	return len(r.Errors) == 0
}

func cmdCuration(args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, curationUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "lint":
		cmdCurationLint(args[1:])
	case "-h", "--help", "help":
		fmt.Print(curationUsage)
	default:
		fmt.Fprintf(os.Stderr, "curation: unknown subcommand %q\n%s", args[0], curationUsage)
		os.Exit(2)
	}
}

const curationUsage = `docs-puller curation — lint hand-curated ranking config

Usage:
  docs-puller curation lint [--json]
`

func cmdCurationLint(args []string) {
	fs := flag.NewFlagSet("curation lint", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON instead of human-readable")
	fs.Parse(args)
	report := lintCuration()
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		printCurationReport(report)
	}
	if !report.clean() {
		os.Exit(1)
	}
}

func lintCuration() curationReport {
	r := curationReport{Errors: []string{}, Warnings: []string{}}
	for source, kws := range sourceKeywordPairs {
		if source != strings.ToLower(strings.TrimSpace(source)) {
			r.Errors = append(r.Errors, fmt.Sprintf("source keyword key %q must be lowercase/trimmed", source))
		}
		if len(kws) == 0 {
			r.Warnings = append(r.Warnings, fmt.Sprintf("source %s has no keywords", source))
		}
		for _, kw := range kws {
			if kw != strings.ToLower(strings.TrimSpace(kw)) {
				r.Errors = append(r.Errors, fmt.Sprintf("keyword %q for %s must be lowercase/trimmed", kw, source))
			}
			if strings.ContainsAny(kw, " \t\n") {
				r.Errors = append(r.Errors, fmt.Sprintf("keyword %q for %s must be a single token", kw, source))
			}
		}
	}
	for title := range genericTitles {
		if title == "" {
			r.Errors = append(r.Errors, "generic title list contains an empty title")
		}
		if title != strings.ToLower(strings.TrimSpace(title)) {
			r.Errors = append(r.Errors, fmt.Sprintf("generic title %q must be lowercase/trimmed", title))
		}
	}
	for _, name := range ListProfiles("") {
		p, err := LoadProfile(name, "")
		if err != nil {
			r.Errors = append(r.Errors, err.Error())
			continue
		}
		for _, src := range p.SourceIDs() {
			if _, ok := sourceKeywordPairs[src]; !ok {
				r.Warnings = append(r.Warnings, fmt.Sprintf("profile %s source %s has no source-keyword entry", name, src))
			}
		}
	}
	sort.Strings(r.Errors)
	sort.Strings(r.Warnings)
	return r
}

func printCurationReport(r curationReport) {
	if len(r.Errors) == 0 && len(r.Warnings) == 0 {
		fmt.Println("curation: clean")
		return
	}
	if len(r.Errors) > 0 {
		fmt.Println("Errors:")
		for _, e := range r.Errors {
			fmt.Printf("  - %s\n", e)
		}
	}
	if len(r.Warnings) > 0 {
		fmt.Println("Warnings:")
		for _, w := range r.Warnings {
			fmt.Printf("  - %s\n", w)
		}
	}
}
