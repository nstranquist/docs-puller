package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nstranquist/docs-puller/internal/releasecontract"
)

type artifactRecord struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	GOOS   string `json:"goos,omitempty"`
	GOARCH string `json:"goarch,omitempty"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type outputManifest struct {
	SchemaVersion   int              `json:"schema_version"`
	Name            string           `json:"name"`
	Module          string           `json:"module"`
	Version         string           `json:"version"`
	Tag             string           `json:"tag"`
	Commit          string           `json:"commit"`
	SourceDateEpoch int64            `json:"source_date_epoch"`
	GoVersion       string           `json:"go_version"`
	Reproducible    bool             `json:"reproducible"`
	Artifacts       []artifactRecord `json:"artifacts"`
}

type distReport struct {
	SchemaVersion int              `json:"schema_version"`
	OK            bool             `json:"ok"`
	Version       string           `json:"version"`
	Commit        string           `json:"commit"`
	Output        string           `json:"output"`
	Reused        bool             `json:"reused"`
	Artifacts     []artifactRecord `json:"artifacts"`
}

type moduleInfo struct {
	Path    string      `json:"Path"`
	Version string      `json:"Version"`
	Sum     string      `json:"Sum"`
	Main    bool        `json:"Main"`
	Replace *moduleInfo `json:"Replace"`
}

type archiveInput struct {
	Name string
	Mode fs.FileMode
	Body []byte
}

func runDist(args []string) error {
	flags := flag.NewFlagSet("dist", flag.ContinueOnError)
	root := flags.String("root", "", "docs-puller repository root")
	version := flags.String("version", "", "expected v-prefixed SemVer")
	out := flags.String("out", "", "release output directory")
	jsonOut := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	repoRoot, manifest, err := loadContext(*root)
	if err != nil {
		return err
	}
	if err := requireVersion(manifest, *version); err != nil {
		return err
	}
	dirty, err := gitDirty(repoRoot)
	if err != nil {
		return err
	}
	if dirty {
		return errors.New("refusing to build release assets from a dirty docs-puller worktree")
	}
	output := *out
	if output == "" {
		output = filepath.Join(repoRoot, "dist", manifest.Version)
	} else if !filepath.IsAbs(output) {
		output = filepath.Join(repoRoot, output)
	}
	report, err := buildDistribution(repoRoot, output, manifest)
	if *jsonOut && (err == nil || report.Version != "") {
		if encodeErr := writeJSON(report); encodeErr != nil {
			return encodeErr
		}
	}
	return err
}

func buildDistribution(repoRoot, output string, manifest releasecontract.Manifest) (distReport, error) {
	commit, err := gitOutput(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return distReport{}, err
	}
	epochText, err := gitOutput(repoRoot, "show", "-s", "--format=%ct", "HEAD")
	if err != nil {
		return distReport{}, err
	}
	epoch, err := strconv.ParseInt(epochText, 10, 64)
	if err != nil {
		return distReport{}, fmt.Errorf("parse commit timestamp: %w", err)
	}
	fixedTime := time.Unix(epoch, 0).UTC()
	goVersion, err := commandOutput(repoRoot, nil, "go", "env", "GOVERSION")
	if err != nil {
		return distReport{}, err
	}
	if !matchesGoVersion(goVersion, manifest.GoVersion) {
		return distReport{}, fmt.Errorf("go version %q does not match release contract %q", goVersion, manifest.GoVersion)
	}
	modules, err := loadModuleGraph(repoRoot)
	if err != nil {
		return distReport{}, err
	}

	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return distReport{}, err
	}
	tempRoot, err := os.MkdirTemp(parent, ".docs-puller-release-")
	if err != nil {
		return distReport{}, err
	}
	defer os.RemoveAll(tempRoot)
	assetRoot := filepath.Join(tempRoot, "assets")
	if err := os.MkdirAll(assetRoot, 0o755); err != nil {
		return distReport{}, err
	}

	docInputs, err := readArchiveDocs(repoRoot)
	if err != nil {
		return distReport{}, err
	}
	artifacts := make([]artifactRecord, 0, len(manifest.Targets)+2)
	for _, target := range manifest.Targets {
		binaryName := manifest.Binary
		if target.GOOS == "windows" {
			binaryName += ".exe"
		}
		first := filepath.Join(tempRoot, "build-a", target.GOOS+"-"+target.GOARCH, binaryName)
		second := filepath.Join(tempRoot, "build-b", target.GOOS+"-"+target.GOARCH, binaryName)
		if err := buildBinary(repoRoot, first, target, manifest, commit, epoch); err != nil {
			return distReport{}, err
		}
		if err := buildBinary(repoRoot, second, target, manifest, commit, epoch); err != nil {
			return distReport{}, err
		}
		firstHash, firstSize, err := hashFile(first)
		if err != nil {
			return distReport{}, err
		}
		secondHash, _, err := hashFile(second)
		if err != nil {
			return distReport{}, err
		}
		if firstHash != secondHash {
			return distReport{}, fmt.Errorf("binary for %s/%s is not reproducible: %s != %s", target.GOOS, target.GOARCH, firstHash, secondHash)
		}
		if err := verifyBuildInfoFile(first, target, manifest, commit); err != nil {
			return distReport{}, err
		}
		binaryBody, err := os.ReadFile(first)
		if err != nil {
			return distReport{}, err
		}
		inputs := append([]archiveInput(nil), docInputs...)
		inputs = append(inputs, archiveInput{Name: binaryName, Mode: 0o755, Body: binaryBody})
		sort.Slice(inputs, func(i, j int) bool { return inputs[i].Name < inputs[j].Name })
		archiveName := manifest.ArchiveName(target)
		archivePath := filepath.Join(assetRoot, archiveName)
		if err := writeArchive(archivePath, target.Archive, inputs, fixedTime); err != nil {
			return distReport{}, err
		}
		hash, size, err := hashFile(archivePath)
		if err != nil {
			return distReport{}, err
		}
		if firstSize == 0 {
			return distReport{}, fmt.Errorf("built empty binary for %s/%s", target.GOOS, target.GOARCH)
		}
		artifacts = append(artifacts, artifactRecord{Name: archiveName, Type: "archive", GOOS: target.GOOS, GOARCH: target.GOARCH, SHA256: hash, Size: size})
	}

	sbomName := manifest.SBOMName()
	sbomBody, err := buildCycloneDX(manifest, commit, fixedTime, modules)
	if err != nil {
		return distReport{}, err
	}
	if err := os.WriteFile(filepath.Join(assetRoot, sbomName), sbomBody, 0o644); err != nil {
		return distReport{}, err
	}
	sbomRecord, err := recordFile(filepath.Join(assetRoot, sbomName), sbomName, "sbom")
	if err != nil {
		return distReport{}, err
	}
	artifacts = append(artifacts, sbomRecord)

	slices.SortFunc(artifacts, func(a, b artifactRecord) int { return strings.Compare(a.Name, b.Name) })
	provenanceName := manifest.ProvenanceName()
	provenanceBody, err := buildProvenance(manifest, commit, fixedTime, modules, artifacts)
	if err != nil {
		return distReport{}, err
	}
	if err := os.WriteFile(filepath.Join(assetRoot, provenanceName), provenanceBody, 0o644); err != nil {
		return distReport{}, err
	}
	provenanceRecord, err := recordFile(filepath.Join(assetRoot, provenanceName), provenanceName, "provenance")
	if err != nil {
		return distReport{}, err
	}
	artifacts = append(artifacts, provenanceRecord)
	slices.SortFunc(artifacts, func(a, b artifactRecord) int { return strings.Compare(a.Name, b.Name) })

	releaseOutput := outputManifest{
		SchemaVersion: 1, Name: manifest.Name, Module: manifest.Module,
		Version: manifest.Version, Tag: manifest.Tag, Commit: commit,
		SourceDateEpoch: epoch, GoVersion: goVersion, Reproducible: true,
		Artifacts: artifacts,
	}
	releaseBody, err := marshalIndented(releaseOutput)
	if err != nil {
		return distReport{}, err
	}
	if err := os.WriteFile(filepath.Join(assetRoot, manifest.ReleaseManifestName), releaseBody, 0o644); err != nil {
		return distReport{}, err
	}
	checksumNames := make([]string, 0, len(artifacts)+1)
	for _, artifact := range artifacts {
		checksumNames = append(checksumNames, artifact.Name)
	}
	checksumNames = append(checksumNames, manifest.ReleaseManifestName)
	sort.Strings(checksumNames)
	var checksumBody strings.Builder
	for _, name := range checksumNames {
		hash, _, err := hashFile(filepath.Join(assetRoot, name))
		if err != nil {
			return distReport{}, err
		}
		fmt.Fprintf(&checksumBody, "%s  %s\n", hash, name)
	}
	if err := os.WriteFile(filepath.Join(assetRoot, manifest.ChecksumsName), []byte(checksumBody.String()), 0o644); err != nil {
		return distReport{}, err
	}

	reused := false
	if info, statErr := os.Stat(output); statErr == nil {
		if !info.IsDir() {
			return distReport{}, fmt.Errorf("release output %s exists and is not a directory", output)
		}
		if err := compareDirectories(assetRoot, output); err != nil {
			return distReport{}, fmt.Errorf("existing release output differs: %w", err)
		}
		reused = true
	} else if !os.IsNotExist(statErr) {
		return distReport{}, statErr
	} else if err := os.Rename(assetRoot, output); err != nil {
		return distReport{}, err
	}

	return distReport{SchemaVersion: 1, OK: true, Version: manifest.Version, Commit: commit, Output: output, Reused: reused, Artifacts: artifacts}, nil
}

func buildBinary(repoRoot, output string, target releasecontract.Target, manifest releasecontract.Manifest, commit string, epoch int64) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	ldflags := strings.Join([]string{
		"-s", "-w", "-buildid=",
		"-X", "main.releaseIdentity=" + releasecontract.BuildIdentity(manifest.Version, commit),
	}, " ")
	env := []string{
		"CGO_ENABLED=0", "GOOS=" + target.GOOS, "GOARCH=" + target.GOARCH,
		"GOWORK=off", "SOURCE_DATE_EPOCH=" + strconv.FormatInt(epoch, 10),
		"TZ=UTC", "LC_ALL=C",
	}
	_, err := commandOutput(repoRoot, env, "go", "build", "-mod=readonly", "-tags", "sqlite_fts5", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", output, ".")
	if err != nil {
		return fmt.Errorf("build %s/%s: %w", target.GOOS, target.GOARCH, err)
	}
	return nil
}

func verifyBuildInfoFile(path string, target releasecontract.Target, manifest releasecontract.Manifest, commit string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read build info %s: %w", path, err)
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	for key, want := range map[string]string{"GOOS": target.GOOS, "GOARCH": target.GOARCH, "CGO_ENABLED": "0"} {
		if settings[key] != want {
			return fmt.Errorf("%s build setting %s = %q, want %q", path, key, settings[key], want)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read release identity %s: %w", path, err)
	}
	identity := releasecontract.BuildIdentity(manifest.Version, commit)
	if !bytes.Contains(body, []byte(identity)) {
		return fmt.Errorf("%s does not contain the embedded release identity", path)
	}
	if settings["-trimpath"] != "true" {
		return fmt.Errorf("%s was not built with -trimpath", path)
	}
	return nil
}

func readArchiveDocs(repoRoot string) ([]archiveInput, error) {
	var inputs []archiveInput
	for _, name := range []string{"LICENSE", "NOTICE", "README.md"} {
		body, err := os.ReadFile(filepath.Join(repoRoot, name))
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, archiveInput{Name: name, Mode: 0o644, Body: body})
	}
	return inputs, nil
}

func writeArchive(path, format string, inputs []archiveInput, fixedTime time.Time) error {
	var body bytes.Buffer
	switch format {
	case "tar.gz":
		gzipWriter, err := gzip.NewWriterLevel(&body, gzip.BestCompression)
		if err != nil {
			return err
		}
		gzipWriter.Header.ModTime = fixedTime
		gzipWriter.Header.OS = 255
		tarWriter := tar.NewWriter(gzipWriter)
		for _, input := range inputs {
			header := &tar.Header{Name: input.Name, Mode: int64(input.Mode.Perm()), Size: int64(len(input.Body)), ModTime: fixedTime, AccessTime: fixedTime, ChangeTime: fixedTime, Format: tar.FormatPAX}
			if err := tarWriter.WriteHeader(header); err != nil {
				return err
			}
			if _, err := tarWriter.Write(input.Body); err != nil {
				return err
			}
		}
		if err := tarWriter.Close(); err != nil {
			return err
		}
		if err := gzipWriter.Close(); err != nil {
			return err
		}
	case "zip":
		zipWriter := zip.NewWriter(&body)
		for _, input := range inputs {
			header := &zip.FileHeader{Name: input.Name, Method: zip.Deflate}
			header.SetMode(input.Mode)
			header.Modified = fixedTime
			writer, err := zipWriter.CreateHeader(header)
			if err != nil {
				return err
			}
			if _, err := writer.Write(input.Body); err != nil {
				return err
			}
		}
		if err := zipWriter.Close(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported archive format %q", format)
	}
	return os.WriteFile(path, body.Bytes(), 0o644)
}

func loadModuleGraph(repoRoot string) ([]moduleInfo, error) {
	body, err := commandOutput(repoRoot, []string{"GOWORK=off"}, "go", "list", "-mod=readonly", "-m", "-json", "all")
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	var modules []moduleInfo
	for {
		var module moduleInfo
		if err := decoder.Decode(&module); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if module.Replace != nil && module.Replace.Version == "" {
			return nil, fmt.Errorf("module %s uses a local replacement, which is not release-reproducible", module.Path)
		}
		modules = append(modules, module)
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Path < modules[j].Path })
	return modules, nil
}

type cdxComponent struct {
	Type       string        `json:"type"`
	Name       string        `json:"name"`
	Version    string        `json:"version"`
	PURL       string        `json:"purl"`
	BOMRef     string        `json:"bom-ref"`
	Properties []cdxProperty `json:"properties,omitempty"`
}

type cdxProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type cycloneDX struct {
	Schema       string         `json:"$schema"`
	BOMFormat    string         `json:"bomFormat"`
	SpecVersion  string         `json:"specVersion"`
	SerialNumber string         `json:"serialNumber"`
	Version      int            `json:"version"`
	Metadata     cdxMetadata    `json:"metadata"`
	Components   []cdxComponent `json:"components"`
}

type cdxMetadata struct {
	Timestamp string       `json:"timestamp"`
	Tools     []cdxTool    `json:"tools"`
	Component cdxComponent `json:"component"`
}

type cdxTool struct {
	Vendor  string `json:"vendor"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

