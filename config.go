package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nstranquist/docs-puller/internal/apppaths"
)

//go:embed config.example.yaml
var configExampleYAML []byte

//go:embed profiles/example.yaml
var profileExampleYAML []byte

const defaultInitProfileName = "my-stack"

func cmdConfig(args []string) {
	if len(args) == 0 {
		printConfigUsage(os.Stderr)
		os.Exit(2)
	}
	switch args[0] {
	case "init":
		cmdConfigInit(args[1:])
	case "path":
		cmdConfigPath(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown config subcommand: %s\n\n", args[0])
		printConfigUsage(os.Stderr)
		os.Exit(2)
	}
}

func printConfigUsage(w *os.File) {
	fmt.Fprint(w, `docs-puller config — operator YAML config

Usage:
  docs-puller config init [--profile NAME] [--force]
  docs-puller config path [--json]

init writes:
  ~/.docs-puller/config.yaml
  ~/.docs-puller/profiles/<profile>.yaml

Config is optional. See config.example.yaml and README.md "Operator config".
`)
}

func cmdConfigPath(args []string) {
	var asJSON bool
	fs := flag.NewFlagSet("config path", flag.ExitOnError)
	fs.BoolVar(&asJSON, "json", false, "emit JSON")
	fs.Parse(args)

	path, err := configFilePath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config path: %v\n", err)
		os.Exit(1)
	}
	_, statErr := os.Stat(path)
	if asJSON {
		fmt.Printf(`{"path":%q,"exists":%t}`+"\n", path, statErr == nil)
		return
	}
	if statErr == nil {
		fmt.Println(path)
		return
	}
	if os.IsNotExist(statErr) {
		fmt.Fprintf(os.Stderr, "no config file at %s (run docs-puller config init)\n", path)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "config path: %v\n", statErr)
	os.Exit(1)
}

func cmdConfigInit(args []string) {
	var (
		profileName string
		force       bool
	)
	fs := flag.NewFlagSet("config init", flag.ExitOnError)
	fs.StringVar(&profileName, "profile", defaultInitProfileName, "profile name to scaffold")
	fs.BoolVar(&force, "force", false, "overwrite existing config files")
	fs.Parse(args)

	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		fmt.Fprintln(os.Stderr, "config init: --profile must not be empty")
		os.Exit(2)
	}

	stateDir, err := apppaths.StateDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config init: %v\n", err)
		os.Exit(1)
	}
	configPath := filepath.Join(stateDir, "config.yaml")
	profilesDir := filepath.Join(stateDir, "profiles")
	profilePath := filepath.Join(profilesDir, profileName+".yaml")

	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "config init: %v\n", err)
		os.Exit(1)
	}

	configBody := bytesReplaceAll(configExampleYAML, []byte("my-stack"), []byte(profileName))
	if err := writeConfigInitFile(configPath, configBody, force); err != nil {
		fmt.Fprintf(os.Stderr, "config init: %v\n", err)
		os.Exit(1)
	}

	profileBody := bytesReplaceAll(profileExampleYAML, []byte("name: example"), []byte("name: "+profileName))
	profileBody = bytesReplaceAll(profileBody, []byte("Example stack profile for local docs-puller development."),
		[]byte("Stack profile for "+profileName+" (edit sources to match your corpus)."))
	if err := writeConfigInitFile(profilePath, profileBody, force); err != nil {
		fmt.Fprintf(os.Stderr, "config init: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("wrote %s\n", configPath)
	fmt.Printf("wrote %s\n", profilePath)
	fmt.Printf("\nNext: edit paths in %s, pull docs, then run profile list / search --profile %s\n",
		configPath, profileName)
}

func configFilePath() (string, error) {
	if v := strings.TrimSpace(os.Getenv("DOCS_PULLER_CONFIG")); v != "" {
		if strings.HasPrefix(v, "~") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			v = filepath.Join(home, strings.TrimPrefix(v, "~/"))
		}
		return filepath.Clean(v), nil
	}
	state, err := apppaths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "config.yaml"), nil
}

func writeConfigInitFile(path string, body []byte, force bool) error {
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists (use --force)", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func bytesReplaceAll(b, old, new []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), string(old), string(new)))
}
