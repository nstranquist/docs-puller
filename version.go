package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"
)

const cliContractVersion = 1

// These values can be overridden by release builds with -ldflags. Local and
// go-install builds fall back to Go's embedded build information.
var (
	releaseVersion = ""
	releaseCommit  = ""
)

type versionInfo struct {
	SchemaVersion   int      `json:"schema_version"`
	Name            string   `json:"name"`
	Module          string   `json:"module"`
	Version         string   `json:"version"`
	Commit          string   `json:"commit,omitempty"`
	Dirty           bool     `json:"dirty,omitempty"`
	ContractVersion int      `json:"contract_version"`
	Commands        []string `json:"commands"`
	Capabilities    []string `json:"capabilities"`
}

func cmdVersion(args []string) {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "emit the versioned CLI contract as JSON")
	expect := fs.String("expect", "", "fail unless the executable version exactly matches this value")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: docs-puller version [--json] [--expect VERSION]")
		os.Exit(2)
	}

	info := currentVersionInfo()
	if err := checkExpectedVersion(info.Version, *expect); err != nil {
		fmt.Fprintf(os.Stderr, "docs-puller version: %v\n", err)
		os.Exit(1)
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			fmt.Fprintf(os.Stderr, "docs-puller version: %v\n", err)
			os.Exit(1)
		}
		return
	}
	fmt.Println(info.Version)
}

func checkExpectedVersion(actual, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected != "" && actual != expected {
		return fmt.Errorf("got %q, want %q", actual, expected)
	}
	return nil
}

func currentVersionInfo() versionInfo {
	version := strings.TrimSpace(releaseVersion)
	commit := strings.TrimSpace(releaseCommit)
	dirty := false
	module := "github.com/nstranquist/docs-puller"
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Path != "" {
			module = bi.Main.Path
		}
		if version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version
		}
		for _, setting := range bi.Settings {
			switch setting.Key {
			case "vcs.revision":
				if commit == "" {
					commit = setting.Value
				}
			case "vcs.modified":
				dirty = setting.Value == "true"
			}
		}
	}
	if version == "" {
		version = "devel"
	}

	commands := []string{
		"config", "crawl-refs", "curation", "embed", "emit-llmstxt", "eval",
		"eval-diagnose", "eval-leaderboard", "eval-suite", "eval-sweep", "extract",
		"init", "list", "log", "pins", "profile", "pull", "pull-article",
		"pull-local-batch", "pull-pins", "pull-url", "refresh", "reindex", "search",
		"search-batch", "serve", "show", "status", "telemetry", "version",
	}
	capabilities := []string{
		"contract.version-json.v1",
		"embed.stale-prune.v1",
		"pull.docc",
		"pull.from",
		"pull.gatsby-pagedata",
		"pull.git-repo",
		"pull.github-repo",
		"pull.llms-txt",
		"pull.local",
		"pull.replace-source-guard.v1",
		"pull.rst",
		"pull.sitemap",
		"search.fts5",
		"search.fts5.self-heal.v1",
		"search.hybrid-source-scope.v1",
		"serve.http.v1",
		"telemetry.provenance.v1",
	}
	sort.Strings(commands)
	sort.Strings(capabilities)
	return versionInfo{
		SchemaVersion:   1,
		Name:            "docs-puller",
		Module:          module,
		Version:         version,
		Commit:          commit,
		Dirty:           dirty,
		ContractVersion: cliContractVersion,
		Commands:        commands,
		Capabilities:    capabilities,
	}
}