func buildCycloneDX(manifest releasecontract.Manifest, commit string, fixedTime time.Time, modules []moduleInfo) ([]byte, error) {
	appPURL := goPURL(manifest.Module, manifest.Version)
	bom := cycloneDX{
		Schema: "https://cyclonedx.org/schema/bom-1.6.schema.json", BOMFormat: "CycloneDX", SpecVersion: "1.6",
		SerialNumber: "urn:uuid:" + deterministicUUID(manifest.Version+":"+commit), Version: 1,
		Metadata: cdxMetadata{
			Timestamp: fixedTime.Format(time.RFC3339),
			Tools:     []cdxTool{{Vendor: "docs-puller", Name: "release-tool", Version: "1"}},
			Component: cdxComponent{Type: "application", Name: manifest.Name, Version: manifest.Version, PURL: appPURL, BOMRef: appPURL},
		},
	}
	for _, module := range modules {
		if module.Main {
			continue
		}
		path, version, sum := module.Path, module.Version, module.Sum
		if module.Replace != nil {
			path, version, sum = module.Replace.Path, module.Replace.Version, module.Replace.Sum
		}
		purl := goPURL(path, version)
		component := cdxComponent{Type: "library", Name: path, Version: version, PURL: purl, BOMRef: purl}
		if sum != "" {
			component.Properties = []cdxProperty{{Name: "docs-puller:go-module-sum", Value: sum}}
		}
		bom.Components = append(bom.Components, component)
	}
	return marshalIndented(bom)
}

