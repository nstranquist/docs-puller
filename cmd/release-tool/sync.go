package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/nstranquist/docs-puller/internal/releasecontract"
)

type dependencySnapshot struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Ownership     string `json:"ownership"`
	Repository    string `json:"repository"`
	Module        string `json:"module"`
	LocalSource   string `json:"local_source"`
	ManagedBinary string `json:"managed_binary"`
	Fallback      struct {
		Version         string `json:"version"`
		ContractVersion int    `json:"contract_version"`
	} `json:"released_fallback"`
	DevelopmentContract struct {
		MinimumVersion       int      `json:"minimum_version"`
		RequiredCapabilities []string `json:"required_capabilities"`
	} `json:"development_contract"`
	Commands          []string `json:"commands"`
	NextReleaseAction string   `json:"next_release_action"`
}

type demoCorpusIdentity struct {
	CorpusDigest string `json:"corpus_digest"`
	IndexDigest  string `json:"index_digest"`
	RetrievedAt  string `json:"retrieved_at"`
}

func checkConsumers(repoRoot, ndevRoot string, manifest releasecontract.Manifest, requireClean bool) (syncReport, error) {
	report := syncReport{SchemaVersion: 1, Version: manifest.Version, Tag: manifest.Tag}
	dirty, err := gitDirty(repoRoot)
	if err != nil {
		return report, err
	}
	report.Dirty = dirty
	if requireClean && dirty {
		report.Drift = append(report.Drift, "docs-puller worktree is dirty")
	}

	manifestPath := filepath.Join(repoRoot, "release", "manifest.json")
	versionPath := filepath.Join(repoRoot, "VERSION")
	launchPath := filepath.Join(repoRoot, manifest.Consumers.NShipLaunch)
	readmePath := filepath.Join(repoRoot, "README.md")
	installPath := filepath.Join(repoRoot, "docs", "user", "install.md")
	firstHourPath := filepath.Join(repoRoot, "docs", "user", "first-hour.md")
	corpusLockPath := filepath.Join(repoRoot, "deploy", "demo", "corpus.lock.json")
	workerConfigPath := filepath.Join(repoRoot, "site", "wrangler.toml")
	deployWorkflowPath := filepath.Join(repoRoot, ".github", "workflows", "demo-deploy.yml")
	localPaths := []string{
		manifestPath,
		versionPath,
		launchPath,
		readmePath,
		installPath,
		firstHourPath,
		corpusLockPath,
		workerConfigPath,
		deployWorkflowPath,
	}
	report.CheckedConsumerPaths = append(report.CheckedConsumerPaths, localPaths...)
	checkVersionFile(&report, versionPath, manifest.SemVer())
	checkNShipLaunch(&report, launchPath, manifest)
	for _, path := range []string{readmePath, installPath, firstHourPath} {
		checkContainsVersion(&report, path, manifest.Version)
	}
	checkDemoDeploymentIdentity(&report, corpusLockPath, workerConfigPath, deployWorkflowPath)

	if ndevRoot != "" {
		ndevAbs, err := filepath.Abs(ndevRoot)
		if err != nil {
			return report, err
		}
		paths := []string{
			filepath.Join(ndevAbs, manifest.Consumers.NDevDependency),
			filepath.Join(ndevAbs, manifest.Consumers.CatalogProduct),
			filepath.Join(ndevAbs, manifest.Consumers.PortfolioPolicy),
			filepath.Join(ndevAbs, manifest.Consumers.ExternalProjects),
		}
		report.CheckedConsumerPaths = append(report.CheckedConsumerPaths, paths...)
		checkDependencySnapshot(&report, paths[0], manifest)
		checkContainsAll(&report, paths[1], []string{
			"/releases/tag/" + manifest.Version,
			`launch_blocker: "OSS ` + manifest.Version,
			"through " + manifest.Version + ".",
			"public and attested " + manifest.Version + " release",
			"Apache-2.0, " + manifest.Version + ", CI green",
		})
		checkContainsCount(&report, paths[1], `  verified_at: "`+manifest.ReleaseDate+`"`, 2)
		checkScopedContainsAll(&report, paths[2], `^    product\.docs-puller:\s*$`, `^    [A-Za-z0-9_.-]+:\s*$`, []string{
			"/releases/tag/" + manifest.Version,
			"Release " + manifest.Version + " is published and attested",
			`verified_at: "` + manifest.ReleaseDate + `"`,
		})
		checkScopedContainsAll(&report, paths[3], `^    - id: product\.docs-puller\s*$`, `^    - id: `, []string{
			"latest release " + manifest.Version,
			`verified_at: "` + manifest.ReleaseDate + `"`,
		})
	}
	slices.Sort(report.CheckedConsumerPaths)
	report.OK = len(report.Drift) == 0
	return report, nil
}

