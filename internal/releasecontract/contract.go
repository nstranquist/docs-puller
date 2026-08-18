// Package releasecontract owns the versioned docs-puller release contract.
package releasecontract

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	semverPattern    = regexp.MustCompile(`^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	goVersionPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$`)
	targetPattern    = regexp.MustCompile(`^[a-z0-9]+$`)
)

type Target struct {
	GOOS    string `json:"goos"`
	GOARCH  string `json:"goarch"`
	Archive string `json:"archive"`
}

type Consumers struct {
	NShipLaunch      string `json:"nship_launch"`
	NDevDependency   string `json:"ndev_dependency"`
	CatalogProduct   string `json:"catalog_product"`
	PortfolioPolicy  string `json:"portfolio_policy"`
	ExternalProjects string `json:"external_projects"`
}

type Manifest struct {
	SchemaVersion       int       `json:"schema_version"`
	ProductID           string    `json:"product_id"`
	Name                string    `json:"name"`
	Module              string    `json:"module"`
	Binary              string    `json:"binary"`
	Version             string    `json:"version"`
	Tag                 string    `json:"tag"`
	ReleaseDate         string    `json:"release_date"`
	GoVersion           string    `json:"go_version"`
	ContractVersion     int       `json:"contract_version"`
	ArchiveTemplate     string    `json:"archive_template"`
	ChecksumsName       string    `json:"checksums_name"`
	SBOMTemplate        string    `json:"sbom_template"`
	ProvenanceTemplate  string    `json:"provenance_template"`
	ReleaseManifestName string    `json:"release_manifest_name"`
	Commands            []string  `json:"commands"`
	Capabilities        []string  `json:"capabilities"`
	Targets             []Target  `json:"targets"`
	Consumers           Consumers `json:"consumers"`
}

func Parse(data []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode release manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return Manifest{}, fmt.Errorf("decode release manifest: trailing data: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("release manifest schema_version = %d, want 1", m.SchemaVersion)
	}
	for name, value := range map[string]string{
		"product_id": m.ProductID, "name": m.Name, "module": m.Module,
		"binary": m.Binary, "version": m.Version, "tag": m.Tag,
		"release_date": m.ReleaseDate,
		"go_version":   m.GoVersion, "archive_template": m.ArchiveTemplate,
		"checksums_name": m.ChecksumsName, "sbom_template": m.SBOMTemplate,
		"provenance_template":   m.ProvenanceTemplate,
		"release_manifest_name": m.ReleaseManifestName,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("release manifest %s is empty", name)
		}
	}
	if !semverPattern.MatchString(m.Version) {
		return fmt.Errorf("release manifest version %q is not SemVer with a v prefix", m.Version)
	}
	if m.Tag != m.Version {
		return fmt.Errorf("release manifest tag %q does not match version %q", m.Tag, m.Version)
	}
	if _, err := time.Parse("2006-01-02", m.ReleaseDate); err != nil {
		return fmt.Errorf("release manifest release_date %q is not YYYY-MM-DD: %w", m.ReleaseDate, err)
	}
	if !goVersionPattern.MatchString(m.GoVersion) {
		return fmt.Errorf("release manifest go_version %q must be major.minor", m.GoVersion)
	}
	if m.ContractVersion < 1 {
		return fmt.Errorf("release manifest contract_version must be positive")
	}
	if err := validateSortedUnique("commands", m.Commands); err != nil {
		return err
	}
	if err := validateSortedUnique("capabilities", m.Capabilities); err != nil {
		return err
	}
	if len(m.Targets) == 0 {
		return fmt.Errorf("release manifest targets are empty")
	}
	seenTargets := map[string]bool{}
	seenAssets := map[string]bool{}
	for _, target := range m.Targets {
		key := target.GOOS + "/" + target.GOARCH
		if !targetPattern.MatchString(target.GOOS) || !targetPattern.MatchString(target.GOARCH) {
			return fmt.Errorf("release target %q must use lowercase letters and digits", key)
		}
		if target.Archive != "tar.gz" && target.Archive != "zip" {
			return fmt.Errorf("release target %s has unsupported archive %q", key, target.Archive)
		}
		if seenTargets[key] {
			return fmt.Errorf("release target %s is duplicated", key)
		}
		seenTargets[key] = true
		asset := m.ArchiveName(target)
		if err := validateAssetName(asset); err != nil {
			return fmt.Errorf("release target %s: %w", key, err)
		}
		if seenAssets[asset] {
			return fmt.Errorf("release asset %q is duplicated", asset)
		}
		seenAssets[asset] = true
	}
	for name, value := range map[string]string{
		"checksums_name":        m.ChecksumsName,
		"sbom_template":         m.SBOMName(),
		"provenance_template":   m.ProvenanceName(),
		"release_manifest_name": m.ReleaseManifestName,
	} {
		if err := validateAssetName(value); err != nil {
			return fmt.Errorf("release manifest %s: %w", name, err)
		}
		if seenAssets[value] {
			return fmt.Errorf("release asset %q is duplicated", value)
		}
		seenAssets[value] = true
	}
	for name, value := range map[string]string{
		"nship_launch":      m.Consumers.NShipLaunch,
		"ndev_dependency":   m.Consumers.NDevDependency,
		"catalog_product":   m.Consumers.CatalogProduct,
		"portfolio_policy":  m.Consumers.PortfolioPolicy,
		"external_projects": m.Consumers.ExternalProjects,
	} {
		if err := validateRelativePath(value); err != nil {
			return fmt.Errorf("release manifest consumers.%s: %w", name, err)
		}
	}
	return nil
}

func validateAssetName(value string) error {
	if value == "" || path.Base(value) != value || value == "." || value == ".." || strings.Contains(value, `\`) || strings.ContainsAny(value, "{}") {
		return fmt.Errorf("asset name %q is not a safe file name", value)
	}
	return nil
}

func validateRelativePath(value string) error {
	if value == "" || path.IsAbs(value) || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") || strings.Contains(value, `\`) {
		return fmt.Errorf("path %q is not a safe relative path", value)
	}
	return nil
}

func validateSortedUnique(name string, values []string) error {
	if len(values) == 0 {
		return fmt.Errorf("release manifest %s are empty", name)
	}
	for i, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("release manifest %s contain an empty value", name)
		}
		if i > 0 && values[i-1] >= value {
			return fmt.Errorf("release manifest %s must be sorted and unique near %q", name, value)
		}
	}
	return nil
}

func (m Manifest) SemVer() string {
	return strings.TrimPrefix(m.Version, "v")
}

func (m Manifest) UserAgent() string {
	return m.Name + "/" + m.SemVer() + " (+https://github.com/nstranquist/docs-puller)"
}

func (m Manifest) ArchiveName(target Target) string {
	replacer := strings.NewReplacer(
		"{semver}", m.SemVer(),
		"{goos}", target.GOOS,
		"{goarch}", target.GOARCH,
		"{ext}", target.Archive,
	)
	return replacer.Replace(m.ArchiveTemplate)
}

func (m Manifest) SBOMName() string {
	return strings.ReplaceAll(m.SBOMTemplate, "{semver}", m.SemVer())
}

func (m Manifest) ProvenanceName() string {
	return strings.ReplaceAll(m.ProvenanceTemplate, "{semver}", m.SemVer())
}

func SortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
