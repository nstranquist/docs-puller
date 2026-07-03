package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Render Supabase reference docs from upstream YAML specs:
//   apps/docs/spec/cli_v1_commands.yaml -> /docs/reference/cli/<name>
//   apps/docs/spec/cli_v1_config.yaml   -> /docs/guides/local-development/cli/config

type cliFlag struct {
	ID             string `yaml:"id"`
	Name           string `yaml:"name"`
	Description    string `yaml:"description"`
	Required       bool   `yaml:"required"`
	DefaultValue   string `yaml:"default_value"`
	AcceptedValues []struct {
		Name string `yaml:"name"`
		Type string `yaml:"type"`
	} `yaml:"accepted_values"`
}

type cliCommand struct {
	ID          string    `yaml:"id"`
	Title       string    `yaml:"title"`
	Summary     string    `yaml:"summary"`
	Description string    `yaml:"description"`
	Usage       string    `yaml:"usage"`
	Subcommands []string  `yaml:"subcommands"`
	Tags        []string  `yaml:"tags"`
	Flags       []cliFlag `yaml:"flags"`
}

type cliCommandsSpec struct {
	Info struct {
		Title       string `yaml:"title"`
		Version     string `yaml:"version"`
		Description string `yaml:"description"`
	} `yaml:"info"`
	Commands []cliCommand `yaml:"commands"`
}

type cliConfigParam struct {
	ID          string   `yaml:"id"`
	Title       string   `yaml:"title"`
	Tags        []string `yaml:"tags"`
	Required    bool     `yaml:"required"`
	Default     string   `yaml:"default"`
	Description string   `yaml:"description"`
	Usage       string   `yaml:"usage"`
	Links       []struct {
		Name string `yaml:"name"`
		Link string `yaml:"link"`
	} `yaml:"links"`
}

type cliConfigSpec struct {
	Info struct {
		Title       string `yaml:"title"`
		Version     string `yaml:"version"`
		Description string `yaml:"description"`
	} `yaml:"info"`
	Parameters []cliConfigParam `yaml:"parameters"`
}

var (
	commandsSpecOnce  sync.Once
	commandsSpecCache *cliCommandsSpec
	commandsSpecErr   error

	configSpecOnce  sync.Once
	configSpecCache *cliConfigSpec
	configSpecErr   error
)

func loadCommandsSpec(sourceCache string) (*cliCommandsSpec, error) {
	commandsSpecOnce.Do(func() {
		path := filepath.Join(sourceCache, "supabase-src", "apps", "docs", "spec", "cli_v1_commands.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			commandsSpecErr = err
			return
		}
		var s cliCommandsSpec
		if err := yaml.Unmarshal(data, &s); err != nil {
			commandsSpecErr = err
			return
		}
		commandsSpecCache = &s
	})
	return commandsSpecCache, commandsSpecErr
}

func loadConfigSpec(sourceCache string) (*cliConfigSpec, error) {
	configSpecOnce.Do(func() {
		path := filepath.Join(sourceCache, "supabase-src", "apps", "docs", "spec", "cli_v1_config.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			configSpecErr = err
			return
		}
		var s cliConfigSpec
		if err := yaml.Unmarshal(data, &s); err != nil {
			configSpecErr = err
			return
		}
		configSpecCache = &s
	})
	return configSpecCache, configSpecErr
}

// pullSupabaseYAML handles supabase URLs that are rendered from spec YAML
// rather than MDX. Returns (result, true, nil) on hit; (_, false, nil) on miss.
func pullSupabaseYAML(u *url.URL, o pullOpts, now string) (result, bool, error) {
	if u.Host != supabaseHost {
		return result{}, false, nil
	}

	switch {
	case u.Path == "/docs/guides/local-development/cli/config":
		spec, err := loadConfigSpec(o.sourceCache)
		if err != nil {
			return result{}, true, err
		}
		md := renderConfigMD(spec)
		return writeYAMLResult(u, "guides/local-development/cli/config.md", md, now, o)

	case strings.HasPrefix(u.Path, "/docs/reference/cli/"):
		name := strings.TrimPrefix(u.Path, "/docs/reference/cli/")
		if name == "" || name == "introduction" {
			spec, err := loadCommandsSpec(o.sourceCache)
			if err != nil {
				return result{}, true, err
			}
			md := renderCommandsIndexMD(spec)
			return writeYAMLResult(u, "reference/cli/index.md", md, now, o)
		}
		spec, err := loadCommandsSpec(o.sourceCache)
		if err != nil {
			return result{}, true, err
		}
		cmd := findCommand(spec, "supabase-"+name)
		if cmd == nil {
			return result{}, false, nil
		}
		md := renderCommandMD(spec, cmd)
		rel := filepath.Join("reference", "cli", name+".md")
		return writeYAMLResult(u, rel, md, now, o)
	}

	return result{}, false, nil
}

