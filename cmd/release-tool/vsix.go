package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxVSIXUncompressedBytes = 64 << 20

var extensionVersionPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

type extensionPackage struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Publisher string `json:"publisher"`
	License   string `json:"license"`
}

type vsixReport struct {
	SchemaVersion    int      `json:"schema_version"`
	OK               bool     `json:"ok"`
	ReleaseVersion   string   `json:"release_version"`
	ExtensionVersion string   `json:"extension_version"`
	Input            string   `json:"input"`
	Output           string   `json:"output"`
	SHA256           string   `json:"sha256"`
	Size             int      `json:"size"`
	Reused           bool     `json:"reused"`
	Files            []string `json:"files"`
}

type vsixPackageManifest struct {
	Identity struct {
		ID        string `xml:"Id,attr"`
		Version   string `xml:"Version,attr"`
		Publisher string `xml:"Publisher,attr"`
	} `xml:"Metadata>Identity"`
	License string `xml:"Metadata>License"`
}

func runVSIX(args []string) error {
	flags := flag.NewFlagSet("vsix", flag.ContinueOnError)
	root := flags.String("root", "", "docs-puller repository root")
	version := flags.String("version", "", "expected v-prefixed docs-puller SemVer")
	input := flags.String("in", "", "raw VSIX input path")
	output := flags.String("out", "", "normalized VSIX output path")
	jsonOut := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("vsix does not accept positional arguments")
	}
	repoRoot, manifest, err := loadContext(*root)
	if err != nil {
		return err
	}
	if err := requireVersion(manifest, *version); err != nil {
		return err
	}

	packageBody, err := os.ReadFile(filepath.Join(repoRoot, "vscode-extension", "package.json"))
	if err != nil {
		return err
	}
	var extension extensionPackage
	if err := json.Unmarshal(packageBody, &extension); err != nil {
		return fmt.Errorf("decode VS Code package: %w", err)
	}
	if err := validateExtensionPackage(extension); err != nil {
		return err
	}

	inputPath := ""
	if strings.TrimSpace(*input) == "" {
		tempParent := filepath.Join(repoRoot, "dist")
		if err := os.MkdirAll(tempParent, 0o755); err != nil {
			return err
		}
		tempRoot, err := os.MkdirTemp(tempParent, ".vsix-raw-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tempRoot)
		inputPath = filepath.Join(tempRoot, extension.Name+".raw.vsix")
		if _, err := commandOutput(filepath.Join(repoRoot, "vscode-extension"), nil, "npm", "exec", "--", "vsce", "package", "--out", inputPath); err != nil {
			return fmt.Errorf("package raw VSIX: %w", err)
		}
	} else {
		inputPath = resolveRepoPath(repoRoot, *input)
	}
	outputPath := *output
	if outputPath == "" {
		outputPath = filepath.Join(repoRoot, "vscode-extension", extension.Name+"-"+extension.Version+".vsix")
	} else {
		outputPath = resolveRepoPath(repoRoot, outputPath)
	}
	if inputPath == outputPath {
		return errors.New("raw and normalized VSIX paths must differ")
	}
	epochText, err := gitOutput(repoRoot, "show", "-s", "--format=%ct", "HEAD")
	if err != nil {
		return err
	}
	epoch, err := strconv.ParseInt(epochText, 10, 64)
	if err != nil {
		return fmt.Errorf("parse commit timestamp: %w", err)
	}
	body, files, err := normalizeVSIX(inputPath, time.Unix(epoch, 0).UTC(), extension)
	if err != nil {
		return err
	}
	reused, err := writeReproducibleOutput(outputPath, body)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	report := vsixReport{
		SchemaVersion: 1, OK: true, ReleaseVersion: manifest.Version,
		ExtensionVersion: extension.Version, Input: inputPath, Output: outputPath,
		SHA256: hex.EncodeToString(digest[:]), Size: len(body), Reused: reused, Files: files,
	}
	if *jsonOut {
		return writeJSON(report)
	}
	fmt.Printf("normalized VSIX %s (%s)\n", outputPath, report.SHA256)
	return nil
}

func resolveRepoPath(repoRoot, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(repoRoot, value)
}

