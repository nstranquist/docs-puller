package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nstranquist/docs-puller/searchruntime"
)

func TestDocsVersionKeyStrategies(t *testing.T) {
	tests := []struct {
		version  string
		strategy string
		want     string
	}{
		{"0.79.6", "semver_minor", "0.79"},
		{"~53.0.19", "semver_major_minor_zero", "53.0.0"},
		{"^5.7.2", "semver_patch", "5.7.2"},
		{"v15.1.11", "semver_major", "15"},
		{"~5.8.3", "semver_minor_hyphen", "5-8"},
	}
	for _, tt := range tests {
		if got := docsVersionKey(tt.version, tt.strategy); got != tt.want {
			t.Fatalf("docsVersionKey(%q, %q) = %q, want %q", tt.version, tt.strategy, got, tt.want)
		}
	}
}

func TestSourceInfoForSourceEmbedsRuntimeMetadata(t *testing.T) {
	pins := &docsPinsFileData{
		Pins: []docsPin{{
			SourceFamily: "react",
			SourceID:     "react__v18",
			DocsVersion:  "18",
			VersionLane:  versionLaneToolsPinned,
			PinScope:     "tools-monorepo",
		}},
	}
	info := sourceInfoForSource("react__v18", pins, nil)
	want := searchruntime.VersionPolicySourceInfo{
		SourceFamily: "react",
		SourceID:     "react__v18",
		DocsVersion:  "18",
		VersionLane:  versionLaneToolsPinned,
		PinScope:     "tools-monorepo",
	}
	if info.VersionPolicySourceInfo != want {
		t.Fatalf("runtime source info = %+v, want %+v", info.VersionPolicySourceInfo, want)
	}
	if info.Pin == nil || info.Pin.SourceID != "react__v18" {
		t.Fatalf("command pin metadata = %#v, want react__v18 pin", info.Pin)
	}

	reg := &versionPolicyRegistry{byID: map[string]versionPolicySource{
		"react": {LatestVersion: "19"},
	}}
	latest := sourceInfoForSource("react", nil, reg)
	if latest.VersionPolicySourceInfo != (searchruntime.VersionPolicySourceInfo{SourceFamily: "react", SourceID: "react", DocsVersion: "19", VersionLane: versionLaneLatest}) {
		t.Fatalf("latest runtime source info = %+v", latest.VersionPolicySourceInfo)
	}

	otherPinned := sourceInfoForSource("react__v17", nil, reg)
	if otherPinned.VersionPolicySourceInfo != (searchruntime.VersionPolicySourceInfo{SourceFamily: "react", SourceID: "react__v17", DocsVersion: "17", VersionLane: versionLaneOtherPinned}) {
		t.Fatalf("other-pinned runtime source info = %+v", otherPinned.VersionPolicySourceInfo)
	}
	if otherPinned.Pin != nil {
		t.Fatalf("other-pinned source carried command pin metadata: %#v", otherPinned.Pin)
	}
}

