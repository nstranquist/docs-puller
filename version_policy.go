package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "embed"

	"github.com/nstranquist/docs-puller/internal/userconfig"
	"github.com/nstranquist/docs-puller/searchruntime"
	"gopkg.in/yaml.v3"
)

//go:embed version_policy.yaml
var embeddedVersionPolicy []byte

const (
	docsPinsFile         = "_DOCS_PINS.json"
	versionPolicyVersion = 1

	versionLaneLatest          = "latest"
	versionLaneWorkspacePinned = "workspace-pinned"
	versionLaneToolsPinned     = "tools-pinned"
	versionLaneOtherPinned     = "other-pinned"
)

var errAtomicDirSwapUnsupported = errors.New("atomic directory swap unsupported")

type versionPolicyRegistry struct {
	SchemaVersion int                   `yaml:"schema_version" json:"schema_version"`
	Sources       []versionPolicySource `yaml:"sources" json:"sources"`

	byID      map[string]versionPolicySource
	byPackage map[string]versionPolicySource
}

type versionPolicySource struct {
	ID                  string              `yaml:"id" json:"id"`
	PackageNames        []string            `yaml:"package_names" json:"package_names"`
	DocsVersionStrategy string              `yaml:"docs_version_strategy" json:"docs_version_strategy"`
	LatestVersion       string              `yaml:"latest_version" json:"latest_version"`
	LatestURL           string              `yaml:"latest_url" json:"latest_url"`
	VersionURLTemplate  string              `yaml:"version_url_template" json:"version_url_template"`
	VersionedPages      []versionPolicyPage `yaml:"versioned_pages" json:"versioned_pages,omitempty"`
	PinPolicy           string              `yaml:"pin_policy" json:"pin_policy"`
}

type versionPolicyPage struct {
	Path        string `yaml:"path" json:"path"`
	URLTemplate string `yaml:"url_template" json:"url_template"`
}

type docsPinsFileData struct {
	SchemaVersion int              `json:"schema_version"`
	GeneratedAt   string           `json:"generated_at"`
	Out           string           `json:"out"`
	Roots         []pinScanRoot    `json:"roots"`
	Pins          []docsPin        `json:"pins"`
	Skipped       []docsPinSkipped `json:"skipped,omitempty"`
	Orphans       []docsPin        `json:"orphans,omitempty"`
}

type pinScanRoot struct {
	Path  string `json:"path"`
	Scope string `json:"scope"`
	Kind  string `json:"kind"`
}

type docsPin struct {
	SourceFamily        string        `json:"source_family"`
	SourceID            string        `json:"source_id"`
	PackageName         string        `json:"package_name"`
	ResolvedVersion     string        `json:"resolved_version"`
	DeclaredVersion     string        `json:"declared_version,omitempty"`
	DocsVersion         string        `json:"docs_version"`
	VersionKey          string        `json:"version_key"`
	VersionLane         string        `json:"version_lane"`
	PinScope            string        `json:"pin_scope"`
	PinPolicy           string        `json:"pin_policy"`
	DocsVersionStrategy string        `json:"docs_version_strategy"`
	PullURL             string        `json:"pull_url,omitempty"`
	LatestEquivalent    bool          `json:"latest_equivalent,omitempty"`
	Evidence            []pinEvidence `json:"evidence"`
	OrphanedAt          string        `json:"orphaned_at,omitempty"`
}

type pinnedCrawlPage struct {
	Path string `json:"path"`
	URL  string `json:"url"`
}

type docsPinSkipped struct {
	SourceFamily    string        `json:"source_family"`
	PackageName     string        `json:"package_name"`
	ResolvedVersion string        `json:"resolved_version"`
	DocsVersion     string        `json:"docs_version"`
	VersionKey      string        `json:"version_key"`
	Reason          string        `json:"reason"`
	LatestVersion   string        `json:"latest_version,omitempty"`
	Evidence        []pinEvidence `json:"evidence"`
}

type pinEvidence struct {
	Root            string `json:"root"`
	Scope           string `json:"scope"`
	Kind            string `json:"kind"`
	File            string `json:"file"`
	PackageName     string `json:"package_name"`
	DeclaredVersion string `json:"declared_version,omitempty"`
	ResolvedVersion string `json:"resolved_version"`
	projectDir      string
	lockfile        bool
}

type versionPolicySourceInfo struct {
	searchruntime.VersionPolicySourceInfo
	Pin *docsPin
}

type versionSearchPolicy struct {
	reg              *versionPolicyRegistry
	pins             *docsPinsFileData
	cwd              string
	sourceFamily     string
	sourceID         string
	version          string
	preferLatest     bool
	latestOnly       bool
	cwdScope         string
	cwdPinnedSources map[string]bool
}

type pinGenerationOptions struct {
	Out            string
	Roots          []string
	Now            time.Time
	KeepOldOrphans bool
	MarkOrphans    bool
}

func cmdPins(args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, pinsUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "show", "list":
		cmdPinsShow(args[1:])
	case "refresh":
		cmdPinsRefresh(args[1:], false)
	case "sync":
		cmdPinsRefresh(args[1:], true)
	case "gc":
		cmdPinsGC(args[1:])
	case "lint":
		cmdPinsLint(args[1:])
	case "-h", "--help", "help":
		fmt.Print(pinsUsage)
	default:
		fmt.Fprintf(os.Stderr, "pins: unknown subcommand %q\n%s", args[0], pinsUsage)
		os.Exit(2)
	}
}

const pinsUsage = `docs-puller pins — manage bounded versioned-doc pins

Usage:
  docs-puller pins show                    [--out DIR] [--json]
  docs-puller pins refresh                 [--out DIR] [--root DIR ...] [--write] [--json]
  docs-puller pins sync                    [--out DIR] [--root DIR ...] [--write] [--json]
  docs-puller pins gc                      [--out DIR] [--grace-days N] [--write] [--json]
  docs-puller pins lint                    [--out DIR] [--json]
  docs-puller pull-pins                    [--out DIR] [--source FAMILY] [--write] [--json]

Pins are generated at <out>/_DOCS_PINS.json from workspace lockfiles.
Latest docs remain canonical; pinned snapshots are bounded source-id overlays
such as react-native__v0.79.
`