func checkDemoDeploymentIdentity(report *syncReport, lockPath, configPath, workflowPath string) {
	body, err := os.ReadFile(lockPath)
	if err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: %v", lockPath, err))
		return
	}
	var identity demoCorpusIdentity
	if err := json.Unmarshal(body, &identity); err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: decode: %v", lockPath, err))
		return
	}

	config, err := os.ReadFile(configPath)
	if err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: %v", configPath, err))
		return
	}
	expected := []struct {
		key   string
		value string
	}{
		{key: "CORPUS_DIGEST", value: identity.CorpusDigest},
		{key: "CORPUS_INDEX_DIGEST", value: identity.IndexDigest},
		{key: "CORPUS_RETRIEVED_AT", value: identity.RetrievedAt},
	}
	for _, item := range expected {
		pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(item.key+` = "`+item.value+`"`) + `$`)
		if item.value == "" || len(pattern.FindAll(config, -1)) != 1 {
			report.Drift = append(report.Drift, fmt.Sprintf("%s: %s does not match the reviewed corpus lock", configPath, item.key))
		}
	}

	workflow, err := os.ReadFile(workflowPath)
	if err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: %v", workflowPath, err))
		return
	}
	for _, key := range []string{"PUBLIC_ORIGIN", "SIDECAR_URL"} {
		pattern := regexp.MustCompile(`--var(?:=|\s+)["']?` + key + `:`)
		if pattern.Match(workflow) {
			report.Drift = append(report.Drift, fmt.Sprintf("%s: deploy overrides config-owned %s", workflowPath, key))
		}
	}
}

func checkNShipLaunch(report *syncReport, path string, manifest releasecontract.Manifest) {
	body, err := os.ReadFile(path)
	if err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: %v", path, err))
		return
	}
	if bytes.Contains(body, []byte("target_channel: source-repository")) {
		repositoryURL := "https://" + strings.TrimSuffix(manifest.Module, "/")
		if !bytes.Contains(body, []byte(repositoryURL)) {
			report.Drift = append(report.Drift, fmt.Sprintf("%s: missing %q", path, repositoryURL))
		}
		return
	}
	checkContainsVersion(report, path, manifest.Module+"@"+manifest.Version)
	checkContainsVersion(report, path, "--expect, "+manifest.Version)
}

func checkVersionFile(report *syncReport, path, expected string) {
	body, err := os.ReadFile(path)
	if err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: %v", path, err))
		return
	}
	if string(body) != expected+"\n" {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: version %q != %q", path, strings.TrimSpace(string(body)), expected))
	}
}

func checkContainsVersion(report *syncReport, path, expected string) {
	body, err := os.ReadFile(path)
	if err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: %v", path, err))
		return
	}
	if !bytes.Contains(body, []byte(expected)) {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: missing %q", path, expected))
	}
}

func checkContainsAll(report *syncReport, path string, expected []string) {
	body, err := os.ReadFile(path)
	if err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: %v", path, err))
		return
	}
	checkBodyContainsAll(report, path, body, expected)
}

func checkContainsCount(report *syncReport, path, expected string, count int) {
	body, err := os.ReadFile(path)
	if err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: %v", path, err))
		return
	}
	if got := bytes.Count(body, []byte(expected)); got != count {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: %q occurs %d times, want %d", path, expected, got, count))
	}
}

func checkScopedContainsAll(report *syncReport, path, startPattern, nextPattern string, expected []string) {
	body, err := os.ReadFile(path)
	if err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: %v", path, err))
		return
	}
	lines, start, end, err := locateScopedYAMLBlock(body, startPattern, nextPattern)
	if err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: %v", path, err))
		return
	}
	checkBodyContainsAll(report, path, []byte(strings.Join(lines[start:end], "\n")), expected)
}

func checkBodyContainsAll(report *syncReport, path string, body []byte, expected []string) {
	for _, value := range expected {
		if !bytes.Contains(body, []byte(value)) {
			report.Drift = append(report.Drift, fmt.Sprintf("%s: missing %q", path, value))
		}
	}
}

