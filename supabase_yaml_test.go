package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sampleCommandsYAML = `
info:
  title: Supabase CLI
  version: "2.95.4"
  description: "test"
commands:
  - id: supabase-start
    title: supabase start
    summary: Start containers
    description: |-
      Starts the Supabase local development stack.
    usage: supabase start [flags]
    subcommands: []
    flags:
      - id: exclude
        name: "-x, --exclude <strings>"
        description: Names of containers to skip
        default_value: "[]"
      - id: ignore-health-check
        name: "--ignore-health-check"
        description: Ignore unhealthy services and exit 0
        required: false
        default_value: "false"
  - id: supabase-init
    title: supabase init
    summary: Initialize a project
    usage: supabase init
    flags: []
`

const sampleConfigYAML = `
info:
  title: CLI
  version: "1.93.0"
  description: "config doc"
parameters:
  - id: project_id
    title: project_id
    tags: ["general"]
    required: true
    description: |
      Project identifier.
  - id: api.port
    title: api.port
    tags: ["api"]
    required: false
    default: "54321"
    description: |
      Port to use.
    usage: |
      [api]
      port = 54321
`

func TestRenderCommandMD(t *testing.T) {
	var spec cliCommandsSpec
	if err := yaml.Unmarshal([]byte(sampleCommandsYAML), &spec); err != nil {
		t.Fatal(err)
	}
	cmd := findCommand(&spec, "supabase-start")
	if cmd == nil {
		t.Fatal("findCommand returned nil")
	}
	out := string(renderCommandMD(&spec, cmd))

	for _, want := range []string{
		"# supabase start",
		"Start containers",
		"Starts the Supabase local development stack",
		"## Usage",
		"supabase start [flags]",
		"### `-x, --exclude <strings>`",
		"Names of containers to skip",
		"Default: `[]`",
		"### `--ignore-health-check`",
		"(Supabase CLI 2.95.4)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestFindCommand(t *testing.T) {
	var spec cliCommandsSpec
	if err := yaml.Unmarshal([]byte(sampleCommandsYAML), &spec); err != nil {
		t.Fatal(err)
	}
	if findCommand(&spec, "supabase-start") == nil {
		t.Error("expected to find supabase-start")
	}
	if findCommand(&spec, "supabase-nonexistent") != nil {
		t.Error("expected nil for missing command")
	}
}

func TestRenderConfigMD(t *testing.T) {
	var spec cliConfigSpec
	if err := yaml.Unmarshal([]byte(sampleConfigYAML), &spec); err != nil {
		t.Fatal(err)
	}
	out := string(renderConfigMD(&spec))

	for _, want := range []string{
		"# CLI — `supabase/config.toml` Reference",
		"## general",
		"### `project_id`",
		"Project identifier",
		"## api",
		"### `api.port`",
		"Default: `54321`",
		"```toml\n[api]\nport = 54321\n```",
		"(CLI 1.93.0)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestRenderCommandsIndexMD(t *testing.T) {
	var spec cliCommandsSpec
	if err := yaml.Unmarshal([]byte(sampleCommandsYAML), &spec); err != nil {
		t.Fatal(err)
	}
	out := string(renderCommandsIndexMD(&spec))
	for _, want := range []string{
		"# Supabase CLI — Reference",
		"`supabase start`",
		"Start containers",
		"`supabase init`",
		"Initialize a project",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}