func goPURL(path, version string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "pkg:golang/" + strings.Join(parts, "/") + "@" + url.PathEscape(version)
}

func deterministicUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	value := append([]byte(nil), sum[:16]...)
	value[6] = (value[6] & 0x0f) | 0x50
	value[8] = (value[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(value)
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:32]
}

type provenanceSubject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

type provenanceStatement struct {
	Type          string              `json:"_type"`
	Subject       []provenanceSubject `json:"subject"`
	PredicateType string              `json:"predicateType"`
	Predicate     any                 `json:"predicate"`
}

func buildProvenance(manifest releasecontract.Manifest, commit string, fixedTime time.Time, modules []moduleInfo, artifacts []artifactRecord) ([]byte, error) {
	subjects := make([]provenanceSubject, 0, len(artifacts))
	for _, artifact := range artifacts {
		subjects = append(subjects, provenanceSubject{Name: artifact.Name, Digest: map[string]string{"sha256": artifact.SHA256}})
	}
	dependencies := make([]map[string]string, 0, len(modules)+1)
	dependencies = append(dependencies, map[string]string{"uri": "git+https://github.com/nstranquist/docs-puller@" + commit})
	for _, module := range modules {
		if module.Main {
			continue
		}
		path, version := module.Path, module.Version
		if module.Replace != nil {
			path, version = module.Replace.Path, module.Replace.Version
		}
		dependencies = append(dependencies, map[string]string{"uri": goPURL(path, version)})
	}
	targets := make([]string, 0, len(manifest.Targets))
	for _, target := range manifest.Targets {
		targets = append(targets, target.GOOS+"/"+target.GOARCH)
	}
	statement := provenanceStatement{
		Type: "https://in-toto.io/Statement/v1", Subject: subjects,
		PredicateType: "https://slsa.dev/provenance/v1",
		Predicate: map[string]any{
			"buildDefinition": map[string]any{
				"buildType":            "https://github.com/nstranquist/docs-puller/release-tool/v1",
				"externalParameters":   map[string]any{"version": manifest.Version, "targets": targets},
				"internalParameters":   map[string]any{"go_version": manifest.GoVersion, "cgo_enabled": false, "trimpath": true},
				"resolvedDependencies": dependencies,
			},
			"runDetails": map[string]any{
				"builder":  map[string]string{"id": "https://github.com/nstranquist/docs-puller/tree/" + commit + "/cmd/release-tool"},
				"metadata": map[string]any{"invocationId": commit, "startedOn": fixedTime.Format(time.RFC3339), "finishedOn": fixedTime.Format(time.RFC3339)},
			},
		},
	}
	body, err := json.Marshal(statement)
	return append(body, '\n'), err
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	root := flags.String("root", "", "docs-puller repository root")
	version := flags.String("version", "", "expected v-prefixed SemVer")
	out := flags.String("out", "", "release output directory")
	jsonOut := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	repoRoot, manifest, err := loadContext(*root)
	if err != nil {
		return err
	}
	if err := requireVersion(manifest, *version); err != nil {
		return err
	}
	output := *out
	if output == "" {
		output = filepath.Join(repoRoot, "dist", manifest.Version)
	} else if !filepath.IsAbs(output) {
		output = filepath.Join(repoRoot, output)
	}
	report, err := verifyDistribution(output, manifest)
	if *jsonOut && (err == nil || report.Version != "") {
		if encodeErr := writeJSON(report); encodeErr != nil {
			return encodeErr
		}
	}
	return err
}

