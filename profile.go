package main

import (
	"embed"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nstranquist/docs-puller/internal/userconfig"
	"github.com/nstranquist/docs-puller/searchruntime"
	"gopkg.in/yaml.v3"
)

// Profile is a named whitelist of doc sources (with optional sub-source
// path globs) that signals the user's active tech stack. At search time,
// profile-matched candidates get a rerank boost so the canonical workspace-
// stack doc wins over equally-ranked off-stack docs as the corpus grows.
//
// Two opt-out paths preserve long-tail discoverability:
//   - --no-profile     ignore the profile entirely
//   - --strict         hard-filter to profile-matched docs only
//
// Profiles are YAML on disk: per-corpus overrides at <out>/profiles/, operator
// config at ~/.docs-puller/profiles/ or beside DOCS_PULLER_CONFIG, plus the
// embedded example profile shipped in the binary.
type Profile struct {
	Name        string          `yaml:"name"`
	Description string          `yaml:"description,omitempty"`
	Sources     []ProfileSource `yaml:"sources"`

	// derived at parse time
	wholeSources map[string]bool             // source ids with no include filter
	pathGlobs    map[string][]*regexp.Regexp // source id → compiled include patterns
}

// ProfileSource is one entry in a profile. `id` is a docs-puller source
// directory name. When `include` is empty the whole source counts as
// in-profile. When `include` has globs, only paths (relative to the source
// directory) matching at least one glob count as in-profile.
type ProfileSource struct {
	ID      string   `yaml:"id"`
	Include []string `yaml:"include,omitempty"`
}

//go:embed profiles/*.yaml
var embeddedProfiles embed.FS

// LoadProfile resolves a profile by name. Lookup order: per-machine
// override at <out>/profiles/<name>.yaml, then embedded profiles. Returns
// a parsed Profile with derived match tables ready.
func LoadProfile(name, out string) (*Profile, error) {
	if name == "" {
		return nil, searchruntime.ProfileNameRequiredError()
	}
	if data, err := readProfileFromSearchDirs(name, out); err == nil {
		return parseProfile(data, name)
	}
	embedded, err := embeddedProfiles.ReadFile("profiles/" + name + ".yaml")
	if err != nil {
		return nil, searchruntime.ProfileNotFoundError(name)
	}
	return parseProfile(embedded, name)
}

func readProfileFromSearchDirs(name, out string) ([]byte, error) {
	dirs, err := userconfig.ProfileSearchDirs(out)
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		path := filepath.Join(dir, name+".yaml")
		if data, err := os.ReadFile(path); err == nil {
			return data, nil
		}
	}
	return nil, os.ErrNotExist
}

// ListProfiles returns every profile name discoverable in the embedded
// set plus the per-machine override dir at <out>/profiles. Names are
// deduplicated and sorted; an override masks an embedded profile by
// shadowing its name.
func ListProfiles(out string) []string {
	seen := map[string]bool{}
	if entries, err := embeddedProfiles.ReadDir("profiles"); err == nil {
		for _, e := range entries {
			n := trimYAMLExt(e.Name())
			if n != "" {
				seen[n] = true
			}
		}
	}
	if dirs, err := userconfig.ProfileSearchDirs(out); err == nil {
		for _, dir := range dirs {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
					continue
				}
				if n := strings.TrimSuffix(e.Name(), ".yaml"); n != "" && n != "config" {
					seen[n] = true
				}
			}
		}
	}
	out2 := make([]string, 0, len(seen))
	for n := range seen {
		out2 = append(out2, n)
	}
	sort.Strings(out2)
	return out2
}

func trimYAMLExt(name string) string {
	return strings.TrimSuffix(name, ".yaml")
}

func parseProfile(data []byte, expectedName string) (*Profile, error) {
	var p Profile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, searchruntime.ProfileParseError(expectedName, err)
	}
	if p.Name == "" {
		p.Name = expectedName
	}
	if p.Name != expectedName {
		return nil, searchruntime.ProfileNameMismatchError(expectedName, p.Name)
	}
	if len(p.Sources) == 0 {
		return nil, searchruntime.ProfileNoSourcesError(expectedName)
	}
	p.wholeSources = map[string]bool{}
	p.pathGlobs = map[string][]*regexp.Regexp{}
	for _, s := range p.Sources {
		if s.ID == "" {
			return nil, searchruntime.ProfileSourceMissingIDError(expectedName)
		}
		if len(s.Include) == 0 {
			p.wholeSources[s.ID] = true
			continue
		}
		var compiled []*regexp.Regexp
		for _, glob := range s.Include {
			rx, err := compileGlob(glob)
			if err != nil {
				return nil, searchruntime.ProfileSourceGlobError(expectedName, s.ID, glob, err)
			}
			compiled = append(compiled, rx)
		}
		p.pathGlobs[s.ID] = compiled
	}
	return &p, nil
}

