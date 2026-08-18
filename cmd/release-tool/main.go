// Command release-tool builds and verifies docs-puller release assets.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nstranquist/docs-puller/internal/releasecontract"
)

type syncReport struct {
	SchemaVersion        int      `json:"schema_version"`
	OK                   bool     `json:"ok"`
	Version              string   `json:"version"`
	Tag                  string   `json:"tag"`
	Dirty                bool     `json:"dirty"`
	Drift                []string `json:"drift"`
	CheckedConsumerPaths []string `json:"checked_consumer_paths"`
	WrittenPaths         []string `json:"written_paths,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("usage: release-tool check|sync|dist|verify [flags]"))
	}
	var err error
	switch os.Args[1] {
	case "check":
		err = runCheck(os.Args[2:])
	case "sync":
		err = runSync(os.Args[2:])
	case "dist":
		err = runDist(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	default:
		err = fmt.Errorf("unknown release-tool subcommand %q", os.Args[1])
	}
	if err != nil {
		fatal(err)
	}
}

func runCheck(args []string) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	root := flags.String("root", "", "docs-puller repository root")
	version := flags.String("version", "", "expected v-prefixed SemVer")
	jsonOut := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	repoRoot, manifest, err := loadContext(*root)
	if err != nil {
		return err
	}
	report, err := checkConsumers(repoRoot, "", manifest, true)
	if err == nil {
		err = requireVersion(manifest, *version)
	}
	if *jsonOut {
		if encodeErr := writeJSON(report); encodeErr != nil {
			return encodeErr
		}
	}
	if err != nil {
		return err
	}
	if !report.OK {
		return fmt.Errorf("release check found drift: %s", strings.Join(report.Drift, "; "))
	}
	return nil
}

func runSync(args []string) error {
	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	root := flags.String("root", "", "docs-puller repository root")
	ndevRoot := flags.String("ndev-root", "", "nicos-tools repository root")
	version := flags.String("version", "", "expected v-prefixed SemVer")
	write := flags.Bool("write", false, "write generated consumer values")
	jsonOut := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*ndevRoot) == "" {
		return errors.New("sync requires --ndev-root")
	}
	repoRoot, manifest, err := loadContext(*root)
	if err != nil {
		return err
	}
	if err := requireVersion(manifest, *version); err != nil {
		return err
	}
	var written []string
	if *write {
		written, err = writeConsumers(repoRoot, *ndevRoot, manifest)
		if err != nil {
			return err
		}
	}
	report, err := checkConsumers(repoRoot, *ndevRoot, manifest, !*write)
	report.WrittenPaths = written
	if *jsonOut {
		if encodeErr := writeJSON(report); encodeErr != nil {
			return encodeErr
		}
	}
	if err != nil {
		return err
	}
	if len(report.Drift) != 0 {
		return fmt.Errorf("release sync found drift: %s", strings.Join(report.Drift, "; "))
	}
	return nil
}

func loadContext(root string) (string, releasecontract.Manifest, error) {
	if strings.TrimSpace(root) == "" {
		resolved, err := gitOutput("", "rev-parse", "--show-toplevel")
		if err != nil {
			return "", releasecontract.Manifest{}, err
		}
		root = resolved
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", releasecontract.Manifest{}, err
	}
	body, err := os.ReadFile(filepath.Join(abs, "release", "manifest.json"))
	if err != nil {
		return "", releasecontract.Manifest{}, fmt.Errorf("read release manifest: %w", err)
	}
	manifest, err := releasecontract.Parse(body)
	if err != nil {
		return "", releasecontract.Manifest{}, err
	}
	return abs, manifest, nil
}

func requireVersion(manifest releasecontract.Manifest, expected string) error {
	if expected != "" && expected != manifest.Version {
		return fmt.Errorf("requested version %q does not match release manifest %q", expected, manifest.Version)
	}
	return nil
}

func gitOutput(root string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	if root != "" {
		command.Dir = root
	}
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func gitDirty(root string) (bool, error) {
	out, err := gitOutput(root, "status", "--porcelain", "--untracked-files=normal")
	return out != "", err
}

func writeJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func decodeJSONStrict(body []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "release-tool:", err)
	os.Exit(1)
}