func verifyDistribution(output string, manifest releasecontract.Manifest) (distReport, error) {
	releaseBody, err := os.ReadFile(filepath.Join(output, manifest.ReleaseManifestName))
	if err != nil {
		return distReport{}, err
	}
	var releaseOutput outputManifest
	if err := decodeJSONStrict(releaseBody, &releaseOutput); err != nil {
		return distReport{}, err
	}
	report := distReport{SchemaVersion: 1, Version: releaseOutput.Version, Commit: releaseOutput.Commit, Output: output, Artifacts: releaseOutput.Artifacts}
	if releaseOutput.SchemaVersion != 1 || releaseOutput.Name != manifest.Name || releaseOutput.Module != manifest.Module || releaseOutput.Version != manifest.Version || releaseOutput.Tag != manifest.Tag || !releaseOutput.Reproducible {
		return report, errors.New("release output manifest does not match the source release contract")
	}
	if releaseOutput.SourceDateEpoch <= 0 || !matchesGoVersion(releaseOutput.GoVersion, manifest.GoVersion) || !validCommit(releaseOutput.Commit) {
		return report, errors.New("release output build metadata is incomplete")
	}
	if err := verifyArtifactContract(output, manifest, releaseOutput.Artifacts); err != nil {
		return report, err
	}
	checksums, err := readChecksums(filepath.Join(output, manifest.ChecksumsName))
	if err != nil {
		return report, err
	}
	expectedNames := make([]string, 0, len(releaseOutput.Artifacts)+1)
	for _, artifact := range releaseOutput.Artifacts {
		expectedNames = append(expectedNames, artifact.Name)
	}
	expectedNames = append(expectedNames, manifest.ReleaseManifestName)
	sort.Strings(expectedNames)
	actualNames := make([]string, 0, len(checksums))
	for name := range checksums {
		actualNames = append(actualNames, name)
	}
	sort.Strings(actualNames)
	if !slices.Equal(expectedNames, actualNames) {
		return report, fmt.Errorf("checksum file names differ: got %v want %v", actualNames, expectedNames)
	}
	for name, want := range checksums {
		got, _, err := hashFile(filepath.Join(output, name))
		if err != nil {
			return report, err
		}
		if got != want {
			return report, fmt.Errorf("checksum mismatch for %s: got %s want %s", name, got, want)
		}
	}

	for _, artifact := range releaseOutput.Artifacts {
		hash, size, err := hashFile(filepath.Join(output, artifact.Name))
		if err != nil {
			return report, err
		}
		if hash != artifact.SHA256 || size != artifact.Size {
			return report, fmt.Errorf("artifact metadata mismatch for %s", artifact.Name)
		}
		if artifact.Type == "archive" {
			target := releasecontract.Target{GOOS: artifact.GOOS, GOARCH: artifact.GOARCH}
			if strings.HasSuffix(artifact.Name, ".tar.gz") {
				target.Archive = "tar.gz"
			} else {
				target.Archive = "zip"
			}
			binary, err := readBinaryFromArchive(filepath.Join(output, artifact.Name), target, manifest)
			if err != nil {
				return report, err
			}
			if err := verifyBuildInfoBytes(binary, target, manifest, releaseOutput.Commit); err != nil {
				return report, err
			}
			if target.GOOS == runtime.GOOS && target.GOARCH == runtime.GOARCH {
				if err := smokeHostBinary(binary, target, manifest); err != nil {
					return report, err
				}
			}
		}
	}
	if err := verifySBOM(filepath.Join(output, manifest.SBOMName()), manifest); err != nil {
		return report, err
	}
	if err := verifyProvenance(filepath.Join(output, manifest.ProvenanceName()), releaseOutput.Artifacts); err != nil {
		return report, err
	}
	report.OK = true
	return report, nil
}