func checkDependencySnapshot(report *syncReport, path string, manifest releasecontract.Manifest) {
	body, err := os.ReadFile(path)
	if err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: %v", path, err))
		return
	}
	var snapshot dependencySnapshot
	if err := decodeJSONStrict(body, &snapshot); err != nil {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: decode: %v", path, err))
		return
	}
	if snapshot.Module != manifest.Module {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: module %q != %q", path, snapshot.Module, manifest.Module))
	}
	if snapshot.Fallback.Version != manifest.Version {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: fallback %q != %q", path, snapshot.Fallback.Version, manifest.Version))
	}
	if snapshot.Fallback.ContractVersion != manifest.ContractVersion {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: contract version %d != %d", path, snapshot.Fallback.ContractVersion, manifest.ContractVersion))
	}
	if !reflect.DeepEqual(snapshot.Commands, manifest.Commands) {
		report.Drift = append(report.Drift, fmt.Sprintf("%s: command contract differs", path))
	}
	available := make(map[string]bool, len(manifest.Capabilities))
	for _, capability := range manifest.Capabilities {
		available[capability] = true
	}
	for _, capability := range snapshot.DevelopmentContract.RequiredCapabilities {
		if !available[capability] {
			report.Drift = append(report.Drift, fmt.Sprintf("%s: required capability %q is absent", path, capability))
		}
	}
}

func writeConsumers(repoRoot, ndevRoot string, manifest releasecontract.Manifest) ([]string, error) {
	ndevAbs, err := filepath.Abs(ndevRoot)
	if err != nil {
		return nil, err
	}
	localLaunch := filepath.Join(repoRoot, manifest.Consumers.NShipLaunch)
	ndevDependency := filepath.Join(ndevAbs, manifest.Consumers.NDevDependency)
	catalogProduct := filepath.Join(ndevAbs, manifest.Consumers.CatalogProduct)
	portfolioPolicy := filepath.Join(ndevAbs, manifest.Consumers.PortfolioPolicy)
	externalProjects := filepath.Join(ndevAbs, manifest.Consumers.ExternalProjects)
	paths := []string{localLaunch, ndevDependency, catalogProduct, portfolioPolicy, externalProjects}
	if err := requireCleanPaths(repoRoot, []string{localLaunch}); err != nil {
		return nil, err
	}
	if err := requireCleanPaths(ndevAbs, paths[1:]); err != nil {
		return nil, err
	}

	written := make([]string, 0, len(paths))
	if changed, err := rewriteFile(localLaunch, func(body []byte) ([]byte, error) {
		return rewriteNShipLaunch(body, manifest), nil
	}); err != nil {
		return nil, err
	} else if changed {
		written = append(written, localLaunch)
	}

	if changed, err := rewriteDependencySnapshot(ndevDependency, manifest); err != nil {
		return nil, err
	} else if changed {
		written = append(written, ndevDependency)
	}

	if changed, err := rewriteFile(catalogProduct, func(body []byte) ([]byte, error) {
		return rewriteCatalogProduct(body, manifest), nil
	}); err != nil {
		return nil, err
	} else if changed {
		written = append(written, catalogProduct)
	}

	if changed, err := rewriteScopedYAMLBlock(portfolioPolicy, `^    product\.docs-puller:\s*$`, `^    [A-Za-z0-9_.-]+:\s*$`, manifest); err != nil {
		return nil, err
	} else if changed {
		written = append(written, portfolioPolicy)
	}
	if changed, err := rewriteScopedYAMLBlock(externalProjects, `^    - id: product\.docs-puller\s*$`, `^    - id: `, manifest); err != nil {
		return nil, err
	} else if changed {
		written = append(written, externalProjects)
	}
	slices.Sort(written)
	return written, nil
}

func rewriteCatalogProduct(body []byte, manifest releasecontract.Manifest) []byte {
	body = replaceAll(body, `(/releases/tag/)v[0-9]+\.[0-9]+\.[0-9]+`, `${1}`+manifest.Version)
	body = replaceAll(body, `(current public release )v[0-9]+\.[0-9]+\.[0-9]+`, `${1}`+manifest.Version)
	body = replaceAll(body, `(public release )v[0-9]+\.[0-9]+\.[0-9]+`, `${1}`+manifest.Version)
	body = replaceAll(body, `(Apache-2\.0, )v[0-9]+\.[0-9]+\.[0-9]+`, `${1}`+manifest.Version)
	body = replaceAll(body, `(OSS )v[0-9]+\.[0-9]+\.[0-9]+`, `${1}`+manifest.Version)
	body = replaceAll(body, `(through )v[0-9]+\.[0-9]+\.[0-9]+`, `${1}`+manifest.Version)
	body = replaceAll(body, `([Pp]ublic and attested )v[0-9]+\.[0-9]+\.[0-9]+`, `${1}`+manifest.Version)
	return replaceAll(body, `(?m)^(\s*verified_at:\s*")[0-9]{4}-[0-9]{2}-[0-9]{2}("\s*)$`, `${1}`+manifest.ReleaseDate+`${2}`)
}

