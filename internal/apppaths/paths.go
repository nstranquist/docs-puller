package apppaths

import (
	"os"
	"path/filepath"
)

// StateDir is the docs-puller config/state root. Override with DOCS_PULLER_HOME
// (full path) or set DOCS_PULLER_LEGACY_NDEV_PATHS=1 to keep ~/.nicos-dev for
// ndev-integrated installs.
func StateDir() (string, error) {
	if v := os.Getenv("DOCS_PULLER_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if os.Getenv("DOCS_PULLER_LEGACY_NDEV_PATHS") == "1" {
		return filepath.Join(home, ".nicos-dev"), nil
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "docs-puller"), nil
	}
	return filepath.Join(home, ".docs-puller"), nil
}

// EvalRunRoot is where `eval --record-run` writes stable artifacts.
func EvalRunRoot() (string, error) {
	state, err := StateDir()
	if err != nil {
		return "", err
	}
	if os.Getenv("DOCS_PULLER_LEGACY_NDEV_PATHS") == "1" {
		return filepath.Join(state, "evals", "docs-puller"), nil
	}
	return filepath.Join(state, "evals"), nil
}

// UsageLogDir holds optional ai-usage.jsonl telemetry.
func UsageLogDir() (string, error) {
	state, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "state"), nil
}