func verifyArtifactContract(output string, manifest releasecontract.Manifest, artifacts []artifactRecord) error {
	expectedArchives := make(map[string]releasecontract.Target, len(manifest.Targets))
	for _, target := range manifest.Targets {
		expectedArchives[manifest.ArchiveName(target)] = target
	}
	if len(artifacts) != len(expectedArchives)+2 {
		return fmt.Errorf("release has %d artifacts, want %d", len(artifacts), len(expectedArchives)+2)
	}

	seen := make(map[string]bool, len(artifacts))
	for i, artifact := range artifacts {
		if i > 0 && artifacts[i-1].Name >= artifact.Name {
			return errors.New("release artifacts must be sorted and unique")
		}
		if !safeArchiveName(artifact.Name) || filepath.Base(artifact.Name) != artifact.Name || !validSHA256(artifact.SHA256) || artifact.Size <= 0 {
			return fmt.Errorf("release artifact metadata is invalid for %q", artifact.Name)
		}
		if seen[artifact.Name] {
			return fmt.Errorf("release artifact %q is duplicated", artifact.Name)
		}
		seen[artifact.Name] = true

		switch artifact.Name {
		case manifest.SBOMName():
			if artifact.Type != "sbom" || artifact.GOOS != "" || artifact.GOARCH != "" {
				return errors.New("SBOM artifact metadata does not match the release contract")
			}
		case manifest.ProvenanceName():
			if artifact.Type != "provenance" || artifact.GOOS != "" || artifact.GOARCH != "" {
				return errors.New("provenance artifact metadata does not match the release contract")
			}
		default:
			target, ok := expectedArchives[artifact.Name]
			if !ok || artifact.Type != "archive" || artifact.GOOS != target.GOOS || artifact.GOARCH != target.GOARCH {
				return fmt.Errorf("archive artifact %q does not match the release target matrix", artifact.Name)
			}
		}
	}

	expectedFiles := make([]string, 0, len(artifacts)+2)
	for _, artifact := range artifacts {
		expectedFiles = append(expectedFiles, artifact.Name)
	}
	expectedFiles = append(expectedFiles, manifest.ChecksumsName, manifest.ReleaseManifestName)
	sort.Strings(expectedFiles)
	actualFiles, err := directoryFiles(output)
	if err != nil {
		return err
	}
	if !slices.Equal(actualFiles, expectedFiles) {
		return fmt.Errorf("release directory file list differs: got %v want %v", actualFiles, expectedFiles)
	}
	return nil
}