func cmdPinsShow(args []string) {
	o := defaultOpts()
	var asJSON bool
	fs := flag.NewFlagSet("pins show", flag.ExitOnError)
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.BoolVar(&asJSON, "json", false, "emit JSON")
	fs.Parse(args)

	pins, err := loadDocsPins(o.out)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			pins = &docsPinsFileData{SchemaVersion: versionPolicyVersion, Out: o.out}
		} else {
			die(err)
		}
	}
	emitPins(pins, asJSON)
}

func cmdPinsRefresh(args []string, syncMode bool) {
	o := defaultOpts()
	var roots stringSliceFlag
	var write, asJSON bool
	fs := flag.NewFlagSet("pins refresh", flag.ExitOnError)
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.Var(&roots, "root", "canonical root to scan (repeatable)")
	fs.BoolVar(&write, "write", false, "write <out>/_DOCS_PINS.json")
	fs.BoolVar(&asJSON, "json", false, "emit JSON")
	fs.Parse(args)

	generated, err := generateDocsPins(pinGenerationOptions{
		Out:            o.out,
		Roots:          roots,
		Now:            time.Now().UTC(),
		KeepOldOrphans: true,
		MarkOrphans:    syncMode,
	})
	if err != nil {
		die(err)
	}
	if write {
		if err := writeDocsPins(o.out, generated); err != nil {
			die(err)
		}
	}
	emitPins(generated, asJSON)
}

func cmdPinsGC(args []string) {
	o := defaultOpts()
	var graceDays int
	var write, asJSON bool
	fs := flag.NewFlagSet("pins gc", flag.ExitOnError)
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.IntVar(&graceDays, "grace-days", 14, "only collect orphans older than this many days")
	fs.BoolVar(&write, "write", false, "remove eligible orphan source directories and rewrite pins file")
	fs.BoolVar(&asJSON, "json", false, "emit JSON")
	fs.Parse(args)
	if graceDays < 0 {
		fmt.Fprintln(os.Stderr, "pins gc: --grace-days must be >= 0")
		os.Exit(2)
	}
	pins, err := loadDocsPins(o.out)
	if err != nil {
		die(err)
	}
	type gcItem struct {
		SourceID   string `json:"source_id"`
		Path       string `json:"path"`
		Eligible   bool   `json:"eligible"`
		Removed    bool   `json:"removed,omitempty"`
		Reason     string `json:"reason,omitempty"`
		OrphanedAt string `json:"orphaned_at,omitempty"`
	}
	cutoff := time.Now().UTC().Add(-time.Duration(graceDays) * 24 * time.Hour)
	items := []gcItem{}
	kept := pins.Orphans[:0]
	for _, p := range pins.Orphans {
		sourcePath, pathErr := pinnedSourceDir(o.out, p.SourceID)
		item := gcItem{
			SourceID:   p.SourceID,
			Path:       sourcePath,
			OrphanedAt: p.OrphanedAt,
		}
		if pathErr != nil {
			item.Reason = pathErr.Error()
			kept = append(kept, p)
			items = append(items, item)
			continue
		}
		orphanedAt, err := time.Parse(time.RFC3339, p.OrphanedAt)
		if p.OrphanedAt == "" || err != nil {
			item.Reason = "missing-or-invalid-orphaned_at"
			kept = append(kept, p)
			items = append(items, item)
			continue
		}
		if orphanedAt.After(cutoff) {
			item.Reason = "inside-grace-window"
			kept = append(kept, p)
			items = append(items, item)
			continue
		}
		item.Eligible = true
		if write {
			if err := os.RemoveAll(item.Path); err != nil {
				item.Reason = err.Error()
				kept = append(kept, p)
			} else {
				item.Removed = true
			}
		} else {
			kept = append(kept, p)
		}
		items = append(items, item)
	}
	if write {
		pins.Orphans = kept
		pins.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
		if err := writeDocsPins(o.out, pins); err != nil {
			die(err)
		}
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"out": o.out, "write": write, "items": items}); err != nil {
			die(err)
		}
		return
	}
	for _, item := range items {
		state := "keep"
		if item.Eligible {
			state = "eligible"
		}
		if item.Removed {
			state = "removed"
		}
		if item.Reason != "" {
			fmt.Printf("%-8s %s (%s)\n", state, item.SourceID, item.Reason)
		} else {
			fmt.Printf("%-8s %s\n", state, item.SourceID)
		}
	}
}

func cmdPinsLint(args []string) {
	o := defaultOpts()
	var asJSON bool
	fs := flag.NewFlagSet("pins lint", flag.ExitOnError)
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.BoolVar(&asJSON, "json", false, "emit JSON")
	fs.Parse(args)

	reg, err := loadVersionPolicyRegistry()
	if err != nil {
		die(err)
	}
	var warnings []string
	for _, s := range reg.Sources {
		if s.ID == "" {
			warnings = append(warnings, "source missing id")
		}
		if len(s.PackageNames) == 0 {
			warnings = append(warnings, fmt.Sprintf("%s: package_names is empty", s.ID))
		}
		if s.DocsVersionStrategy == "" {
			warnings = append(warnings, fmt.Sprintf("%s: docs_version_strategy is required", s.ID))
		}
		if s.VersionURLTemplate == "" {
			warnings = append(warnings, fmt.Sprintf("%s: version_url_template is required", s.ID))
		}
		if s.LatestVersion == "" {
			warnings = append(warnings, fmt.Sprintf("%s: latest_version is required for skip-when-latest", s.ID))
		}
		for i, page := range s.VersionedPages {
			if page.Path == "" {
				warnings = append(warnings, fmt.Sprintf("%s: versioned_pages[%d].path is required", s.ID, i))
			}
			if page.URLTemplate == "" {
				warnings = append(warnings, fmt.Sprintf("%s: versioned_pages[%d].url_template is required", s.ID, i))
			}
			if _, err := cleanPinnedPagePath(renderVersionURL(page.Path, "1.2.3")); err != nil {
				warnings = append(warnings, fmt.Sprintf("%s: versioned_pages[%d].path is invalid: %v", s.ID, i, err))
			}
		}
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"ok": len(warnings) == 0, "warnings": warnings, "sources": len(reg.Sources)})
	} else if len(warnings) == 0 {
		fmt.Printf("version policy registry ok (%d sources)\n", len(reg.Sources))
	} else {
		for _, w := range warnings {
			fmt.Println("WARN", w)
		}
	}
	if len(warnings) > 0 {
		os.Exit(1)
	}
}