// Match reports whether (source, relPath) is in-profile and whether the
// match came from a sub-source glob (`sub`). relPath is the path relative
// to the source directory (no leading "<source>/" prefix).
//
// Returns (false, false) when the source isn't covered by the profile.
// Returns (true, false) for whole-source members (no include filter).
// Returns (true, true) when a sub-source glob matched.
func (p *Profile) Match(source, relPath string) (in bool, sub bool) {
	if p == nil {
		return false, false
	}
	if p.wholeSources[source] {
		return true, false
	}
	globs, ok := p.pathGlobs[source]
	if !ok {
		return false, false
	}
	for _, rx := range globs {
		if rx.MatchString(relPath) {
			return true, true
		}
	}
	return false, false
}

// SourceIDs returns every source id referenced by the profile (whole and
// sub-source) sorted alphabetically.
func (p *Profile) SourceIDs() []string {
	if p == nil {
		return nil
	}
	seen := map[string]bool{}
	for s := range p.wholeSources {
		seen[s] = true
	}
	for s := range p.pathGlobs {
		seen[s] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// WholeSourceIDs returns only the sources matched without a glob — these
// can be cheaply filtered at SQL level for strict mode.
func (p *Profile) WholeSourceIDs() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.wholeSources))
	for s := range p.wholeSources {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// SubSourceGlobs returns the sub-source path globs by source id.
func (p *Profile) SubSourceGlobs() map[string][]string {
	if p == nil {
		return nil
	}
	out := map[string][]string{}
	for _, s := range p.Sources {
		if len(s.Include) > 0 {
			out[s.ID] = append([]string(nil), s.Include...)
		}
	}
	return out
}

// compileGlob compiles a docs-style glob into a regexp anchored at both
// ends. Supports:
//
//	**       → zero or more path segments (greedy, may include /)
//	*        → zero or more chars within one segment (no /)
//	?        → exactly one char (no /)
//	literal  → escaped
//
// Examples:
//
//	azure/cli/**           matches azure/cli/login.md, azure/cli/storage/foo.md
//	**/index.md            matches any-depth/index.md
//	guides/*.md            matches guides/foo.md, NOT guides/sub/foo.md
func compileGlob(glob string) (*regexp.Regexp, error) {
	if glob == "" {
		return nil, searchruntime.ProfileEmptyGlobError()
	}
	var sb strings.Builder
	sb.WriteString("^")
	i := 0
	for i < len(glob) {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				sb.WriteString(".*")
				i += 2
				if i < len(glob) && glob[i] == '/' {
					i++
				}
			} else {
				sb.WriteString("[^/]*")
				i++
			}
		case '?':
			sb.WriteString("[^/]")
			i++
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
			i++
		default:
			sb.WriteByte(c)
			i++
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

// ResolveOpts is the input shape for ResolveActiveProfile.
type ResolveOpts struct {
	// FlagProfile is the value of --profile (empty when not set).
	FlagProfile string
	// FlagNoProfile is true when --no-profile was passed.
	FlagNoProfile bool
	// Out is the docs root (typically ~/code/docs); used to read <out>/.profile.
	Out string
	// Cwd is the current working directory; used for the cwd heuristic.
	Cwd string
	// Env is the process env (so tests can inject DOCS_PROFILE).
	Env func(key string) string
}

// ResolveActiveProfile picks the active profile name + reason. Returns
// ("", "", reason) when no profile is active.
//
// Precedence (first match wins):
//
//  1. --no-profile flag                         → "", reason flag-no-profile
//  2. --profile NAME flag                       → NAME, reason flag-explicit
//  3. DOCS_PROFILE env var                      → value, reason env-DOCS_PROFILE
//  4. <out>/.profile text file                  → contents, reason out-pin
//  5. cwd ancestor matches a configured workspace root → profile, reason cwd-<profile>
//  6. fallback                                  → "", reason none
func ResolveActiveProfile(o ResolveOpts) (name, reason string) {
	if o.FlagNoProfile {
		return "", "flag-no-profile"
	}
	if strings.TrimSpace(o.FlagProfile) != "" {
		return strings.TrimSpace(o.FlagProfile), "flag-explicit"
	}
	getenv := o.Env
	if getenv == nil {
		getenv = os.Getenv
	}
	if v := strings.TrimSpace(getenv("DOCS_PROFILE")); v != "" {
		return v, "env-DOCS_PROFILE"
	}
	if o.Out != "" {
		if data, err := os.ReadFile(filepath.Join(o.Out, ".profile")); err == nil {
			line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
			if line != "" {
				return line, "out-pin"
			}
		}
	}
	if name, ok := userconfig.MatchCwdProfile(o.Cwd); ok {
		return name, "cwd-" + name
	}
	return "", "none"
}