func matchesGoVersion(actual, majorMinor string) bool {
	return actual == "go"+majorMinor || strings.HasPrefix(actual, "go"+majorMinor+".")
}

func validCommit(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value && (len(decoded) == 20 || len(decoded) == 32)
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value && len(decoded) == sha256.Size
}

func readBinaryFromArchive(path string, target releasecontract.Target, manifest releasecontract.Manifest) ([]byte, error) {
	binaryName := manifest.Binary
	if target.GOOS == "windows" {
		binaryName += ".exe"
	}
	expected := []string{"LICENSE", "NOTICE", "README.md", binaryName}
	sort.Strings(expected)
	var names []string
	var binary []byte
	if target.Archive == "tar.gz" {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			return nil, err
		}
		defer gzipReader.Close()
		reader := tar.NewReader(gzipReader)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if !safeArchiveName(header.Name) {
				return nil, fmt.Errorf("unsafe archive path %q", header.Name)
			}
			names = append(names, header.Name)
			if header.Name == binaryName {
				binary, err = io.ReadAll(reader)
				if err != nil {
					return nil, err
				}
			}
		}
	} else {
		reader, err := zip.OpenReader(path)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		for _, file := range reader.File {
			if !safeArchiveName(file.Name) {
				return nil, fmt.Errorf("unsafe archive path %q", file.Name)
			}
			names = append(names, file.Name)
			if file.Name == binaryName {
				stream, err := file.Open()
				if err != nil {
					return nil, err
				}
				binary, err = io.ReadAll(stream)
				stream.Close()
				if err != nil {
					return nil, err
				}
			}
		}
	}
	sort.Strings(names)
	if !slices.Equal(names, expected) {
		return nil, fmt.Errorf("archive entries differ for %s: got %v want %v", path, names, expected)
	}
	if len(binary) == 0 {
		return nil, fmt.Errorf("archive %s has no binary", path)
	}
	return binary, nil
}