func TestGenerateDocsPinsSkipsLatestAndExcludesNonCanonicalDirs(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "docs")
	workspace := filepath.Join(tmp, "dev", "nicos-app")
	tools := filepath.Join(tmp, "code", "tools-monorepo")
	mustWriteFile(t, filepath.Join(workspace, "package.json"), `{
  "dependencies": {
    "react-native": "0.79.6",
    "expo": "~53.0.0"
  }
}`)
	mustWriteFile(t, filepath.Join(workspace, "package-lock.json"), `{
  "packages": {
    "node_modules/react-native": {"version": "0.79.6"},
    "node_modules/expo": {"version": "53.0.27"}
  }
}`)
	mustWriteFile(t, filepath.Join(workspace, "feat", "scratch", "package.json"), `{
  "dependencies": {
    "react-native": "0.78.1"
  }
}`)
	mustWriteFile(t, filepath.Join(tools, "apps", "mobile", "nicos-remote-rn", "package.json"), `{
  "dependencies": {
    "react-native": "0.83.6",
    "expo": "~55.0.19"
  }
}`)
	mustWriteFile(t, filepath.Join(tools, "apps", "mobile", "nicos-remote-rn", "package-lock.json"), `{
  "packages": {
    "node_modules/react-native": {"version": "0.83.6"},
    "node_modules/expo": {"version": "55.0.19"}
  }
}`)
	mustWriteFile(t, filepath.Join(tools, ".claude", "worktrees", "agent", "package.json"), `{
  "dependencies": {
    "react": "17.0.2"
  }
}`)

	pins, err := generateDocsPins(pinGenerationOptions{
		Out:   out,
		Roots: []string{workspace, tools},
		Now:   time.Date(2026, 5, 4, 2, 25, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pins.Pins) != 2 {
		t.Fatalf("active pins = %d, want 2: %#v", len(pins.Pins), pins.Pins)
	}
	byID := map[string]docsPin{}
	for _, p := range pins.Pins {
		byID[p.SourceID] = p
	}
	if p, ok := byID["react-native__v0.79"]; !ok || p.VersionLane != versionLaneWorkspacePinned || p.PinScope != "nicos-app" {
		t.Fatalf("react-native workspace pin = %#v", p)
	}
	if p, ok := byID["expo__v53.0.0"]; !ok || p.VersionLane != versionLaneWorkspacePinned || p.PinScope != "nicos-app" {
		t.Fatalf("expo workspace pin = %#v", p)
	}
	for _, p := range pins.Pins {
		if p.SourceID == "react-native__v0.78" {
			t.Fatalf("feat/ scratch pin should have been excluded: %#v", p)
		}
		if p.SourceID == "react__v17" {
			t.Fatalf(".claude worktree pin should have been excluded: %#v", p)
		}
	}
	if len(pins.Skipped) != 2 {
		t.Fatalf("skipped latest-equivalent pins = %d, want 2: %#v", len(pins.Skipped), pins.Skipped)
	}
}

func TestGenerateDocsPinsForAuditedHighBreakageSources(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "docs")
	platform := filepath.Join(tmp, "dev", "platform")
	nicosApp := filepath.Join(tmp, "dev", "nicos-app")
	tools := filepath.Join(tmp, "code", "tools-monorepo")
	mustWriteFile(t, filepath.Join(platform, "package.json"), `{
  "devDependencies": {
    "typescript": "^5.4.0"
  }
}`)
	mustWriteFile(t, filepath.Join(platform, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  .:
    devDependencies:
      typescript:
        specifier: ^5.4.0
        version: 5.7.2
  apps/e2e:
    devDependencies:
      '@playwright/test':
        specifier: ^1.49.1
        version: 1.49.1
`)
	mustWriteFile(t, filepath.Join(nicosApp, "package-lock.json"), `{
  "packages": {
    "node_modules/react": {"version": "19.0.0"},
    "node_modules/typescript": {"version": "5.8.3"}
  }
}`)
	mustWriteFile(t, filepath.Join(tools, "services", "kanban", "web", "package-lock.json"), `{
  "packages": {
    "node_modules/react": {"version": "18.3.1"},
    "node_modules/react-dom": {"version": "18.3.1"}
  }
}`)
	mustWriteFile(t, filepath.Join(tools, "apps", "browser-extensions", "companion", "package-lock.json"), `{
  "packages": {
    "node_modules/playwright": {"version": "1.59.1"},
    "node_modules/typescript": {"version": "5.9.3"}
  }
}`)
	mustWriteFile(t, filepath.Join(tools, "packages", "range-only", "package.json"), `{
  "devDependencies": {
    "typescript": "^5.4.0"
  }
}`)

	pins, err := generateDocsPins(pinGenerationOptions{
		Out:   out,
		Roots: []string{platform, nicosApp, tools},
		Now:   time.Date(2026, 5, 4, 7, 55, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]docsPin{}
	for _, p := range pins.Pins {
		byID[p.SourceID] = p
	}
	for _, want := range []string{
		"playwright__v1.49.1",
		"react__v18",
		"typescript__v5-7",
		"typescript__v5-8",
		"typescript__v5-9",
	} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("missing active pin %s; got %#v", want, pins.Pins)
		}
	}
	if _, ok := byID["typescript__v5-4"]; ok {
		t.Fatalf("package.json range should not beat pnpm lockfile evidence: %#v", pins.Pins)
	}
	if p := byID["playwright__v1.49.1"]; p.VersionLane != versionLaneWorkspacePinned || p.PinScope != "platform" {
		t.Fatalf("playwright pin = %#v", p)
	}
	if p := byID["react__v18"]; p.VersionLane != versionLaneWorkspacePinned || p.PinScope != "tools-monorepo" {
		t.Fatalf("react tools pin = %#v", p)
	}
}

func TestPinnedCrawlPagesFromRegistry(t *testing.T) {
	tests := []struct {
		pin      docsPin
		wantPath string
		wantURL  string
	}{
		{
			pin:      docsPin{SourceFamily: "react", SourceID: "react__v18", VersionKey: "18", PullURL: "https://18.react.dev/reference/react"},
			wantPath: "reference/react/useState.md",
			wantURL:  "https://18.react.dev/reference/react/useState",
		},
		{
			pin:      docsPin{SourceFamily: "playwright", SourceID: "playwright__v1.49.1", VersionKey: "1.49.1", PullURL: "https://raw.githubusercontent.com/microsoft/playwright/v1.49.1/docs/src/intro-js.md"},
			wantPath: "docs/locators.md",
			wantURL:  "https://raw.githubusercontent.com/microsoft/playwright/v1.49.1/docs/src/locators.md",
		},
		{
			pin:      docsPin{SourceFamily: "typescript", SourceID: "typescript__v5-7", VersionKey: "5-7", PullURL: "https://www.typescriptlang.org/docs/handbook/release-notes/typescript-5-7.html"},
			wantPath: "release-notes/typescript-5-7.md",
			wantURL:  "https://www.typescriptlang.org/docs/handbook/release-notes/typescript-5-7.html",
		},
	}
	for _, tt := range tests {
		pages, err := pinnedCrawlPages(tt.pin)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, page := range pages {
			if page.Path == tt.wantPath && page.URL == tt.wantURL {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s pages missing %s -> %s: %#v", tt.pin.SourceID, tt.wantPath, tt.wantURL, pages)
		}
	}
}

func TestSeedPinnedSourceCrawlPagesAreSearchable(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "docs")
	tools := filepath.Join(tmp, "code", "tools-monorepo")
	pins := &docsPinsFileData{
		SchemaVersion: versionPolicyVersion,
		GeneratedAt:   "2026-05-04T09:00:00Z",
		Out:           out,
		Roots: []pinScanRoot{{
			Path: tools, Scope: "tools-monorepo", Kind: "tools",
		}},
		Pins: []docsPin{
			{SourceFamily: "react", SourceID: "react__v18", DocsVersion: "18", VersionKey: "18", VersionLane: versionLaneToolsPinned, PinScope: "tools-monorepo", PullURL: "https://18.react.dev/reference/react"},
			{SourceFamily: "playwright", SourceID: "playwright__v1.49.1", DocsVersion: "1.49.1", VersionKey: "1.49.1", VersionLane: versionLaneWorkspacePinned, PinScope: "platform", PullURL: "https://raw.githubusercontent.com/microsoft/playwright/v1.49.1/docs/src/intro-js.md"},
			{SourceFamily: "typescript", SourceID: "typescript__v5-7", DocsVersion: "5-7", VersionKey: "5-7", VersionLane: versionLaneWorkspacePinned, PinScope: "platform", PullURL: "https://www.typescriptlang.org/docs/handbook/release-notes/typescript-5-7.html"},
		},
	}
	if err := writeDocsPins(out, pins); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(out, "react__v18", "index.md"), "# stale sparse index\n\nstale entrypoint")

	bodies := map[string][]byte{
		"https://18.react.dev/reference/react":                                                          []byte("# React Reference\n\nReact 18 reference overview."),
		"https://18.react.dev/reference/react/useState":                                                 []byte("# useState\n\nQueued updater function state hook lazy initializer."),
		"https://18.react.dev/reference/react/useEffect":                                                []byte("# useEffect\n\nSynchronize effects after rendering."),
		"https://18.react.dev/reference/react-dom/client/createRoot":                                    []byte("# createRoot\n\nRender a React DOM root."),
		"https://raw.githubusercontent.com/microsoft/playwright/v1.49.1/docs/src/intro-js.md":           []byte("# Installation\n\nInstall Playwright test."),
		"https://raw.githubusercontent.com/microsoft/playwright/v1.49.1/docs/src/writing-tests-js.md":   []byte("# Writing tests\n\nWrite Playwright tests."),
		"https://raw.githubusercontent.com/microsoft/playwright/v1.49.1/docs/src/locators.md":           []byte("# Locators\n\ngetByRole locator strictness auto waiting."),
		"https://raw.githubusercontent.com/microsoft/playwright/v1.49.1/docs/src/test-assertions-js.md": []byte("# Assertions\n\nExpect assertions for web testing."),
		"https://raw.githubusercontent.com/microsoft/playwright/v1.49.1/docs/src/test-fixtures-js.md":   []byte("# Fixtures\n\nWorker fixture test isolation."),
		"https://www.typescriptlang.org/docs/handbook/release-notes/typescript-5-7.html":                []byte("# TypeScript 5.7\n\nThe rewrite relative import extensions compiler option is documented here."),
	}
	fetch := func(raw string) ([]byte, error) {
		if body, ok := bodies[raw]; ok {
			return body, nil
		}
		t.Fatalf("unexpected fetch %s", raw)
		return nil, nil
	}
	for _, pin := range pins.Pins {
		if _, err := seedPinnedSourceWithFetcher(out, pin, fetch); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "react__v18", "index.md")); !os.IsNotExist(err) {
		t.Fatalf("stale sparse index.md should be pruned, stat err=%v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(out, ".react__v18.stage-*")); err != nil || len(matches) != 0 {
		t.Fatalf("stage dirs = %v, err=%v; want cleaned up", matches, err)
	}
	if err := regenerateIndex(out, nil); err != nil {
		t.Fatal(err)
	}
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.rebuild(out); err != nil {
		idx.close()
		t.Fatal(err)
	}
	idx.close()

	for _, tt := range []struct {
		source  string
		version string
		query   string
		want    string
	}{
		{"react", "18", "queued updater function state hook", "react__v18/reference/react/useState.md"},
		{"playwright", "1.49.1", "getByRole locator strictness", "playwright__v1.49.1/docs/locators.md"},
		{"typescript", "5-7", "rewrite relative import extensions", "typescript__v5-7/release-notes/typescript-5-7.md"},
	} {
		hits, _, _ := dispatchSearch(tt.query, searchOpts{out: out, source: tt.source, version: tt.version, limit: 5, noSnippets: true}, nil)
		if len(hits) == 0 || hits[0].Path != tt.want {
			t.Fatalf("%s@%s search = %#v, want top path %s", tt.source, tt.version, hits, tt.want)
		}
	}
}

func TestReplacePinnedSourceDirRestoresExistingWhenStageRenameFails(t *testing.T) {
	out := t.TempDir()
	sourceDir := filepath.Join(out, "react__v18")
	mustWriteFile(t, filepath.Join(sourceDir, "keep.md"), "# Keep\n\nprevious mirror")

	err := replacePinnedSourceDir(sourceDir, filepath.Join(out, ".missing-stage"))
	if err == nil {
		t.Fatalf("replacePinnedSourceDir succeeded with missing stage")
	}
	if _, statErr := os.Stat(filepath.Join(sourceDir, "keep.md")); statErr != nil {
		t.Fatalf("previous mirror was not restored: %v", statErr)
	}
	if _, statErr := os.Stat(pinnedReplacementBackupDir(sourceDir)); !os.IsNotExist(statErr) {
		t.Fatalf("backup dir should be gone, stat err=%v", statErr)
	}
}

func TestScanPnpmLockEvidence(t *testing.T) {
	tmp := t.TempDir()
	root := pinScanRoot{Path: tmp, Scope: "platform", Kind: "workspace"}
	lock := filepath.Join(tmp, "pnpm-lock.yaml")
	mustWriteFile(t, lock, `lockfileVersion: '9.0'
importers:
  .:
    dependencies:
      react-native:
        specifier: 0.79.6
        version: 0.79.6
`)
	reg, err := loadVersionPolicyRegistry()
	if err != nil {
		t.Fatal(err)
	}
	ev, err := scanPnpmLock(lock, root, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].PackageName != "react-native" || ev[0].ResolvedVersion != "0.79.6" {
		t.Fatalf("pnpm evidence = %#v", ev)
	}
}

func TestApplySearchVersionPolicyRanksCwdPinAndLatestIntent(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "docs")
	cwd := filepath.Join(tmp, "dev", "nicos-app", "apps", "mobile")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	pins := &docsPinsFileData{
		SchemaVersion: versionPolicyVersion,
		GeneratedAt:   "2026-05-04T00:00:00Z",
		Out:           out,
		Roots: []pinScanRoot{{
			Path: filepath.Join(tmp, "dev", "nicos-app"), Scope: "nicos-app", Kind: "workspace",
		}},
		Pins: []docsPin{{
			SourceFamily: "react-native",
			SourceID:     "react-native__v0.79",
			DocsVersion:  "0.79",
			VersionKey:   "0.79",
			VersionLane:  versionLaneWorkspacePinned,
			PinScope:     "nicos-app",
		}},
	}
	if err := writeDocsPins(out, pins); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)

	hits := []searchHit{
		{Path: "react-native/getting-started.md", Source: "react-native", SourceID: "react-native", Score: 100},
		{Path: "react-native__v0.79/getting-started.md", Source: "react-native__v0.79", SourceID: "react-native__v0.79", Score: 1},
	}
	o := searchOpts{out: out, limit: 10, source: "react-native"}
	resolveSearchVersionPolicy("flatlist performance", &o)
	got := applySearchVersionPolicy("flatlist performance", append([]searchHit(nil), hits...), o)
	if len(got) != 2 || got[0].SourceID != "react-native__v0.79" {
		t.Fatalf("cwd pin ranking = %#v", got)
	}
	if got[0].SourceFamily != "react-native" || got[0].DocsVersion != "0.79" {
		t.Fatalf("pinned metadata = %#v", got[0])
	}

	latest := searchOpts{out: out, limit: 10, source: "react-native"}
	resolveSearchVersionPolicy("upgrade react native latest flatlist", &latest)
	got = applySearchVersionPolicy("upgrade react native latest flatlist", append([]searchHit(nil), hits...), latest)
	if len(got) != 2 || got[0].SourceID != "react-native" {
		t.Fatalf("latest-intent ranking = %#v", got)
	}
}

// TestFilterByVersionPolicyPreservesOrderAndFilters verifies the post-rerank
// filter strips non-matching versions WITHOUT re-sorting (the destructive
// sort in applySearchVersionPolicy was the bug that crashed Hit@1 by 12pp).
// Specifically: hits that survive the version-policy must come back in the
// same order they entered, and non-matching hits must be removed.
func TestFilterByVersionPolicyPreservesOrderAndFilters(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "docs")
	o := searchOpts{out: out, source: "react-native", version: "0.79", limit: 10}
	resolveSearchVersionPolicy("flatlist", &o)

	// Mimic post-rerank order: low-Score canonical at position 0 (rerank
	// chose it), high-Score off-version at position 1 (BM25 had it first).
	// The destructive sort would put the high-Score one first; the filter
	// alone must preserve the post-rerank order.
	hits := []searchHit{
		{Path: "react-native__v0.79/flatlist.md", Source: "react-native__v0.79", SourceID: "react-native__v0.79", Score: 5},
		{Path: "react-native/flatlist.md", Source: "react-native", SourceID: "react-native", Score: 100},
		{Path: "react-native__v0.80/flatlist.md", Source: "react-native__v0.80", SourceID: "react-native__v0.80", Score: 50},
	}
	got := filterByVersionPolicy(hits, o)
	if len(got) != 1 {
		t.Fatalf("expected 1 hit (only react-native__v0.79), got %d: %#v", len(got), got)
	}
	if got[0].SourceID != "react-native__v0.79" {
		t.Errorf("kept wrong source: %s", got[0].SourceID)
	}
	if got[0].Score != 5 {
		t.Errorf("Score should be unchanged (no version-policy boost in filter mode), got %d", got[0].Score)
	}
}

// TestFilterByVersionPolicyNoOpsWithoutPolicy guards the fast path: when no
// version policy is active, filterByVersionPolicy must return hits unchanged.
func TestFilterByVersionPolicyNoOpsWithoutPolicy(t *testing.T) {
	hits := []searchHit{
		{Path: "supabase/rls.md", Score: 100},
		{Path: "postgresql/indexes.md", Score: 50},
	}
	got := filterByVersionPolicy(hits, searchOpts{})
	if len(got) != 2 || got[0].Path != "supabase/rls.md" || got[1].Path != "postgresql/indexes.md" {
		t.Fatalf("nil-policy filter must be no-op: %#v", got)
	}
}

// TestFilterByVersionPolicyGatesSoftPolicy guards the fast path the gate
// added on 2026-05-04 evening: when the policy is "soft" (sourceFamily-only
// or preferLatest, no explicit version constraint), filterByVersionPolicy
// must NOT filter, because the pre-rerank applySearchVersionPolicy already
// did the matching/scoring. Without this gate, family-source queries like
// "--source microsoft-learn" got over-filtered post-rerank, costing -2pp Hit@1.
func TestFilterByVersionPolicyGatesSoftPolicy(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "docs")
	o := searchOpts{out: out, source: "microsoft-learn", limit: 10}
	resolveSearchVersionPolicy("azure resource group", &o)

	// Hits include a docs-from-family hit and an off-family hit. The gate
	// must let BOTH through (the pre-rerank pass owns this filtering, not us).
	hits := []searchHit{
		{Path: "microsoft-learn/cli/azure/group.md", Source: "microsoft-learn", Score: 100},
		{Path: "supabase/guides/database/postgres/row-level-security.md", Source: "supabase", Score: 50},
	}
	got := filterByVersionPolicy(hits, o)
	if len(got) != 2 {
		t.Fatalf("soft-policy gate should not filter; got %d hits, want 2: %#v", len(got), got)
	}
	if got[0].Path != "microsoft-learn/cli/azure/group.md" {
		t.Errorf("hit order must be preserved (rerank wins); got[0]=%s", got[0].Path)
	}
}

func TestApplySearchVersionPolicyVersionOverride(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "docs")
	o := searchOpts{out: out, source: "react-native", version: "0.79", limit: 10}
	resolveSearchVersionPolicy("flatlist", &o)
	hits := []searchHit{
		{Path: "react-native/getting-started.md", Source: "react-native", Score: 100},
		{Path: "react-native__v0.79/getting-started.md", Source: "react-native__v0.79", Score: 1},
	}
	got := applySearchVersionPolicy("flatlist", hits, o)
	if len(got) != 1 || got[0].SourceID != "react-native__v0.79" {
		t.Fatalf("--version filter = %#v", got)
	}
}

func TestSourceInfoForHitReactNativePathVersions(t *testing.T) {
	reg := mustLoadVersionPolicyRegistry(t)
	cases := []struct {
		name        string
		hit         searchHit
		sourceID    string
		docsVersion string
		lane        string
	}{
		{
			name:        "canonical latest",
			hit:         searchHit{Path: "react-native/docs/debugging.md", Source: "react-native"},
			sourceID:    "react-native",
			docsVersion: "0.83",
			lane:        versionLaneLatest,
		},
		{
			name:        "archived minor",
			hit:         searchHit{Path: "react-native/docs/0.79/debugging.md", Source: "react-native"},
			sourceID:    "react-native__v0.79",
			docsVersion: "0.79",
			lane:        versionLaneArchived,
		},
		{
			name:        "next",
			hit:         searchHit{Path: "react-native/docs/next/debugging.md", Source: "react-native"},
			sourceID:    "react-native__vnext",
			docsVersion: "next",
			lane:        versionLaneNext,
		},
		{
			name:        "canonical legacy topic",
			hit:         searchHit{Path: "react-native/docs/legacy/native-modules-android.md", Source: "react-native"},
			sourceID:    "react-native",
			docsVersion: "0.83",
			lane:        versionLaneLatest,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := sourceInfoForHit(tt.hit, nil, reg).VersionPolicySourceInfo
			if got.SourceFamily != "react-native" || got.SourceID != tt.sourceID || got.DocsVersion != tt.docsVersion || got.VersionLane != tt.lane {
				t.Fatalf("sourceInfoForHit() = %+v, want family=react-native source=%s version=%s lane=%s", got, tt.sourceID, tt.docsVersion, tt.lane)
			}
		})
	}
}

func TestApplySearchVersionPolicyReactNativePathVersionOverride(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "docs")
	if err := os.MkdirAll(filepath.Join(out, "react-native", "docs", "0.79"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := searchOpts{out: out, source: "react-native", version: "0.79", limit: 10}
	resolveSearchVersionPolicy("react native debugging", &o)
	if o.versionPolicy == nil {
		t.Fatal("version policy not resolved")
	}
	if o.versionPolicy.sourceID != "" {
		t.Fatalf("path-versioned React Native search should fan out by family, sourceID=%q", o.versionPolicy.sourceID)
	}

	hits := []searchHit{
		{Path: "react-native/docs/debugging.md", Source: "react-native", Score: 100},
		{Path: "react-native/docs/0.79/debugging.md", Source: "react-native", Score: 90},
		{Path: "react-native/docs/0.80/debugging.md", Source: "react-native", Score: 80},
	}
	got := applySearchVersionPolicy("react native debugging", hits, o)
	if len(got) != 1 {
		t.Fatalf("--version 0.79 path filter = %#v, want one 0.79 hit", got)
	}
	if got[0].Path != "react-native/docs/0.79/debugging.md" || got[0].SourceID != "react-native__v0.79" || got[0].DocsVersion != "0.79" {
		t.Fatalf("--version 0.79 kept wrong hit: %#v", got[0])
	}
}

func TestApplySearchVersionPolicyLatestFiltersReactNativeArchivedPaths(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "docs")
	o := searchOpts{out: out, source: "react-native", version: "latest", limit: 10}
	resolveSearchVersionPolicy("react native debugging", &o)

	hits := []searchHit{
		{Path: "react-native/docs/0.83/debugging.md", Source: "react-native", Score: 100},
		{Path: "react-native/docs/debugging.md", Source: "react-native", Score: 90},
		{Path: "react-native/docs/next/debugging.md", Source: "react-native", Score: 80},
	}
	got := applySearchVersionPolicy("react native debugging", hits, o)
	if len(got) != 1 {
		t.Fatalf("--version latest path filter = %#v, want one canonical latest hit", got)
	}
	if got[0].Path != "react-native/docs/debugging.md" || got[0].SourceID != "react-native" || got[0].VersionLane != versionLaneLatest {
		t.Fatalf("--version latest kept wrong hit: %#v", got[0])
	}
}

func TestApplySearchVersionPolicyLatestIntentPrefersLatestReact(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "docs")
	pins := &docsPinsFileData{
		Pins: []docsPin{{
			SourceFamily: "react",
			SourceID:     "react__v18",
			DocsVersion:  "18",
			VersionLane:  versionLaneToolsPinned,
			PinScope:     "tools-monorepo",
		}},
	}
	policy := &versionSearchPolicy{
		pins:         pins,
		reg:          mustLoadVersionPolicyRegistry(t),
		sourceFamily: "react",
		preferLatest: true,
	}
	hits := []searchHit{
		{Path: "react__v18/reference/react/useState.md", Source: "react__v18", SourceID: "react__v18", Score: 1000},
		{Path: "react/reference/react/useState.md", Source: "react", SourceID: "react", Score: 700},
	}
	got := applySearchVersionPolicy("react migration latest useState", hits, searchOpts{out: out, source: "react", limit: 10, versionPolicy: policy})
	if len(got) != 2 || got[0].SourceID != "react" {
		t.Fatalf("latest-intent react ranking = %#v", got)
	}
}

func TestPinnedSourceDirRejectsEscapes(t *testing.T) {
	tmp := t.TempDir()
	if got, err := pinnedSourceDir(tmp, "react-native__v0.79"); err != nil || got != filepath.Join(tmp, "react-native__v0.79") {
		t.Fatalf("safe pinned source dir = %q, %v", got, err)
	}
	for _, sourceID := range []string{
		"../outside__v1",
		"react-native/../../outside__v1",
		"react-native__v..",
		"react-native",
	} {
		if got, err := pinnedSourceDir(tmp, sourceID); err == nil {
			t.Fatalf("pinnedSourceDir(%q) = %q, want error", sourceID, got)
		}
	}
}

func BenchmarkApplySearchVersionPolicy(b *testing.B) {
	reg, err := loadVersionPolicyRegistry()
	if err != nil {
		b.Fatal(err)
	}
	pins := &docsPinsFileData{
		Pins: []docsPin{{
			SourceFamily: "react-native",
			SourceID:     "react-native__v0.79",
			DocsVersion:  "0.79",
			VersionLane:  versionLaneWorkspacePinned,
			PinScope:     "nicos-app",
		}},
	}
	policy := &versionSearchPolicy{
		reg:              reg,
		pins:             pins,
		sourceFamily:     "react-native",
		cwdScope:         "nicos-app",
		cwdPinnedSources: map[string]bool{"react-native__v0.79": true},
	}
	base := make([]searchHit, 0, 64)
	for i := 0; i < 32; i++ {
		base = append(base,
			searchHit{Path: "react-native/latest.md", Source: "react-native", SourceID: "react-native", Score: 100 + i},
			searchHit{Path: "react-native__v0.79/pinned.md", Source: "react-native__v0.79", SourceID: "react-native__v0.79", Score: 50 + i},
		)
	}
	opts := searchOpts{source: "react-native", limit: len(base), versionPolicy: policy}
	scratch := make([]searchHit, len(base))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(scratch, base)
		if got := applySearchVersionPolicy("flatlist", scratch, opts); len(got) == 0 || got[0].SourceID != "react-native__v0.79" {
			b.Fatalf("unexpected top hit: %#v", got)
		}
	}
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustLoadVersionPolicyRegistry(t *testing.T) *versionPolicyRegistry {
	t.Helper()
	reg, err := loadVersionPolicyRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}