func cmdPullPins(args []string) {
	o := defaultOpts()
	var source string
	var write, asJSON bool
	fs := flag.NewFlagSet("pull-pins", flag.ExitOnError)
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.StringVar(&source, "source", "", "only seed pins for this source family")
	fs.BoolVar(&write, "write", false, "fetch and write pinned source entrypoints")
	fs.BoolVar(&asJSON, "json", false, "emit JSON")
	fs.Parse(args)

	pins, err := loadDocsPins(o.out)
	if err != nil {
		die(err)
	}
	type pullPlan struct {
		SourceFamily string            `json:"source_family"`
		SourceID     string            `json:"source_id"`
		VersionKey   string            `json:"version_key"`
		URL          string            `json:"url,omitempty"`
		Pages        []pinnedCrawlPage `json:"pages,omitempty"`
		Wrote        bool              `json:"wrote,omitempty"`
		Error        string            `json:"error,omitempty"`
	}
	plans := []pullPlan{}
	type selectedPullPin struct {
		pin       docsPin
		planIndex int
	}
	selectedPins := []selectedPullPin{}
	for _, pin := range pins.Pins {
		if source != "" && pin.SourceFamily != source {
			continue
		}
		pages, pageErr := pinnedCrawlPages(pin)
		plan := pullPlan{
			SourceFamily: pin.SourceFamily,
			SourceID:     pin.SourceID,
			VersionKey:   pin.VersionKey,
			URL:          pin.PullURL,
			Pages:        pages,
		}
		if pageErr != nil {
			plan.Error = pageErr.Error()
			plans = append(plans, plan)
			continue
		}
		plans = append(plans, plan)
		selectedPins = append(selectedPins, selectedPullPin{pin: pin, planIndex: len(plans) - 1})
	}
	if write {
		if err := withWriteLock(o.out, func() error {
			var changedSources []string
			for _, selected := range selectedPins {
				results, err := seedPinnedSource(o.out, selected.pin)
				if err != nil {
					plans[selected.planIndex].Error = err.Error()
					continue
				}
				plans[selected.planIndex].Wrote = true
				changedSources = append(changedSources, distinctSources(results)...)
			}
			if err := regenerateIndex(o.out, uniqueStrings(changedSources)); err != nil {
				return err
			}
			idx, err := openFTSIndex(o.out)
			if err != nil {
				return err
			}
			defer idx.close()
			return idx.replaceSources(o.out, uniqueStrings(changedSources))
		}); err != nil {
			die(err)
		}
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"out": o.out, "write": write, "plans": plans})
		return
	}
	for _, p := range plans {
		if p.Error != "" {
			fmt.Printf("ERR   %-24s %s — %s\n", p.SourceID, p.URL, p.Error)
		} else if p.Wrote {
			fmt.Printf("wrote %-24s %d pages\n", p.SourceID, len(p.Pages))
		} else {
			fmt.Printf("plan  %-24s %d pages\n", p.SourceID, len(p.Pages))
		}
	}
}

func seedPinnedSource(out string, pin docsPin) ([]result, error) {
	return seedPinnedSourceWithFetcher(out, pin, fetchPinnedPage)
}

type pinnedPageFetcher func(string) ([]byte, error)

func seedPinnedSourceWithFetcher(out string, pin docsPin, fetch pinnedPageFetcher) ([]result, error) {
	pages, err := pinnedCrawlPages(pin)
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("%s has no pinned crawl pages", pin.SourceID)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	sourceDir, err := pinnedSourceDir(out, pin.SourceID)
	if err != nil {
		return nil, err
	}
	type fetchedPage struct {
		page pinnedCrawlPage
		rel  string
		data []byte
		sum  string
	}
	fetched := make([]fetchedPage, 0, len(pages))
	for _, page := range pages {
		data, err := fetch(page.URL)
		if err != nil {
			return nil, searchruntime.VersionPolicyPinnedPageFetchError(pin.SourceID, page.URL, err)
		}
		rel, err := cleanPinnedPagePath(page.Path)
		if err != nil {
			return nil, searchruntime.VersionPolicyPinnedPagePathError(pin.SourceID, page.Path, err)
		}
		sum := sha256.Sum256(data)
		fetched = append(fetched, fetchedPage{
			page: page,
			rel:  rel,
			data: data,
			sum:  hex.EncodeToString(sum[:]),
		})
	}
	if err := os.MkdirAll(filepath.Clean(out), 0o755); err != nil {
		return nil, err
	}
	stageDir, err := os.MkdirTemp(filepath.Clean(out), "."+pin.SourceID+".stage-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stageDir)

	m := newManifest()
	results := make([]result, 0, len(fetched))
	for _, page := range fetched {
		outPath := filepath.Join(stageDir, page.rel)
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(outPath, page.data, 0o644); err != nil {
			return nil, err
		}
		r := result{
			URL:       page.page.URL,
			Source:    pin.SourceID,
			Path:      filepath.Join(pin.SourceID, page.rel),
			Mode:      "pin-crawl",
			SHA256:    page.sum,
			FetchedAt: now,
		}
		m.Entries[page.page.URL] = r
		results = append(results, r)
	}
	if err := writeManifestAtomic(stageDir, m); err != nil {
		return nil, err
	}
	if err := replacePinnedSourceDir(sourceDir, stageDir); err != nil {
		return nil, err
	}
	return results, nil
}

func replacePinnedSourceDir(sourceDir, stageDir string) error {
	backupDir := pinnedReplacementBackupDir(sourceDir)
	if err := recoverPinnedSourceReplacement(sourceDir, backupDir); err != nil {
		return err
	}

	sourceExists, err := dirExists(sourceDir)
	if err != nil {
		return err
	}
	if sourceExists {
		if err := swapPinnedSourceDirs(stageDir, sourceDir); err == nil {
			return os.RemoveAll(stageDir)
		}
		if err := os.Rename(sourceDir, backupDir); err != nil {
			return err
		}
	}
	if err := os.Rename(stageDir, sourceDir); err != nil {
		if sourceExists {
			if restoreErr := os.Rename(backupDir, sourceDir); restoreErr != nil {
				return fmt.Errorf("%w; failed to restore previous pinned source: %v", err, restoreErr)
			}
		}
		return err
	}
	if sourceExists {
		if err := os.RemoveAll(backupDir); err != nil {
			return err
		}
	}
	return nil
}

func recoverPinnedSourceReplacement(sourceDir, backupDir string) error {
	sourceExists, err := dirExists(sourceDir)
	if err != nil {
		return err
	}
	backupExists, err := dirExists(backupDir)
	if err != nil {
		return err
	}
	switch {
	case backupExists && !sourceExists:
		return os.Rename(backupDir, sourceDir)
	case backupExists && sourceExists:
		return os.RemoveAll(backupDir)
	default:
		return nil
	}
}

func pinnedReplacementBackupDir(sourceDir string) string {
	return filepath.Join(filepath.Dir(sourceDir), "."+filepath.Base(sourceDir)+".replace-backup")
}

func dirExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("%s exists but is not a directory", path)
		}
		return info.IsDir(), nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func fetchPinnedPage(rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if isNativeMarkdownURL(u) {
		return httpGet(rawURL)
	}
	return fetchAndConvert(rawURL)
}