func rewriteNShipLaunch(body []byte, manifest releasecontract.Manifest) []byte {
	versionedModule := regexp.MustCompile(regexp.QuoteMeta(manifest.Module) + `@v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?`)
	body = versionedModule.ReplaceAll(body, []byte(manifest.Module+"@"+manifest.Version))
	return replaceAll(body, `(--expect,\s*)v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?`, `${1}`+manifest.Version)
}

func rewriteDependencySnapshot(path string, manifest releasecontract.Manifest) (bool, error) {
	return rewriteFile(path, func(body []byte) ([]byte, error) {
		var snapshot dependencySnapshot
		if err := decodeJSONStrict(body, &snapshot); err != nil {
			return nil, err
		}
		snapshot.Module = manifest.Module
		snapshot.Fallback.Version = manifest.Version
		snapshot.Fallback.ContractVersion = manifest.ContractVersion
		snapshot.Commands = append([]string(nil), manifest.Commands...)
		out, err := json.MarshalIndent(snapshot, "", "  ")
		return append(out, '\n'), err
	})
}

func rewriteScopedYAMLBlock(path, startPattern, nextPattern string, manifest releasecontract.Manifest) (bool, error) {
	return rewriteFile(path, func(body []byte) ([]byte, error) {
		lines, start, end, err := locateScopedYAMLBlock(body, startPattern, nextPattern)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		block := strings.Join(lines[start:end], "\n")
		block = string(replaceAll([]byte(block), `(/releases/tag/)v[0-9]+\.[0-9]+\.[0-9]+`, `${1}`+manifest.Version))
		block = string(replaceAll([]byte(block), `(latest release )v[0-9]+\.[0-9]+\.[0-9]+`, `${1}`+manifest.Version))
		block = string(replaceAll([]byte(block), `(Release )v[0-9]+\.[0-9]+\.[0-9]+`, `${1}`+manifest.Version))
		block = string(replaceAll([]byte(block), `([Pp]ublic and attested )v[0-9]+\.[0-9]+\.[0-9]+`, `${1}`+manifest.Version))
		block = string(replaceAll([]byte(block), `(as of )[0-9]{4}-[0-9]{2}-[0-9]{2}`, `${1}`+manifest.ReleaseDate))
		block = string(replaceAll([]byte(block), `(?m)^(\s*verified_at:\s*")[0-9]{4}-[0-9]{2}-[0-9]{2}("\s*)$`, `${1}`+manifest.ReleaseDate+`${2}`))
		lines[start] = block
		lines = append(lines[:start+1], lines[end:]...)
		return []byte(strings.Join(lines, "\n")), nil
	})
}

func locateScopedYAMLBlock(body []byte, startPattern, nextPattern string) ([]string, int, int, error) {
	lines := strings.Split(string(body), "\n")
	startRE := regexp.MustCompile(startPattern)
	nextRE := regexp.MustCompile(nextPattern)
	start, end := -1, len(lines)
	for i, line := range lines {
		if start < 0 && startRE.MatchString(line) {
			start = i
			continue
		}
		if start >= 0 && nextRE.MatchString(line) {
			end = i
			break
		}
	}
	if start < 0 {
		return nil, 0, 0, errors.New("docs-puller block not found")
	}
	return lines, start, end, nil
}

func replaceAll(body []byte, pattern, replacement string) []byte {
	return regexp.MustCompile(pattern).ReplaceAll(body, []byte(replacement))
}

func rewriteFile(path string, transform func([]byte) ([]byte, error)) (bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	out, err := transform(body)
	if err != nil {
		return false, err
	}
	if bytes.Equal(body, out) {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(path, out, info.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}

func requireCleanPaths(root string, paths []string) error {
	rel := make([]string, 0, len(paths))
	for _, path := range paths {
		value, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = append(rel, value)
	}
	args := append([]string{"status", "--porcelain", "--"}, rel...)
	out, err := gitOutput(root, args...)
	if err != nil {
		return err
	}
	if out != "" {
		return fmt.Errorf("refusing to overwrite dirty consumer paths:\n%s", out)
	}
	return nil
}