func safeArchiveName(name string) bool {
	return name != "" && !strings.Contains(name, `\`) && !strings.HasPrefix(name, "/") && pathpkg.Clean(name) == name && name != "." && name != ".." && !strings.HasPrefix(name, "../")
}

func verifyBuildInfoBytes(binary []byte, target releasecontract.Target, manifest releasecontract.Manifest, commit string) error {
	temp, err := os.CreateTemp("", "docs-puller-buildinfo-")
	if err != nil {
		return err
	}
	path := temp.Name()
	defer os.Remove(path)
	if _, err := temp.Write(binary); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return verifyBuildInfoFile(path, target, manifest, commit)
}

func smokeHostBinary(binary []byte, target releasecontract.Target, manifest releasecontract.Manifest) error {
	dir, err := os.MkdirTemp("", "docs-puller-release-smoke-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	name := manifest.Binary
	if target.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, binary, 0o755); err != nil {
		return err
	}
	versionBody, err := commandOutput("", nil, path, "version", "--expect", manifest.Version, "--json")
	if err != nil {
		return fmt.Errorf("host package version smoke: %w", err)
	}
	var version struct {
		Version string `json:"version"`
		Dirty   bool   `json:"dirty"`
	}
	if err := json.Unmarshal([]byte(versionBody), &version); err != nil {
		return err
	}
	if version.Version != manifest.Version || version.Dirty {
		return fmt.Errorf("host package version contract = %+v", version)
	}
	demoBody, err := commandOutput("", nil, path, "demo", "--json")
	if err != nil {
		return fmt.Errorf("host package demo smoke: %w", err)
	}
	var demo struct {
		OK        bool `json:"ok"`
		Documents int  `json:"documents"`
	}
	if err := json.Unmarshal([]byte(demoBody), &demo); err != nil {
		return err
	}
	if !demo.OK || demo.Documents != 3 {
		return fmt.Errorf("host package demo contract = %+v", demo)
	}
	return nil
}

func verifySBOM(path string, manifest releasecontract.Manifest) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var bom cycloneDX
	if err := decodeJSONStrict(body, &bom); err != nil {
		return err
	}
	if bom.BOMFormat != "CycloneDX" || bom.SpecVersion != "1.6" || bom.Metadata.Component.Name != manifest.Name || bom.Metadata.Component.Version != manifest.Version || bom.Metadata.Component.PURL != goPURL(manifest.Module, manifest.Version) || len(bom.Components) == 0 {
		return errors.New("SBOM is incomplete or does not match the release")
	}
	seen := map[string]bool{bom.Metadata.Component.BOMRef: true}
	for _, component := range bom.Components {
		if component.Name == "" || component.Version == "" || component.PURL == "" || component.BOMRef == "" || seen[component.BOMRef] {
			return fmt.Errorf("SBOM has an incomplete or duplicate component %q", component.Name)
		}
		seen[component.BOMRef] = true
	}
	return nil
}

func verifyProvenance(path string, artifacts []artifactRecord) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var statement provenanceStatement
	if err := decodeJSONStrict(body, &statement); err != nil {
		return err
	}
	want := map[string]string{}
	for _, artifact := range artifacts {
		if artifact.Type != "provenance" {
			want[artifact.Name] = artifact.SHA256
		}
	}
	for _, subject := range statement.Subject {
		if want[subject.Name] != subject.Digest["sha256"] {
			return fmt.Errorf("provenance subject mismatch for %s", subject.Name)
		}
		delete(want, subject.Name)
	}
	if len(want) != 0 || statement.Type != "https://in-toto.io/Statement/v1" || statement.PredicateType != "https://slsa.dev/provenance/v1" {
		return fmt.Errorf("provenance is incomplete: remaining subjects %v", want)
	}
	return nil
}

func readChecksums(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), "  ", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid checksum line %q", scanner.Text())
		}
		decoded, decodeErr := hex.DecodeString(parts[0])
		if len(parts[0]) != 64 || decodeErr != nil || len(decoded) != sha256.Size || strings.ToLower(parts[0]) != parts[0] || !safeArchiveName(parts[1]) || filepath.Base(parts[1]) != parts[1] {
			return nil, fmt.Errorf("invalid checksum line %q", scanner.Text())
		}
		if values[parts[1]] != "" {
			return nil, fmt.Errorf("duplicate checksum for %s", parts[1])
		}
		values[parts[1]] = parts[0]
	}
	return values, scanner.Err()
}

func recordFile(path, name, kind string) (artifactRecord, error) {
	hash, size, err := hashFile(path)
	return artifactRecord{Name: name, Type: kind, SHA256: hash, Size: size}, err
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

func commandOutput(root string, env []string, name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	if root != "" {
		command.Dir = root
	}
	command.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	body, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(body)), nil
}

func compareDirectories(left, right string) error {
	leftFiles, err := directoryFiles(left)
	if err != nil {
		return err
	}
	rightFiles, err := directoryFiles(right)
	if err != nil {
		return err
	}
	if !slices.Equal(leftFiles, rightFiles) {
		return fmt.Errorf("file lists differ: %v != %v", leftFiles, rightFiles)
	}
	for _, rel := range leftFiles {
		leftBody, err := os.ReadFile(filepath.Join(left, rel))
		if err != nil {
			return err
		}
		rightBody, err := os.ReadFile(filepath.Join(right, rel))
		if err != nil {
			return err
		}
		if !bytes.Equal(leftBody, rightBody) {
			return fmt.Errorf("%s differs", rel)
		}
	}
	return nil
}

func directoryFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(files)
	return files, err
}

func marshalIndented(value any) ([]byte, error) {
	body, err := json.MarshalIndent(value, "", "  ")
	return append(body, '\n'), err
}