func pinnedCrawlPages(pin docsPin) ([]pinnedCrawlPage, error) {
	reg, err := loadVersionPolicyRegistry()
	if err != nil {
		return nil, err
	}
	src, ok := reg.byID[pin.SourceFamily]
	if !ok {
		return nil, fmt.Errorf("%s: source family not in version policy registry", pin.SourceFamily)
	}
	if len(src.VersionedPages) == 0 {
		if pin.PullURL == "" {
			return nil, fmt.Errorf("%s has no pull_url", pin.SourceID)
		}
		return []pinnedCrawlPage{{Path: "index.md", URL: pin.PullURL}}, nil
	}
	pages := make([]pinnedCrawlPage, 0, len(src.VersionedPages))
	seenPaths := map[string]bool{}
	for _, spec := range src.VersionedPages {
		rel, err := cleanPinnedPagePath(renderVersionURL(spec.Path, pin.VersionKey))
		if err != nil {
			return nil, err
		}
		if seenPaths[rel] {
			continue
		}
		seenPaths[rel] = true
		pages = append(pages, pinnedCrawlPage{
			Path: rel,
			URL:  renderVersionURL(spec.URLTemplate, pin.VersionKey),
		})
	}
	return pages, nil
}

func cleanPinnedPagePath(path string) (string, error) {
	path = filepath.ToSlash(strings.TrimSpace(path))
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", fmt.Errorf("empty pinned page path")
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || clean == ".." {
		return "", fmt.Errorf("pinned page path escapes source root: %q", path)
	}
	if !strings.HasSuffix(clean, ".md") {
		return "", fmt.Errorf("pinned page path must end in .md: %q", path)
	}
	return clean, nil
}