func findCommand(spec *cliCommandsSpec, id string) *cliCommand {
	for i := range spec.Commands {
		if spec.Commands[i].ID == id {
			return &spec.Commands[i]
		}
	}
	return nil
}

func writeYAMLResult(u *url.URL, rel string, md []byte, now string, o pullOpts) (result, bool, error) {
	outPath := filepath.Join(o.out, "supabase", rel)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return result{}, true, err
	}
	if err := os.WriteFile(outPath, md, 0o644); err != nil {
		return result{}, true, err
	}
	sum := sha256.Sum256(md)
	return result{
		URL: u.String(), Source: "supabase",
		Path: filepath.Join("supabase", rel), Mode: "yaml",
		SHA256: hex.EncodeToString(sum[:]), FetchedAt: now,
	}, true, nil
}

func renderCommandMD(spec *cliCommandsSpec, c *cliCommand) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", c.Title)
	if c.Summary != "" {
		fmt.Fprintf(&b, "%s\n\n", c.Summary)
	}
	if c.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(c.Description))
	}
	if c.Usage != "" {
		fmt.Fprintf(&b, "## Usage\n\n```sh\n%s\n```\n\n", strings.TrimSpace(c.Usage))
	}
	if len(c.Flags) > 0 {
		b.WriteString("## Flags\n\n")
		for _, f := range c.Flags {
			fmt.Fprintf(&b, "### `%s`\n\n", f.Name)
			if f.Description != "" {
				fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(f.Description))
			}
			if f.DefaultValue != "" {
				fmt.Fprintf(&b, "- Default: `%s`\n", f.DefaultValue)
			}
			if f.Required {
				b.WriteString("- Required\n")
			}
			if len(f.AcceptedValues) > 0 {
				b.WriteString("- Accepted values: ")
				vals := make([]string, 0, len(f.AcceptedValues))
				for _, v := range f.AcceptedValues {
					vals = append(vals, "`"+v.Name+"`")
				}
				b.WriteString(strings.Join(vals, ", ") + "\n")
			}
			b.WriteString("\n")
		}
	}
	if len(c.Subcommands) > 0 {
		b.WriteString("## Subcommands\n\n")
		for _, s := range c.Subcommands {
			fmt.Fprintf(&b, "- `%s`\n", strings.ReplaceAll(s, "-", " "))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "---\n\nSource: `apps/docs/spec/cli_v1_commands.yaml` (Supabase CLI %s)\n", spec.Info.Version)
	return []byte(b.String())
}

func renderCommandsIndexMD(spec *cliCommandsSpec) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — Reference\n\n", spec.Info.Title)
	if spec.Info.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(spec.Info.Description))
	}
	b.WriteString("## Commands\n\n")
	for _, c := range spec.Commands {
		fmt.Fprintf(&b, "- **`%s`** — %s\n", c.Title, c.Summary)
	}
	fmt.Fprintf(&b, "\n---\n\nSource: `apps/docs/spec/cli_v1_commands.yaml` (Supabase CLI %s)\n", spec.Info.Version)
	return []byte(b.String())
}

func renderConfigMD(spec *cliConfigSpec) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — `supabase/config.toml` Reference\n\n", spec.Info.Title)
	if spec.Info.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(spec.Info.Description))
	}
	// Group params by their first tag for readability.
	groups := map[string][]cliConfigParam{}
	order := []string{}
	for _, p := range spec.Parameters {
		key := "general"
		if len(p.Tags) > 0 {
			key = p.Tags[0]
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], p)
	}
	for _, g := range order {
		fmt.Fprintf(&b, "## %s\n\n", g)
		for _, p := range groups[g] {
			fmt.Fprintf(&b, "### `%s`\n\n", p.ID)
			if p.Description != "" {
				fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(p.Description))
			}
			if p.Default != "" {
				fmt.Fprintf(&b, "- Default: `%s`\n", p.Default)
			}
			if p.Required {
				b.WriteString("- Required\n")
			}
			if p.Usage != "" {
				fmt.Fprintf(&b, "\n```toml\n%s\n```\n", strings.TrimSpace(p.Usage))
			}
			for _, l := range p.Links {
				fmt.Fprintf(&b, "- [%s](%s)\n", l.Name, l.Link)
			}
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "---\n\nSource: `apps/docs/spec/cli_v1_config.yaml` (CLI %s)\n", spec.Info.Version)
	return []byte(b.String())
}