func validateExtensionPackage(extension extensionPackage) error {
	if extension.Name == "" || extension.Publisher == "" || extension.License == "" {
		return errors.New("VS Code package name, publisher, and license are required")
	}
	if filepath.Base(extension.Name) != extension.Name || strings.ContainsAny(extension.Name, `\/`) {
		return fmt.Errorf("VS Code package name %q is unsafe", extension.Name)
	}
	if !extensionVersionPattern.MatchString(extension.Version) {
		return fmt.Errorf("VS Code package version %q is not SemVer", extension.Version)
	}
	return nil
}

func normalizeVSIX(path string, fixedTime time.Time, expected extensionPackage) ([]byte, []string, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, nil, err
	}
	defer reader.Close()

	required := map[string]bool{
		"[Content_Types].xml":        false,
		"extension.vsixmanifest":     false,
		"extension/LICENSE.txt":      false,
		"extension/NOTICE":           false,
		"extension/out/client.js":    false,
		"extension/out/extension.js": false,
		"extension/package.json":     false,
		"extension/readme.md":        false,
	}
	seen := map[string]bool{}
	inputs := make([]archiveInput, 0, len(reader.File))
	total := uint64(0)
	for _, file := range reader.File {
		name := file.Name
		if !safeArchiveName(name) || seen[name] || file.FileInfo().IsDir() || file.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("VSIX has unsafe or duplicate entry %q", name)
		}
		if _, allowed := required[name]; !allowed {
			return nil, nil, fmt.Errorf("VSIX contains forbidden entry %q", name)
		}
		if file.UncompressedSize64 > uint64(maxVSIXUncompressedBytes)-total {
			return nil, nil, errors.New("VSIX exceeds the uncompressed size limit")
		}
		total += file.UncompressedSize64
		stream, err := file.Open()
		if err != nil {
			return nil, nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(stream, int64(file.UncompressedSize64)+1))
		closeErr := stream.Close()
		if readErr != nil {
			return nil, nil, readErr
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
		if uint64(len(body)) != file.UncompressedSize64 {
			return nil, nil, fmt.Errorf("VSIX entry %q size does not match its header", name)
		}
		seen[name] = true
		if _, ok := required[name]; ok {
			required[name] = true
		}
		inputs = append(inputs, archiveInput{Name: name, Mode: 0o644, Body: body})
	}
	for name, present := range required {
		if !present {
			return nil, nil, fmt.Errorf("VSIX is missing required entry %q", name)
		}
	}

	var packaged extensionPackage
	var packagedManifest vsixPackageManifest
	for _, input := range inputs {
		switch input.Name {
		case "extension/package.json":
			if err := json.Unmarshal(input.Body, &packaged); err != nil {
				return nil, nil, fmt.Errorf("decode packaged extension metadata: %w", err)
			}
		case "extension.vsixmanifest":
			if err := xml.Unmarshal(input.Body, &packagedManifest); err != nil {
				return nil, nil, fmt.Errorf("decode VSIX manifest: %w", err)
			}
		}
	}
	if packaged != expected {
		return nil, nil, fmt.Errorf("packaged extension metadata %+v does not match source %+v", packaged, expected)
	}
	if packagedManifest.Identity.ID != expected.Name || packagedManifest.Identity.Version != expected.Version || packagedManifest.Identity.Publisher != expected.Publisher || strings.TrimSpace(packagedManifest.License) != "extension/LICENSE.txt" {
		return nil, nil, fmt.Errorf("VSIX identity %+v does not match source package", packagedManifest.Identity)
	}

	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Name < inputs[j].Name })
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	files := make([]string, 0, len(inputs))
	for _, input := range inputs {
		header := &zip.FileHeader{Name: input.Name, Method: zip.Deflate, Modified: fixedTime}
		header.SetMode(input.Mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			return nil, nil, err
		}
		if _, err := entry.Write(input.Body); err != nil {
			return nil, nil, err
		}
		files = append(files, input.Name)
	}
	if err := writer.Close(); err != nil {
		return nil, nil, err
	}
	if !slices.IsSorted(files) {
		return nil, nil, errors.New("normalized VSIX file list is not sorted")
	}
	return output.Bytes(), files, nil
}

func writeReproducibleOutput(path string, body []byte) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(existing, body) {
			return false, fmt.Errorf("existing normalized VSIX %s differs", path)
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, err
	}
	written := false
	defer func() {
		if !written {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return false, err
	}
	if err := file.Sync(); err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	written = true
	return false, nil
}
