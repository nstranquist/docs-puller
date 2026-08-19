package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	pdfProviderPinSchemaVersion = 1
	pdfProviderName             = "pdf-inspector"
	pdfProviderRepository       = "https://github.com/firecrawl/pdf-inspector"
)

type pdfProviderPin struct {
	SchemaVersion int                    `json:"schema_version"`
	Provider      string                 `json:"provider"`
	Source        pdfProviderSource      `json:"source"`
	Artifacts     map[string]pdfArtifact `json:"artifacts"`
}

type pdfProviderSource struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
	Version    string `json:"version"`
}

type pdfArtifact struct {
	SHA256 string `json:"sha256"`
}

type pdfArtifactReport struct {
	Name           string `json:"name"`
	Path           string `json:"path"`
	SHA256         string `json:"sha256,omitempty"`
	ExpectedSHA256 string `json:"expected_sha256"`
	OK             bool   `json:"ok"`
}

type pdfProviderReport struct {
	SchemaVersion int                 `json:"schema_version"`
	Provider      string              `json:"provider"`
	Pin           string              `json:"pin"`
	Source        pdfProviderSource   `json:"source"`
	Artifacts     []pdfArtifactReport `json:"artifacts"`
	OK            bool                `json:"ok"`
}

type pdfProviderVerification struct {
	Report      pdfProviderReport
	DetectPath  string
	ConvertPath string
}

func cmdPDFDoctor(args []string) {
	fs := newContinueFlagSet("pdf-doctor")
	pinPath := fs.String("provider-pin", "", "existing pdf-inspector pin JSON")
	writePath := fs.String("write-pin", "", "write a new pdf-inspector pin JSON")
	detectBinary := fs.String("detect-pdf", "detect-pdf", "pdf-inspector classifier executable")
	convertBinary := fs.String("pdf2md", "pdf2md", "pdf-inspector Markdown executable")
	sourceRevision := fs.String("source-revision", "", "source revision for --write-pin")
	sourceVersion := fs.String("source-version", "", "source version for --write-pin")
	force := fs.Bool("force", false, "overwrite an existing --write-pin file")
	jsonOut := fs.Bool("json", false, "emit the provider report as JSON")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: docs-puller pdf-doctor --provider-pin PATH [flags]")
		os.Exit(2)
	}
	if strings.TrimSpace(*pinPath) != "" && strings.TrimSpace(*writePath) != "" {
		fmt.Fprintln(os.Stderr, "pdf-doctor: use one of --provider-pin or --write-pin")
		os.Exit(2)
	}
	if strings.TrimSpace(*writePath) != "" {
		if strings.TrimSpace(*sourceRevision) == "" || strings.TrimSpace(*sourceVersion) == "" {
			fmt.Fprintln(os.Stderr, "pdf-doctor: --write-pin requires --source-revision and --source-version")
			os.Exit(2)
		}
		if err := writePDFProviderPin(*writePath, *detectBinary, *convertBinary,
			pdfProviderSource{
				Repository: pdfProviderRepository,
				Revision:   strings.TrimSpace(*sourceRevision),
				Version:    strings.TrimSpace(*sourceVersion),
			}, *force); err != nil {
			fmt.Fprintf(os.Stderr, "pdf-doctor: %v\n", err)
			os.Exit(1)
		}
		*pinPath = *writePath
	}
	if strings.TrimSpace(*pinPath) == "" {
		fmt.Fprintln(os.Stderr, "pdf-doctor: pass --provider-pin or --write-pin")
		os.Exit(2)
	}
	verification, err := verifyPDFProvider(*pinPath, *detectBinary, *convertBinary)
	if *jsonOut {
		if err != nil {
			verification.Report.OK = false
		}
		encErr := json.NewEncoder(os.Stdout).Encode(verification.Report)
		if encErr != nil {
			fmt.Fprintf(os.Stderr, "pdf-doctor: write JSON report: %v\n", encErr)
			os.Exit(1)
		}
	} else if err == nil {
		fmt.Printf("pdf-inspector pin ok: %s\n", *pinPath)
		for _, artifact := range verification.Report.Artifacts {
			fmt.Printf("  %s %s\n", artifact.Name, artifact.Path)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pdf-doctor: %v\n", err)
		os.Exit(1)
	}
}

func newContinueFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func loadPDFProviderPin(path string) (pdfProviderPin, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return pdfProviderPin{}, fmt.Errorf("read provider pin: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	var pin pdfProviderPin
	if err := dec.Decode(&pin); err != nil {
		return pdfProviderPin{}, fmt.Errorf("parse provider pin: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return pdfProviderPin{}, fmt.Errorf("parse provider pin: trailing JSON data")
		}
		return pdfProviderPin{}, fmt.Errorf("parse provider pin: trailing data: %w", err)
	}
	if err := validatePDFProviderPin(pin); err != nil {
		return pdfProviderPin{}, err
	}
	return pin, nil
}

func validatePDFProviderPin(pin pdfProviderPin) error {
	if pin.SchemaVersion != pdfProviderPinSchemaVersion {
		return fmt.Errorf("provider pin schema version %d is unsupported", pin.SchemaVersion)
	}
	if strings.TrimSpace(pin.Provider) != pdfProviderName {
		return fmt.Errorf("provider pin names %q, want %q", pin.Provider, pdfProviderName)
	}
	if strings.TrimSpace(pin.Source.Repository) != pdfProviderRepository {
		return fmt.Errorf("provider pin repository %q, want %q", pin.Source.Repository, pdfProviderRepository)
	}
	if strings.TrimSpace(pin.Source.Repository) == "" ||
		strings.TrimSpace(pin.Source.Revision) == "" ||
		strings.TrimSpace(pin.Source.Version) == "" {
		return fmt.Errorf("provider pin source must include repository, revision, and version")
	}
	for _, name := range []string{"detect-pdf", "pdf2md"} {
		artifact, ok := pin.Artifacts[name]
		if !ok {
			return fmt.Errorf("provider pin is missing %s", name)
		}
		if err := validateSHA256(artifact.SHA256); err != nil {
			return fmt.Errorf("provider pin %s: %w", name, err)
		}
	}
	return nil
}

func validateSHA256(value string) error {
	value = strings.TrimSpace(value)
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("sha256 must contain %d hexadecimal characters", sha256.Size*2)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("sha256 is not hexadecimal: %w", err)
	}
	return nil
}

func verifyPDFProvider(pinPath, detectBinary, convertBinary string) (pdfProviderVerification, error) {
	pin, err := loadPDFProviderPin(pinPath)
	if err != nil {
		return pdfProviderVerification{Report: pdfProviderReport{Pin: pinPath}}, err
	}
	report := pdfProviderReport{
		SchemaVersion: pin.SchemaVersion,
		Provider:      pin.Provider,
		Pin:           pinPath,
		Source:        pin.Source,
		Artifacts:     make([]pdfArtifactReport, 0, 2),
		OK:            true,
	}
	verification := pdfProviderVerification{Report: report}
	for _, spec := range []struct {
		name string
		path string
	}{
		{name: "detect-pdf", path: detectBinary},
		{name: "pdf2md", path: convertBinary},
	} {
		resolved, hash, err := hashExecutable(spec.path)
		artifact := pdfArtifactReport{
			Name:           spec.name,
			Path:           resolved,
			SHA256:         hash,
			ExpectedSHA256: strings.ToLower(strings.TrimSpace(pin.Artifacts[spec.name].SHA256)),
			OK:             err == nil && strings.EqualFold(hash, pin.Artifacts[spec.name].SHA256),
		}
		report.Artifacts = append(report.Artifacts, artifact)
		if err != nil {
			report.OK = false
			return pdfProviderVerification{Report: report}, fmt.Errorf("%s: %w", spec.name, err)
		}
		if !artifact.OK {
			report.OK = false
			return pdfProviderVerification{Report: report}, fmt.Errorf("%s checksum mismatch: got %s, want %s", spec.name, hash, artifact.ExpectedSHA256)
		}
		if spec.name == "detect-pdf" {
			verification.DetectPath = resolved
		} else {
			verification.ConvertPath = resolved
		}
	}
	return pdfProviderVerification{Report: report, DetectPath: verification.DetectPath, ConvertPath: verification.ConvertPath}, nil
}

func hashExecutable(path string) (string, string, error) {
	resolved, err := resolveExecutable(path)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return resolved, "", fmt.Errorf("stat executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return resolved, "", fmt.Errorf("executable is not a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return resolved, "", fmt.Errorf("executable is not executable")
	}
	f, err := os.Open(resolved)
	if err != nil {
		return resolved, "", fmt.Errorf("open executable: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return resolved, "", fmt.Errorf("hash executable: %w", err)
	}
	return resolved, hex.EncodeToString(h.Sum(nil)), nil
}

func resolveExecutable(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("executable path is empty")
	}
	if strings.ContainsRune(path, os.PathSeparator) || filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return path, fmt.Errorf("find executable: %w", err)
	}
	return resolved, nil
}

func writePDFProviderPin(path, detectBinary, convertBinary string, source pdfProviderSource, force bool) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return fmt.Errorf("pin path is empty")
	}
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("pin already exists at %s (use --force to replace it)", path)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("check pin path: %w", err)
	}
	detectPath, detectHash, err := hashExecutable(detectBinary)
	if err != nil {
		return fmt.Errorf("detect-pdf: %w", err)
	}
	convertPath, convertHash, err := hashExecutable(convertBinary)
	if err != nil {
		return fmt.Errorf("pdf2md: %w", err)
	}
	if detectPath == convertPath {
		return fmt.Errorf("detect-pdf and pdf2md resolve to the same executable")
	}
	pin := pdfProviderPin{
		SchemaVersion: pdfProviderPinSchemaVersion,
		Provider:      pdfProviderName,
		Source:        source,
		Artifacts: map[string]pdfArtifact{
			"detect-pdf": {SHA256: detectHash},
			"pdf2md":     {SHA256: convertHash},
		},
	}
	if err := validatePDFProviderPin(pin); err != nil {
		return err
	}
	body, err := json.MarshalIndent(pin, "", "  ")
	if err != nil {
		return fmt.Errorf("encode provider pin: %w", err)
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create pin directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pdf-provider-pin-*")
	if err != nil {
		return fmt.Errorf("create pin temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set pin permissions: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write provider pin: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync provider pin: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close provider pin: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish provider pin: %w", err)
	}
	return nil
}