func pinnedSourceDir(out, sourceID string) (string, error) {
	if sourceID == "" || !strings.Contains(sourceID, "__v") {
		return "", fmt.Errorf("invalid pinned source id %q", sourceID)
	}
	if strings.ContainsAny(sourceID, `/\`) || sourceID == "." || sourceID == ".." || strings.Contains(sourceID, "..") {
		return "", fmt.Errorf("invalid pinned source id %q", sourceID)
	}
	out = filepath.Clean(out)
	path := filepath.Join(out, sourceID)
	rel, err := filepath.Rel(out, path)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("pinned source path escapes docs root: %q", sourceID)
	}
	return path, nil
}

func emitPins(pins *docsPinsFileData, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(pins); err != nil {
			die(err)
		}
		return
	}
	fmt.Printf("%d active pins, %d skipped, %d orphans in %s\n",
		len(pins.Pins), len(pins.Skipped), len(pins.Orphans), filepath.Join(pins.Out, docsPinsFile))
	for _, p := range pins.Pins {
		fmt.Printf("  %-24s %-18s %-16s %s\n", p.SourceID, p.VersionLane, p.PinScope, p.PullURL)
	}
	for _, s := range pins.Skipped {
		fmt.Printf("  SKIP %-16s %-10s %s\n", s.SourceFamily, s.VersionKey, s.Reason)
	}
	for _, o := range pins.Orphans {
		fmt.Printf("  ORPHAN %-22s orphaned_at=%s\n", o.SourceID, o.OrphanedAt)
	}
}

func generateDocsPins(o pinGenerationOptions) (*docsPinsFileData, error) {
	reg, err := loadVersionPolicyRegistry()
	if err != nil {
		return nil, err
	}
	if o.Now.IsZero() {
		o.Now = time.Now().UTC()
	}
	roots := o.Roots
	if len(roots) == 0 {
		roots = defaultPinRoots()
	}
	scanRoots := make([]pinScanRoot, 0, len(roots))
	evidence := []pinEvidence{}
	for _, root := range roots {
		if root == "" {
			continue
		}
		info := classifyPinRoot(root)
		scanRoots = append(scanRoots, info)
		ev, err := scanPinRoot(info, reg)
		if err != nil {
			return nil, err
		}
		evidence = append(evidence, ev...)
	}
	active, skipped := pinsFromEvidence(reg, evidence)
	out := &docsPinsFileData{
		SchemaVersion: versionPolicyVersion,
		GeneratedAt:   o.Now.Format(time.RFC3339),
		Out:           o.Out,
		Roots:         scanRoots,
		Pins:          active,
		Skipped:       skipped,
		Orphans:       []docsPin{},
	}
	prev, err := loadDocsPins(o.Out)
	if err == nil && prev != nil {
		if o.KeepOldOrphans {
			out.Orphans = append(out.Orphans, prev.Orphans...)
		}
		if o.MarkOrphans {
			activeKeys := map[string]bool{}
			for _, p := range active {
				activeKeys[p.SourceID+"|"+p.PinScope] = true
			}
			orphanKeys := map[string]bool{}
			for _, p := range out.Orphans {
				orphanKeys[p.SourceID+"|"+p.PinScope] = true
			}
			for _, p := range prev.Pins {
				key := p.SourceID + "|" + p.PinScope
				if activeKeys[key] || orphanKeys[key] {
					continue
				}
				p.OrphanedAt = o.Now.Format(time.RFC3339)
				out.Orphans = append(out.Orphans, p)
			}
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	sortDocsPins(out.Pins)
	sortSkippedPins(out.Skipped)
	sortDocsPins(out.Orphans)
	return out, nil
}

func loadVersionPolicyRegistry() (*versionPolicyRegistry, error) {
	var reg versionPolicyRegistry
	if err := yaml.Unmarshal(embeddedVersionPolicy, &reg); err != nil {
		return nil, searchruntime.VersionPolicyEmbeddedParseError(err)
	}
	if reg.SchemaVersion == 0 {
		reg.SchemaVersion = versionPolicyVersion
	}
	reg.byID = map[string]versionPolicySource{}
	reg.byPackage = map[string]versionPolicySource{}
	for _, src := range reg.Sources {
		if src.ID == "" {
			return nil, fmt.Errorf("version policy source missing id")
		}
		reg.byID[src.ID] = src
		for _, pkg := range src.PackageNames {
			reg.byPackage[pkg] = src
		}
	}
	return &reg, nil
}

func defaultPinRoots() []string {
	roots, err := userconfig.PinScanRoots()
	if err != nil {
		return nil
	}
	return roots
}

func classifyPinRoot(root string) pinScanRoot {
	root = filepath.Clean(root)
	base := filepath.Base(root)
	if kind, scope, ok := userconfig.ClassifyToolsMonorepo(root); ok {
		return pinScanRoot{Path: root, Scope: scope, Kind: kind}
	}
	return pinScanRoot{Path: root, Scope: base, Kind: "workspace"}
}

func scanPinRoot(root pinScanRoot, reg *versionPolicyRegistry) ([]pinEvidence, error) {
	var evidence []pinEvidence
	err := filepath.WalkDir(root.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if shouldSkipPinScanDir(d.Name()) && path != root.Path {
				return filepath.SkipDir
			}
			return nil
		}
		switch d.Name() {
		case "package.json":
			ev, err := scanPackageJSON(path, root, reg)
			if err != nil {
				return err
			}
			evidence = append(evidence, ev...)
		case "package-lock.json":
			ev, err := scanPackageLock(path, root, reg)
			if err != nil {
				return err
			}
			evidence = append(evidence, ev...)
		case "pnpm-lock.yaml":
			ev, err := scanPnpmLock(path, root, reg)
			if err != nil {
				return err
			}
			evidence = append(evidence, ev...)
		case "go.mod":
			ev, err := scanGoMod(path, root, reg)
			if err != nil {
				return err
			}
			evidence = append(evidence, ev...)
		}
		return nil
	})
	return evidence, err
}

func shouldSkipPinScanDir(name string) bool {
	switch name {
	case ".git", ".claude", "node_modules", "_worktrees", "feat", "dist", "build", ".next", "archive", "archives":
		return true
	default:
		return false
	}
}

func scanPackageJSON(path string, root pinScanRoot, reg *versionPolicyRegistry) ([]pinEvidence, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, searchruntime.VersionPolicyFileParseError(path, err)
	}
	var out []pinEvidence
	add := func(deps map[string]string) {
		for name, version := range deps {
			if _, ok := reg.byPackage[name]; !ok {
				continue
			}
			if !packageJSONVersionIsPinned(version) {
				continue
			}
			out = append(out, pinEvidence{
				Root:            root.Path,
				Scope:           root.Scope,
				Kind:            root.Kind,
				File:            path,
				PackageName:     name,
				DeclaredVersion: version,
				ResolvedVersion: version,
				projectDir:      filepath.Dir(path),
			})
		}
	}
	add(pkg.Dependencies)
	add(pkg.DevDependencies)
	return out, nil
}

func packageJSONVersionIsPinned(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" {
		return false
	}
	if strings.HasPrefix(version, "workspace:") ||
		strings.HasPrefix(version, "file:") ||
		strings.HasPrefix(version, "link:") ||
		strings.HasPrefix(version, "portal:") ||
		strings.HasPrefix(version, "catalog:") {
		return false
	}
	if strings.ContainsAny(version, "^~<>=*xX|") {
		return false
	}
	return cleanResolvedVersion(version) != ""
}

func scanPackageLock(path string, root pinScanRoot, reg *versionPolicyRegistry) ([]pinEvidence, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, searchruntime.VersionPolicyFileParseError(path, err)
	}
	var out []pinEvidence
	for pkg := range reg.byPackage {
		if entry, ok := lock.Packages["node_modules/"+pkg]; ok && entry.Version != "" {
			out = append(out, pinEvidence{
				Root:            root.Path,
				Scope:           root.Scope,
				Kind:            root.Kind,
				File:            path,
				PackageName:     pkg,
				ResolvedVersion: entry.Version,
				projectDir:      filepath.Dir(path),
				lockfile:        true,
			})
		}
	}
	return out, nil
}

type pnpmDep struct {
	Specifier string `yaml:"specifier"`
	Version   string `yaml:"version"`
}

func (p *pnpmDep) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		p.Version = value.Value
		return nil
	case yaml.MappingNode:
		type alias pnpmDep
		var a alias
		if err := value.Decode(&a); err != nil {
			return err
		}
		*p = pnpmDep(a)
		return nil
	default:
		return nil
	}
}

func scanPnpmLock(path string, root pinScanRoot, reg *versionPolicyRegistry) ([]pinEvidence, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lock struct {
		Importers map[string]struct {
			Dependencies         map[string]pnpmDep `yaml:"dependencies"`
			DevDependencies      map[string]pnpmDep `yaml:"devDependencies"`
			OptionalDependencies map[string]pnpmDep `yaml:"optionalDependencies"`
		} `yaml:"importers"`
	}
	if err := yaml.Unmarshal(data, &lock); err != nil {
		return nil, searchruntime.VersionPolicyFileParseError(path, err)
	}
	var out []pinEvidence
	for importer, deps := range lock.Importers {
		importerDir := filepath.Clean(filepath.Join(filepath.Dir(path), importer))
		add := func(m map[string]pnpmDep) {
			for name, dep := range m {
				if _, ok := reg.byPackage[name]; !ok {
					continue
				}
				version := dep.Version
				if version == "" {
					version = dep.Specifier
				}
				out = append(out, pinEvidence{
					Root:            root.Path,
					Scope:           root.Scope,
					Kind:            root.Kind,
					File:            path + "#" + importer,
					PackageName:     name,
					DeclaredVersion: dep.Specifier,
					ResolvedVersion: version,
					projectDir:      importerDir,
					lockfile:        true,
				})
			}
		}
		add(deps.Dependencies)
		add(deps.DevDependencies)
		add(deps.OptionalDependencies)
	}
	return out, nil
}

var goRequireRE = regexp.MustCompile(`^\s*([A-Za-z0-9_.\-/]+)\s+(v[0-9][^\s]*)`)

func scanGoMod(path string, root pinScanRoot, reg *versionPolicyRegistry) ([]pinEvidence, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []pinEvidence
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		m := goRequireRE.FindStringSubmatch(line)
		if len(m) != 3 {
			continue
		}
		name := m[1]
		if _, ok := reg.byPackage[name]; !ok {
			continue
		}
		out = append(out, pinEvidence{
			Root:            root.Path,
			Scope:           root.Scope,
			Kind:            root.Kind,
			File:            path,
			PackageName:     name,
			ResolvedVersion: strings.TrimPrefix(m[2], "v"),
			projectDir:      filepath.Dir(path),
			lockfile:        true,
		})
	}
	return out, nil
}

func pinsFromEvidence(reg *versionPolicyRegistry, evidence []pinEvidence) ([]docsPin, []docsPinSkipped) {
	evidence = preferLockfilePinEvidence(evidence)
	byKey := map[string]*docsPin{}
	skippedByKey := map[string]*docsPinSkipped{}
	for _, ev := range evidence {
		src, ok := reg.byPackage[ev.PackageName]
		if !ok {
			continue
		}
		resolved := cleanResolvedVersion(ev.ResolvedVersion)
		if resolved == "" {
			resolved = cleanResolvedVersion(ev.DeclaredVersion)
		}
		versionKey := docsVersionKey(resolved, src.DocsVersionStrategy)
		if versionKey == "" {
			continue
		}
		if versionKey == src.LatestVersion {
			key := src.ID + "|" + ev.PackageName + "|" + versionKey
			s := skippedByKey[key]
			if s == nil {
				s = &docsPinSkipped{
					SourceFamily:    src.ID,
					PackageName:     ev.PackageName,
					ResolvedVersion: resolved,
					DocsVersion:     versionKey,
					VersionKey:      versionKey,
					Reason:          "pin-equals-latest",
					LatestVersion:   src.LatestVersion,
				}
				skippedByKey[key] = s
			}
			s.Evidence = append(s.Evidence, ev)
			continue
		}
		lane := versionLaneWorkspacePinned
		if ev.Kind == "tools" {
			lane = versionLaneToolsPinned
		}
		sourceID := pinnedSourceID(src.ID, versionKey)
		key := sourceID + "|" + lane + "|" + ev.Scope
		p := byKey[key]
		if p == nil {
			p = &docsPin{
				SourceFamily:        src.ID,
				SourceID:            sourceID,
				PackageName:         ev.PackageName,
				ResolvedVersion:     resolved,
				DeclaredVersion:     ev.DeclaredVersion,
				DocsVersion:         versionKey,
				VersionKey:          versionKey,
				VersionLane:         lane,
				PinScope:            ev.Scope,
				PinPolicy:           src.PinPolicy,
				DocsVersionStrategy: src.DocsVersionStrategy,
				PullURL:             renderVersionURL(src.VersionURLTemplate, versionKey),
			}
			byKey[key] = p
		}
		p.Evidence = append(p.Evidence, ev)
	}
	active := make([]docsPin, 0, len(byKey))
	for _, p := range byKey {
		active = append(active, *p)
	}
	skipped := make([]docsPinSkipped, 0, len(skippedByKey))
	for _, s := range skippedByKey {
		skipped = append(skipped, *s)
	}
	return active, skipped
}

func preferLockfilePinEvidence(evidence []pinEvidence) []pinEvidence {
	hasLockfile := map[string]bool{}
	for _, ev := range evidence {
		if ev.lockfile {
			hasLockfile[pinEvidenceProjectKey(ev)] = true
		}
	}
	if len(hasLockfile) == 0 {
		return evidence
	}
	out := evidence[:0]
	for _, ev := range evidence {
		if !ev.lockfile && hasLockfile[pinEvidenceProjectKey(ev)] {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func pinEvidenceProjectKey(ev pinEvidence) string {
	projectDir := ev.projectDir
	if projectDir == "" {
		file := ev.File
		if i := strings.Index(file, "#"); i >= 0 {
			file = file[:i]
		}
		projectDir = filepath.Dir(file)
	}
	return ev.Root + "|" + ev.Scope + "|" + projectDir + "|" + ev.PackageName
}

func cleanResolvedVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "npm:")
	v = strings.TrimLeft(v, "^~<>= ")
	if i := strings.IndexAny(v, " ("); i >= 0 {
		v = v[:i]
	}
	v = strings.TrimPrefix(v, "v")
	return v
}

func docsVersionKey(version, strategy string) string {
	version = cleanResolvedVersion(version)
	parts := strings.Split(version, ".")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	switch strategy {
	case "semver_major":
		return parts[0]
	case "semver_minor":
		if len(parts) < 2 {
			return parts[0]
		}
		return parts[0] + "." + parts[1]
	case "semver_minor_hyphen":
		if len(parts) < 2 {
			return parts[0]
		}
		return parts[0] + "-" + parts[1]
	case "semver_major_minor_zero":
		if len(parts) < 2 {
			return parts[0] + ".0.0"
		}
		return parts[0] + "." + parts[1] + ".0"
	case "semver_patch", "":
		for len(parts) < 3 {
			parts = append(parts, "0")
		}
		return parts[0] + "." + parts[1] + "." + parts[2]
	case "latest_only":
		return ""
	default:
		return version
	}
}

func pinnedSourceID(family, versionKey string) string {
	safe := strings.NewReplacer("/", "-", "@", "", " ", "-").Replace(versionKey)
	return family + "__v" + safe
}

func renderVersionURL(tmpl, versionKey string) string {
	return strings.ReplaceAll(tmpl, "{{version_key}}", versionKey)
}

func sortDocsPins(pins []docsPin) {
	sort.Slice(pins, func(i, j int) bool {
		a := pins[i].SourceID + "|" + pins[i].PinScope + "|" + pins[i].VersionLane
		b := pins[j].SourceID + "|" + pins[j].PinScope + "|" + pins[j].VersionLane
		return a < b
	})
}

func sortSkippedPins(pins []docsPinSkipped) {
	sort.Slice(pins, func(i, j int) bool {
		a := pins[i].SourceFamily + "|" + pins[i].VersionKey
		b := pins[j].SourceFamily + "|" + pins[j].VersionKey
		return a < b
	})
}

func loadDocsPins(out string) (*docsPinsFileData, error) {
	path := filepath.Join(out, docsPinsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pins docsPinsFileData
	if err := json.Unmarshal(data, &pins); err != nil {
		return nil, searchruntime.VersionPolicyFileParseError(path, err)
	}
	if pins.SchemaVersion == 0 {
		pins.SchemaVersion = versionPolicyVersion
	}
	return &pins, nil
}

func writeDocsPins(out string, pins *docsPinsFileData) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	path := filepath.Join(out, docsPinsFile)
	tmp, err := os.CreateTemp(out, ".DOCS_PINS.*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(pins); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func sourceInfoForSource(source string, pins *docsPinsFileData, reg *versionPolicyRegistry) versionPolicySourceInfo {
	var pin *docsPin
	info := searchruntime.ResolveVersionPolicySourceInfo(searchruntime.VersionPolicySourceInfoResolveInput{
		Source:          source,
		LatestLane:      versionLaneLatest,
		OtherPinnedLane: versionLaneOtherPinned,
		PinnedSourceInfo: func(source string) (searchruntime.VersionPolicySourceInfo, bool) {
			if pins == nil {
				return searchruntime.VersionPolicySourceInfo{}, false
			}
			for i := range pins.Pins {
				p := &pins.Pins[i]
				if p.SourceID == source {
					pin = p
					return searchruntime.VersionPolicySourceInfo{
						SourceFamily: p.SourceFamily,
						SourceID:     p.SourceID,
						DocsVersion:  p.DocsVersion,
						VersionLane:  p.VersionLane,
						PinScope:     p.PinScope,
					}, true
				}
			}
			return searchruntime.VersionPolicySourceInfo{}, false
		},
		ParsePinnedSourceID: func(source string) (string, string, bool) {
			return parsePinnedSourceID(source, reg)
		},
		LatestVersion: func(source string) (string, bool) {
			if reg == nil {
				return "", false
			}
			src, ok := reg.byID[source]
			return src.LatestVersion, ok
		},
	})
	return versionPolicySourceInfo{VersionPolicySourceInfo: info, Pin: pin}
}

func parsePinnedSourceID(source string, reg *versionPolicyRegistry) (family, version string, ok bool) {
	if reg != nil {
		return searchruntime.ParseVersionPolicyPinnedSourceID(source, func(family string) bool {
			_, exists := reg.byID[family]
			return exists
		})
	}
	return searchruntime.ParseVersionPolicyPinnedSourceID(source, nil)
}

func sourceFamilyMatches(source, family string, reg *versionPolicyRegistry) bool {
	if family == "" {
		return true
	}
	info := sourceInfoForSource(source, nil, reg)
	return info.SourceFamily == family
}

func resolveSearchVersionPolicy(query string, opts *searchOpts) {
	reg, err := loadVersionPolicyRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "version-policy: %v\n", err)
		return
	}
	pins, err := loadDocsPins(opts.out)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "version-policy: %v\n", err)
	}
	cwd, _ := os.Getwd()
	p := &versionSearchPolicy{
		reg:              reg,
		pins:             pins,
		cwd:              cwd,
		version:          strings.TrimSpace(opts.version),
		preferLatest:     queryPrefersLatest(query),
		cwdPinnedSources: map[string]bool{},
	}
	p.cwdScope = pinScopeForCwd(cwd, pins)
	if pins != nil && p.cwdScope != "" {
		for _, pin := range pins.Pins {
			if pin.PinScope == p.cwdScope {
				p.cwdPinnedSources[pin.SourceID] = true
			}
		}
	}
	if opts.source != "" {
		if _, _, ok := parsePinnedSourceID(opts.source, reg); ok {
			p.sourceID = opts.source
		} else if _, ok := reg.byID[opts.source]; ok {
			p.sourceFamily = opts.source
			opts.source = ""
		}
	}
	if p.version != "" {
		if p.version == "latest" {
			p.latestOnly = true
			p.preferLatest = true
			if p.sourceFamily != "" {
				p.sourceID = p.sourceFamily
			}
		} else if p.sourceFamily != "" {
			if src, ok := reg.byID[p.sourceFamily]; ok {
				key := docsVersionKey(p.version, src.DocsVersionStrategy)
				if key == "" {
					key = cleanResolvedVersion(p.version)
				}
				p.version = key
				if !sourceHasPathVersion(opts.out, p.sourceFamily, key) {
					p.sourceID = pinnedSourceID(p.sourceFamily, key)
				}
			}
		} else {
			p.version = cleanResolvedVersion(p.version)
		}
	}
	opts.versionPolicy = p
}

func pinScopeForCwd(cwd string, pins *docsPinsFileData) string {
	if pins == nil || cwd == "" {
		return ""
	}
	cwd = filepath.Clean(cwd)
	bestLen := -1
	best := ""
	for _, root := range pins.Roots {
		rootPath := filepath.Clean(root.Path)
		if cwd == rootPath || strings.HasPrefix(cwd, rootPath+string(os.PathSeparator)) {
			if len(rootPath) > bestLen {
				bestLen = len(rootPath)
				best = root.Scope
			}
		}
	}
	return best
}

func queryPrefersLatest(query string) bool {
	tokens := tokenizeForFTS(query)
	for _, t := range tokens {
		switch t {
		case "latest", "upgrade", "upgrading", "migrate", "migration", "migrations", "changelog", "release", "releases":
			return true
		}
	}
	return false
}

func (p *versionSearchPolicy) NeedsWideCandidatePool() bool {
	return searchruntime.VersionPolicyNeedsWideCandidatePool(p.runtimeState())
}

func (p *versionSearchPolicy) Header() string {
	return searchruntime.VersionPolicyHeader(p.runtimeState())
}

func (p *versionSearchPolicy) runtimeState() searchruntime.VersionPolicyState {
	if p == nil {
		return searchruntime.VersionPolicyState{}
	}
	return searchruntime.VersionPolicyState{
		SourceFamily: p.sourceFamily,
		SourceID:     p.sourceID,
		Version:      p.version,
		LatestOnly:   p.latestOnly,
		CwdScope:     p.cwdScope,
		PreferLatest: p.preferLatest,
	}
}

func applySearchVersionPolicy(query string, hits []searchHit, o searchOpts) []searchHit {
	p := o.versionPolicy
	if p == nil {
		return hits
	}
	state := p.runtimeState()
	return searchruntime.ApplyVersionPolicy(hits, func(h searchHit) (searchHit, bool) {
		info := sourceInfoForHit(h, p.pins, p.reg)
		return searchruntime.ApplyAndMatchVersionPolicySourceInfo(h, info.VersionPolicySourceInfo, state)
	}, func(h searchHit) int {
		return searchruntime.VersionPolicyScoreFromState(h, state, p.cwdPinnedSources)
	})
}

// filterByVersionPolicy removes hits that don't match the active version
// policy without changing the order of surviving hits. Use this AFTER
// rerank so the rerank's careful ordering is preserved (the full
// applySearchVersionPolicy re-sorts by Score, which undoes rerank since
// rerank doesn't update Score). The hybrid first-stage retrieval pulls
// embedding-cosine candidates that bypass BM25's source-scoping, so a
// post-rerank filter-only pass is necessary to keep `--version <X>`
// queries honest.
//
// IMPORTANT: this is gated to fire ONLY when the version policy has an
// active version constraint (sourceID, latestOnly, or version set). For
// "soft" policies (sourceFamily-only, preferLatest, etc.) the pre-rerank
// applySearchVersionPolicy already did the matching and scoring; running
// this post-rerank with a soft-policy gate would over-filter family-source
// queries because hit metadata population can race with the rerank's
// candidate-pool munging. Empirically this caused -2.4pp Hit@1 on the 168
// fixture for queries like "--source microsoft-learn azure resource group".
func filterByVersionPolicy(hits []searchHit, o searchOpts) []searchHit {
	p := o.versionPolicy
	if p == nil {
		return hits
	}
	state := p.runtimeState()
	return searchruntime.FilterByVersionPolicy(hits, searchruntime.VersionPolicyHardFilterOptions(state), func(h searchHit) (searchHit, bool) {
		// sourceInfoForHit is cheap (map lookup + path parsing);
		// safe to re-run here even if the pre-rerank pass already populated
		// the SourceFamily/SourceID/DocsVersion fields. The filter relies
		// on those being set.
		info := sourceInfoForHit(h, p.pins, p.reg)
		return searchruntime.ApplyAndMatchVersionPolicySourceInfo(h, info.VersionPolicySourceInfo, state)
	})
}

func searchFTSWithVersionPolicy(idx *ftsIndex, query string, o searchOpts) ([]searchHit, error) {
	p := o.versionPolicy
	return searchruntime.RunVersionPolicyFTSQuery(searchruntime.VersionPolicyFTSQueryInput{
		UseFamilyFanout: p != nil && p.sourceFamily != "" && p.sourceID == "",
		SourceIDs: func() []string {
			if p == nil {
				return nil
			}
			return p.familySourceIDs(o.out)
		}(),
		Direct: func() ([]searchHit, error) {
			return idx.searchWithOptions(query, o.source, o.limit, o.exact, o.profile, o.strict, !o.noSnippets)
		},
		SearchSource: func(sourceID string) ([]searchHit, error) {
			return idx.searchWithOptions(query, sourceID, o.limit, o.exact, o.profile, o.strict, !o.noSnippets)
		},
	})
}

func (p *versionSearchPolicy) familySourceIDs(out string) []string {
	if p == nil || p.sourceFamily == "" {
		return nil
	}
	sources, err := listSources(out)
	if err != nil {
		return nil
	}
	return searchruntime.VersionPolicyFamilySourceIDsFromSources(p.sourceFamily, sources, func(source string) searchruntime.VersionPolicySourceInfo {
		return sourceInfoForSource(source, p.pins, p.reg).VersionPolicySourceInfo
	})
}
